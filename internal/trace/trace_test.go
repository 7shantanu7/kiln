package trace

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/7shantanu7/kiln/internal/ninja"
)

func TestCriticalPath(t *testing.T) {
	// a.o (100ms) and b.o (300ms) feed lib.a (10ms); c.o (350ms) is independent.
	edges := []*ninja.Edge{
		{Rule: "CXX_COMPILER__x", Outputs: []string{"/b/a.o"}},
		{Rule: "CXX_COMPILER__x", Outputs: []string{"/b/b.o"}},
		{Rule: "CXX_STATIC_LIBRARY_LINKER__x", Outputs: []string{"/b/lib.a"}, Explicit: []string{"/b/a.o", "/b/b.o"}},
		{Rule: "CXX_COMPILER__y", Outputs: []string{"/b/c.o"}},
	}
	g := &ninja.Graph{Dir: "/b", Edges: edges, Producer: map[string]*ninja.Edge{}}
	for _, e := range edges {
		g.Producer[e.Outputs[0]] = e
	}
	log := map[string]ninja.LogEntry{
		"/b/a.o":   {StartMs: 0, EndMs: 100},
		"/b/b.o":   {StartMs: 0, EndMs: 300},
		"/b/lib.a": {StartMs: 300, EndMs: 310},
		"/b/c.o":   {StartMs: 100, EndMs: 450},
	}
	s := Analyze(g, log, 2)
	if s.CriticalPathMs != 350 || s.CriticalPath[0].Output != "c.o" {
		t.Fatalf("critical path %dms %v, want 350ms through c.o", s.CriticalPathMs, s.CriticalPath)
	}
	if s.CPUMs != 760 || s.WallMs != 450 || s.ByRule["compile"] != 750 || s.ByRule["archive"] != 10 {
		t.Fatalf("summary %+v", s)
	}
	log["/b/c.o"] = ninja.LogEntry{StartMs: 100, EndMs: 200}
	s = Analyze(g, log, 2)
	if s.CriticalPathMs != 310 || len(s.CriticalPath) != 2 || s.CriticalPath[0].Output != "b.o" {
		t.Fatalf("critical path %dms %v, want b.o -> lib.a = 310ms", s.CriticalPathMs, s.CriticalPath)
	}
}

func TestCompare(t *testing.T) {
	base := Summary{CPUMs: 10000, CriticalPathMs: 4000, ByRule: map[string]int64{"compile": 9000, "link": 500}}
	cur := Summary{CPUMs: 13000, CriticalPathMs: 4100, ByRule: map[string]int64{"compile": 12000, "link": 900}}
	regs := Compare(base, cur, 25, 1000)
	if len(regs) != 2 || regs[0].Metric != "cpu_ms" || regs[1].Metric != "cpu_ms.compile" {
		t.Fatalf("regressions %+v (link is below min-ms and must be ignored)", regs)
	}
}

func TestReadTimeTraces(t *testing.T) {
	// a.cpp includes x.h (10ms), which includes y.h (4ms); Clang writes
	// children first. The CMake compiler-id trace must be ignored.
	dir := t.TempDir()
	write := func(rel, body string) {
		p := filepath.Join(dir, rel)
		if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(p, []byte(body), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	write("lib/CMakeFiles/lib.dir/a.cpp.json", `{"traceEvents":[
	  {"ph":"X","tid":1,"ts":2000,"dur":4000,"name":"Source","args":{"detail":"/inc/sub/../y.h"}},
	  {"ph":"X","tid":1,"ts":1000,"dur":10000,"name":"Source","args":{"detail":"/inc/x.h"}},
	  {"ph":"X","tid":1,"ts":12000,"dur":1000,"name":"Source","args":{"detail":"/inc/z.h"}},
	  {"ph":"X","tid":2,"ts":0,"dur":50000,"name":"Total ExecuteCompiler"},
	  {"ph":"X","tid":3,"ts":0,"dur":30000,"name":"Total Frontend"}]}`)
	write("CMakeFiles/3.28.3/CompilerIdCXX/a-CMakeCXXCompilerId.json", `{"traceEvents":[
	  {"ph":"X","tid":2,"ts":0,"dur":50000,"name":"Total ExecuteCompiler"}]}`)
	r, err := ReadTimeTraces(dir, 10)
	if err != nil {
		t.Fatal(err)
	}
	if r.Files != 1 || r.TotalMs != 50 || r.FrontendMs != 30 {
		t.Fatalf("report %+v", r)
	}
	self := map[string]float64{}
	for _, c := range r.HeaderSelf {
		self[c.Name] = c.Ms
	}
	if self["/inc/x.h"] != 6 || self["/inc/y.h"] != 4 || self["/inc/z.h"] != 1 {
		t.Fatalf("self times %v, want x.h 6ms, y.h 4ms, z.h 1ms", self)
	}
	if r.Headers[0].Name != "/inc/x.h" || r.Headers[0].Ms != 10 {
		t.Fatalf("inclusive %+v", r.Headers)
	}
}
