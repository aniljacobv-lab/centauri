package store

import (
	"fmt"
	"path/filepath"
	"testing"

	"github.com/proxima360/centauri/internal/model"
)

// IngestForeign must store a peer's facts (preserving id + recorded_time),
// dedup on re-ingest (echo protection), and keep current/history correct.
func TestIngestForeign(t *testing.T) {
	a, err := OpenOptions(filepath.Join(t.TempDir(), "a.log"), Options{})
	if err != nil {
		t.Fatalf("open a: %v", err)
	}
	defer a.Close()
	put := func(price int, now int64) {
		e := &model.Event{Subject: "item:1", Facet: "source", Type: model.Observed,
			Value: map[string]any{"price_cents": price}, Provenance: model.SystemFeed, Confidence: 1}
		if err := a.Append(now, []*model.Event{e}, nil); err != nil {
			t.Fatalf("append: %v", err)
		}
	}
	put(100, 1000)
	put(200, 2000) // supersedes within a's log

	facts := a.History("item:1", "") // both events, oldest→newest
	if len(facts) != 2 {
		t.Fatalf("a history = %d, want 2", len(facts))
	}

	b, err := OpenOptions(filepath.Join(t.TempDir(), "b.log"), Options{})
	if err != nil {
		t.Fatalf("open b: %v", err)
	}
	defer b.Close()

	n, err := b.IngestForeign(facts)
	if err != nil || n != 2 {
		t.Fatalf("ingest = %d, %v; want 2", n, err)
	}
	// current converges to the latest fact, recorded_time preserved (not re-stamped).
	cur := b.Current("item:1", "")
	if len(cur) != 1 || fmt.Sprint(cur[0].Value["price_cents"]) != "200" {
		t.Fatalf("b current = %#v, want price 200", cur)
	}
	if cur[0].RecordedTime != 2000 {
		t.Fatalf("recorded_time = %d, want 2000 (preserved, not re-stamped)", cur[0].RecordedTime)
	}
	if h := b.History("item:1", ""); len(h) != 2 {
		t.Fatalf("b history = %d, want 2", len(h))
	}
	// bi-temporal read still works off the preserved timestamps.
	if got := b.AsOf("item:1", "", 1500, 1500); len(got) != 1 || fmt.Sprint(got[0].Value["price_cents"]) != "100" {
		t.Fatalf("b AsOf 1500 = %#v, want price 100", got)
	}

	// re-ingest the same facts: all deduped, nothing new (echo protection).
	if n2, _ := b.IngestForeign(facts); n2 != 0 {
		t.Fatalf("re-ingest stored %d, want 0 (dedup)", n2)
	}
}
