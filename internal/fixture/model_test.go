package fixture

import (
	"os"
	"path/filepath"
	"reflect"
	"testing"
)

func smallConfig() Config {
	c := DefaultConfig()
	c.Libs, c.Modules, c.TestReps = 30, 3, 1
	c.MissingDeps = 4
	return c
}

func TestBuildIsDeterministic(t *testing.T) {
	a, err := Build(smallConfig())
	if err != nil {
		t.Fatal(err)
	}
	b, _ := Build(smallConfig())
	if !reflect.DeepEqual(a.Injected, b.Injected) {
		t.Fatal("same seed produced different injections")
	}
	for i := range a.Libs {
		if a.Eval(a.Libs[i], 1, 42) != b.Eval(b.Libs[i], 1, 42) {
			t.Fatalf("lib %d evaluates differently for the same seed", i)
		}
	}
}

func TestInjectedRacesAreReallyUndeclared(t *testing.T) {
	r, err := Build(smallConfig())
	if err != nil {
		t.Fatal(err)
	}
	races := 0
	for _, l := range r.Libs {
		for _, g := range l.RacyGen {
			races++
			if r.dependsOn(l.Index, g) {
				t.Errorf("%s depends on %s, so its include is declared", l.Name, r.Libs[g].Name)
			}
		}
	}
	if races != 2 || len(r.Shared) != 2 {
		t.Fatalf("want 2 races and 2 undeclared codegen inputs, got %d and %d", races, len(r.Shared))
	}
}

func TestDependenciesPointBackwards(t *testing.T) {
	r, _ := Build(smallConfig())
	for _, l := range r.Libs {
		for _, d := range l.Deps {
			if d >= l.Index || d < l.Index-r.Cfg.DepWindow {
				t.Fatalf("%s depends on lib %d outside its window", l.Name, d)
			}
		}
	}
}

func TestWriteLayout(t *testing.T) {
	cfg := smallConfig()
	cfg.Nondet = map[string]bool{"codegen-order": true}
	r, _ := Build(cfg)
	dir := t.TempDir()
	if err := r.Write(dir); err != nil {
		t.Fatal(err)
	}
	for _, p := range []string{"CMakeLists.txt", "tools/codegen.py", "kiln-fixture.json",
		"libs/lib_0001/CMakeLists.txt", "libs/lib_0001/src/m02.cpp", "libs/lib_0001/test/lib_0001_test.cpp"} {
		if _, err := os.Stat(filepath.Join(dir, p)); err != nil {
			t.Errorf("missing %s", p)
		}
	}
}
