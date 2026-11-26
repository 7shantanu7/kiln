// Command kiln-trace shows where build time goes.
//
//	kiln-trace path      -C build            critical path and slowest steps of the last clean build
//	kiln-trace headers   -C build            Clang -ftime-trace breakdown (build with -ftime-trace)
//	kiln-trace snapshot  -C build -o m.json  save build-time metrics
//	kiln-trace compare   base.json cur.json  fail if build time grew too much (CI gate)
package main

import (
	"encoding/json"
	"flag"
	"fmt"
	"os"
	"sort"

	"github.com/7shantanu7/kiln/internal/ninja"
	"github.com/7shantanu7/kiln/internal/trace"
)

func main() {
	if len(os.Args) < 2 {
		usage()
	}
	cmd, args := os.Args[1], os.Args[2:]
	switch cmd {
	case "path":
		fs := flag.NewFlagSet("path", flag.ExitOnError)
		dir := fs.String("C", "build", "Ninja build directory")
		top := fs.Int("top", 10, "how many of the slowest steps to list")
		fs.Parse(args)
		s := analyze(*dir, *top)
		printSummary(s)
	case "headers":
		fs := flag.NewFlagSet("headers", flag.ExitOnError)
		dir := fs.String("C", "build", "build directory containing Clang -ftime-trace JSON files")
		top := fs.Int("top", 10, "rows per table")
		fs.Parse(args)
		r, err := trace.ReadTimeTraces(*dir, *top)
		check(err)
		if r.Files == 0 {
			fail("no -ftime-trace files under %s (configure with CMAKE_CXX_FLAGS=-ftime-trace and Clang)", *dir)
		}
		fmt.Printf("%d translation units, %.1fs compiling (%.0f%% frontend, %.0f%% backend)\n\n",
			r.Files, r.TotalMs/1000, 100*r.FrontendMs/r.TotalMs, 100*r.BackendMs/r.TotalMs)
		table("Headers by own parse time (excluding what they include)", r.HeaderSelf)
		table("Headers by total parse time (including what they include)", r.Headers)
		table("Template instantiations", r.Templates)
		table("Slowest translation units", r.SlowestTUs)
	case "snapshot":
		fs := flag.NewFlagSet("snapshot", flag.ExitOnError)
		dir := fs.String("C", "build", "Ninja build directory")
		out := fs.String("o", "build-metrics.json", "output file")
		fs.Parse(args)
		s := analyze(*dir, 20)
		b, _ := json.MarshalIndent(s, "", "  ")
		check(os.WriteFile(*out, append(b, '\n'), 0o644))
		fmt.Printf("wrote %s: %d steps, %s CPU, %s wall, %s critical path\n", *out, s.Steps,
			trace.FormatMs(s.CPUMs), trace.FormatMs(s.WallMs), trace.FormatMs(s.CriticalPathMs))
	case "compare":
		fs := flag.NewFlagSet("compare", flag.ExitOnError)
		maxPct := fs.Float64("max-regression", 25, "fail when a metric grows by more than this percent")
		minMs := fs.Int64("min-ms", 2000, "ignore metrics whose baseline is below this many milliseconds")
		fs.Parse(args)
		if fs.NArg() != 2 {
			fail("usage: kiln-trace compare [-max-regression 25] baseline.json current.json")
		}
		base, cur := load(fs.Arg(0)), load(fs.Arg(1))
		regs := trace.Compare(base, cur, *maxPct, *minMs)
		fmt.Printf("CPU %s -> %s, critical path %s -> %s\n", trace.FormatMs(base.CPUMs), trace.FormatMs(cur.CPUMs),
			trace.FormatMs(base.CriticalPathMs), trace.FormatMs(cur.CriticalPathMs))
		if len(regs) == 0 {
			fmt.Printf("ok: no metric grew more than %.0f%%\n", *maxPct)
			return
		}
		for _, r := range regs {
			fmt.Printf("REGRESSION %s: %s -> %s (+%.1f%%)\n", r.Metric, trace.FormatMs(r.Baseline), trace.FormatMs(r.Current), r.Percent)
		}
		os.Exit(1)
	default:
		usage()
	}
}

func analyze(dir string, top int) trace.Summary {
	g, err := ninja.Load(dir)
	check(err)
	log, err := ninja.ReadLog(dir)
	check(err)
	return trace.Analyze(g, log, top)
}

func printSummary(s trace.Summary) {
	fmt.Printf("%d steps: %s wall, %s CPU (parallelism %.1f), critical path %s\n",
		s.Steps, trace.FormatMs(s.WallMs), trace.FormatMs(s.CPUMs), s.Parallelism, trace.FormatMs(s.CriticalPathMs))
	var kinds []string
	for k := range s.ByRule {
		kinds = append(kinds, k)
	}
	sort.Slice(kinds, func(i, j int) bool { return s.ByRule[kinds[i]] > s.ByRule[kinds[j]] })
	for _, k := range kinds {
		fmt.Printf("  %-8s %s (%.0f%%)\n", k, trace.FormatMs(s.ByRule[k]), 100*float64(s.ByRule[k])/float64(s.CPUMs))
	}
	fmt.Printf("\nCritical path (%d steps):\n", len(s.CriticalPath))
	for _, st := range s.CriticalPath {
		fmt.Printf("  %8s  %s\n", trace.FormatMs(st.DurationMs), st.Output)
	}
	fmt.Printf("\nSlowest steps:\n")
	for _, st := range s.Slowest {
		fmt.Printf("  %8s  %s\n", trace.FormatMs(st.DurationMs), st.Output)
	}
}

func table(title string, rows []trace.Cost) {
	fmt.Println(title)
	for _, c := range rows {
		fmt.Printf("  %9.1fms  x%-5d %s\n", c.Ms, c.Count, c.Name)
	}
	fmt.Println()
}

func load(path string) trace.Summary {
	b, err := os.ReadFile(path)
	check(err)
	var s trace.Summary
	check(json.Unmarshal(b, &s))
	return s
}

func check(err error) {
	if err != nil {
		fail("%v", err)
	}
}

func fail(format string, args ...any) {
	fmt.Fprintf(os.Stderr, "kiln-trace: "+format+"\n", args...)
	os.Exit(2)
}

func usage() {
	fmt.Fprintln(os.Stderr, "usage: kiln-trace path|headers|snapshot|compare [flags]")
	os.Exit(2)
}
