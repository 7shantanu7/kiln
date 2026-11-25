// Package fixture generates a synthetic CMake + Ninja C++ monorepo.
//
// Every value the generated C++ computes is also computed here in Go, so the
// generated tests check real expected values instead of just "it ran".
package fixture

import (
	"fmt"
	"math/rand"
	"sort"
	"strconv"
)

// Config controls the shape of the generated monorepo.
type Config struct {
	Seed        int64
	Libs        int // number of static libraries; each also gets one test executable
	Modules     int // translation units per library
	MaxDeps     int // max direct library dependencies per library
	DepWindow   int // a library may only depend on the previous DepWindow libraries
	CodegenEach int // every Nth library runs the codegen tool (0 = never)
	TestReps    int // how many times each test repeats its checks (sets test runtime)

	// MissingDeps injects this many undeclared dependencies, alternating between
	// a generated-header race and a codegen input missing from DEPENDS.
	MissingDeps int

	// Nondet selects which of the reproducibility bugs in NondetKinds to
	// inject. An empty set gives a fully reproducible fixture.
	Nondet map[string]bool
}

// NondetKinds are the reproducibility bugs the fixture can inject. A sixth
// source, absolute paths in DWARF debug info, comes from any -g build and is
// not a fixture option.
var NondetKinds = []string{
	"date-time",           // __DATE__/__TIME__ in a build-info string
	"configure-timestamp", // string(TIMESTAMP) baked into a configured header
	"file-macro",          // __FILE__ (an absolute path) in check messages
	"codegen-order",       // Python codegen iterates a set (hash-randomized order)
	"configured-abspath",  // configure_file bakes ${CMAKE_BINARY_DIR} into a header
}

// Has reports whether reproducibility bug kind is injected.
func (c Config) Has(kind string) bool { return c.Nondet[kind] }

// DefaultConfig is the benchmark shape: 250 libraries + 250 tests = 500 targets.
func DefaultConfig() Config {
	return Config{Seed: 1, Libs: 250, Modules: 16, MaxDeps: 3, DepWindow: 40, CodegenEach: 10, TestReps: 2000}
}

// Lib is one generated static library.
type Lib struct {
	Index   int
	Name    string
	Seed    uint64
	Deps    []int // direct dependencies (indices of earlier libs)
	Codegen *Codegen
	// RacyGen lists codegen libraries whose generated header this library
	// includes without depending on them (injected missing dependency, type A).
	RacyGen []int
}

// Codegen describes the .def file a library feeds to the codegen tool.
type Codegen struct {
	Entries []Entry
	// SharedInclude, when non-empty, is a shared .def file read by the codegen
	// tool but not listed in the custom command's DEPENDS (type B).
	SharedInclude string
}

// Entry is one name/value pair in a .def file.
type Entry struct {
	Name  string
	Value uint64
}

// Repo is the full generated model.
type Repo struct {
	Cfg    Config
	Libs   []*Lib
	Shared map[string][]Entry // shared .def files by base name
	// Injected records every injected bug so tests and docs can compare
	// against what the kiln tools report.
	Injected []Injection
}

// Injection records one deliberately injected missing dependency.
type Injection struct {
	Kind     string // "generated-header-race" or "undeclared-codegen-input"
	Consumer string
	Producer string
}

func splitmix64(x uint64) uint64 {
	x += 0x9E3779B97F4A7C15
	z := x
	z = (z ^ (z >> 30)) * 0xBF58476D1CE4E5B9
	z = (z ^ (z >> 27)) * 0x94D049BB133111EB
	return z ^ (z >> 31)
}

// Build lays out libraries, dependencies and injections deterministically from cfg.Seed.
func Build(cfg Config) (*Repo, error) {
	if cfg.Libs < 2 || cfg.Modules < 2 {
		return nil, fmt.Errorf("need at least 2 libs and 2 modules")
	}
	rng := rand.New(rand.NewSource(cfg.Seed))
	r := &Repo{Cfg: cfg, Shared: map[string][]Entry{}}
	for i := 0; i < cfg.Libs; i++ {
		l := &Lib{Index: i, Name: fmt.Sprintf("lib_%04d", i), Seed: splitmix64(uint64(cfg.Seed)<<32 ^ uint64(i))}
		if i > 0 {
			lo := i - cfg.DepWindow
			if lo < 0 {
				lo = 0
			}
			n := 1 + rng.Intn(cfg.MaxDeps)
			seen := map[int]bool{}
			for len(l.Deps) < n && len(seen) < i-lo {
				d := lo + rng.Intn(i-lo)
				if !seen[d] {
					seen[d] = true
					l.Deps = append(l.Deps, d)
				}
			}
			sort.Ints(l.Deps)
		}
		if cfg.CodegenEach > 0 && i%cfg.CodegenEach == cfg.CodegenEach/2 {
			cg := &Codegen{}
			for k := 0; k < 6+rng.Intn(6); k++ {
				cg.Entries = append(cg.Entries, Entry{Name: fmt.Sprintf("K%02d", k), Value: uint64(rng.Int63n(1 << 40))})
			}
			l.Codegen = cg
		}
		r.Libs = append(r.Libs, l)
	}
	if err := r.inject(rng); err != nil {
		return nil, err
	}
	return r, nil
}

