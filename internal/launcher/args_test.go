package launcher

import (
	"bytes"
	"reflect"
	"strings"
	"testing"
)

func TestParseCMakeCompile(t *testing.T) {
	argv := strings.Fields("/usr/bin/c++ -I/src/include -O2 -std=gnu++17 -MD -MT obj/a.o -MF obj/a.o.d -o obj/a.o -c /src/a.cpp")
	c := Parse(argv)
	if c.Uncacheable != "" || c.Source != "/src/a.cpp" || c.Output != "obj/a.o" || c.DepFile != "obj/a.o.d" {
		t.Fatalf("Parse = %+v", c)
	}
	want := strings.Fields("-I/src/include -O2 -std=gnu++17 /src/a.cpp -E")
	if got := c.PreprocessArgs(); !reflect.DeepEqual(got, want) {
		t.Fatalf("PreprocessArgs = %v, want %v", got, want)
	}
}

func TestParseUncacheable(t *testing.T) {
	for _, cmd := range []string{
		"c++ -o app a.o b.o",               // link
		"c++ -c a.cpp b.cpp -o x.o",        // two sources
		"c++ -c a.cpp -o a.o -ftime-trace", // side output
		"c++ -E a.cpp",                     // preprocess only
		"c++ @args.rsp",                    // response file
		"c++ -c -x c++ - -o a.o",           // stdin
	} {
		if c := Parse(strings.Fields(cmd)); c.Uncacheable == "" {
			t.Errorf("%q should be uncacheable", cmd)
		}
	}
}

func TestDepFileDefaultsToOutputName(t *testing.T) {
	c := Parse(strings.Fields("c++ -MMD -c a.cpp -o build/a.o"))
	if c.DepFile != "build/a.d" {
		t.Fatalf("DepFile = %q", c.DepFile)
	}
}

func TestPathMapLineMarkersOnly(t *testing.T) {
	pm := newPathMap([]string{"/work/src", "/work/src/build"})
	pre := []byte("# 1 \"/work/src/a.cpp\"\nconst char* f = \"/work/src/a.cpp\";\n# 3 \"/work/src/build/gen.h\" 1\n")
	got := string(pm.normalizeLineMarkers(pre))
	want := "# 1 \"@BASE0@/a.cpp\"\nconst char* f = \"/work/src/a.cpp\";\n# 3 \"@BASE1@/gen.h\" 1\n"
	if got != want {
		t.Fatalf("got\n%s\nwant\n%s", got, want)
	}
	if back := pm.denormalize(pm.normalizeBytes([]byte("/work/src/build/x.h /work/src/y.h"))); !bytes.Equal(back, []byte("/work/src/build/x.h /work/src/y.h")) {
		t.Fatalf("round trip = %s", back)
	}
}

func TestDebugInfoNeedsPrefixMaps(t *testing.T) {
	pm := newPathMap([]string{"/w/src", "/w/build"})
	c := Parse(strings.Fields("c++ -g -c a.cpp -o a.o -ffile-prefix-map=/w/src=. -ffile-prefix-map=/w/build=."))
	if !c.HasDebugInfo() || !pm.coveredBy(c.PrefixMaps()) {
		t.Fatal("both base dirs are remapped; debug objects should be shareable")
	}
	c = Parse(strings.Fields("c++ -g -c a.cpp -o a.o -ffile-prefix-map=/w/src=."))
	if pm.coveredBy(c.PrefixMaps()) {
		t.Fatal("build dir is not remapped")
	}
	if Parse(strings.Fields("c++ -g -g0 -c a.cpp -o a.o")).HasDebugInfo() {
		t.Fatal("-g0 after -g disables debug info")
	}
}
