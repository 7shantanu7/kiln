package tia

import (
	"reflect"
	"testing"

	"github.com/7shantanu7/kiln/internal/ninja"
)

// graph: a.cpp -> a.o -> liba.a -> {a_test, b_test}; b.cpp -> b.o -> b_test;
// a.h is a depfile dependency of a.o only.
func testGraph() *ninja.Graph {
	edges := []*ninja.Edge{
		{Rule: "CXX_COMPILER", Outputs: []string{"/b/a.o"}, Explicit: []string{"/s/a.cpp"}, Deps: []string{"/s/a.h"}},
		{Rule: "CXX_STATIC_LIBRARY_LINKER", Outputs: []string{"/b/liba.a"}, Explicit: []string{"/b/a.o"}},
		{Rule: "CXX_COMPILER", Outputs: []string{"/b/b.o"}, Explicit: []string{"/s/b.cpp"}, OrderOnly: []string{"/b/liba.a"}},
		{Rule: "CXX_EXECUTABLE_LINKER", Outputs: []string{"/b/a_test"}, Explicit: []string{"/b/liba.a"}},
		{Rule: "CXX_EXECUTABLE_LINKER", Outputs: []string{"/b/b_test"}, Explicit: []string{"/b/b.o"}, Implicit: []string{"/b/liba.a"}},
	}
	g := &ninja.Graph{Dir: "/b", Edges: edges, Producer: map[string]*ninja.Edge{}}
	for _, e := range edges {
		g.Producer[e.Outputs[0]] = e
	}
	return g
}

var tests = []Test{{"a_test", "/b/a_test"}, {"b_test", "/b/b_test"}}

func TestSelect(t *testing.T) {
	g := testGraph()
	cases := []struct {
		changed []string
		want    []string
	}{
		{[]string{"/s/a.h"}, []string{"a_test", "b_test"}},
		{[]string{"/s/b.cpp"}, []string{"b_test"}},
		{[]string{"/s/README.md"}, nil},
		{[]string{"/s/CMakeLists.txt"}, []string{"a_test", "b_test"}},
	}
	for _, c := range cases {
		got := Select(g, tests, c.changed).Tests
		if !reflect.DeepEqual(got, c.want) {
			t.Errorf("Select(%v) = %v, want %v", c.changed, got, c.want)
		}
	}
}

func TestOrderOnlyDoesNotPropagate(t *testing.T) {
	// b.o has an order-only dependency on liba.a: rebuilding liba.a must not
	// select b_test through b.o, only through b_test's implicit link input.
	g := testGraph()
	g.Edges[4].Implicit = nil
	if got := Select(g, tests, []string{"/s/a.cpp"}).Tests; !reflect.DeepEqual(got, []string{"a_test"}) {
		t.Fatalf("got %v", got)
	}
}

func TestCTestRegex(t *testing.T) {
	if got := CTestRegex([]string{"a.b", "c"}); got != `^(a\.b|c)$` {
		t.Fatalf("got %s", got)
	}
	if got := CTestRegex(nil); got != "^$" {
		t.Fatalf("got %s", got)
	}
}
