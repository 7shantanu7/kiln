package launcher

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"os"
	"strconv"
	"strings"
	"time"

	repb "github.com/bazelbuild/remote-apis/build/bazel/remote/execution/v2"

	"github.com/7shantanu7/kiln/internal/remote"
)

// Direct mode finds a compile's cache key without running the preprocessor,
// which is most of the cost of a cache hit.
//
// The manifest key covers what the preprocessor's input is apart from the
// headers: the compiler, the normalized arguments, the include-path
// environment and the source file's content. Its value, stored in the action
// cache like any result, lists the files earlier compiles of that source read
// (every file named in the preprocessor's line markers, system headers
// included) with their hashes, and the preprocessed key they produced. If
// every listed file still has the same content, the preprocessed key is the
// same, so the compile is looked up under it directly.
//
// As with ccache's direct mode, a header created earlier on the include path
// than the one a compile used, shadowing it, is not detected. Compiles whose
// output depends on more than their inputs are never recorded: anything that
// uses __DATE__, __TIME__ or __TIMESTAMP__, anything that uses __FILE__ when
// base directories are rewritten but __FILE__ is not prefix-mapped, and any
// compile that read a file modified while it ran.
const directVersion = "kiln-cc-direct/1"

const (
	manifestFile       = "manifest.json"
	maxManifestEntries = 8
)

// Environment variables that change where headers are found.
var includeEnv = []string{"CPATH", "C_INCLUDE_PATH", "CPLUS_INCLUDE_PATH", "OBJC_INCLUDE_PATH"}

type manifest struct {
	Entries []manifestEntry `json:"entries"`
}

type manifestEntry struct {
	Files  []fileHash `json:"files"`
	Action string     `json:"action"` // hash/size of the preprocessed key
}

type fileHash struct {
	Path   string `json:"path"` // base directories normalized
	SHA256 string `json:"sha256"`
	Size   int64  `json:"size"`
}

type directKey struct {
	action *repb.Digest
	blobs  [][]byte
	pm     *pathMap
	have   *manifest           // the manifest found by lookup, if any
	hashes map[string]fileHash // per-invocation memo, by real path
}

func (kb *keyBase) directKey(c *Compile) (*directKey, error) {
	src, err := os.ReadFile(c.Source)
	if err != nil {
		return nil, err
	}
	cmd := &repb.Command{
		Arguments:            append([]string{directVersion}, kb.args...),
		EnvironmentVariables: append([]*repb.Command_EnvironmentVariable(nil), kb.env...),
		OutputPaths:          []string{manifestFile},
	}
	for _, k := range includeEnv {
		if v, ok := os.LookupEnv(k); ok {
			cmd.EnvironmentVariables = append(cmd.EnvironmentVariables,
				&repb.Command_EnvironmentVariable{Name: k, Value: kb.pm.normalize(v)})
		}
	}
	d, blobs, err := actionDigest(cmd, "source", remote.Digest(src))
	if err != nil {
		return nil, err
	}
	return &directKey{action: d, blobs: blobs, pm: kb.pm, hashes: map[string]fileHash{}}, nil
}

// lookup returns the preprocessed key of the first manifest entry whose files
// all match, or nil.
func (dk *directKey) lookup(ctx context.Context, client *remote.Client) (*repb.Digest, error) {
	ar, ok, err := client.GetActionResult(ctx, dk.action, manifestFile)
	if err != nil || !ok {
		return nil, err
	}
	_, _, files, err := client.ResultBlobs(ctx, ar)
	if err != nil {
		return nil, err
	}
	m := &manifest{}
	if err := json.Unmarshal(files[manifestFile], m); err != nil {
		return nil, fmt.Errorf("bad manifest: %v", err)
	}
	dk.have = m
	for _, e := range m.Entries {
		if dk.matches(e) {
			return parseDigest(e.Action)
		}
	}
	return nil, nil
}

func (dk *directKey) matches(e manifestEntry) bool {
	for _, f := range e.Files {
		got, err := dk.hash(string(dk.pm.denormalize([]byte(f.Path))))
		if err != nil || got.Size != f.Size || got.SHA256 != f.SHA256 {
			return false
		}
	}
	return len(e.Files) > 0
}

func (dk *directKey) hash(path string) (fileHash, error) {
	if h, ok := dk.hashes[path]; ok {
		return h, nil
	}
	b, err := os.ReadFile(path)
	if err != nil {
		return fileHash{}, err
	}
	sum := sha256.Sum256(b)
	h := fileHash{Path: path, SHA256: hex.EncodeToString(sum[:]), Size: int64(len(b))}
	dk.hashes[path] = h
	return h, nil
}

