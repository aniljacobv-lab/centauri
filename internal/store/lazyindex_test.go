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

// Causal trace over the archive must follow Link records in both directions.
func TestScanTrace(t *testing.T) {
	dir := t.TempDir()
	logp := filepath.Join(dir, "src.log")
	st, err := OpenOptions(logp, Options{})
	if err != nil {
		t.Fatal(err)
	}
	a := &model.Event{EventID: "evA", Subject: "order:1", Facet: "f", Type: model.Observed,
		Value: map[string]any{"x": 1}, EffectiveTime: 1000, Provenance: model.SystemFeed, Confidence: 1}
	if err := st.Append(1000, []*model.Event{a}, nil); err != nil {
		t.Fatal(err)
	}
	b := &model.Event{EventID: "evB", Subject: "shipment:1", Facet: "f", Type: model.Observed,
		Value: map[string]any{"x": 2}, EffectiveTime: 2000, Provenance: model.SystemFeed, Confidence: 1}
	// evA caused evB.
	if err := st.Append(2000, []*model.Event{b},
		[]model.CausalLink{{From: "evA", To: "evB", Type: model.Triggered}}); err != nil {
		t.Fatal(err)
	}
	st.Close()

	arch := filepath.Join(dir, "arch")
	if _, err := WriteArchive(logp, arch, 2); err != nil {
		t.Fatal(err)
	}

	causes, err := ScanTrace(arch, "evB", "cause", 16)
	if err != nil {
		t.Fatal(err)
	}
	if len(causes) != 1 || causes[0].Event.EventID != "evA" {
		t.Fatalf("cause trace of evB = %v, want [evA]", causes)
	}
	effects, err := ScanTrace(arch, "evA", "effect", 16)
	if err != nil {
		t.Fatal(err)
	}
	if len(effects) != 1 || effects[0].Event.EventID != "evB" {
		t.Fatalf("effect trace of evA = %v, want [evB]", effects)
	}
}

// The cached reader must serve repeat queries from RAM (hits grow), the segment
// inspector must list the tablespace, and integrity verification must pass.
func TestLazyCacheAndInspect(t *testing.T) {
	dir := t.TempDir()
	logp := filepath.Join(dir, "src.log")
	st, err := OpenOptions(logp, Options{})
	if err != nil {
		t.Fatal(err)
	}
	for s := 0; s < 3; s++ {
		for v := 0; v < 3; v++ {
			now := int64(1000 + s*100 + v)
			e := &model.Event{Subject: fmt.Sprintf("item:%d", s), Facet: "f", Type: model.Observed,
				Value: map[string]any{"v": v}, EffectiveTime: now, Provenance: model.SystemFeed, Confidence: 1}
			if err := st.Append(now, []*model.Event{e}, nil); err != nil {
				t.Fatal(err)
			}
		}
	}
	st.Close()

	arch := filepath.Join(dir, "arch")
	if _, err := WriteArchive(logp, arch, 3); err != nil {
		t.Fatal(err)
	}

	li, err := OpenLazyIndex(arch)
	if err != nil {
		t.Fatal(err)
	}

	// Inspector: the archive has at least one segment.
	man, err := li.Manifest()
	if err != nil {
		t.Fatal(err)
	}
	if len(man.Segments) == 0 {
		t.Fatal("manifest has no segments")
	}

	// Repeat queries should hit the cache.
	before := li.CacheStats()
	for i := 0; i < 3; i++ {
		if _, err := li.History("item:0", "f"); err != nil {
			t.Fatal(err)
		}
	}
	after := li.CacheStats()
	if after.Hits <= before.Hits {
		t.Fatalf("expected segment-cache hits to grow (before=%d after=%d)", before.Hits, after.Hits)
	}

	// Integrity verification passes on a faithful archive.
	if _, _, err := li.Verify(); err != nil {
		t.Fatalf("verify: %v", err)
	}
}

// The secondary field index must answer equality over current facts from the
// index (sub-linear), and fall back to a scan for unknown fields.
func TestLazyFieldIndex(t *testing.T) {
	dir := t.TempDir()
	logp := filepath.Join(dir, "src.log")
	st, err := OpenOptions(logp, Options{})
	if err != nil {
		t.Fatal(err)
	}
	put := func(subject, status string) {
		now := time.Now().UnixMicro()
		e := &model.Event{Subject: subject, Facet: "pdt", Type: model.Observed,
			Value: map[string]any{"status": status}, EffectiveTime: now,
			Provenance: model.SystemFeed, Confidence: 1}
		if err := st.Append(now, []*model.Event{e}, nil); err != nil {
			t.Fatal(err)
		}
	}
	put("item:1", "active")
	put("item:2", "active")
	put("item:3", "retired")
	st.Close()

	arch := filepath.Join(dir, "arch")
	if _, err := WriteArchive(logp, arch, 2); err != nil {
		t.Fatal(err)
	}
	li, err := OpenLazyIndex(arch)
	if err != nil {
		t.Fatal(err)
	}

	got, indexed := li.Lookup("status", "active")
	if !indexed {
		t.Fatal("status should be served by the secondary index")
	}
	if len(got) != 2 {
		t.Fatalf("status=active returned %d, want 2", len(got))
	}
	// Unknown field: not indexed, falls back to scan (no matches).
	if ev, idx := li.Lookup("nosuchfield", "x"); idx || len(ev) != 0 {
		t.Fatalf("unknown field: indexed=%v count=%d, want false/0", idx, len(ev))
	}
}

