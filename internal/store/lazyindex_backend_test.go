package store

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/proxima360/centauri/internal/model"
	"github.com/proxima360/centauri/internal/objstore"
	"github.com/proxima360/centauri/internal/segment"
)

// memStore is an in-memory SegmentStore standing in for an object store.
type memStore struct{ m map[string][]byte }

func (s *memStore) Get(key string) ([]byte, error) {
	b, ok := s.m[key]
	if !ok {
		return nil, objstore.ErrNotFound
	}
	return b, nil
}
func (s *memStore) Put(key string, data []byte) error { s.m[key] = append([]byte(nil), data...); return nil }
func (s *memStore) Exists(key string) (bool, error)   { _, ok := s.m[key]; return ok, nil }

func TestOpenLazyIndexBackend(t *testing.T) {
	dir := t.TempDir()
	logp := filepath.Join(dir, "src.log")
	st, err := OpenOptions(logp, Options{})
	if err != nil {
		t.Fatal(err)
	}
	for i := 0; i < 10; i++ {
		e := &model.Event{Subject: "item:" + itoa(i), Facet: "f", Type: model.Observed,
			EffectiveTime: int64(1000 + i), Provenance: model.SystemFeed, Confidence: 1,
			Value: map[string]any{"v": i}}
		if err := st.Append(int64(1000+i), []*model.Event{e}, nil); err != nil {
			t.Fatal(err)
		}
	}
	st.Close()

	arch := filepath.Join(dir, "arch")
	if _, err := WriteArchive(logp, arch, 2); err != nil { // several segments
		t.Fatal(err)
	}

	// Load every archive file into the in-memory store, keyed by relative slash path.
	mem := &memStore{m: map[string][]byte{}}
	err = filepath.Walk(arch, func(p string, info os.FileInfo, err error) error {
		if err != nil || info.IsDir() {
			return err
		}
		rel, _ := filepath.Rel(arch, p)
		data, rerr := os.ReadFile(p)
		if rerr != nil {
			return rerr
		}
		mem.m[filepath.ToSlash(rel)] = data
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}

	// Serve from the backend (Merkle-verified on fetch).
	li, err := OpenLazyIndexBackend(mem)
	if err != nil {
		t.Fatalf("open from backend: %v", err)
	}
	if c := li.Current("item:5", "f"); len(c) != 1 || fmtV(c[0].Value["v"]) != "5" {
		t.Fatalf("current item:5 over backend = %v", c)
	}
	if h, _ := li.History("item:5", "f"); len(h) != 1 {
		t.Fatalf("history item:5 over backend = %d, want 1", len(h))
	}

	// Tamper a segment with validly-compressed but WRONG content → Merkle must reject.
	for k := range mem.m {
		if strings.HasPrefix(k, "segments/") {
			bad, _ := segment.Compress([]byte("tampered\n"))
			mem.m[k] = bad
			break
		}
	}
	if _, err := OpenLazyIndexBackend(mem); err == nil {
		t.Fatal("Merkle verification should have rejected the tampered segment")
	}
}

func itoa(i int) string { return string(rune('0'+i)) } // 0..9 only, fine for this test
func fmtV(v any) string {
	switch x := v.(type) {
	case int:
		return itoa(x)
	case float64:
		return itoa(int(x))
	}
	return ""
}
