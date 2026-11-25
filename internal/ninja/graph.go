// Package ninja loads a Ninja build graph, its recorded depfile dependencies
// and its build log, using only Ninja's own query tools (no build.ninja parsing).
package ninja

import (
	"bufio"
	"bytes"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
)

// Edge is one build statement.
type Edge struct {
	Rule      string
	Outputs   []string // absolute paths
	Explicit  []string
	Implicit  []string
	OrderOnly []string
	// Deps are the dependencies Ninja recorded from depfiles (ninja -t deps).
	Deps []string
}

// Phony reports whether the edge is a phony alias.
func (e *Edge) Phony() bool { return e.Rule == "phony" }

// Inputs returns explicit, implicit and depfile inputs: everything that can
// change the edge's outputs. Order-only inputs are excluded.
func (e *Edge) Inputs() []string {
	out := make([]string, 0, len(e.Explicit)+len(e.Implicit)+len(e.Deps))
	out = append(out, e.Explicit...)
	out = append(out, e.Implicit...)
	return append(out, e.Deps...)
}

// AllInputs also includes order-only inputs: everything that must be built
// before the edge runs.
func (e *Edge) AllInputs() []string {
	return append(e.Inputs(), e.OrderOnly...)
}

// Graph is the loaded build graph.
type Graph struct {
	Dir      string // absolute build directory
	Edges    []*Edge
	Producer map[string]*Edge // output path -> edge

	keys map[string]*Edge // merges outputs of multi-output edges
}

// Abs makes a Ninja path absolute relative to the build directory.
func (g *Graph) Abs(p string) string {
	if filepath.IsAbs(p) {
		return filepath.Clean(p)
	}
	return filepath.Join(g.Dir, p)
}

func ninjaTool(dir string, args ...string) ([]byte, error) {
	cmd := exec.Command("ninja", append([]string{"-C", dir}, args...)...)
	var stderr bytes.Buffer
	cmd.Stderr = &stderr
	out, err := cmd.Output()
	if err != nil {
		return nil, fmt.Errorf("ninja %s: %v: %s", strings.Join(args, " "), err, strings.TrimSpace(stderr.String()))
	}
	// "ninja -C dir" prints "ninja: Entering directory" on stdout for some tools.
	out = bytes.TrimPrefix(out, []byte(fmt.Sprintf("ninja: Entering directory `%s'\n", dir)))
	return out, nil
}

// Load reads every edge with `ninja -t targets all` and `ninja -t query`, and
// the recorded depfile dependencies with `ninja -t deps`.
func Load(dir string) (*Graph, error) {
	abs, err := filepath.Abs(dir)
	if err != nil {
		return nil, err
	}
	g := &Graph{Dir: abs, Producer: map[string]*Edge{}, keys: map[string]*Edge{}}
	out, err := ninjaTool(abs, "-t", "targets", "all")
	if err != nil {
		return nil, err
	}
	var targets []string
	for _, line := range strings.Split(string(out), "\n") {
		if i := strings.LastIndex(line, ": "); i > 0 {
			targets = append(targets, line[:i])
		}
	}
	const batch = 500
	for i := 0; i < len(targets); i += batch {
		j := min(i+batch, len(targets))
		q, err := ninjaTool(abs, append([]string{"-t", "query"}, targets[i:j]...)...)
		if err != nil {
			return nil, err
		}
		g.parseQuery(q)
	}
	if err := g.loadDeps(); err != nil {
		return nil, err
	}
	return g, nil
}

// parseQuery reads `ninja -t query` output:
//
//	target:
//	  input: RULE
//	    explicit
//	    | implicit
//	    || order-only
//	  outputs:
//	    consumer
func (g *Graph) parseQuery(q []byte) {
	sc := bufio.NewScanner(bytes.NewReader(q))
	sc.Buffer(make([]byte, 1<<20), 1<<26)
	var target string
	var edge *Edge
	section := ""
	flush := func() {
		if edge != nil && target != "" {
			out := g.Abs(target)
			if _, seen := g.Producer[out]; !seen {
				// Outputs of one multi-output edge are queried separately;
				// merge them by identical rule and inputs.
				key := edgeKey(edge)
				if prev, ok := g.keys[key]; ok && !edge.Phony() && len(edge.Explicit)+len(edge.Implicit) > 0 {
					prev.Outputs = append(prev.Outputs, out)
					g.Producer[out] = prev
				} else {
					edge.Outputs = []string{out}
					g.Edges = append(g.Edges, edge)
					g.Producer[out] = edge
					g.keys[key] = edge
				}
			}
		}
		edge, target, section = nil, "", ""
	}
	for sc.Scan() {
		line := sc.Text()
		switch {
		case line == "":
		case !strings.HasPrefix(line, " "):
			flush()
			target = strings.TrimSuffix(line, ":")
		case strings.HasPrefix(line, "  input: "):
			edge = &Edge{Rule: strings.TrimPrefix(line, "  input: ")}
			section = "input"
		case strings.HasPrefix(line, "  outputs:"):
			section = "outputs"
		case strings.HasPrefix(line, "  validations:"):
			section = "validations"
		case strings.HasPrefix(line, "    ") && section == "input" && edge != nil:
			p := strings.TrimPrefix(line, "    ")
			switch {
			case strings.HasPrefix(p, "|| "):
				edge.OrderOnly = append(edge.OrderOnly, g.Abs(p[3:]))
			case strings.HasPrefix(p, "| "):
				edge.Implicit = append(edge.Implicit, g.Abs(p[2:]))
			default:
				edge.Explicit = append(edge.Explicit, g.Abs(p))
			}
		}
	}
	flush()
}

