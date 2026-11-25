package launcher

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"sort"
	"strings"
	"time"

	repb "github.com/bazelbuild/remote-apis/build/bazel/remote/execution/v2"

	"github.com/7shantanu7/kiln/internal/remote"
)

// Version is mixed into every action key; bump it when key computation changes.
const Version = "kiln-cc/1"

// Options configures one launcher invocation.
type Options struct {
	Addr     string   // cache address; empty disables caching
	Instance string   // REAPI instance name
	BaseDirs []string // path prefixes rewritten so different checkouts share entries
	ReadOnly bool     // never upload (for untrusted builds such as pull requests)
	LogPath  string   // optional JSON-lines log of every invocation
	Timeout  time.Duration
}

// Result is what one invocation did; it is written to the log.
type Result struct {
	Source  string  `json:"source,omitempty"`
	Outcome string  `json:"outcome"` // hit, miss, skip, error
	Reason  string  `json:"reason,omitempty"`
	Millis  float64 `json:"ms"`
	Exit    int     `json:"exit"`
}

// Run executes argv (compiler first) through the cache and returns the exit code.
func Run(opts Options, argv []string) int {
	start := time.Now()
	res := run(opts, argv)
	res.Millis = float64(time.Since(start).Microseconds()) / 1000
	if opts.LogPath != "" {
		if line, err := json.Marshal(res); err == nil {
			if f, err := os.OpenFile(opts.LogPath, os.O_WRONLY|os.O_APPEND|os.O_CREATE, 0o644); err == nil {
				f.Write(append(line, '\n'))
				f.Close()
			}
		}
	}
	return res.Exit
}

func run(opts Options, argv []string) Result {
	c := Parse(argv)
	if opts.Addr == "" {
		return Result{Source: c.Source, Outcome: "skip", Reason: "no cache address", Exit: passthrough(argv)}
	}
	if c.Uncacheable != "" {
		return Result{Source: c.Source, Outcome: "skip", Reason: c.Uncacheable, Exit: passthrough(argv)}
	}
	if opts.Timeout == 0 {
		opts.Timeout = 30 * time.Second
	}
	ctx, cancel := context.WithTimeout(context.Background(), opts.Timeout)
	defer cancel()

	key, err := ComputeKey(c, opts.BaseDirs)
	if err != nil {
		return Result{Source: c.Source, Outcome: "error", Reason: "key: " + err.Error(), Exit: passthrough(argv)}
	}
	client, err := remote.Dial(opts.Addr, opts.Instance)
	if err != nil {
		return Result{Source: c.Source, Outcome: "error", Reason: err.Error(), Exit: passthrough(argv)}
	}
	defer client.Close()

	if ar, ok, err := client.GetActionResult(ctx, key.Action); err != nil {
		warn("cache lookup failed, compiling locally: %v", err)
	} else if ok {
		if err := restore(ctx, client, c, key, ar); err == nil {
			os.Stdout.Write(ar.StdoutRaw)
			os.Stderr.Write(ar.StderrRaw)
			return Result{Source: c.Source, Outcome: "hit", Exit: 0}
		} else {
			warn("cache restore failed, compiling locally: %v", err)
		}
	}

	var stdout, stderr bytes.Buffer
	exit := runWith(argv, &stdout, &stderr)
	os.Stdout.Write(stdout.Bytes())
	os.Stderr.Write(stderr.Bytes())
	if exit != 0 {
		return Result{Source: c.Source, Outcome: "miss", Reason: "compile failed (not cached)", Exit: exit}
	}
	if opts.ReadOnly {
		return Result{Source: c.Source, Outcome: "miss", Reason: "read-only", Exit: 0}
	}
	if err := store(ctx, client, c, key, stdout.Bytes(), stderr.Bytes()); err != nil {
		warn("cache upload failed: %v", err)
		return Result{Source: c.Source, Outcome: "miss", Reason: "upload: " + err.Error(), Exit: 0}
	}
	return Result{Source: c.Source, Outcome: "miss", Exit: 0}
}

func warn(format string, args ...any) {
	fmt.Fprintf(os.Stderr, "kiln-cc: "+format+"\n", args...)
}

func passthrough(argv []string) int {
	return runWith(argv, os.Stdout, os.Stderr)
}

func runWith(argv []string, stdout, stderr interface{ Write([]byte) (int, error) }) int {
	cmd := exec.Command(argv[0], argv[1:]...)
	cmd.Stdin = os.Stdin
	cmd.Stdout = stdout
	cmd.Stderr = stderr
	err := cmd.Run()
	var ee *exec.ExitError
	if errors.As(err, &ee) {
		return ee.ExitCode()
	}
	if err != nil {
		fmt.Fprintf(os.Stderr, "kiln-cc: %v\n", err)
		return 127
	}
	return 0
}

