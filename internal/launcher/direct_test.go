package launcher

import (
	"bytes"
	"encoding/json"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"google.golang.org/grpc"

	"github.com/7shantanu7/kiln/internal/cacheserver"
	"github.com/7shantanu7/kiln/internal/cas"
)

func TestLineMarkerFiles(t *testing.T) {
	pre := []byte(`# 0 "/w/a.cpp"
# 0 "/w/build//"
# 0 "<built-in>"
# 0 "<command-line>"
# 1 "/usr/include/stdc-predef.h" 1 3 4
# 1 "/w/a.cpp"
# 1 "/w/inc/a \"q\".h" 1
int x;
# 2 "/w/a.cpp" 2
#pragma once
`)
	got := lineMarkerFiles(pre)
	want := []string{"/w/a.cpp", "/usr/include/stdc-predef.h", `/w/inc/a "q".h`}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("lineMarkerFiles = %q, want %q", got, want)
	}
}

// startCache serves a kiln-cache on a loopback port for the launcher to dial.
func startCache(t *testing.T) string {
	t.Helper()
	store, err := cas.Open(t.TempDir(), 0)
	if err != nil {
		t.Fatal(err)
	}
	lis, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	g := grpc.NewServer(grpc.MaxRecvMsgSize(cacheserver.MaxBatchBytes + (1 << 20)))
	cacheserver.New(store).Register(g)
	go g.Serve(lis)
	t.Cleanup(g.Stop)
	return lis.Addr().String()
}

// checkout writes a.cpp and inc/a.h under root.
func checkout(t *testing.T, root, header, source string) {
	t.Helper()
	for name, body := range map[string]string{"inc/a.h": header, "a.cpp": source} {
		p := filepath.Join(root, name)
		if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(p, []byte(body), 0o644); err != nil {
			t.Fatal(err)
		}
	}
}

func TestDirectMode(t *testing.T) {
	cxx, err := exec.LookPath("c++")
	if err != nil {
		t.Skip("no c++ compiler")
	}
	t.Setenv("XDG_CACHE_HOME", t.TempDir())
	addr := startCache(t)
	tmp := t.TempDir()
	log := filepath.Join(tmp, "log.jsonl")
	header := "#pragma once\n#include <string>\ninline std::string name() { return \"kiln\"; }\n"
	source := "#include \"a.h\"\nint size() { return name().size(); }\n"

	compile := func(root string, extra ...string) Result {
		t.Helper()
		os.Remove(log)
		opts := Options{Addr: addr, BaseDirs: []string{root}, LogPath: log}
		argv := append([]string{cxx, "-O1", "-I" + root + "/inc", "-ffile-prefix-map=" + root + "=.",
			"-MD", "-MF", root + "/a.o.d", "-c", root + "/a.cpp", "-o", root + "/a.o"}, extra...)
		if code := Run(opts, argv); code != 0 {
			t.Fatalf("compile in %s exited %d", root, code)
		}
		b, err := os.ReadFile(log)
		if err != nil {
			t.Fatal(err)
		}
		var r Result
		if err := json.Unmarshal(b, &r); err != nil {
			t.Fatal(err)
		}
		return r
	}

	a, b := filepath.Join(tmp, "a"), filepath.Join(tmp, "b-elsewhere")
	checkout(t, a, header, source)
	checkout(t, b, header, source)
	if r := compile(a); r.Outcome != "miss" {
		t.Fatalf("first compile: %+v, want miss", r)
	}
	if r := compile(b); r.Outcome != "hit" || r.Mode != "direct" {
		t.Fatalf("second checkout: %+v, want a direct hit", r)
	}
	objA, _ := os.ReadFile(filepath.Join(a, "a.o"))
	objB, _ := os.ReadFile(filepath.Join(b, "a.o"))
	if len(objA) == 0 || !bytes.Equal(objA, objB) {
		t.Fatal("direct hit restored a different object")
	}
	dep, _ := os.ReadFile(filepath.Join(b, "a.o.d"))
	if !strings.Contains(string(dep), b+"/inc/a.h") {
		t.Fatalf("depfile not rewritten for checkout b:\n%s", dep)
	}

	// A header change must never be served from the old entry.
	checkout(t, b, header+"inline int changed() { return 1; }\n", source)
	if r := compile(b); r.Outcome != "miss" {
		t.Fatalf("after editing a header: %+v, want miss", r)
	}
	// Both header versions are now in the manifest.
	checkout(t, a, header, source)
	if r := compile(a); r.Outcome != "hit" || r.Mode != "direct" {
		t.Fatalf("original header again: %+v, want a direct hit", r)
	}

	// __TIME__ makes the output depend on when it was compiled: never direct.
	timed := source + "const char* built = __TIME__;\n"
	checkout(t, a, header, timed)
	compile(a)
	if r := compile(a); r.Mode == "direct" {
		t.Fatalf("__TIME__ source served in direct mode: %+v", r)
	}
}
