// Command kiln-repro checks that a CMake + Ninja project builds bit for bit
// reproducibly, and explains every difference it finds.
//
// It copies the sources into two directories, configures and builds each
// (RelWithDebInfo by default, so debug info is checked too) one after the
// other with different time zones, then compares every build output and
// every configure-generated file the build reads. A difference whose inputs
// are identical is a root cause; it is classified (debug paths, __FILE__,
// __DATE__/__TIME__, timestamps, generator ordering, ...) with a fix.
//
//	kiln-repro -src . [-toolchain-fixes] [-j 8] [-json out.json] [-- extra cmake args]
//
// -toolchain-fixes applies the fixes that need no source changes:
// SOURCE_DATE_EPOCH and kiln.cmake's -ffile-prefix-map. What remains needs
// source changes. Exit status is 1 when any output differs.
package main

import (
	"encoding/json"
	"flag"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"time"

	kilncmake "github.com/7shantanu7/kiln/cmake"
	"github.com/7shantanu7/kiln/internal/repro"
)

func main() {
	src := flag.String("src", ".", "source directory (CMake project)")
	work := flag.String("work", "", "working directory for the two builds (default: a new temp dir, removed afterwards)")
	fixes := flag.Bool("toolchain-fixes", false, "set SOURCE_DATE_EPOCH and load kiln.cmake (-ffile-prefix-map)")
	sde := flag.Int64("source-date-epoch", 0, "SOURCE_DATE_EPOCH for -toolchain-fixes (default: last git commit time, else 1735689600)")
	buildType := flag.String("build-type", "RelWithDebInfo", "CMAKE_BUILD_TYPE")
	jobs := flag.Int("j", 0, "parallel jobs (0 = Ninja's default)")
	jsonOut := flag.String("json", "", "write the report as JSON")
	keep := flag.Bool("keep", false, "keep the temporary working directory")
	flag.Parse()
	extra := flag.Args()

	srcAbs, err := filepath.Abs(*src)
	check(err)
	workDir := *work
	if workDir == "" {
		workDir, err = os.MkdirTemp("", "kiln-repro-")
		check(err)
		if !*keep {
			defer os.RemoveAll(workDir)
		}
	} else {
		workDir, _ = filepath.Abs(workDir)
		check(os.MkdirAll(workDir, 0o755))
	}

	env := os.Environ()
	cmakeArgs := []string{"-G", "Ninja", "-DCMAKE_BUILD_TYPE=" + *buildType}
	if *fixes {
		epoch := *sde
		if epoch == 0 {
			epoch = gitEpoch(srcAbs)
		}
		env = append(env, "SOURCE_DATE_EPOCH="+strconv.FormatInt(epoch, 10))
		mod := filepath.Join(workDir, "kiln.cmake")
		check(os.WriteFile(mod, kilncmake.Module, 0o644))
		cmakeArgs = append(cmakeArgs, "-DCMAKE_PROJECT_INCLUDE="+mod, "-DKILN_CACHE_ADDR=")
		fmt.Printf("kiln-repro: toolchain fixes on (SOURCE_DATE_EPOCH=%d, -ffile-prefix-map)\n", epoch)
	}
	cmakeArgs = append(cmakeArgs, extra...)

	// Two roots of equal length so paths can be compared byte for byte; the
	// builds also run in different time zones.
	var builds [2]repro.Build
	for i, name := range []string{"a", "b"} {
		root := filepath.Join(workDir, name)
		b := repro.Build{Root: root, Src: filepath.Join(root, "src"), Dir: filepath.Join(root, "build")}
		builds[i] = b
		check(os.RemoveAll(root))
		check(os.MkdirAll(root, 0o755))
		run("", env, "cp", "-a", srcAbs, b.Src)
		tz := []string{"TZ=UTC", "TZ=Pacific/Kiritimati"}[i]
		benv := append(append([]string{}, env...), tz)
		fmt.Printf("kiln-repro: build %s in %s (%s)\n", name, root, tz)
		run("", benv, "cmake", append([]string{"-S", b.Src, "-B", b.Dir}, cmakeArgs...)...)
		nargs := []string{"-C", b.Dir}
		if *jobs > 0 {
			nargs = append(nargs, "-j", strconv.Itoa(*jobs))
		}
		run("", benv, "ninja", nargs...)
		if i == 0 {
			time.Sleep(1100 * time.Millisecond) // make sure the clock has moved
		}
	}

	rep, err := repro.Compare(builds[0], builds[1])
	check(err)
	roots := rep.Roots()
	fmt.Printf("\nkiln-repro: compared %d files: %d differ, %d of them root causes\n", rep.Compared, len(rep.Diffs), len(roots))

	byKind := map[string][]repro.Diff{}
	for _, d := range roots {
		for _, c := range d.Causes {
			byKind[c.Kind] = append(byKind[c.Kind], d)
		}
	}
	var kinds []string
	for k := range byKind {
		kinds = append(kinds, k)
	}
	sort.Strings(kinds)
	for _, k := range kinds {
		ds := byKind[k]
		rem := repro.Remedies[k]
		how := "needs a source change"
		if rem.ByToolchain {
			how = "fixed by -toolchain-fixes"
		}
		fmt.Printf("\n%s (%d files, %s)\n  fix: %s\n", k, len(ds), how, rem.Fix)
		for i, d := range ds {
			if i == 3 {
				fmt.Printf("  ... and %d more\n", len(ds)-3)
				break
			}
			for _, c := range d.Causes {
				if c.Kind == k {
					fmt.Printf("  %s: %s\n", d.Path, truncate(c.Detail, 120))
				}
			}
		}
	}
	if *jsonOut != "" {
		b, _ := json.MarshalIndent(rep, "", "  ")
		check(os.WriteFile(*jsonOut, append(b, '\n'), 0o644))
	}
	if len(rep.Diffs) > 0 {
		os.Exit(1)
	}
	fmt.Println("kiln-repro: builds are bit-for-bit identical")
}

func gitEpoch(dir string) int64 {
	out, err := exec.Command("git", "-C", dir, "log", "-1", "--format=%ct").Output()
	if err == nil {
		if n, err := strconv.ParseInt(strings.TrimSpace(string(out)), 10, 64); err == nil {
			return n
		}
	}
	return 1735689600 // 2025-01-01T00:00:00Z
}

func run(dir string, env []string, name string, args ...string) {
	cmd := exec.Command(name, args...)
	cmd.Dir, cmd.Env = dir, env
	out, err := cmd.CombinedOutput()
	if err != nil {
		fail("%s %s: %v\n%s", name, strings.Join(args, " "), err, tail(string(out), 40))
	}
}

func tail(s string, n int) string {
	ls := strings.Split(strings.TrimRight(s, "\n"), "\n")
	if len(ls) > n {
		ls = ls[len(ls)-n:]
	}
	return strings.Join(ls, "\n")
}

func truncate(s string, n int) string {
	if len(s) > n {
		return s[:n] + "..."
	}
	return s
}

func check(err error) {
	if err != nil {
		fail("%v", err)
	}
}

func fail(format string, args ...any) {
	fmt.Fprintf(os.Stderr, "kiln-repro: "+format+"\n", args...)
	os.Exit(2)
}
