package store

import (
	"fmt"
	"os"
	"path/filepath"
	"testing"

	"github.com/proxima360/centauri/internal/model"
)

// Sealing a log into compressed segments must reproduce the ENGINE's exact hash
// chain (proving the archive is byte-faithful), and tampering with a segment
// must be detected.
func TestArchiveRoundTripAndTamper(t *testing.T) {
	dir := t.TempDir()
	logp := filepath.Join(dir, "c.log")
	st, err := OpenOptions(logp, Options{})
	if err != nil {
		t.Fatal(err)
	}
	for i := 0; i < 5; i++ {
		e := &model.Event{Subject: fmt.Sprintf("item:%d", i), Facet: "f", Type: model.Observed,
			Value:      map[string]any{"price_cents": i * 100, "region": "EU"},
			Provenance: model.SystemFeed, Confidence: 1}
		if err := st.Append(int64(1000+i), []*model.Event{e}, nil); err != nil {
			t.Fatal(err)
		}
	}
	liveHead, _ := st.ChainHead()
	st.Close()

	arch := filepath.Join(dir, "archive")
	man, err := WriteArchive(logp, arch, 2) // small segments → several
	if err != nil {
		t.Fatal(err)
	}
	if len(man.Segments) < 2 {
		t.Fatalf("expected multiple segments, got %d", len(man.Segments))
	}

	head, recs, err := VerifyArchive(arch)
	if err != nil {
		t.Fatalf("verify: %v", err)
	}
	if recs != 5 {
		t.Fatalf("records = %d, want 5", recs)
	}
	if head != liveHead {
		t.Fatalf("archive chain head %s != live %s — archive is not byte-faithful", head, liveHead)
	}

	// Tamper: corrupt the first segment; verification must fail.
	p := filepath.Join(arch, filepath.FromSlash(man.Segments[0].Path))
	b, err := os.ReadFile(p)
	if err != nil || len(b) == 0 {
		t.Fatalf("read segment: %v", err)
	}
	b[len(b)/2] ^= 0xFF
	if err := os.WriteFile(p, b, 0o644); err != nil {
		t.Fatal(err)
	}
	if _, _, err := VerifyArchive(arch); err == nil {
		t.Fatal("verification must fail on a tampered segment")
	}
}

// Opening a store directly on a segmented archive must reproduce the source
// log's chain + index, queries must work, and appends must land in the tail and
// persist across reopen.
func TestOpenArchive(t *testing.T) {
	dir := t.TempDir()
	logp := filepath.Join(dir, "src.log")
	st, err := OpenOptions(logp, Options{})
	if err != nil {
		t.Fatal(err)
	}
	for i := 0; i < 4; i++ {
		e := &model.Event{Subject: fmt.Sprintf("item:%d", i), Facet: "f", Type: model.Observed,
			Value: map[string]any{"price_cents": i * 10}, Provenance: model.SystemFeed, Confidence: 1}
		if err := st.Append(int64(1000+i), []*model.Event{e}, nil); err != nil {
			t.Fatal(err)
		}
	}
	liveHead, _ := st.ChainHead()
	wantSubjects := len(st.Subjects())
	st.Close()

	arch := filepath.Join(dir, "arch")
	if _, err := WriteArchive(logp, arch, 2); err != nil {
		t.Fatal(err)
	}

	a, err := OpenArchive(arch, Options{})
	if err != nil {
		t.Fatalf("OpenArchive: %v", err)
	}
	if got, _ := a.ChainHead(); got != liveHead {
		t.Fatalf("archive head %s != live %s — segment replay not faithful", got, liveHead)
	}
	if len(a.Subjects()) != wantSubjects {
		t.Fatalf("archive subjects = %d, want %d", len(a.Subjects()), wantSubjects)
	}
	if c := a.Current("item:2", "f"); len(c) != 1 || fmt.Sprint(c[0].Value["price_cents"]) != "20" {
		t.Fatalf("item:2 current = %#v, want price 20", c)
	}
	// Append → lands in the appendable tail.
	if err := a.Append(5000, []*model.Event{{Subject: "item:new", Facet: "f", Type: model.Observed,
		Value: map[string]any{"price_cents": 999}, Provenance: model.SystemFeed, Confidence: 1}}, nil); err != nil {
		t.Fatal(err)
	}
	a.Close()

	b, err := OpenArchive(arch, Options{})
	if err != nil {
		t.Fatal(err)
	}
	defer b.Close()
	if c := b.Current("item:new", "f"); len(c) != 1 || fmt.Sprint(c[0].Value["price_cents"]) != "999" {
		t.Fatalf("appended fact didn't persist in the tail: %#v", c)
	}
}

