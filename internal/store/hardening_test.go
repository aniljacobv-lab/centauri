package store

// Regression tests for the 2026-07 store hardening pass:
//   - a parseable final record without its trailing '\n' is a torn write and
//     must be truncated on open (B1);
//   - legal holds survive a LazyPayloads reopen (B3);
//   - public query paths return stable snapshots, not shared pointers (B7);
//   - IngestRaw never double-applies a redelivered chunk (S6);
//   - a failed batch leaves no trace on the caller's events (S7).

import (
	"bytes"
	"encoding/json"
	"os"
	"path/filepath"
	"testing"

	"github.com/aniljacobv-lab/centauri/internal/model"
)

// TestReplayDiscardsUnterminatedTail exercises the torn-write case where the
// crash persisted a fully parseable JSON record but not its trailing newline.
// The record must be discarded (truncated), never applied — otherwise the next
// Append would produce `{...}{...}\n` corruption.
func TestReplayDiscardsUnterminatedTail(t *testing.T) {
	path := filepath.Join(t.TempDir(), "c.log")
	s := openT(t, path)
	if err := s.Append(t1, []*model.Event{ev("item:1", "source", model.Observed, 100, t1)}, nil); err != nil {
		t.Fatal(err)
	}
	if err := s.Close(); err != nil {
		t.Fatal(err)
	}
	fi, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	goodSize := fi.Size()

	// Simulate the torn write: a valid record, no trailing '\n'.
	torn, err := json.Marshal(&record{Event: &model.Event{
		EventID: "torn-tail-ev", Subject: "item:1", Facet: "source", Type: model.Observed,
		Value: map[string]any{"price_cents": 999}, EffectiveTime: t2, RecordedTime: t2,
		Provenance: model.SystemFeed, Confidence: 1,
	}})
	if err != nil {
		t.Fatal(err)
	}
	if torn[len(torn)-1] == '\n' {
		t.Fatal("test setup: marshaled record must not end in newline")
	}
	f, err := os.OpenFile(path, os.O_WRONLY|os.O_APPEND, 0o644)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := f.Write(torn); err != nil {
		t.Fatal(err)
	}
	if err := f.Close(); err != nil {
		t.Fatal(err)
	}
	// The checkpoint written by Close would let reopen skip replay of the torn
	// region only if it covered it — it doesn't (it covers goodSize), so replay
	// starts before the torn bytes either way. Remove it to force full replay
	// and keep the test independent of checkpoint behavior.
	_ = os.Remove(path + ".checkpoint")

	s2 := openT(t, path)
	if _, ok := s2.events["torn-tail-ev"]; ok {
		t.Fatal("torn (unterminated) record must not be applied on replay")
	}
	fi, err = os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	if fi.Size() != goodSize {
		t.Fatalf("torn tail must be truncated: size = %d, want %d", fi.Size(), goodSize)
	}
	// A fresh append after recovery must leave a clean, verifiable log.
	if err := s2.Append(t3, []*model.Event{ev("item:1", "source", model.Observed, 200, t3)}, nil); err != nil {
		t.Fatal(err)
	}
	res, err := s2.Integrity()
	if err != nil {
		t.Fatal(err)
	}
	if res["verified"] != true {
		t.Fatalf("chain must verify after torn-tail recovery: %+v", res)
	}
	if err := s2.Close(); err != nil {
		t.Fatal(err)
	}
	s3 := openT(t, path)
	defer s3.Close()
	cur := s3.Current("item:1", "source")
	if len(cur) != 1 || cur[0].Value["price_cents"] != float64(200) {
		t.Fatalf("post-recovery reopen: current = %+v", cur)
	}
}

// TestLegalHoldSurvivesLazyReopen: with LazyPayloads, hold facts have their
// Value offloaded after a reopen; heldLocked must hydrate them or RETIRE
// slips through an active hold.
func TestLegalHoldSurvivesLazyReopen(t *testing.T) {
	path := filepath.Join(t.TempDir(), "l.log")
	s, err := OpenOptions(path, Options{LazyPayloads: true})
	if err != nil {
		t.Fatal(err)
	}
	hold := &model.Event{Subject: "hold:gdpr", Facet: "policy", Type: model.Observed,
		Value:      map[string]any{"pattern": "user:*", "active": true},
		Provenance: model.SystemFeed, Confidence: 1}
	fact := &model.Event{Subject: "user:1", Facet: "f", Type: model.Observed,
		Value:      map[string]any{"name": "a"},
		Provenance: model.SystemFeed, Confidence: 1}
	if err := s.Append(t1, []*model.Event{hold, fact}, nil); err != nil {
		t.Fatal(err)
	}
	if err := s.Close(); err != nil {
		t.Fatal(err)
	}

	s2, err := OpenOptions(path, Options{LazyPayloads: true})
	if err != nil {
		t.Fatal(err)
	}
	defer s2.Close()
	retire := &model.Event{Subject: "user:1", Facet: "f", Type: model.Correction,
		Value:      map[string]any{"retired": true},
		Provenance: model.SystemFeed, Confidence: 1}
	if err := s2.Append(t2, []*model.Event{retire}, nil); err == nil {
		t.Fatal("RETIRE must stay blocked by an active hold after a lazy reopen")
	}
}

