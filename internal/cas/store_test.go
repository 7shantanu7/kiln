package cas

import (
	"bytes"
	"errors"
	"testing"
	"time"
)

func TestPutGetVerifies(t *testing.T) {
	s, err := Open(t.TempDir(), 0)
	if err != nil {
		t.Fatal(err)
	}
	data := []byte("hello kiln")
	h := HashBytes(data)
	if err := s.Put(CAS, h, data); err != nil {
		t.Fatal(err)
	}
	got, err := s.Get(CAS, h)
	if err != nil || !bytes.Equal(got, data) {
		t.Fatalf("Get = %q, %v", got, err)
	}
	if err := s.Put(CAS, HashBytes([]byte("other")), data); err == nil {
		t.Fatal("Put accepted content that does not match its digest")
	}
	if _, err := s.Get(CAS, HashBytes([]byte("missing"))); !errors.Is(err, ErrNotFound) {
		t.Fatalf("Get missing: %v", err)
	}
	if !s.Has(CAS, EmptyHash) {
		t.Fatal("empty blob must always be present")
	}
}

func TestReopenKeepsIndex(t *testing.T) {
	dir := t.TempDir()
	s, _ := Open(dir, 0)
	data := []byte("persisted")
	s.Put(CAS, HashBytes(data), data)
	s2, err := Open(dir, 0)
	if err != nil {
		t.Fatal(err)
	}
	if n, size, _ := s2.Usage(); n != 1 || size != int64(len(data)) {
		t.Fatalf("Usage after reopen = %d entries, %d bytes", n, size)
	}
}

func TestEvictsLeastRecentlyUsed(t *testing.T) {
	s, _ := Open(t.TempDir(), 3000)
	blob := func(c byte) []byte { return bytes.Repeat([]byte{c}, 1000) }
	a, b, c := blob('a'), blob('b'), blob('c')
	s.Put(CAS, HashBytes(a), a)
	time.Sleep(2 * time.Millisecond)
	s.Put(CAS, HashBytes(b), b)
	time.Sleep(2 * time.Millisecond)
	s.Has(CAS, HashBytes(a)) // a is now more recent than b
	time.Sleep(2 * time.Millisecond)
	s.Put(CAS, HashBytes(c), c)
	d := blob('d')
	s.Put(CAS, HashBytes(d), d) // over the limit: evict down to 90%
	if s.Has(CAS, HashBytes(b)) {
		t.Fatal("least recently used blob b was not evicted")
	}
	if !s.Has(CAS, HashBytes(d)) {
		t.Fatal("newest blob d was evicted")
	}
	if _, size, evicted := s.Usage(); size > 3000 || evicted == 0 {
		t.Fatalf("size %d, evicted %d", size, evicted)
	}
}
