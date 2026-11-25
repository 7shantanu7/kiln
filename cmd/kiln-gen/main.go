// Command kiln-gen writes a synthetic CMake + Ninja C++ monorepo.
//
// The default shape is 250 static libraries, each with 16 translation units
// and one test executable (500 targets, 4,250 TUs). Bugs for the other kiln
// tools to find can be injected with -missing-deps and -nondet.
package main

import (
	"flag"
	"fmt"
	"os"
	"strings"

	"github.com/7shantanu7/kiln/internal/fixture"
)

func main() {
	cfg := fixture.DefaultConfig()
	out := flag.String("out", "", "output directory (must not exist)")
	flag.Int64Var(&cfg.Seed, "seed", cfg.Seed, "random seed")
	flag.IntVar(&cfg.Libs, "libs", cfg.Libs, "number of libraries (each adds one test target)")
	flag.IntVar(&cfg.Modules, "modules", cfg.Modules, "translation units per library")
	flag.IntVar(&cfg.MaxDeps, "max-deps", cfg.MaxDeps, "max direct dependencies per library")
	flag.IntVar(&cfg.DepWindow, "dep-window", cfg.DepWindow, "libraries may depend only on the previous N libraries")
	flag.IntVar(&cfg.CodegenEach, "codegen-each", cfg.CodegenEach, "every Nth library runs codegen (0 = none)")
	flag.IntVar(&cfg.TestReps, "test-reps", cfg.TestReps, "repetitions per test (controls test runtime)")
	flag.IntVar(&cfg.MissingDeps, "missing-deps", 0, "number of undeclared dependencies to inject")
	nondet := flag.String("nondet", "", "reproducibility bugs to inject: 'all' or a comma list of "+strings.Join(fixture.NondetKinds, ","))
	flag.Parse()

	if *out == "" {
		fail("-out is required")
	}
	if _, err := os.Stat(*out); err == nil {
		fail("%s already exists", *out)
	}
	cfg.Nondet = map[string]bool{}
	if *nondet == "all" {
		for _, k := range fixture.NondetKinds {
			cfg.Nondet[k] = true
		}
	} else if *nondet != "" {
		valid := map[string]bool{}
		for _, k := range fixture.NondetKinds {
			valid[k] = true
		}
		for _, k := range strings.Split(*nondet, ",") {
			if !valid[k] {
				fail("unknown -nondet kind %q", k)
			}
			cfg.Nondet[k] = true
		}
	}

	repo, err := fixture.Build(cfg)
	if err != nil {
		fail("%v", err)
	}
	if err := repo.Write(*out); err != nil {
		fail("%v", err)
	}
	fmt.Printf("kiln-gen: wrote %d libraries, %d targets, %d translation units to %s\n",
		cfg.Libs, 2*cfg.Libs, cfg.Libs*(cfg.Modules+1), *out)
	for _, inj := range repo.Injected {
		fmt.Printf("  injected %s: %s -> %s\n", inj.Kind, inj.Consumer, inj.Producer)
	}
}

func fail(format string, args ...any) {
	fmt.Fprintf(os.Stderr, "kiln-gen: "+format+"\n", args...)
	os.Exit(2)
}
