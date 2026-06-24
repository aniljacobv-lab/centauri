package store

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/proxima360/centauri/internal/model"
)

func TestVerifyArchiveParallel(t *testing.T) {
	dir := t.TempDir()
	logp := filepath.Join(dir, "src.log")
	st, err := OpenOptions(logp, Options{})
	if err != nil {
		t.Fatal(err)
	}
	for i := 0; i < 10; i++ {
		e := &model.Event{Subject: "item:1", Facet: "f", Type: model.Observed, EffectiveTime: int64(1000 + i),
			Provenance: model.SystemFeed, Confidence: 1, Value: map[string]any{"v": i}}
		if err := st.Append(int64(1000+i), []*model.Event{e}, nil); err != nil {
			t.Fatal(err)
		}
	}
	st.Close()

	arch := filepath.Join(dir, "arch")
	if _, err := WriteArchive(logp, arch, 2); err != nil { // small segMax → several segments
		t.Fatal(err)
	}

	// Faithful archive: parallel scrub passes and counts every record.
	records, err := VerifyArchiveParallel(arch)
	if err != nil {
		t.Fatalf("parallel verify of a faithful archive failed: %v", err)
	}
	// Cross-check the count against the sequential verifier.
	_, seqRecords, serr := VerifyArchive(arch)
	if serr != nil {
		t.Fatal(serr)
	}
	if records != seqRecords {
		t.Fatalf("parallel records=%d, sequential=%d", records, seqRecords)
	}

	// Tamper one segment's bytes → parallel scrub must catch it.
	entries, err := os.ReadDir(filepath.Join(arch, "segments"))
	if err != nil || len(entries) == 0 {
		t.Fatalf("no segments to tamper: %v", err)
	}
	victim := filepath.Join(arch, "segments", entries[0].Name())
	if err := os.WriteFile(victim, []byte("CORRUPTED"), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := VerifyArchiveParallel(arch); err == nil {
		t.Fatal("parallel verify should have detected the tampered segment")
	}
}