// Sealing must roll the tail into a new segment without changing the chain,
// reset the tail, keep data queryable, and survive a reopen + verify.
func TestSeal(t *testing.T) {
	dir := t.TempDir()
	logp := filepath.Join(dir, "src.log")
	st, err := OpenOptions(logp, Options{})
	if err != nil {
		t.Fatal(err)
	}
	if err := st.Append(1000, []*model.Event{{Subject: "item:1", Facet: "f", Type: model.Observed,
		Value: map[string]any{"n": 1}, Provenance: model.SystemFeed, Confidence: 1}}, nil); err != nil {
		t.Fatal(err)
	}
	st.Close()
	arch := filepath.Join(dir, "arch")
	if _, err := WriteArchive(logp, arch, 100); err != nil {
		t.Fatal(err)
	}

	a, err := OpenArchive(arch, Options{})
	if err != nil {
		t.Fatal(err)
	}
	for i := 2; i <= 4; i++ {
		if err := a.Append(int64(1000+i), []*model.Event{{Subject: fmt.Sprintf("item:%d", i), Facet: "f",
			Type: model.Observed, Value: map[string]any{"n": i}, Provenance: model.SystemFeed, Confidence: 1}}, nil); err != nil {
			t.Fatal(err)
		}
	}
	headBefore, _ := a.ChainHead()
	if err := a.Seal(); err != nil {
		t.Fatalf("seal: %v", err)
	}
	headAfter, sz := a.ChainHead()
	if headAfter != headBefore {
		t.Fatalf("seal must not change the chain head: %s != %s", headAfter, headBefore)
	}
	if sz != 0 {
		t.Fatalf("tail size after seal = %d, want 0", sz)
	}
	if c := a.Current("item:3", "f"); len(c) != 1 {
		t.Fatal("item:3 missing in-RAM after seal")
	}
	a.Close()

	// Reopen entirely from segments; data + chain intact, archive verifies.
	b, err := OpenArchive(arch, Options{})
	if err != nil {
		t.Fatal(err)
	}
	defer b.Close()
	if h, _ := b.ChainHead(); h != headBefore {
		t.Fatalf("reopen head %s != %s", h, headBefore)
	}
	for i := 1; i <= 4; i++ {
		if c := b.Current(fmt.Sprintf("item:%d", i), "f"); len(c) != 1 {
			t.Fatalf("item:%d missing after reopen", i)
		}
	}
	if _, _, err := VerifyArchive(arch); err != nil {
		t.Fatalf("verify after seal: %v", err)
	}
}

// GC must remove crash-orphaned files (stale tail generations, unreferenced
// segments, a temp manifest) and nothing the manifest references.
func TestGCArchive(t *testing.T) {
	dir := t.TempDir()
	logp := filepath.Join(dir, "src.log")
	st, err := OpenOptions(logp, Options{})
	if err != nil {
		t.Fatal(err)
	}
	if err := st.Append(1000, []*model.Event{{Subject: "item:1", Facet: "f", Type: model.Observed,
		Value: map[string]any{"n": 1}, Provenance: model.SystemFeed, Confidence: 1}}, nil); err != nil {
		t.Fatal(err)
	}
	st.Close()
	arch := filepath.Join(dir, "arch")
	if _, err := WriteArchive(logp, arch, 100); err != nil {
		t.Fatal(err)
	}

	// Plant crash-orphans.
	orphans := []string{"current.00000099.log", filepath.Join("segments", "00000099.seg"), "manifest.json.tmp"}
	for _, o := range orphans {
		if err := os.WriteFile(filepath.Join(arch, o), []byte("junk"), 0o644); err != nil {
			t.Fatal(err)
		}
	}

	removed, err := GCArchive(arch)
	if err != nil {
		t.Fatal(err)
	}
	if len(removed) != 3 {
		t.Fatalf("removed %v, want 3 orphans", removed)
	}
	for _, o := range orphans {
		if _, err := os.Stat(filepath.Join(arch, o)); !os.IsNotExist(err) {
			t.Fatalf("orphan %s should have been removed", o)
		}
	}
	// The real segment + manifest survive and the archive still verifies.
	if _, err := os.Stat(filepath.Join(arch, "segments", "00000001.seg")); err != nil {
		t.Fatal("GC removed a referenced segment!")
	}
	if _, _, err := VerifyArchive(arch); err != nil {
		t.Fatalf("verify after GC: %v", err)
	}
}

