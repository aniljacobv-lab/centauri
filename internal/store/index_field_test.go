package store

import (
	"path/filepath"
	"testing"

	"github.com/proxima360/centauri/internal/model"
)

// The secondary string index must return only CURRENT matches, drop
// superseded ones, ignore unknown fields, and rebuild on reopen.
func TestFieldIndex(t *testing.T) {
	p := filepath.Join(t.TempDir(), "c.log")
	s, err := OpenOptions(p, Options{})
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	put := func(subj, region string, now int64) {
		e := &model.Event{Subject: subj, Facet: "f", Type: model.Observed,
			Value: map[string]any{"region": region}, Provenance: model.SystemFeed, Confidence: 1}
		if err := s.Append(now, []*model.Event{e}, nil); err != nil {
			t.Fatalf("append: %v", err)
		}
	}
	put("item:1", "EU", 1000)
	put("item:2", "US", 1100)
	put("item:3", "EU", 1200)

	eu, ok := s.CurrentByField("region", "EU")
	if !ok || len(eu) != 2 {
		t.Fatalf("EU = %d (ok=%v), want 2", len(eu), ok)
	}
	if _, ok := s.CurrentByField("nope", "x"); ok {
		t.Fatal("unknown field must be unusable (force a scan)")
	}

	// Supersede item:1 EU→US; the index must drop the stale EU entry.
	put("item:1", "US", 1300)
	if eu2, _ := s.CurrentByField("region", "EU"); len(eu2) != 1 || eu2[0].Subject != "item:3" {
		t.Fatalf("after supersede EU = %v, want [item:3]", eu2)
	}
	if us, _ := s.CurrentByField("region", "US"); len(us) != 2 {
		t.Fatalf("US = %d, want 2", len(us))
	}
	s.Close()

	// Reopen: the index rebuilds (checkpoint or replay).
	s2, err := OpenOptions(p, Options{})
	if err != nil {
		t.Fatalf("reopen: %v", err)
	}
	defer s2.Close()
	if eu3, ok := s2.CurrentByField("region", "EU"); !ok || len(eu3) != 1 {
		t.Fatalf("after reopen EU = %d (ok=%v), want 1", len(eu3), ok)
	}
}
