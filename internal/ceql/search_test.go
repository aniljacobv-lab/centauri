package ceql

import (
	"testing"

	"github.com/proxima360/centauri/internal/model"
)

func searchTop(t *testing.T, r map[string]any) *model.Event {
	t.Helper()
	hits, _ := r["hits"].([]map[string]any)
	if len(hits) == 0 {
		t.Fatal("search returned no hits")
	}
	e, _ := hits[0]["event"].(*model.Event)
	if e == nil {
		t.Fatal("top hit has no event")
	}
	return e
}

// With identical text relevance, the fresher fact wins — a signal a plain
// inverted index can't use.
func TestSearchRecencyTieBreak(t *testing.T) {
	st := newStore(t)
	run(t, st, "PUT item:old SET note='late markdown applied'", 1000)
	run(t, st, "PUT item:new SET note='late markdown applied'", 5000)
	top := searchTop(t, run(t, st, "SEARCH 'late markdown'", 9000))
	if top.Subject != "item:new" {
		t.Fatalf("recency tie-break: expected item:new first, got %s", top.Subject)
	}
}

// With identical text and recency, the more trusted fact (higher
// confidence × stronger provenance) wins.
func TestSearchTrustTieBreak(t *testing.T) {
	st := newStore(t)
	run(t, st, "PUT item:lo SET note='penny rounding' CONFIDENCE 0.3 PROVENANCE AI_INFERRED", 1000)
	run(t, st, "PUT item:hi SET note='penny rounding' CONFIDENCE 1.0 PROVENANCE SCAN_VERIFIED", 1000)
	top := searchTop(t, run(t, st, "SEARCH 'penny rounding'", 9000))
	if top.Subject != "item:hi" {
		t.Fatalf("trust tie-break: expected item:hi first, got %s", top.Subject)
	}
}

// Every hit carries its signal breakdown so the ranking is explainable.
func TestSearchSignalBreakdown(t *testing.T) {
	st := newStore(t)
	run(t, st, "PUT item:1 SET note='clearance markdown'", 1000)
	r := run(t, st, "SEARCH 'markdown'", 2000)
	hits := r["hits"].([]map[string]any)
	if len(hits) != 1 {
		t.Fatalf("expected 1 hit, got %d", len(hits))
	}
	for _, k := range []string{"score", "relevance", "recency", "trust", "centrality", "bm25"} {
		if _, ok := hits[0][k]; !ok {
			t.Fatalf("hit missing signal %q", k)
		}
	}
}
