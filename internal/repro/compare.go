// Package repro compares two builds of the same sources and explains why
// their outputs differ.
package repro

import (
	"bytes"
	"debug/elf"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"

	"github.com/7shantanu7/kiln/internal/ninja"
)

// Cause kinds, each with a remedy.
const (
	CauseDebugPath   = "absolute-path-in-debug-info"
	CauseSourcePath  = "source-path-in-program-data" // usually __FILE__
	CauseBuildPath   = "build-path-in-program-data"  // e.g. a configured ${CMAKE_BINARY_DIR}
	CauseDateTime    = "build-date-time"             // __DATE__ / __TIME__
	CauseTimestamp   = "embedded-timestamp"          // e.g. CMake string(TIMESTAMP)
	CauseOrder       = "unstable-generator-order"
	CauseArchiveMeta = "archive-metadata"
	CauseUnknown     = "unknown"
)

// Remedies explains how to fix each cause, and whether kiln's toolchain
// settings (SOURCE_DATE_EPOCH and -ffile-prefix-map) fix it without source changes.
var Remedies = map[string]struct {
	Fix         string
	ByToolchain bool
}{
	CauseDebugPath:   {"-ffile-prefix-map for source and build directories", true},
	CauseSourcePath:  {"-ffile-prefix-map (also rewrites __FILE__)", true},
	CauseBuildPath:   {"source change: do not bake the build directory into the program; resolve paths at run time", false},
	CauseDateTime:    {"SOURCE_DATE_EPOCH (GCC and Clang use it for __DATE__/__TIME__)", true},
	CauseTimestamp:   {"SOURCE_DATE_EPOCH (CMake's string(TIMESTAMP) honors it)", true},
	CauseOrder:       {"source change: sort before emitting in the generator", false},
	CauseArchiveMeta: {"deterministic archives (ar D, the default on most distributions)", true},
	CauseUnknown:     {"inspect with diffoscope", false},
}

// Cause is one reason a file differs.
type Cause struct {
	Kind   string `json:"kind"`
	Detail string `json:"detail"`
}

// Diff is one output that differs between the builds.
type Diff struct {
	Path   string  `json:"path"` // relative to the build directory
	Root   bool    `json:"root"` // false when an input that differs explains it
	Causes []Cause `json:"causes,omitempty"`
}

// Report is the comparison result.
type Report struct {
	Compared int    `json:"compared"`
	Diffs    []Diff `json:"diffs"`
}

// Roots returns the root-cause diffs.
func (r *Report) Roots() []Diff {
	var out []Diff
	for _, d := range r.Diffs {
		if d.Root {
			out = append(out, d)
		}
	}
	return out
}

// Build is one of the two builds.
type Build struct {
	Root string // common parent of Src and Dir; A and B roots must have equal length
	Src  string // source directory
	Dir  string // build directory
}

// Compare compares every build output, plus configure-generated files the
// build reads, between a and b.
func Compare(a, b Build) (*Report, error) {
	if len(a.Root) != len(b.Root) {
		return nil, fmt.Errorf("build roots must have the same length to compare paths byte for byte")
	}
	g, err := ninja.Load(a.Dir)
	if err != nil {
		return nil, err
	}
	// Files to compare: edge outputs, and files in the build directory that
	// steps read but no step produces (written by CMake at configure time).
	files := map[string]bool{}
	for _, e := range g.Edges {
		// RERUN_CMAKE regenerates build.ninja from CMake's own bookkeeping,
		// which records absolute paths by design.
		if e.Phony() || e.Rule == "RERUN_CMAKE" {
			continue
		}
		for _, o := range e.Outputs {
			files[o] = true
		}
		for _, in := range e.Inputs() {
			if strings.HasPrefix(in, g.Dir+"/") && !g.IsGenerated(in) {
				files[in] = true
			}
		}
	}
	rep := &Report{Diffs: []Diff{}} // [] rather than null in JSON
	differs := map[string]bool{}
	var paths []string
	for f := range files {
		paths = append(paths, f)
	}
	sort.Strings(paths)
	contents := map[string][2][]byte{}
	for _, f := range paths {
		rel, _ := filepath.Rel(g.Dir, f)
		da, errA := os.ReadFile(f)
		db, errB := os.ReadFile(filepath.Join(b.Dir, rel))
		if errA != nil || errB != nil {
			continue // not built (e.g. a stamp file) or removed (depfiles)
		}
		rep.Compared++
		if !bytes.Equal(da, db) {
			differs[f] = true
			contents[f] = [2][]byte{da, db}
		}
	}
	for _, f := range paths {
		if !differs[f] {
			continue
		}
		rel, _ := filepath.Rel(g.Dir, f)
		d := Diff{Path: rel, Root: true}
		if e, ok := g.Producer[f]; ok {
			// Order-only inputs cannot change a step's output, so only
			// explicit, implicit and depfile inputs can explain a difference.
			for _, in := range e.Inputs() {
				if differs[in] {
					d.Root = false
					break
				}
			}
		}
		if d.Root {
			c := contents[f]
			d.Causes = classify(c[0], c[1], a.Root, b.Root, a.Src)
		}
		rep.Diffs = append(rep.Diffs, d)
	}
	return rep, nil
}