// Key identifies a compile in the action cache.
type Key struct {
	Action *repb.Digest
	// Blobs are the Action, Command and input root Directory. The
	// preprocessed source itself is identified only by its digest: a
	// cache-only client never needs it, and it is ~1 MB per C++ file.
	Blobs   [][]byte
	baseMap *pathMap
}

// Output file names inside the action; they never depend on the build directory.
const (
	outObj = "out.o"
	outDep = "out.d"
)

// ComputeKey preprocesses the source and builds the REAPI Action for it.
func ComputeKey(c *Compile, baseDirs []string) (*Key, error) {
	pm := newPathMap(baseDirs)
	var pre, preErr bytes.Buffer
	if code := runWith(append([]string{c.Compiler}, c.PreprocessArgs()...), &pre, &preErr); code != 0 {
		return nil, fmt.Errorf("preprocessing failed: %s", firstLine(preErr.String()))
	}
	normPre := pm.normalizeLineMarkers(pre.Bytes())

	compilerID, err := compilerIdentity(c.Compiler)
	if err != nil {
		return nil, err
	}
	args := []string{Version, compilerID}
	for i := 0; i < len(c.Args); i++ {
		a := c.Args[i]
		switch {
		case a == "-o" || a == "-MF":
			args = append(args, a, "@OUTPUT@")
			i++
			continue
		case strings.HasPrefix(a, "-o") && len(a) > 2 && !strings.HasPrefix(a, "-objc"):
			args = append(args, "-o@OUTPUT@")
			continue
		}
		args = append(args, pm.normalize(a))
	}
	cmd := &repb.Command{
		Arguments:   args,
		OutputPaths: []string{outObj},
		Platform: &repb.Platform{Properties: []*repb.Platform_Property{
			{Name: "ISA", Value: runtime.GOARCH}, {Name: "OSFamily", Value: runtime.GOOS}}},
	}
	if c.DepFile != "" {
		cmd.OutputPaths = append(cmd.OutputPaths, outDep)
	}
	// Debug info records the working directory and absolute paths unless every
	// base directory is remapped, so such objects are only shared by builds
	// in the same location.
	if c.HasDebugInfo() && !pm.coveredBy(c.PrefixMaps()) {
		cwd, _ := os.Getwd()
		cmd.EnvironmentVariables = append(cmd.EnvironmentVariables,
			&repb.Command_EnvironmentVariable{Name: "KILN_CWD", Value: cwd},
			&repb.Command_EnvironmentVariable{Name: "KILN_BASE_DIRS", Value: strings.Join(baseDirs, ":")})
	}
	cmdDigest, cmdBytes, err := remote.DigestProto(cmd)
	if err != nil {
		return nil, err
	}
	srcDigest := remote.Digest(normPre)
	root := &repb.Directory{Files: []*repb.FileNode{{Name: "source.ii", Digest: srcDigest}}}
	rootDigest, rootBytes, err := remote.DigestProto(root)
	if err != nil {
		return nil, err
	}
	action := &repb.Action{CommandDigest: cmdDigest, InputRootDigest: rootDigest}
	actionDigest, actionBytes, err := remote.DigestProto(action)
	if err != nil {
		return nil, err
	}
	return &Key{Action: actionDigest, Blobs: [][]byte{cmdBytes, rootBytes, actionBytes}, baseMap: pm}, nil
}

func firstLine(s string) string {
	if i := strings.IndexByte(s, '\n'); i >= 0 {
		return s[:i]
	}
	return s
}

func restore(ctx context.Context, client *remote.Client, c *Compile, key *Key, ar *repb.ActionResult) error {
	if ar.ExitCode != 0 {
		return fmt.Errorf("cached result has exit code %d", ar.ExitCode)
	}
	files := map[string]*repb.Digest{}
	var digests []*repb.Digest
	for _, f := range ar.OutputFiles {
		files[f.Path] = f.Digest
		digests = append(digests, f.Digest)
	}
	if files[outObj] == nil || (c.DepFile != "" && files[outDep] == nil) {
		return errors.New("cached result is missing outputs")
	}
	blobs, err := client.Download(ctx, digests)
	if err != nil {
		return err
	}
	if err := writeAtomic(c.Output, blobs[files[outObj].Hash]); err != nil {
		return err
	}
	if c.DepFile != "" {
		if err := writeAtomic(c.DepFile, key.baseMap.denormalize(blobs[files[outDep].Hash])); err != nil {
			return err
		}
	}
	return nil
}

func store(ctx context.Context, client *remote.Client, c *Compile, key *Key, stdout, stderr []byte) error {
	obj, err := os.ReadFile(c.Output)
	if err != nil {
		return err
	}
	ar := &repb.ActionResult{
		ExitCode:    0,
		OutputFiles: []*repb.OutputFile{{Path: outObj, Digest: remote.Digest(obj)}},
		StdoutRaw:   stdout,
		StderrRaw:   stderr,
	}
	blobs := append([][]byte{obj}, key.Blobs...)
	if c.DepFile != "" {
		dep, err := os.ReadFile(c.DepFile)
		if err != nil {
			return err
		}
		norm := key.baseMap.normalizeBytes(dep)
		ar.OutputFiles = append(ar.OutputFiles, &repb.OutputFile{Path: outDep, Digest: remote.Digest(norm)})
		blobs = append(blobs, norm)
	}
	if err := client.Upload(ctx, blobs); err != nil {
		return err
	}
	return client.UpdateActionResult(ctx, key.Action, ar)
}

