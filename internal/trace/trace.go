// Package trace analyses where build time goes: the critical path through the
// Ninja graph, Clang -ftime-trace breakdowns, and build-time regressions.
package trace

import (
	"encoding/json"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"sort"
	"strings"

	"github.com/7shantanu7/kiln/internal/ninja"
)

// Step is one edge with its timing from .ninja_log.
type Step struct {
	Output     string `json:"output"` // relative to the build directory
	Rule       string `json:"rule"`
	DurationMs int64  `json:"duration_ms"`
	StartMs    int64  `json:"start_ms"`
}

// Summary describes one build.
type Summary struct {
	Steps          int              `json:"steps"`
	WallMs         int64            `json:"wall_ms"`          // first start to last end
	CPUMs          int64            `json:"cpu_ms"`           // sum of step durations
	CriticalPathMs int64            `json:"critical_path_ms"` // lower bound on wall time with unlimited cores
	Parallelism    float64          `json:"parallelism"`      // cpu / wall
	ByRule         map[string]int64 `json:"cpu_ms_by_rule_kind"`
	CriticalPath   []Step           `json:"critical_path"`
	Slowest        []Step           `json:"slowest"`
}

// ruleKind collapses CMake's per-target rule names ("CXX_COMPILER__lib_0003_Release")
// into kinds ("compile", "archive", "link", "custom").
func ruleKind(rule string) string {
	switch {
	case strings.Contains(rule, "COMPILER"):
		return "compile"
	case strings.Contains(rule, "STATIC_LIBRARY"):
		return "archive"
	case strings.Contains(rule, "LINKER"):
		return "link"
	case strings.HasPrefix(rule, "CUSTOM_COMMAND"):
		return "custom"
	default:
		return strings.ToLower(rule)
	}
}

// Analyze joins the graph with the log. Only edges present in the log count,
// so run it on a clean build for a complete picture.
func Analyze(g *ninja.Graph, log map[string]ninja.LogEntry, top int) Summary {
	rel := func(p string) string {
		if r, err := filepath.Rel(g.Dir, p); err == nil && !strings.HasPrefix(r, "..") {
			return r
		}
		return p
	}
	entry := map[*ninja.Edge]ninja.LogEntry{}
	for _, e := range g.Edges {
		for _, o := range e.Outputs {
			if l, ok := log[o]; ok {
				entry[e] = l
				break
			}
		}
	}
	s := Summary{ByRule: map[string]int64{}}
	var first, last int64 = -1, 0
	var steps []Step
	for e, l := range entry {
		d := l.Duration()
		s.Steps++
		s.CPUMs += d
		s.ByRule[ruleKind(e.Rule)] += d
		if first < 0 || l.StartMs < first {
			first = l.StartMs
		}
		if l.EndMs > last {
			last = l.EndMs
		}
		steps = append(steps, Step{Output: rel(e.Outputs[0]), Rule: e.Rule, DurationMs: d, StartMs: l.StartMs})
	}
	if s.Steps == 0 {
		return s
	}
	s.WallMs = last - first
	if s.WallMs > 0 {
		s.Parallelism = float64(s.CPUMs) / float64(s.WallMs)
	}
	sort.Slice(steps, func(i, j int) bool { return steps[i].DurationMs > steps[j].DurationMs })
	if len(steps) > top {
		steps = steps[:top]
	}
	s.Slowest = steps

	// Longest path: finish(e) = duration(e) + max finish over the edges that
	// produce its inputs (order-only included, since they gate scheduling).
	finish := map[*ninja.Edge]int64{}
	prev := map[*ninja.Edge]*ninja.Edge{}
	state := map[*ninja.Edge]int{} // 1 visiting, 2 done
	var visit func(e *ninja.Edge) int64
	visit = func(e *ninja.Edge) int64 {
		if state[e] == 2 {
			return finish[e]
		}
		if state[e] == 1 {
			return 0 // cycle guard; Ninja graphs are acyclic
		}
		state[e] = 1
		var best int64
		var bestEdge *ninja.Edge
		for _, in := range e.StaticInputs() {
			p, ok := g.Producer[in]
			if !ok {
				continue
			}
			if f := visit(p); f > best || bestEdge == nil && f == best {
				best, bestEdge = f, p
			}
		}
		finish[e] = best + entry[e].Duration()
		prev[e] = bestEdge
		state[e] = 2
		return finish[e]
	}
	var end *ninja.Edge
	for _, e := range g.Edges {
		if f := visit(e); end == nil || f > finish[end] {
			end = e
		}
	}
	s.CriticalPathMs = finish[end]
	for e := end; e != nil; e = prev[e] {
		if l, ok := entry[e]; ok && !e.Phony() {
			s.CriticalPath = append(s.CriticalPath, Step{Output: rel(e.Outputs[0]), Rule: e.Rule, DurationMs: l.Duration(), StartMs: l.StartMs})
		}
	}
	// Report the path in build order.
	for i, j := 0, len(s.CriticalPath)-1; i < j; i, j = i+1, j-1 {
		s.CriticalPath[i], s.CriticalPath[j] = s.CriticalPath[j], s.CriticalPath[i]
	}
	return s
}

