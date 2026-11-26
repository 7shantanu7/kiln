// Package launcher implements kiln-cc, a compiler launcher that caches
// compile results in a Remote Execution API cache.
package launcher

import (
	"path/filepath"
	"strings"
)

// takesValue lists options whose value is the next argument.
var takesValue = map[string]bool{
	"-o": true, "-MF": true, "-MT": true, "-MQ": true, "-I": true, "-isystem": true,
	"-iquote": true, "-idirafter": true, "-include": true, "-imacros": true, "-x": true,
	"-D": true, "-U": true, "-isysroot": true, "--sysroot": true, "-target": true,
	"--target": true, "-Xclang": true, "-Xpreprocessor": true, "-Xassembler": true,
	"-Xlinker": true, "-arch": true, "-A": true, "-L": true, "-l": true, "-iprefix": true,
	"-iwithprefix": true, "-iwithprefixbefore": true, "-MJ": true,
}

var sourceExts = map[string]bool{".c": true, ".cc": true, ".cpp": true, ".cxx": true, ".c++": true, ".C": true, ".cp": true}

// Compile is a parsed compiler invocation.
type Compile struct {
	Compiler string
	Args     []string // arguments after the compiler
	Source   string
	Output   string
	DepFile  string // empty when no depfile is written
	// Uncacheable is set, with a reason, for invocations kiln-cc passes through.
	Uncacheable string
}

// Parse inspects a compiler command line (argv[0] is the compiler).
func Parse(argv []string) *Compile {
	c := &Compile{Compiler: argv[0], Args: argv[1:]}
	var sawC, md bool
	var sources []string
	args := c.Args
	for i := 0; i < len(args); i++ {
		a := args[i]
		switch {
		case a == "-c":
			sawC = true
		case a == "-o" && i+1 < len(args):
			c.Output = args[i+1]
		case strings.HasPrefix(a, "-o") && len(a) > 2 && !strings.HasPrefix(a, "-objc"):
			c.Output = a[2:]
		case a == "-MD" || a == "-MMD":
			md = true
		case a == "-MF" && i+1 < len(args):
			c.DepFile = args[i+1]
		case a == "-E" || a == "-S" || a == "-M" || a == "-MM":
			c.Uncacheable = "preprocess or assembly-only invocation " + a
		case strings.HasPrefix(a, "-ftime-trace"), strings.HasPrefix(a, "-fprofile"),
			strings.HasPrefix(a, "-save-temps"), a == "--coverage", strings.HasPrefix(a, "-fdump-"),
			strings.HasPrefix(a, "-MJ"), strings.HasPrefix(a, "-gsplit-dwarf"):
			c.Uncacheable = "option with side outputs: " + a
		case strings.HasPrefix(a, "@"):
			c.Uncacheable = "response files are not supported"
		case a == "-":
			c.Uncacheable = "reads source from stdin"
		case !strings.HasPrefix(a, "-") && sourceExts[filepath.Ext(a)]:
			sources = append(sources, a)
		}
		if takesValue[a] {
			i++
		}
	}
	if len(sources) == 1 {
		c.Source = sources[0]
	}
	switch {
	case c.Uncacheable != "":
	case !sawC:
		c.Uncacheable = "not a compile (-c) invocation"
	case len(sources) != 1:
		c.Uncacheable = "needs exactly one source file"
	case c.Output == "":
		c.Uncacheable = "no -o output"
	}
	if md && c.DepFile == "" && c.Output != "" {
		c.DepFile = strings.TrimSuffix(c.Output, filepath.Ext(c.Output)) + ".d"
	}
	if !md {
		c.DepFile = ""
	}
	return c
}

// PreprocessArgs returns arguments that print the preprocessed source to stdout.
func (c *Compile) PreprocessArgs() []string {
	var out []string
	args := c.Args
	for i := 0; i < len(args); i++ {
		a := args[i]
		switch a {
		case "-c", "-MD", "-MMD", "-MP":
			continue
		case "-o", "-MF", "-MT", "-MQ":
			i++
			continue
		}
		if strings.HasPrefix(a, "-o") && len(a) > 2 && !strings.HasPrefix(a, "-objc") {
			continue
		}
		out = append(out, a)
	}
	return append(out, "-E")
}

// HasDebugInfo reports whether the invocation emits debug information.
func (c *Compile) HasDebugInfo() bool {
	debug := false
	for _, a := range c.Args {
		if a == "-g0" {
			debug = false
		} else if strings.HasPrefix(a, "-g") {
			debug = true
		}
	}
	return debug
}

// PrefixMaps returns the OLD side of every -ffile-prefix-map / -fdebug-prefix-map.
func (c *Compile) PrefixMaps() []string {
	var olds []string
	for _, a := range c.Args {
		for _, p := range []string{"-ffile-prefix-map=", "-fdebug-prefix-map="} {
			if strings.HasPrefix(a, p) {
				kv := strings.TrimPrefix(a, p)
				if i := strings.LastIndex(kv, "="); i > 0 {
					olds = append(olds, kv[:i])
				}
			}
		}
	}
	return olds
}

// MacroPrefixMaps returns the OLD side of every prefix map that also applies
// to __FILE__ (-ffile-prefix-map and -fmacro-prefix-map).
func (c *Compile) MacroPrefixMaps() []string {
	var olds []string
	for _, a := range c.Args {
		for _, p := range []string{"-ffile-prefix-map=", "-fmacro-prefix-map="} {
			if strings.HasPrefix(a, p) {
				kv := strings.TrimPrefix(a, p)
				if i := strings.LastIndex(kv, "="); i > 0 {
					olds = append(olds, kv[:i])
				}
			}
		}
	}
	return olds
}
