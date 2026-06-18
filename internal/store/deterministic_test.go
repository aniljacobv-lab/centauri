package store

import (
	"fmt"
	"path/filepath"
	"testing"

	"github.com/proxima360/centauri/internal/model"
)

func fact(id string, eff, rec int64, price int) *model.Event {
	return &model.Event{
		EventID: id, Subject: "item:9", Facet: "src", Type: model.Observed,
		Value:      map[string]any{"price_cents": price},
		EffectiveTime: eff, RecordedTime: rec,
		Provenance: model.SystemFeed, Confidence: 1,
	}
}

// The current-fact pointer must be a deterministic function of the set of
// facts, NOT of ingest order. Two replicas that receive the same independent
// facts (no supersession between them — the multi-master case) in different
// orders must agree on which is current. This is the property the
// last-write-wins rule (effective→recorded→id) buys us. See design-sync.md §3.
func TestDeterministicCurrentConverges(t *testing.T) {
	eA := fact("id-aaa", 1000, 1000, 100)
	eB := fact("id-bbb", 2000, 2000, 200) // beats eA: higher effective_time

	mk := func(order []*model.Event) *Store {
		st, err := OpenOptions(filepath.Join(t.TempDir(), "s.log"), Options{})
		if err != nil {
			t.Fatalf("open: %v", err)
		}
		if _, err := st.IngestForeign(order); err != nil {
			t.Fatalf("ingest: %v", err)
		}
		return st
	}
	s1 := mk([]*model.Event{eA, eB}) // applied A then B
	defer s1.Close()
	s2 := mk([]*model.Event{eB, eA}) // applied B then A (shuffled)
	defer s2.Close()

	for _, tc := range []struct {
		name string
		st   *Store
	}{{"A,B", s1}, {"B,A", s2}} {
		cur := tc.st.Current("item:9", "")
		if len(cur) != 1 {
			t.Fatalf("%s: current = %d facts, want 1", tc.name, len(cur))
		}
		if cur[0].EventID != "id-bbb" || fmt.Sprint(cur[0].Value["price_cents"]) != "200" {
			t.Fatalf("%s: current = %s/%v, want id-bbb/200 (order-independent)",
				tc.name, cur[0].EventID, cur[0].Value["price_cents"])
		}
	}
}

// Tie-break is the event id (stable, replica-independent): equal effective and
// recorded times resolve to the lexicographically greater id, both orders.
func TestDeterministicCurrentTieBreak(t *testing.T) {
	lo := fact("id-111", 5000, 5000, 1)
	hi := fact("id-999", 5000, 5000, 2) // same times, greater id → wins

	for _, order := range [][]*model.Event{{lo, hi}, {hi, lo}} {
		st, err := OpenOptions(filepath.Join(t.TempDir(), "s.log"), Options{})
		if err != nil {
			t.Fatalf("open: %v", err)
		}
		if _, err := st.IngestForeign(order); err != nil {
			t.Fatalf("ingest: %v", err)
		}
		cur := st.Current("item:9", "")
		if len(cur) != 1 || cur[0].EventID != "id-999" {
			t.Fatalf("order %v: current id = %v, want id-999 (id tiebreak)", order, cur)
		}
		st.Close()
	}
}

// A correction that supersedes the current fact must become current even when
// it carries an EARLIER effective time — because the prior is explicitly
// superseded (ineligible), recomputeOpen restores the next-best, which is the
// correction. (Guards the supersession path of the deterministic rule.)
func TestCorrectionEarlierEffectiveWins(t *testing.T) {
	st, err := OpenOptions(filepath.Join(t.TempDir(), "s.log"), Options{})
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	defer st.Close()

	e1 := fact("", 5000, 0, 100) // effective far in the future
	if err := st.Append(1000, []*model.Event{e1}, nil); err != nil {
		t.Fatalf("append e1: %v", err)
	}
	e2 := fact("", 3000, 0, 999) // earlier effective, but a later correction
	if err := st.Append(2000, []*model.Event{e2}, nil); err != nil {
		t.Fatalf("append e2: %v", err)
	}
	cur := st.Current("item:9", "")
	if len(cur) != 1 || fmt.Sprint(cur[0].Value["price_cents"]) != "999" {
		t.Fatalf("current = %#v, want price 999 (the correction)", cur)
	}
}