func edgeKey(e *Edge) string {
	return e.Rule + "\x00" + strings.Join(e.Explicit, "\x00") + "\x01" + strings.Join(e.Implicit, "\x00")
}

// loadDeps attaches depfile dependencies recorded in .ninja_deps.
func (g *Graph) loadDeps() error {
	out, err := ninjaTool(g.Dir, "-t", "deps")
	if err != nil {
		return err
	}
	var cur *Edge
	for _, line := range strings.Split(string(out), "\n") {
		switch {
		case line == "":
			cur = nil
		case !strings.HasPrefix(line, " "):
			cur = nil
			if i := strings.Index(line, ": #deps"); i > 0 {
				cur = g.Producer[g.Abs(line[:i])]
			}
		case cur != nil:
			cur.Deps = append(cur.Deps, g.Abs(strings.TrimSpace(line)))
		}
	}
	for _, e := range g.Edges {
		e.Deps = dedupe(e.Deps)
	}
	return nil
}

func dedupe(xs []string) []string {
	if len(xs) < 2 {
		return xs
	}
	sort.Strings(xs)
	out := xs[:1]
	for _, x := range xs[1:] {
		if x != out[len(out)-1] {
			out = append(out, x)
		}
	}
	return out
}

// IsGenerated reports whether path is produced by a non-phony edge.
func (g *Graph) IsGenerated(path string) bool {
	e, ok := g.Producer[path]
	return ok && !e.Phony()
}

// StaticInputs returns explicit, implicit and order-only inputs: the inputs
// that order a clean build. Depfile deps are not known until after a step
// first runs, so they cannot order it.
func (e *Edge) StaticInputs() []string {
	out := make([]string, 0, len(e.Explicit)+len(e.Implicit)+len(e.OrderOnly))
	out = append(out, e.Explicit...)
	out = append(out, e.Implicit...)
	return append(out, e.OrderOnly...)
}

// Upstream returns every edge guaranteed to finish before e starts in a
// clean build, following static inputs only.
func (g *Graph) Upstream(e *Edge) map[*Edge]bool {
	seen := map[*Edge]bool{}
	var walk func(*Edge)
	walk = func(e *Edge) {
		for _, in := range e.StaticInputs() {
			if p, ok := g.Producer[in]; ok && !seen[p] {
				seen[p] = true
				walk(p)
			}
		}
	}
	walk(e)
	return seen
}

// LogEntry is one line of .ninja_log.
type LogEntry struct {
	StartMs, EndMs int64
	Output         string // absolute
}

// Duration returns the edge's wall time in milliseconds.
func (l LogEntry) Duration() int64 { return l.EndMs - l.StartMs }

// ReadLog parses .ninja_log (format v5 or v6) and returns the most recent
// entry per output. Entries are only meaningful for the latest build when
// that build was a clean one.
func ReadLog(dir string) (map[string]LogEntry, error) {
	abs, _ := filepath.Abs(dir)
	f, err := os.Open(filepath.Join(abs, ".ninja_log"))
	if err != nil {
		return nil, err
	}
	defer f.Close()
	out := map[string]LogEntry{}
	sc := bufio.NewScanner(f)
	sc.Buffer(make([]byte, 1<<20), 1<<24)
	for sc.Scan() {
		line := sc.Text()
		if strings.HasPrefix(line, "#") {
			if !strings.HasPrefix(line, "# ninja log v5") && !strings.HasPrefix(line, "# ninja log v6") {
				return nil, fmt.Errorf("unsupported %s", line)
			}
			continue
		}
		f := strings.Split(line, "\t")
		if len(f) < 4 {
			continue
		}
		start, err1 := strconv.ParseInt(f[0], 10, 64)
		end, err2 := strconv.ParseInt(f[1], 10, 64)
		if err1 != nil || err2 != nil {
			continue
		}
		p := f[3]
		if !filepath.IsAbs(p) {
			p = filepath.Join(abs, p)
		}
		out[p] = LogEntry{StartMs: start, EndMs: end, Output: p}
	}
	return out, sc.Err()
}
