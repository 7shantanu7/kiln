// Command kiln-cc is a compiler launcher that caches object files in a
// Remote Execution API cache such as kiln-cache, bazel-remote or buildbarn.
//
// Use it as CMAKE_<LANG>_COMPILER_LAUNCHER, so no build files change:
//
//	cmake -DCMAKE_CXX_COMPILER_LAUNCHER="kiln-cc;--base-dir=$PWD;--base-dir=$PWD/build" ...
//
// Options come before the compiler; each also has an environment variable:
//
//	--addr=HOST:PORT     KILN_CACHE_ADDR      cache endpoint (unset: just run the compiler)
//	--instance=NAME      KILN_CACHE_INSTANCE  REAPI instance name
//	--base-dir=DIR       KILN_BASE_DIRS       paths rewritten so checkouts share entries (repeatable; env is colon-separated)
//	--read-only          KILN_CACHE_READONLY=1  look up but never upload
//	--log=FILE           KILN_CC_LOG          append one JSON line per invocation
//	--no-direct          KILN_CC_DIRECT=0     always run the preprocessor to compute the key
//	                                          (direct mode looks keys up by header hashes instead)
package main

import (
	"fmt"
	"os"
	"strings"

	"github.com/7shantanu7/kiln/internal/launcher"
)

func main() {
	opts := launcher.Options{
		Addr:     os.Getenv("KILN_CACHE_ADDR"),
		Instance: os.Getenv("KILN_CACHE_INSTANCE"),
		ReadOnly: os.Getenv("KILN_CACHE_READONLY") == "1",
		LogPath:  os.Getenv("KILN_CC_LOG"),
		NoDirect: os.Getenv("KILN_CC_DIRECT") == "0",
	}
	if v := os.Getenv("KILN_BASE_DIRS"); v != "" {
		opts.BaseDirs = strings.Split(v, ":")
	}
	args := os.Args[1:]
	for len(args) > 0 && strings.HasPrefix(args[0], "--") {
		a := args[0]
		args = args[1:]
		if a == "--" {
			break
		}
		k, v, _ := strings.Cut(a, "=")
		switch k {
		case "--addr":
			opts.Addr = v
		case "--instance":
			opts.Instance = v
		case "--base-dir":
			opts.BaseDirs = append(opts.BaseDirs, v)
		case "--read-only":
			opts.ReadOnly = true
		case "--log":
			opts.LogPath = v
		case "--no-direct":
			opts.NoDirect = true
		default:
			fmt.Fprintf(os.Stderr, "kiln-cc: unknown option %s\n", a)
			os.Exit(2)
		}
	}
	if len(args) == 0 {
		fmt.Fprintln(os.Stderr, "usage: kiln-cc [options] <compiler> <args...>")
		os.Exit(2)
	}
	if os.Getenv("KILN_CC_DISABLE") == "1" {
		opts.Addr = ""
	}
	os.Exit(launcher.Run(opts, args))
}
