package repro

import (
	"fmt"
	"testing"
)

func kinds(cs []Cause) map[string]bool {
	m := map[string]bool{}
	for _, c := range cs {
		m[c.Kind] = true
	}
	return m
}

func TestClassifyText(t *testing.T) {
	const ra, rb = "/w/a", "/w/b"
	cases := []struct {
		name string
		a, b string
		want []string
	}{
		{"reordered", "x\ny\nz\n", "z\nx\ny\n", []string{CauseOrder}},
		{"timestamp", `#define STAMP "2026-11-25T21:57:08Z"` + "\n", `#define STAMP "2026-11-25T21:57:09Z"` + "\n", []string{CauseTimestamp}},
		{"build path", `#define DIR "/w/a/build/data"` + "\n", `#define DIR "/w/b/build/data"` + "\n", []string{CauseBuildPath}},
		{"source path", `const char* f = "/w/a/src/x.cpp";` + "\n", `const char* f = "/w/b/src/x.cpp";` + "\n", []string{CauseSourcePath}},
		{"two causes", "/w/a/build\n2026-01-01T00:00:00\n", "/w/b/build\n2026-01-01T00:00:01\n", []string{CauseBuildPath, CauseTimestamp}},
	}
	for _, c := range cases {
		got := kinds(classify([]byte(c.a), []byte(c.b), ra, rb, "/w/a/src"))
		if len(got) != len(c.want) {
			t.Errorf("%s: got %v, want %v", c.name, got, c.want)
			continue
		}
		for _, k := range c.want {
			if !got[k] {
				t.Errorf("%s: got %v, want %v", c.name, got, c.want)
			}
		}
	}
}

func arArchive(mtime int, members ...string) []byte {
	b := []byte("!<arch>\n")
	for i, m := range members {
		hdr := fmt.Sprintf("%-16s%-12d%-6d%-6d%-8s%-10d`\n", fmt.Sprintf("m%d.o/", i), mtime, 0, 0, "644", len(m))
		b = append(b, hdr...)
		b = append(b, m...)
		if len(m)%2 == 1 {
			b = append(b, '\n')
		}
	}
	return b
}

func TestArchiveMetadataOnly(t *testing.T) {
	a := arArchive(1700000000, "abc", "defg")
	b := arArchive(1700000099, "abc", "defg")
	if got := classify(a, b, "/w/a", "/w/b", "/w/a/src"); len(got) != 1 || got[0].Kind != CauseArchiveMeta {
		t.Fatalf("got %+v", got)
	}
	if sameMembers(a, arArchive(1700000000, "abc", "xxxx")) {
		t.Fatal("different member contents compared equal")
	}
}