// record adds an entry mapping the files this compile read (from the line
// markers in pre) to its preprocessed key. began is when the invocation
// started: a file modified since then may not be what the compiler read.
func (dk *directKey) record(ctx context.Context, client *remote.Client, c *Compile, kb *keyBase, action *repb.Digest, pre []byte, began time.Time) error {
	fileMacroSafe := len(kb.pm.dirs) == 0 || kb.pm.coveredBy(c.MacroPrefixMaps())
	var entry manifestEntry
	for _, path := range lineMarkerFiles(pre) {
		info, err := os.Stat(path)
		if err != nil {
			return nil // a file the compile read is gone; nothing reliable to record
		}
		if info.IsDir() {
			continue
		}
		if !info.ModTime().Before(began) {
			return nil
		}
		b, err := os.ReadFile(path)
		if err != nil {
			return nil
		}
		if bytes.Contains(b, []byte("__DATE__")) || bytes.Contains(b, []byte("__TIME__")) ||
			bytes.Contains(b, []byte("__TIMESTAMP__")) {
			return nil
		}
		if !fileMacroSafe && (bytes.Contains(b, []byte("__FILE__")) || bytes.Contains(b, []byte("__BASE_FILE__"))) {
			return nil
		}
		sum := sha256.Sum256(b)
		entry.Files = append(entry.Files, fileHash{
			Path: kb.pm.normalize(path), SHA256: hex.EncodeToString(sum[:]), Size: int64(len(b))})
	}
	if len(entry.Files) == 0 {
		return nil
	}
	entry.Action = fmt.Sprintf("%s/%d", action.Hash, action.SizeBytes)

	m := &manifest{Entries: []manifestEntry{entry}}
	if dk.have != nil {
		for _, e := range dk.have.Entries {
			if sameEntry(e, entry) {
				return nil // already recorded
			}
		}
		for _, e := range dk.have.Entries {
			if e.Action != entry.Action && len(m.Entries) < maxManifestEntries {
				m.Entries = append(m.Entries, e)
			}
		}
	}
	body, err := json.Marshal(m)
	if err != nil {
		return err
	}
	if err := client.Upload(ctx, append([][]byte{body}, dk.blobs...)); err != nil {
		return err
	}
	return client.UpdateActionResult(ctx, dk.action, &repb.ActionResult{
		OutputFiles: []*repb.OutputFile{{Path: manifestFile, Digest: remote.Digest(body)}},
	})
}

func sameEntry(a, b manifestEntry) bool {
	if a.Action != b.Action || len(a.Files) != len(b.Files) {
		return false
	}
	for i := range a.Files {
		if a.Files[i] != b.Files[i] {
			return false
		}
	}
	return true
}

// lineMarkerFiles returns, in order of first appearance, the files named in
// preprocessor line markers (`# 12 "path" 1 3`), skipping <built-in> and
// <command-line> and GCC's working-directory marker.
func lineMarkerFiles(pre []byte) []string {
	seen := map[string]bool{}
	var out []string
	for len(pre) > 0 {
		line := pre
		if i := bytes.IndexByte(pre, '\n'); i >= 0 {
			line, pre = pre[:i], pre[i+1:]
		} else {
			pre = nil
		}
		if len(line) < 5 || line[0] != '#' || line[1] != ' ' || line[2] < '0' || line[2] > '9' {
			continue
		}
		q := bytes.IndexByte(line, '"')
		if q < 0 {
			continue
		}
		name, ok := unquote(line[q:])
		// GCC names the working directory as "dir//" in -g builds; the key
		// already covers it (by prefix maps, or KILN_CWD).
		if !ok || name == "" || name[0] == '<' || strings.HasSuffix(name, "//") || seen[name] {
			continue
		}
		seen[name] = true
		out = append(out, name)
	}
	return out
}

// unquote reads the C string literal at the start of b.
func unquote(b []byte) (string, bool) {
	end := 1
	for end < len(b) && b[end] != '"' {
		if b[end] == '\\' {
			end++
		}
		end++
	}
	if end >= len(b) {
		return "", false
	}
	s, err := strconv.Unquote(string(b[:end+1]))
	if err != nil {
		return string(b[1:end]), true
	}
	return s, true
}

func parseDigest(s string) (*repb.Digest, error) {
	i := bytes.LastIndexByte([]byte(s), '/')
	if i < 0 {
		return nil, fmt.Errorf("bad digest %q", s)
	}
	n, err := strconv.ParseInt(s[i+1:], 10, 64)
	if err != nil {
		return nil, fmt.Errorf("bad digest %q", s)
	}
	return &repb.Digest{Hash: s[:i], SizeBytes: n}, nil
}