// TestQueriesReturnStableSnapshots: events returned by public query paths must
// be copies — a later supersession mutates the stored event, never one already
// handed to a caller.
func TestQueriesReturnStableSnapshots(t *testing.T) {
	path := filepath.Join(t.TempDir(), "q.log")
	s := openT(t, path)
	defer s.Close()

	a := ev("item:1", "source", model.Observed, 100, t1)
	if err := s.Append(t1, []*model.Event{a}, nil); err != nil {
		t.Fatal(err)
	}
	curBefore := s.Current("item:1", "source")
	histBefore := s.History("item:1", "source")
	asOfBefore := s.AsOf("item:1", "source", t1, 0)
	if len(curBefore) != 1 || len(histBefore) != 1 || len(asOfBefore) != 1 {
		t.Fatalf("setup: expected one event from each query")
	}

	b := ev("item:1", "source", model.Observed, 200, t2)
	if err := s.Append(t2, []*model.Event{b}, nil); err != nil {
		t.Fatal(err)
	}
	// The stored event was superseded...
	if a.SupersededBy != b.EventID {
		t.Fatalf("stored event must be superseded, got %q", a.SupersededBy)
	}
	// ...but the snapshots handed out before the supersession must not move.
	for name, got := range map[string]*model.Event{
		"Current": curBefore[0], "History": histBefore[0], "AsOf": asOfBefore[0],
	} {
		if got.SupersededBy != "" || got.EffectiveEnd != 0 {
			t.Fatalf("%s snapshot mutated by later supersession: %+v", name, got)
		}
	}
}

// TestIngestRawDuplicateChunk: redelivering the same shipped chunk must not
// double-apply events, supersessions, or links, and the follower's chain must
// still verify (skipped lines are not written).
func TestIngestRawDuplicateChunk(t *testing.T) {
	pdir := t.TempDir()
	primary := openT(t, filepath.Join(pdir, "p.log"))
	defer primary.Close()
	if err := primary.Append(t1, []*model.Event{ev("item:1", "source", model.Observed, 100, t1)}, nil); err != nil {
		t.Fatal(err)
	}
	if err := primary.Append(t2, []*model.Event{ev("item:1", "source", model.Observed, 200, t2)}, nil); err != nil {
		t.Fatal(err)
	}
	chunk, err := primary.ReadLog(0)
	if err != nil {
		t.Fatal(err)
	}

	follower := openT(t, filepath.Join(pdir, "f.log"))
	defer follower.Close()
	if err := follower.IngestRaw(chunk); err != nil {
		t.Fatal(err)
	}
	sizeAfterFirst := follower.LogSize()
	// Redeliver the identical chunk (retry after a lost ack).
	if err := follower.IngestRaw(chunk); err != nil {
		t.Fatal(err)
	}
	if got := follower.LogSize(); got != sizeAfterFirst {
		t.Fatalf("duplicate chunk must write nothing: size %d, want %d", got, sizeAfterFirst)
	}
	if h := follower.History("item:1", "source"); len(h) != 2 {
		t.Fatalf("history must not duplicate: got %d events, want 2", len(h))
	}
	if links := follower.CausalEdges(); len(links) != 1 {
		t.Fatalf("links must not duplicate: got %d, want 1", len(links))
	}
	res, err := follower.Integrity()
	if err != nil {
		t.Fatal(err)
	}
	if res["verified"] != true {
		t.Fatalf("follower chain must verify after redelivery: %+v", res)
	}
	// Byte-identical to the primary: same bytes, same chain head.
	ph, psize := primary.ChainHead()
	fh, fsize := follower.ChainHead()
	if ph != fh || psize != fsize {
		t.Fatalf("follower diverged from primary: %s@%d vs %s@%d", fh, fsize, ph, psize)
	}
}

// TestFailedBatchLeavesNoTrace: when one event in a batch fails validation,
// the earlier events must keep their pre-call server fields and remain
// appendable afterwards (S7).
func TestFailedBatchLeavesNoTrace(t *testing.T) {
	path := filepath.Join(t.TempDir(), "b.log")
	s := openT(t, path)
	defer s.Close()

	good := ev("item:1", "source", model.Observed, 100, t1)
	good.EventID = "explicit-id-1"
	bad := ev("item:2", "source", "BOGUS", 200, t1)
	if err := s.Append(t1, []*model.Event{good, bad}, nil); err == nil {
		t.Fatal("batch with an invalid event must fail")
	}
	if good.RecordedTime != 0 {
		t.Fatalf("failed batch must not assign server fields: RecordedTime = %d", good.RecordedTime)
	}
	// The id was never claimed: appending it alone must succeed.
	if err := s.Append(t1, []*model.Event{good}, nil); err != nil {
		t.Fatalf("re-append after failed batch: %v", err)
	}
}

// TestIngestRawPartialOverlap: a chunk overlapping already-ingested records
// applies only the new suffix and stays byte-identical to the primary.
func TestIngestRawPartialOverlap(t *testing.T) {
	dir := t.TempDir()
	primary := openT(t, filepath.Join(dir, "p.log"))
	defer primary.Close()
	if err := primary.Append(t1, []*model.Event{ev("item:1", "source", model.Observed, 100, t1)}, nil); err != nil {
		t.Fatal(err)
	}
	first, err := primary.ReadLog(0)
	if err != nil {
		t.Fatal(err)
	}
	if err := primary.Append(t2, []*model.Event{ev("item:2", "source", model.Observed, 200, t2)}, nil); err != nil {
		t.Fatal(err)
	}
	full, err := primary.ReadLog(0)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.HasPrefix(full, first) {
		t.Fatal("setup: full log must start with the first chunk")
	}

	follower := openT(t, filepath.Join(dir, "f.log"))
	defer follower.Close()
	if err := follower.IngestRaw(first); err != nil {
		t.Fatal(err)
	}
	if err := follower.IngestRaw(full); err != nil { // overlaps the first chunk
		t.Fatal(err)
	}
	ph, psize := primary.ChainHead()
	fh, fsize := follower.ChainHead()
	if ph != fh || psize != fsize {
		t.Fatalf("follower diverged after overlapping ingest: %s@%d vs %s@%d", fh, fsize, ph, psize)
	}
}
