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