func writeAtomic(path string, data []byte) error {
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return err
	}
	tmp, err := os.CreateTemp(filepath.Dir(path), ".kiln-cc-*")
	if err != nil {
		return err
	}
	if _, err := tmp.Write(data); err != nil {
		tmp.Close()
		os.Remove(tmp.Name())
		return err
	}
	if err := tmp.Close(); err != nil {
		os.Remove(tmp.Name())
		return err
	}
	os.Chmod(tmp.Name(), 0o644)
	return os.Rename(tmp.Name(), path)
}

// compilerIdentity hashes the resolved compiler path and its --version output,
// caching the result by path, size and modification time.
func compilerIdentity(compiler string) (string, error) {
	path, err := exec.LookPath(compiler)
	if err != nil {
		return "", err
	}
	if real, err := filepath.EvalSymlinks(path); err == nil {
		path = real
	}
	info, err := os.Stat(path)
	if err != nil {
		return "", err
	}
	stamp := fmt.Sprintf("%s|%d|%d", path, info.Size(), info.ModTime().UnixNano())
	sum := sha256.Sum256([]byte(stamp))
	cacheDir, _ := os.UserCacheDir()
	cacheFile := filepath.Join(cacheDir, "kiln", "compilers", hex.EncodeToString(sum[:16]))
	if b, err := os.ReadFile(cacheFile); err == nil && len(b) > 0 {
		return string(b), nil
	}
	out, err := exec.Command(path, "--version").Output()
	if err != nil {
		return "", fmt.Errorf("%s --version: %v", path, err)
	}
	h := sha256.Sum256(append([]byte(filepath.Base(path)+"\n"), out...))
	id := filepath.Base(path) + "@" + hex.EncodeToString(h[:8])
	os.MkdirAll(filepath.Dir(cacheFile), 0o755)
	writeAtomic(cacheFile, []byte(id))
	return id, nil
}

// pathMap rewrites base directories to stable placeholders.
type pathMap struct {
	dirs  []string // longest first
	names map[string]string
}

func newPathMap(baseDirs []string) *pathMap {
	pm := &pathMap{names: map[string]string{}}
	for i, d := range baseDirs {
		d = filepath.Clean(d)
		if d == "/" || d == "." || d == "" {
			continue
		}
		pm.dirs = append(pm.dirs, d)
		pm.names[d] = fmt.Sprintf("@BASE%d@", i)
	}
	sort.Slice(pm.dirs, func(i, j int) bool { return len(pm.dirs[i]) > len(pm.dirs[j]) })
	return pm
}

func (pm *pathMap) normalize(s string) string {
	for _, d := range pm.dirs {
		s = strings.ReplaceAll(s, d, pm.names[d])
	}
	return s
}

func (pm *pathMap) normalizeBytes(b []byte) []byte {
	for _, d := range pm.dirs {
		b = bytes.ReplaceAll(b, []byte(d), []byte(pm.names[d]))
	}
	return b
}

func (pm *pathMap) denormalize(b []byte) []byte {
	for _, d := range pm.dirs {
		b = bytes.ReplaceAll(b, []byte(pm.names[d]), []byte(d))
	}
	return b
}

// normalizeLineMarkers rewrites base directories only inside preprocessor
// line markers (`# 12 "/path" 1`). Paths elsewhere, such as an expanded
// __FILE__, end up in the object file, so they must stay in the key.
func (pm *pathMap) normalizeLineMarkers(pre []byte) []byte {
	if len(pm.dirs) == 0 {
		return pre
	}
	var out bytes.Buffer
	out.Grow(len(pre))
	for len(pre) > 0 {
		line := pre
		if i := bytes.IndexByte(pre, '\n'); i >= 0 {
			line, pre = pre[:i+1], pre[i+1:]
		} else {
			pre = nil
		}
		if len(line) > 2 && line[0] == '#' && line[1] == ' ' {
			line = pm.normalizeBytes(line)
		}
		out.Write(line)
	}
	return out.Bytes()
}

// coveredBy reports whether every base directory is remapped by a prefix map.
func (pm *pathMap) coveredBy(olds []string) bool {
	if len(pm.dirs) == 0 {
		return false
	}
	for _, d := range pm.dirs {
		ok := false
		for _, o := range olds {
			o = filepath.Clean(o)
			if d == o || strings.HasPrefix(d, o+"/") {
				ok = true
			}
		}
		if !ok {
			return false
		}
	}
	return true
}
