// Package cas is a disk-backed content-addressable store with an action cache
// and size-bounded least-recently-used eviction.
//
// Layout under the root directory:
//
//	cas/<first 2 hex>/<sha256>   blob contents
//	ac/<first 2 hex>/<sha256>    serialized ActionResult, keyed by action digest
//	tmp/                         in-flight writes, renamed into place when verified
package cas

import (
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"sort"
	"sync"
	"time"
)

// EmptyHash is the SHA-256 of the empty blob, which is always present.
const EmptyHash = "e3b0c44298fc1c149afbf4c8996fb92427ae41e4649b934ca495991b7852b855"

// ErrNotFound is returned when a blob or action result is missing.
var ErrNotFound = errors.New("not found")

// Kind selects the content-addressed area or the action cache.
type Kind int

const (
	CAS Kind = iota
	AC
)

func (k Kind) dir() string {
	if k == AC {
		return "ac"
	}
	return "cas"
}

type entry struct {
	size   int64
	access time.Time
}

// Store is safe for concurrent use.
type Store struct {
	root    string
	maxSize int64

	mu    sync.Mutex
	index map[string]*entry // key: "<kind dir>/<hash>"
	total int64

	evicted int64
}

// Open opens or creates a store under root. maxSize <= 0 disables eviction.
func Open(root string, maxSize int64) (*Store, error) {
	s := &Store{root: root, maxSize: maxSize, index: map[string]*entry{}}
	for _, d := range []string{"cas", "ac", "tmp"} {
		if err := os.MkdirAll(filepath.Join(root, d), 0o755); err != nil {
			return nil, err
		}
	}
	// Leftover temp files are from writes that never finished.
	tmps, _ := os.ReadDir(filepath.Join(root, "tmp"))
	for _, t := range tmps {
		os.Remove(filepath.Join(root, "tmp", t.Name()))
	}
	for _, k := range []Kind{CAS, AC} {
		base := filepath.Join(root, k.dir())
		err := filepath.WalkDir(base, func(p string, d fs.DirEntry, err error) error {
			if err != nil || d.IsDir() {
				return err
			}
			info, err := d.Info()
			if err != nil {
				return err
			}
			s.index[k.dir()+"/"+d.Name()] = &entry{size: info.Size(), access: info.ModTime()}
			s.total += info.Size()
			return nil
		})
		if err != nil {
			return nil, err
		}
	}
	return s, nil
}

func validHash(h string) bool {
	if len(h) != 64 {
		return false
	}
	_, err := hex.DecodeString(h)
	return err == nil
}

func (s *Store) path(k Kind, hash string) string {
	return filepath.Join(s.root, k.dir(), hash[:2], hash)
}