var (
	dateTimeRe  = regexp.MustCompile(`(Jan|Feb|Mar|Apr|May|Jun|Jul|Aug|Sep|Oct|Nov|Dec) [ 0-9]\d \d{4}|\d\d:\d\d:\d\d`)
	isoStampRe  = regexp.MustCompile(`\d{4}-\d\d-\d\dT\d\d:\d\d:\d\d`)
	sourceExtRe = regexp.MustCompile(`\.(c|cc|cpp|cxx|h|hh|hpp|hxx|inc)$`)
)

func classify(a, b []byte, rootA, rootB, srcA string) []Cause {
	bSub := bytes.ReplaceAll(b, []byte(rootB), []byte(rootA))
	if bytes.HasPrefix(a, []byte("!<arch>\n")) {
		if sameMembers(a, b) {
			return []Cause{{CauseArchiveMeta, "members are identical; header fields differ"}}
		}
	}
	if ef, err := elf.NewFile(bytes.NewReader(a)); err == nil {
		return classifyELF(ef, a, bSub, rootA, srcA)
	}
	// Text (generated or configured files).
	var causes []Cause
	if !bytes.Equal(a, bSub) || bytes.Contains(a, []byte(rootA)) {
		if bytes.Contains(a, []byte(rootA)) {
			kind := CauseBuildPath
			for _, s := range pathStrings(a, rootA) {
				if strings.HasPrefix(s, srcA) && sourceExtRe.MatchString(s) {
					kind = CauseSourcePath
				}
			}
			causes = append(causes, Cause{kind, "contains " + strings.Join(pathStrings(a, rootA), ", ")})
		}
	}
	if !bytes.Equal(a, bSub) {
		la, lb := lines(a), lines(bSub)
		if sameMultiset(la, lb) {
			causes = append(causes, Cause{CauseOrder, "same lines in a different order"})
		} else {
			changed := changedLines(la, lb)
			if anyMatch(isoStampRe, changed) {
				causes = append(causes, Cause{CauseTimestamp, firstMatch(isoStampRe, changed)})
			} else if anyMatch(dateTimeRe, changed) {
				causes = append(causes, Cause{CauseDateTime, firstMatch(dateTimeRe, changed)})
			} else if len(causes) == 0 {
				causes = append(causes, Cause{CauseUnknown, fmt.Sprintf("%d lines differ", len(changed))})
			}
		}
	}
	return causes
}

func classifyELF(ef *elf.File, a, bSub []byte, rootA, srcA string) []Cause {
	sectionAt := func(off int) string {
		for _, s := range ef.Sections {
			if s.Type != elf.SHT_NOBITS && uint64(off) >= s.Offset && uint64(off) < s.Offset+s.FileSize {
				return s.Name
			}
		}
		return "?"
	}
	seen := map[string]bool{}
	var causes []Cause
	add := func(kind, detail string) {
		if !seen[kind] {
			seen[kind] = true
			causes = append(causes, Cause{kind, detail})
		}
	}
	// Absolute paths of this build, by section.
	for off := 0; ; {
		i := bytes.Index(a[off:], []byte(rootA))
		if i < 0 {
			break
		}
		at := off + i
		sec := sectionAt(at)
		s := cString(a, at)
		switch {
		case strings.HasPrefix(sec, ".debug") || strings.HasPrefix(sec, ".zdebug"):
			add(CauseDebugPath, sec)
		case strings.HasPrefix(s, srcA) && sourceExtRe.MatchString(s):
			add(CauseSourcePath, fmt.Sprintf("%q in %s", s, sec))
		default:
			add(CauseBuildPath, fmt.Sprintf("%q in %s", s, sec))
		}
		off = at + len(rootA)
	}
	// Remaining differences after paths are accounted for.
	if !bytes.Equal(a, bSub) && len(a) == len(bSub) {
		for _, r := range diffRanges(a, bSub) {
			sec := sectionAt(r[0])
			s := printableAround(a, r[0])
			switch {
			case isoStampRe.MatchString(s):
				add(CauseTimestamp, fmt.Sprintf("%q in %s", s, sec))
			case dateTimeRe.MatchString(s):
				add(CauseDateTime, fmt.Sprintf("%q in %s", s, sec))
			case strings.Contains(sec, ".debug"):
				// Debug sections and their relocations hold offsets into
				// string tables whose layout follows the path strings above.
				if !seen[CauseDebugPath] && !seen[CauseSourcePath] {
					add(CauseUnknown, "debug info differs in "+sec)
				}
			default:
				add(CauseUnknown, fmt.Sprintf("bytes differ in %s", sec))
			}
		}
	} else if !bytes.Equal(a, bSub) && len(causes) == 0 {
		add(CauseUnknown, "sizes differ")
	}
	return causes
}

