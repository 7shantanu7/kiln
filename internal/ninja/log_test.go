package ninja

import (
	"os"
	"path/filepath"
	"testing"
)

func TestReadLogVersions(t *testing.T) {
	for _, v := range []string{"5", "6", "7"} {
		dir := t.TempDir()
		log := "# ninja log v" + v + "\n" +
			"0\t100\t1700000000000000000\ta.o\t1f2e3d4c\n" +
			"0\t90\t1700000000000000000\ta.o\t1f2e3d4c\n" + // a later, rebuilt entry wins
			"100\t150\t1700000000000000000\tlib.a\t5a6b7c8d\n"
		if err := os.WriteFile(filepath.Join(dir, ".ninja_log"), []byte(log), 0o644); err != nil {
			t.Fatal(err)
		}
		got, err := ReadLog(dir)
		if err != nil {
			t.Fatalf("v%s: %v", v, err)
		}
		if a := got[filepath.Join(dir, "a.o")]; len(got) != 2 || a.Duration() != 90 {
			t.Fatalf("v%s: entries %+v", v, got)
		}
	}
	dir := t.TempDir()
	os.WriteFile(filepath.Join(dir, ".ninja_log"), []byte("# ninja log v4\n"), 0o644)
	if _, err := ReadLog(dir); err == nil {
		t.Fatal("v4 should be rejected")
	}
}