func (r *Repo) codegenLibs() []*Lib {
	var out []*Lib
	for _, l := range r.Libs {
		if l.Codegen != nil {
			out = append(out, l)
		}
	}
	return out
}

// dependsOn reports whether lib a depends on lib b directly or transitively.
func (r *Repo) dependsOn(a, b int) bool {
	seen := map[int]bool{}
	var walk func(int) bool
	walk = func(i int) bool {
		if i == b {
			return true
		}
		if seen[i] {
			return false
		}
		seen[i] = true
		for _, d := range r.Libs[i].Deps {
			if walk(d) {
				return true
			}
		}
		return false
	}
	return walk(a)
}

func (r *Repo) inject(rng *rand.Rand) error {
	gens := r.codegenLibs()
	if r.Cfg.MissingDeps > 0 && len(gens) == 0 {
		return fmt.Errorf("missing-deps needs codegen libraries (set codegen-each)")
	}
	usedB := map[int]bool{}
	for n := 0; n < r.Cfg.MissingDeps; n++ {
		if n%2 == 0 {
			// Type A: a later library includes a generated header from an
			// earlier codegen library it does not depend on.
			placed := false
			for try := 0; try < 1000 && !placed; try++ {
				g := gens[rng.Intn(len(gens))]
				j := g.Index + 1 + rng.Intn(len(r.Libs)-g.Index-1+1)
				if j >= len(r.Libs) || j == g.Index || r.dependsOn(j, g.Index) || contains(r.Libs[j].RacyGen, g.Index) {
					continue
				}
				r.Libs[j].RacyGen = append(r.Libs[j].RacyGen, g.Index)
				r.Injected = append(r.Injected, Injection{"generated-header-race", r.Libs[j].Name, g.Name})
				placed = true
			}
			if !placed {
				return fmt.Errorf("could not place generated-header race #%d", n)
			}
		} else {
			// Type B: a codegen library's .def includes a shared .def file the
			// custom command does not declare.
			placed := false
			for _, g := range gens {
				if usedB[g.Index] {
					continue
				}
				usedB[g.Index] = true
				name := fmt.Sprintf("common_%02d.def", len(r.Shared))
				var es []Entry
				for k := 0; k < 3; k++ {
					es = append(es, Entry{Name: fmt.Sprintf("S%02d_%d", len(r.Shared), k), Value: uint64(rng.Int63n(1 << 40))})
				}
				r.Shared[name] = es
				g.Codegen.SharedInclude = name
				r.Injected = append(r.Injected, Injection{"undeclared-codegen-input", g.Name, "libs/shared/" + name})
				placed = true
				break
			}
			if !placed {
				return fmt.Errorf("not enough codegen libraries for %d missing deps", r.Cfg.MissingDeps)
			}
		}
	}
	return nil
}

func contains(xs []int, x int) bool {
	for _, v := range xs {
		if v == x {
			return true
		}
	}
	return false
}

// moduleConsts returns the per-module multiplier (odd) and offset.
func (l *Lib) moduleConsts(m int) (k1, k2 uint64) {
	return splitmix64(l.Seed+uint64(2*m+1)) | 1, splitmix64(l.Seed + uint64(2*m+2))
}

// GenSum is the order-independent sum the codegen tool emits for lib l.
func (r *Repo) GenSum(l *Lib) uint64 {
	var s uint64
	for _, e := range l.Codegen.Entries {
		s += e.Value
	}
	if l.Codegen.SharedInclude != "" {
		for _, e := range r.Shared[l.Codegen.SharedInclude] {
			s += e.Value
		}
	}
	return s
}

// Eval computes lib_<i>_m<m>(x) exactly as the generated C++ does.
func (r *Repo) Eval(l *Lib, m int, x uint64) uint64 {
	k1, k2 := l.moduleConsts(m)
	a := make([]uint64, 8)
	for k := range a {
		a[k] = (x + uint64(k)*0x9E3779B97F4A7C15) * k1
	}
	sort.Slice(a, func(i, j int) bool { return a[i] < a[j] })
	tally := map[string]uint64{}
	for _, v := range a {
		tally["k"+strconv.FormatUint(v%5, 10)] += v >> 3
	}
	keys := make([]string, 0, len(tally))
	for k := range tally {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	v := k2
	for _, k := range keys {
		v = (v^tally[k])*0x100000001B3 + uint64(len(k))
	}
	for _, d := range l.Deps {
		v ^= r.Libs[d].Seed
	}
	if m == 0 && l.Codegen != nil {
		v += r.GenSum(l)
	}
	if m == 1 {
		for _, g := range l.RacyGen {
			v += r.GenSum(r.Libs[g])
		}
	}
	return v
}