func cString(b []byte, at int) string {
	end := bytes.IndexByte(b[at:], 0)
	if end < 0 || end > 512 {
		end = min(512, len(b)-at)
	}
	return string(b[at : at+end])
}

func printableAround(b []byte, at int) string {
	lo, hi := at, at
	for lo > 0 && b[lo-1] >= 0x20 && b[lo-1] < 0x7f && at-lo < 64 {
		lo--
	}
	for hi < len(b) && b[hi] >= 0x20 && b[hi] < 0x7f && hi-at < 64 {
		hi++
	}
	return string(b[lo:hi])
}

// diffRanges returns [start, end) ranges where a and b differ, merging close ones.
func diffRanges(a, b []byte) [][2]int {
	var out [][2]int
	for i := 0; i < len(a); i++ {
		if a[i] == b[i] {
			continue
		}
		j := i
		for j < len(a) && (a[j] != b[j] || (j+8 < len(a) && !bytes.Equal(a[j:j+8], b[j:j+8]))) {
			j++
		}
		out = append(out, [2]int{i, j})
		i = j
		if len(out) > 1000 {
			break
		}
	}
	return out
}

func pathStrings(b []byte, root string) []string {
	re := regexp.MustCompile(regexp.QuoteMeta(root) + `[^"\s\x00]*`)
	seen := map[string]bool{}
	var out []string
	for _, m := range re.FindAll(b, -1) {
		if !seen[string(m)] {
			seen[string(m)] = true
			out = append(out, string(m))
		}
	}
	return out
}

func lines(b []byte) []string { return strings.Split(string(b), "\n") }

func sameMultiset(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	count := map[string]int{}
	for _, l := range a {
		count[l]++
	}
	for _, l := range b {
		count[l]--
	}
	for _, c := range count {
		if c != 0 {
			return false
		}
	}
	return true
}

func changedLines(a, b []string) []string {
	var out []string
	for i := 0; i < len(a) || i < len(b); i++ {
		var x, y string
		if i < len(a) {
			x = a[i]
		}
		if i < len(b) {
			y = b[i]
		}
		if x != y {
			out = append(out, x, y)
		}
	}
	return out
}

func anyMatch(re *regexp.Regexp, ls []string) bool { return firstMatch(re, ls) != "" }

func firstMatch(re *regexp.Regexp, ls []string) string {
	for _, l := range ls {
		if re.MatchString(l) {
			return strings.TrimSpace(l)
		}
	}
	return ""
}

// sameMembers reports whether two ar archives hold the same member names and contents.
func sameMembers(a, b []byte) bool {
	ma, ok1 := arMembers(a)
	mb, ok2 := arMembers(b)
	if !ok1 || !ok2 || len(ma) != len(mb) {
		return false
	}
	for i := range ma {
		if ma[i] != mb[i] {
			return false
		}
	}
	return true
}

func arMembers(b []byte) ([]string, bool) {
	b = b[8:]
	var out []string
	for len(b) >= 60 {
		hdr := b[:60]
		var size int
		if _, err := fmt.Sscanf(strings.TrimSpace(string(hdr[48:58])), "%d", &size); err != nil || 60+size > len(b) {
			return nil, false
		}
		name := strings.TrimSpace(string(hdr[:16]))
		body := b[60 : 60+size]
		out = append(out, name+"\x00"+string(body))
		b = b[60+size:]
		if size%2 == 1 && len(b) > 0 {
			b = b[1:]
		}
	}
	return out, true
}