// End-to-end: seed (with a correction, an indexed field, text, a causal link) ->
// archive -> retrieve every way -> insert (new fact + correction) -> re-archive
// -> see the updated state. This is the deterministic proof behind the
// `centauri tablespace-demo` command.
func TestTablespaceLifecycle(t *testing.T) {
	dir := t.TempDir()
	logp := filepath.Join(dir, "src.log")
	st, err := OpenOptions(logp, Options{NoSync: true})
	if err != nil {
		t.Fatal(err)
	}
	mk := func(id, subj, status, name string, price int, typ model.EventType, eff int64) *model.Event {
		return &model.Event{EventID: id, Subject: subj, Facet: "f", Type: typ, EffectiveTime: eff,
			Provenance: model.SystemFeed, Confidence: 1,
			Value: map[string]any{"status": status, "name": name, "price_cents": price}}
	}
	must := func(now int64, e *model.Event, links []model.CausalLink) {
		if err := st.Append(now, []*model.Event{e}, links); err != nil {
			t.Fatal(err)
		}
	}
	must(1000, mk("e1", "item:1", "active", "winter jacket", 100, model.Observed, 1000), nil)
	must(2000, mk("e2", "item:1", "active", "winter jacket", 90, model.Correction, 2000), nil) // supersedes e1
	must(1000, mk("e3", "item:2", "retired", "summer hat", 50, model.Observed, 1000), nil)
	must(1500, &model.Event{EventID: "ord", Subject: "order:1", Facet: "f", Type: model.Observed,
		EffectiveTime: 1500, Provenance: model.SystemFeed, Confidence: 1, Value: map[string]any{"status": "placed"}}, nil)
	must(1600, &model.Event{EventID: "shp", Subject: "shipment:1", Facet: "f", Type: model.Observed,
		EffectiveTime: 1600, Provenance: model.SystemFeed, Confidence: 1, Value: map[string]any{"status": "shipped"}},
		[]model.CausalLink{{From: "ord", To: "shp", Type: model.Triggered}})
	st.Close()

	arch := filepath.Join(dir, "arch")
	if _, err := WriteArchive(logp, arch, 2); err != nil {
		t.Fatal(err)
	}
	li, err := OpenLazyIndex(arch)
	if err != nil {
		t.Fatal(err)
	}

	if c := li.Current("item:1", "f"); len(c) != 1 || fmt.Sprint(c[0].Value["price_cents"]) != "90" {
		t.Fatalf("current item:1 = %v, want corrected 90", c)
	}
	if act, indexed := li.Lookup("status", "active"); !indexed || len(act) != 1 || act[0].Subject != "item:1" {
		t.Fatalf("lookup status=active = %v (indexed=%v), want item:1", act, indexed)
	}
	if h, _ := li.History("item:1", "f"); len(h) != 2 {
		t.Fatalf("history item:1 = %d, want 2", len(h))
	}
	if a, _ := li.AsOf("item:1", "f", 1500, 0); len(a) != 1 || fmt.Sprint(a[0].Value["price_cents"]) != "100" {
		t.Fatalf("asof item:1 @1500 = %v, want pre-correction 100", a)
	}
	if hits := li.Search("jacket", 5); len(hits) != 1 || hits[0].Event.Subject != "item:1" {
		t.Fatalf("search 'jacket' = %v, want item:1", hits)
	}
	if tr, _ := li.Trace("shp", "cause", 8); len(tr) != 1 || tr[0].Event.EventID != "ord" {
		t.Fatalf("trace cause of shp = %v, want [ord]", tr)
	}
	if _, _, err := li.Verify(); err != nil {
		t.Fatalf("verify: %v", err)
	}

	// INSERT: a brand-new fact + a correction, then re-archive and reopen.
	st2, err := OpenOptions(logp, Options{NoSync: true})
	if err != nil {
		t.Fatal(err)
	}
	if err := st2.Append(3000, []*model.Event{mk("e4", "item:9", "active", "new boots", 200, model.Observed, 3000)}, nil); err != nil {
		t.Fatal(err)
	}
	if err := st2.Append(3000, []*model.Event{mk("e5", "item:1", "clearance", "winter jacket", 70, model.Correction, 3000)}, nil); err != nil {
		t.Fatal(err)
	}
	st2.Close()
	if _, err := WriteArchive(logp, arch, 2); err != nil {
		t.Fatal(err)
	}
	li2, err := OpenLazyIndex(arch)
	if err != nil {
		t.Fatal(err)
	}
	if c := li2.Current("item:9", "f"); len(c) != 1 {
		t.Fatalf("inserted item:9 not retrievable after re-archive")
	}
	if c := li2.Current("item:1", "f"); len(c) != 1 || fmt.Sprint(c[0].Value["price_cents"]) != "70" {
		t.Fatalf("item:1 after correction = %v, want 70", c)
	}
	if h, _ := li2.History("item:1", "f"); len(h) != 3 {
		t.Fatalf("history item:1 after insert = %d, want 3", len(h))
	}
}