// Has reports whether the blob is present (and marks it recently used).
func (s *Store) Has(k Kind, hash string) bool {
	if k == CAS && hash == EmptyHash {
		return true
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	e, ok := s.index[k.dir()+"/"+hash]
	if ok {
		e.access = time.Now()
	}
	return ok
}

// Get returns the full contents of a blob.
func (s *Store) Get(k Kind, hash string) ([]byte, error) {
	if k == CAS && hash == EmptyHash {
		return nil, nil
	}
	if !validHash(hash) {
		return nil, fmt.Errorf("invalid hash %q", hash)
	}
	b, err := os.ReadFile(s.path(k, hash))
	if errors.Is(err, fs.ErrNotExist) {
		s.forget(k, hash)
		return nil, ErrNotFound
	}
	if err != nil {
		return nil, err
	}
	s.touch(k, hash)
	return b, nil
}

// Open returns a reader for a CAS blob, for streaming large reads.
func (s *Store) OpenBlob(hash string) (*os.File, int64, error) {
	if !validHash(hash) {
		return nil, 0, fmt.Errorf("invalid hash %q", hash)
	}
	f, err := os.Open(s.path(CAS, hash))
	if errors.Is(err, fs.ErrNotExist) {
		s.forget(CAS, hash)
		return nil, 0, ErrNotFound
	}
	if err != nil {
		return nil, 0, err
	}
	info, err := f.Stat()
	if err != nil {
		f.Close()
		return nil, 0, err
	}
	s.touch(CAS, hash)
	return f, info.Size(), nil
}

// Put stores data. For CAS the hash must match the SHA-256 of data; for AC
// the hash is the action digest and data is a serialized ActionResult.
func (s *Store) Put(k Kind, hash string, data []byte) error {
	w, err := s.NewWriter(k, hash)
	if err != nil {
		return err
	}
	if _, err := w.Write(data); err != nil {
		w.Abort()
		return err
	}
	return w.Commit(int64(len(data)))
}

// Writer streams a blob into the store; Commit verifies and publishes it.
type Writer struct {
	s    *Store
	kind Kind
	hash string
	f    *os.File
	h    hashWriter
	n    int64
}

type hashWriter interface {
	io.Writer
	Sum([]byte) []byte
}

// NewWriter starts a streaming write.
func (s *Store) NewWriter(k Kind, hash string) (*Writer, error) {
	if !validHash(hash) {
		return nil, fmt.Errorf("invalid hash %q", hash)
	}
	f, err := os.CreateTemp(filepath.Join(s.root, "tmp"), hash[:8]+"-*")
	if err != nil {
		return nil, err
	}
	return &Writer{s: s, kind: k, hash: hash, f: f, h: sha256.New()}, nil
}

func (w *Writer) Write(p []byte) (int, error) {
	n, err := w.f.Write(p)
	w.h.Write(p[:n])
	w.n += int64(n)
	return n, err
}

// Abort discards the write.
func (w *Writer) Abort() {
	w.f.Close()
	os.Remove(w.f.Name())
}

// Commit checks the size (and, for CAS, the hash) and publishes the blob.
func (w *Writer) Commit(wantSize int64) error {
	defer os.Remove(w.f.Name())
	if err := w.f.Close(); err != nil {
		return err
	}
	if wantSize >= 0 && w.n != wantSize {
		return fmt.Errorf("size mismatch: got %d bytes, want %d", w.n, wantSize)
	}
	if w.kind == CAS {
		if got := hex.EncodeToString(w.h.Sum(nil)); got != w.hash {
			return fmt.Errorf("digest mismatch: content hashes to %s, not %s", got, w.hash)
		}
	}
	dst := w.s.path(w.kind, w.hash)
	if err := os.MkdirAll(filepath.Dir(dst), 0o755); err != nil {
		return err
	}
	if err := os.Rename(w.f.Name(), dst); err != nil {
		return err
	}
	w.s.added(w.kind, w.hash, w.n)
	return nil
}

func (s *Store) touch(k Kind, hash string) {
	s.mu.Lock()
	if e, ok := s.index[k.dir()+"/"+hash]; ok {
		e.access = time.Now()
	}
	s.mu.Unlock()
}

func (s *Store) forget(k Kind, hash string) {
	s.mu.Lock()
	if e, ok := s.index[k.dir()+"/"+hash]; ok {
		s.total -= e.size
		delete(s.index, k.dir()+"/"+hash)
	}
	s.mu.Unlock()
}

func (s *Store) added(k Kind, hash string, size int64) {
	s.mu.Lock()
	defer s.mu.Unlock()
	key := k.dir() + "/" + hash
	if e, ok := s.index[key]; ok {
		s.total -= e.size
	}
	s.index[key] = &entry{size: size, access: time.Now()}
	s.total += size
	if s.maxSize > 0 && s.total > s.maxSize {
		s.evictLocked(s.maxSize * 9 / 10)
	}
}

// evictLocked removes least recently used entries until total <= target.
func (s *Store) evictLocked(target int64) {
	type kv struct {
		key string
		e   *entry
	}
	all := make([]kv, 0, len(s.index))
	for k, e := range s.index {
		all = append(all, kv{k, e})
	}
	sort.Slice(all, func(i, j int) bool { return all[i].e.access.Before(all[j].e.access) })
	for _, it := range all {
		if s.total <= target {
			break
		}
		dir, hash := filepath.Split(it.key)
		os.Remove(filepath.Join(s.root, dir, hash[:2], hash))
		s.total -= it.e.size
		delete(s.index, it.key)
		s.evicted++
	}
}

// Usage returns the number of stored entries, their total size, and how many
// entries have been evicted since Open.
func (s *Store) Usage() (entries int, bytes, evicted int64) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return len(s.index), s.total, s.evicted
}

// HashBytes returns the hex SHA-256 of b.
func HashBytes(b []byte) string {
	h := sha256.Sum256(b)
	return hex.EncodeToString(h[:])
}
