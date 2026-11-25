package sandbox

import (
	"os"
	"path/filepath"
	"testing"
)

func TestMain(m *testing.M) {
	Shim() // the test binary doubles as the shim when re-executed
	os.Exit(m.Run())
}

func TestTraceShellCommand(t *testing.T) {
	dir := t.TempDir()
	os.WriteFile(filepath.Join(dir, "in.txt"), []byte("hello\n"), 0o644)
	res, err := Run(dir, []string{"/bin/sh", "-c", "cat in.txt > tmp.txt && mv tmp.txt out.txt && ls . >/dev/null; exit 3"}, nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	if res.ExitCode != 3 {
		t.Fatalf("exit code %d, want 3", res.ExitCode)
	}
	read, wrote, renamed, listed := false, false, false, false
	for _, p := range res.Subtree(res.Root) {
		for _, a := range p.Accesses {
			switch {
			case a.Path == filepath.Join(dir, "in.txt") && !a.Write:
				read = true
			case a.Path == filepath.Join(dir, "tmp.txt") && a.Write:
				wrote = true
			case a.Path == filepath.Join(dir, "out.txt") && a.Write:
				renamed = true
			case a.Path == dir && a.Dir:
				listed = true
			}
		}
	}
	if !read || !wrote || !renamed || !listed {
		t.Fatalf("read=%v wrote=%v renamed=%v listed=%v", read, wrote, renamed, listed)
	}
	// sh forks cat, mv and ls: they must appear as descendants with argv.
	argv0 := map[string]bool{}
	for _, p := range res.Subtree(res.Root) {
		if len(p.Argv) > 0 {
			argv0[filepath.Base(p.Argv[0])] = true
		}
	}
	for _, want := range []string{"cat", "mv", "ls"} {
		if !argv0[want] {
			t.Errorf("no traced process for %s (saw %v)", want, argv0)
		}
	}
}

func TestFailedOpenIsMarkedMissing(t *testing.T) {
	dir := t.TempDir()
	res, err := Run(dir, []string{"/bin/sh", "-c", "cat missing.txt 2>/dev/null; true"}, nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	found := false
	for _, p := range res.Subtree(res.Root) {
		for _, a := range p.Accesses {
			if filepath.Base(a.Path) == "missing.txt" {
				if !a.Missing || a.Write {
					t.Fatalf("failed open recorded as %+v", a)
				}
				found = true
			}
		}
	}
	if !found {
		t.Fatal("failed open of missing.txt not recorded")
	}
}

func TestArgvIsFirstExec(t *testing.T) {
	// sh -c with a single command execs it in place; Argv must stay sh's.
	res, err := Run(t.TempDir(), []string{"/bin/sh", "-c", "exec /bin/true"}, nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	if argv := res.Processes[res.Root].Argv; len(argv) < 3 || argv[0] != "/bin/sh" {
		t.Fatalf("root argv = %v", argv)
	}
}
