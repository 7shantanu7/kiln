// Command kiln-tia selects the tests affected by a change.
//
//	kiln-tia -C build -src . path/to/changed.cpp ...
//	kiln-tia -C build -src . -git-diff origin/main -format regex   # for ctest -R
//
// It walks the Ninja graph (including depfile dependencies) from each changed
// file to the test executables that consume it. Changes to CMake files select
// every test.
package main

import (
	"bytes"
	"encoding/json"
	"flag"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"

	"github.com/7shantanu7/kiln/internal/ninja"
	"github.com/7shantanu7/kiln/internal/tia"
)

func main() {
	dir := flag.String("C", "build", "Ninja build directory (already built once, so depfile deps are known)")
	src := flag.String("src", ".", "source root that changed paths are relative to")
	diff := flag.String("git-diff", "", "use the files changed since this git revision")
	format := flag.String("format", "list", "output: list, regex (for ctest -R) or json")
	flag.Parse()

	srcAbs, err := filepath.Abs(*src)
	check(err)
	changed := flag.Args()
	if *diff != "" {
		cmd := exec.Command("git", "diff", "--name-only", *diff)
		cmd.Dir = srcAbs
		var stderr bytes.Buffer
		cmd.Stderr = &stderr
		out, err := cmd.Output()
		if err != nil {
			fail("git diff: %v: %s", err, stderr.String())
		}
		changed = append(changed, strings.Fields(string(out))...)
	}
	for i, c := range changed {
		if !filepath.IsAbs(c) {
			changed[i] = filepath.Join(srcAbs, c)
		}
	}

	g, err := ninja.Load(*dir)
	check(err)
	tests, err := tia.LoadTests(g.Dir)
	check(err)
	sel := tia.Select(g, tests, changed)

	switch *format {
	case "list":
		for _, t := range sel.Tests {
			fmt.Println(t)
		}
		note := fmt.Sprintf("kiln-tia: %d of %d tests selected", len(sel.Tests), len(tests))
		if sel.All {
			note += " (" + sel.Reason + ")"
		}
		if len(sel.Unknown) > 0 {
			note += fmt.Sprintf("; %d changed files are not build inputs", len(sel.Unknown))
		}
		fmt.Fprintln(os.Stderr, note)
	case "regex":
		fmt.Println(tia.CTestRegex(sel.Tests))
	case "json":
		b, _ := json.MarshalIndent(sel, "", "  ")
		fmt.Println(string(b))
	default:
		fail("unknown -format %q", *format)
	}
}

func check(err error) {
	if err != nil {
		fail("%v", err)
	}
}

func fail(format string, args ...any) {
	fmt.Fprintf(os.Stderr, "kiln-tia: "+format+"\n", args...)
	os.Exit(2)
}
