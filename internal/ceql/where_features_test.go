package ceql

import (
	"testing"

	"github.com/proxima360/centauri/internal/model"
)

// String equality on a value field returns the right rows (index fast path).
func TestWhereIndexEquality(t *testing.T) {
	st := newStore(t)
	run(t, st, "PUT item:1 SET region='EU', price_cents=100", 1000)
	run(t, st, "PUT item:2 SET region='US', price_cents=200", 1100)
	run(t, st, "PUT item:3 SET region='EU', price_cents=300", 1200)

	if evs := events(run(t, st, "FACTS OF * WHERE region='EU'", 2000)); len(evs) != 2 {
		t.Fatalf("region=EU returned %d, want 2", len(evs))
	}
	evs := events(run(t, st, "FACTS OF item:* WHERE region='US'", 2000))
	if len(evs) != 1 || evs[0].Subject != "item:2" {
		t.Fatalf("region=US returned %v, want [item:2]", evs)
	}
	// no match → empty (index returns an empty, usable set)
	if evs := events(run(t, st, "FACTS OF * WHERE region='ZZ'", 2000)); len(evs) != 0 {
		t.Fatalf("region=ZZ returned %d, want 0", len(evs))
	}
}

// EXISTS matches on field presence.
func TestWhereExists(t *testing.T) {
	st := newStore(t)
	run(t, st, "PUT item:1 SET color='red'", 1000)
	run(t, st, "PUT item:2 SET price_cents=200", 1100)
	evs := events(run(t, st, "FACTS OF * WHERE EXISTS color", 2000))
	if len(evs) != 1 || evs[0].Subject != "item:1" {
		t.Fatalf("EXISTS color returned %v, want [item:1]", evs)
	}
}

// Dotted paths reach into nested value objects (scan path).
func TestWhereDotPath(t *testing.T) {
	st := newStore(t)
	add := func(subj, city string, now int64) {
		e := &model.Event{Subject: subj, Facet: "source", Type: model.Observed,
			Value:      map[string]any{"addr": map[string]any{"city": city}},
			Provenance: model.SystemFeed, Confidence: 1}
		if err := st.Append(now, []*model.Event{e}, nil); err != nil {
			t.Fatalf("append: %v", err)
		}
	}
	add("item:1", "EU", 1000)
	add("item:2", "US", 1100)
	evs := events(run(t, st, "FACTS OF * WHERE addr.city='EU'", 2000))
	if len(evs) != 1 || evs[0].Subject != "item:1" {
		t.Fatalf("addr.city=EU returned %v, want [item:1]", evs)
	}
}
