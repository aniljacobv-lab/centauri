package store

import (
	"fmt"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/proxima360/centauri/internal/model"
)

// The lazy index must hold only the CURRENT fact per (subject,facet) — so its RAM
// footprint scales with live subjects, not total events — while still answering
// Current like the in-RAM engine and History from disk.
func TestLazyIndexScalesWithSubjects(t *testing.T) {
	dir := t.TempDir()
	logp := filepath.Join(dir, "src.log")
	st, err := OpenOptions(logp, Options{})
	if err != nil {
		t.Fatal(err)
	}
	const subjects, versions = 3, 5
	for s := 0; s < subjects; s++ {
		for v := 0; v < versions; v++ {
			now := int64(1000 + s*100 + v) // strictly increasing effective/recorded
			e := &model.Event{Subject: fmt.Sprintf("item:%d", s), Facet: "f", Type: model.Observed,
				Value: map[string]any{"v": v}, EffectiveTime: now,
				Provenance: model.SystemFeed, Confidence: 1}
			if err := st.Append(now, []*model.Event{e}, nil); err != nil {
				t.Fatal(err)
			}
		}
	}
	wantCur := make([][]*model.Event, subjects)
	for s := 0; s < subjects; s++ {
		wantCur[s] = st.Current(fmt.Sprintf("item:%d", s), "f")
	}
	st.Close()

	arch := filepath.Join(dir, "arch")
	if _, err := WriteArchive(logp, arch, 4); err != nil {
		t.Fatal(err)
	}

	li, err := OpenLazyIndex(arch)
	if err != nil {
		t.Fatal(err)
	}
	// RAM footprint = one pointer per subject, NOT subjects*versions events.
	if li.Keys() != subjects {
		t.Fatalf("lazy index holds %d keys, want %d (must not scale with %d total events)",
			li.Keys(), subjects, subjects*versions)
	}
	for s := 0; s < subjects; s++ {
		subj := fmt.Sprintf("item:%d", s)
		c := li.Current(subj, "f")
		if len(c) != 1 {
			t.Fatalf("lazy Current(%s) returned %d, want 1", subj, len(c))
		}
		if fmt.Sprint(c[0].Value["v"]) != fmt.Sprint(wantCur[s][0].Value["v"]) {
			t.Fatalf("lazy Current(%s) = %v, want %v (in-RAM)", subj, c[0].Value["v"], wantCur[s][0].Value["v"])
		}
		h, err := li.History(subj, "f")
		if err != nil {
			t.Fatal(err)
		}
		if len(h) != versions {
			t.Fatalf("lazy History(%s) = %d events, want %d (full timeline from disk)", subj, len(h), versions)
		}
	}
}

// The pointer-checkpoint must let reopen skip segments already folded in: after
// SaveCheckpoint, corrupting every segment file must NOT break Current (it is
// served from the checkpoint, not re-read from disk), and the answer must match.
func TestLazyCheckpointSkipsFoldedSegments(t *testing.T) {
	dir := t.TempDir()
	logp := filepath.Join(dir, "src.log")
	st, err := OpenOptions(logp, Options{})
	if err != nil {
		t.Fatal(err)
	}
	for v := 0; v < 3; v++ {
		now := int64(1000 + v)
		e := &model.Event{Subject: "item:1", Facet: "f", Type: model.Observed,
			Value: map[string]any{"v": v}, EffectiveTime: now,
			Provenance: model.SystemFeed, Confidence: 1}
		if err := st.Append(now, []*model.Event{e}, nil); err != nil {
			t.Fatal(err)
		}
	}
	st.Close()

	arch := filepath.Join(dir, "arch")
	if _, err := WriteArchive(logp, arch, 1); err != nil { // every record in its own segment
		t.Fatal(err)
	}

	li1, err := OpenLazyIndex(arch)
	if err != nil {
		t.Fatal(err)
	}
	want := li1.Current("item:1", "f")
	if len(want) != 1 {
		t.Fatalf("first open Current = %d, want 1", len(want))
	}
	if err := li1.SaveCheckpoint(); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join(arch, lazyCheckpointName)); err != nil {
		t.Fatalf("checkpoint not written: %v", err)
	}

	// Corrupt every sealed segment. A correct reopen seeds from the checkpoint and
	// never re-reads them, so Current must still work.
	segDir := filepath.Join(arch, "segments")
	entries, err := os.ReadDir(segDir)
	if err != nil {
		t.Fatal(err)
	}
	for _, e := range entries {
		if err := os.WriteFile(filepath.Join(segDir, e.Name()), []byte("CORRUPT"), 0o644); err != nil {
			t.Fatal(err)
		}
	}

	li2, err := OpenLazyIndex(arch)
	if err != nil {
		t.Fatalf("reopen from checkpoint failed (did it re-read corrupt segments?): %v", err)
	}
	got := li2.Current("item:1", "f")
	if len(got) != 1 || fmt.Sprint(got[0].Value["v"]) != fmt.Sprint(want[0].Value["v"]) {
		t.Fatalf("reopen Current = %v, want %v (from checkpoint)", got, want)
	}
}

// Keyword BM25 over the archive must find the right current fact, both via the
// resident LazyIndex.Search and the standalone ScanSearch.
func TestLazySearch(t *testing.T) {
	dir := t.TempDir()
	logp := filepath.Join(dir, "src.log")
	st, err := OpenOptions(logp, Options{})
	if err != nil {
		t.Fatal(err)
	}
	put := func(subject, name string) {
		now := time.Now().UnixMicro()
		e := &model.Event{Subject: subject, Facet: "pdt", Type: model.Observed,
			Value: map[string]any{"name": name}, EffectiveTime: now,
			Provenance: model.SystemFeed, Confidence: 1}
		if err := st.Append(now, []*model.Event{e}, nil); err != nil {
			t.Fatal(err)
		}
	}
	put("item:1", "red winter jacket")
	put("item:2", "blue denim jeans")
	put("item:3", "winter gloves")
	st.Close()

	arch := filepath.Join(dir, "arch")
	if _, err := WriteArchive(logp, arch, 2); err != nil {
		t.Fatal(err)
	}

	// "jacket" appears only in item:1.
	li, err := OpenLazyIndex(arch)
	if err != nil {
		t.Fatal(err)
	}
	hits := li.Search("jacket", 10)
	if len(hits) != 1 || hits[0].Event.Subject != "item:1" {
		t.Fatalf("Search(jacket) = %v, want only item:1", hits)
	}
	// "winter" appears in item:1 and item:3.
	winter := li.Search("winter", 10)
	if len(winter) != 2 {
		t.Fatalf("Search(winter) returned %d hits, want 2", len(winter))
	}
	// Standalone disk scan agrees on the top hit.
	scan, err := ScanSearch(arch, "jacket", 10)
	if err != nil {
		t.Fatal(err)
	}
	if len(scan) != 1 || scan[0].Event.Subject != "item:1" {
		t.Fatalf("ScanSearch(jacket) = %v, want only item:1", scan)
	}
}