// Regression is one metric that grew past the allowed threshold.
type Regression struct {
	Metric            string
	Baseline, Current int64
	Percent           float64
}

// Compare returns the metrics in cur that exceed base by more than maxPercent.
// Metrics below minMs in the baseline are ignored as noise.
func Compare(base, cur Summary, maxPercent float64, minMs int64) []Regression {
	type m struct {
		name string
		b, c int64
	}
	ms := []m{{"cpu_ms", base.CPUMs, cur.CPUMs}, {"critical_path_ms", base.CriticalPathMs, cur.CriticalPathMs}}
	kinds := map[string]bool{}
	for k := range base.ByRule {
		kinds[k] = true
	}
	for k := range cur.ByRule {
		kinds[k] = true
	}
	var names []string
	for k := range kinds {
		names = append(names, k)
	}
	sort.Strings(names)
	for _, k := range names {
		ms = append(ms, m{"cpu_ms." + k, base.ByRule[k], cur.ByRule[k]})
	}
	var out []Regression
	for _, x := range ms {
		if x.b < minMs {
			continue
		}
		pct := 100 * float64(x.c-x.b) / float64(x.b)
		if pct > maxPercent {
			out = append(out, Regression{x.name, x.b, x.c, pct})
		}
	}
	return out
}

// TimeTraceReport aggregates Clang -ftime-trace files.
type TimeTraceReport struct {
	Files      int
	TotalMs    float64
	FrontendMs float64
	BackendMs  float64
	Headers    []Cost // inclusive parse time per header, summed over TUs
	Templates  []Cost // instantiation time per template
	SlowestTUs []Cost
}

// Cost is a named duration with how many times it occurred.
type Cost struct {
	Name  string
	Ms    float64
	Count int
}

type traceFile struct {
	TraceEvents []struct {
		Name string  `json:"name"`
		Ph   string  `json:"ph"`
		Dur  float64 `json:"dur"` // microseconds
		Args struct {
			Detail string `json:"detail"`
		} `json:"args"`
	} `json:"traceEvents"`
}

// ReadTimeTraces walks dir for Clang time-trace JSON files (those next to
// object files, named *.json with a "traceEvents" array) and aggregates them.
func ReadTimeTraces(dir string, top int) (*TimeTraceReport, error) {
	r := &TimeTraceReport{}
	headers := map[string]*Cost{}
	templates := map[string]*Cost{}
	var tus []Cost
	err := filepath.WalkDir(dir, func(p string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() || !strings.HasSuffix(p, ".json") || !strings.Contains(p, "CMakeFiles") {
			return nil
		}
		b, err := os.ReadFile(p)
		if err != nil || !strings.Contains(string(b[:min(len(b), 64)]), "traceEvents") {
			return nil
		}
		var tf traceFile
		if json.Unmarshal(b, &tf) != nil {
			return nil
		}
		r.Files++
		for _, ev := range tf.TraceEvents {
			ms := ev.Dur / 1000
			switch ev.Name {
			case "Total ExecuteCompiler":
				r.TotalMs += ms
				tus = append(tus, Cost{Name: strings.TrimSuffix(p, ".json"), Ms: ms, Count: 1})
			case "Total Frontend":
				r.FrontendMs += ms
			case "Total Backend":
				r.BackendMs += ms
			case "Source":
				add(headers, ev.Args.Detail, ms)
			case "InstantiateClass", "InstantiateFunction":
				add(templates, ev.Args.Detail, ms)
			}
		}
		return nil
	})
	if err != nil {
		return nil, err
	}
	r.Headers = topN(headers, top)
	r.Templates = topN(templates, top)
	sort.Slice(tus, func(i, j int) bool { return tus[i].Ms > tus[j].Ms })
	if len(tus) > top {
		tus = tus[:top]
	}
	r.SlowestTUs = tus
	return r, nil
}

func add(m map[string]*Cost, name string, ms float64) {
	c, ok := m[name]
	if !ok {
		c = &Cost{Name: name}
		m[name] = c
	}
	c.Ms += ms
	c.Count++
}

func topN(m map[string]*Cost, n int) []Cost {
	out := make([]Cost, 0, len(m))
	for _, c := range m {
		out = append(out, *c)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Ms > out[j].Ms })
	if len(out) > n {
		out = out[:n]
	}
	return out
}

// FormatMs renders milliseconds as "1m 02.3s" or "843ms".
func FormatMs(ms int64) string {
	if ms < 1000 {
		return fmt.Sprintf("%dms", ms)
	}
	if ms < 60000 {
		return fmt.Sprintf("%.1fs", float64(ms)/1000)
	}
	return fmt.Sprintf("%dm %04.1fs", ms/60000, float64(ms%60000)/1000)
}
