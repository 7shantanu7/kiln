// Package tia does test impact analysis on a Ninja build graph: given changed
// files, it finds the tests whose executables can change as a result.
//
// The answer is only as good as the graph. A file a build step reads without
// declaring it (see kiln-dep) is invisible here, so changing it selects no
// tests. Run kiln-dep in CI to keep the graph honest.
package tia

import (
	"bytes"
	"encoding/json"
	"fmt"
	"os/exec"
	"path/filepath"
	"sort"
	"strings"

	"github.com/7shantanu7/kiln/internal/ninja"
)

// Test is one CTest test and the executable it runs.
type Test struct {
	Name string
	Exe  string // absolute path; empty if the command is not a built file
}

// LoadTests lists tests with `ctest --show-only=json-v1`.
func LoadTests(buildDir string) ([]Test, error) {
	cmd := exec.Command("ctest", "--show-only=json-v1")
	cmd.Dir = buildDir
	var stderr bytes.Buffer
	cmd.Stderr = &stderr
	out, err := cmd.Output()
	if err != nil {
		return nil, fmt.Errorf("ctest --show-only: %v: %s", err, stderr.String())
	}
	var doc struct {
		Tests []struct {
			Name    string   `json:"name"`
			Command []string `json:"command"`
		} `json:"tests"`
	}
	if err := json.Unmarshal(out, &doc); err != nil {
		return nil, err
	}
	tests := make([]Test, 0, len(doc.Tests))
	for _, t := range doc.Tests {
		tt := Test{Name: t.Name}
		if len(t.Command) > 0 {
			tt.Exe = filepath.Clean(t.Command[0])
		}
		tests = append(tests, tt)
	}
	return tests, nil
}

// Selection is the result of an analysis.
type Selection struct {
	Tests   []string            `json:"tests"`
	All     bool                `json:"all"`              // every test selected
	Reason  string              `json:"reason,omitempty"` // why All is set
	Unknown []string            `json:"not_build_inputs"` // changed files the graph never reads
	Why     map[string][]string `json:"why"`              // test -> changed files that reach it
}

// IsBuildConfig reports whether a change to path can alter the whole graph.
func IsBuildConfig(path string) bool {
	base := filepath.Base(path)
	return base == "CMakeLists.txt" || base == "CMakePresets.json" || strings.HasSuffix(base, ".cmake") || strings.HasSuffix(base, ".cmake.in")
}

// Select returns the tests affected by the changed files (absolute paths).
func Select(g *ninja.Graph, tests []Test, changed []string) Selection {
	sel := Selection{Why: map[string][]string{}}
	for _, c := range changed {
		if IsBuildConfig(c) {
			sel.All, sel.Reason = true, "build configuration changed: "+c
			for _, t := range tests {
				sel.Tests = append(sel.Tests, t.Name)
			}
			return sel
		}
	}
	consumers := map[string][]*ninja.Edge{}
	for _, e := range g.Edges {
		for _, in := range e.Inputs() {
			consumers[in] = append(consumers[in], e)
		}
	}
	byExe := map[string][]string{}
	for _, t := range tests {
		byExe[t.Exe] = append(byExe[t.Exe], t.Name)
	}
	for _, c := range changed {
		c = filepath.Clean(c)
		if len(consumers[c]) == 0 {
			sel.Unknown = append(sel.Unknown, c)
			continue
		}
		seen := map[string]bool{c: true}
		queue := []string{c}
		for len(queue) > 0 {
			f := queue[0]
			queue = queue[1:]
			for _, name := range byExe[f] {
				sel.Why[name] = append(sel.Why[name], c)
			}
			for _, e := range consumers[f] {
				for _, o := range e.Outputs {
					if !seen[o] {
						seen[o] = true
						queue = append(queue, o)
					}
				}
			}
		}
	}
	for name := range sel.Why {
		sel.Tests = append(sel.Tests, name)
	}
	sort.Strings(sel.Tests)
	return sel
}

// CTestRegex renders names as an anchored ctest -R expression.
func CTestRegex(names []string) string {
	if len(names) == 0 {
		return "^$"
	}
	quoted := make([]string, len(names))
	for i, n := range names {
		quoted[i] = regexpQuote(n)
	}
	return "^(" + strings.Join(quoted, "|") + ")$"
}

func regexpQuote(s string) string {
	var b strings.Builder
	for _, r := range s {
		if strings.ContainsRune(`\.+*?()|[]{}^$`, r) {
			b.WriteByte('\\')
		}
		b.WriteRune(r)
	}
	return b.String()
}
