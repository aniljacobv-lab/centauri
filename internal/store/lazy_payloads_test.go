package store

import (
	"fmt"
	"path/filepath"
	"testing"

	"github.com/proxima360/centauri/internal/model"
)

// With LazyPayloads on, payloads must leave RAM after replay yet still read
// back correctly (hydrated from disk on demand).
func TestLazyPayloadsRoundTrip(t *testing.T) {
	p := filepath.Join(t.TempDir(), "c.log")

	// Session 1: write events with payloads.
	s, err := OpenOptions(p, Options{LazyPayloads: true})
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	for i := 0; i < 3; i++ {
		e := &model.Event{
			Subject: fmt.Sprintf("item:%d", i), Facet: "f", Type: model.Observed,
			Value:      map[string]any{"price_cents": i * 100, "note": fmt.Sprintf("n%d", i)},
			Provenance: model.SystemFeed, Confidence: 1.0, SourceSystem: "TEST",
		}
		if err := s.Append(int64(1000+i), []*model.Event{e}, nil); err != nil {
			t.Fatalf("append: %v", err)
		}
	}
	if err := s.Close(); err != nil {
		t.Fatalf("close: %v", err)
	}

	// Session 2: reopen lazily → replay offloads payloads to disk.
	s2, err := OpenOptions(p, Options{LazyPayloads: true})
	if err != nil {
		t.Fatalf("reopen: %v", err)
	}
	defer s2.Close()

	// The in-RAM stub for a known subject must have its payload offloaded.
	s2.mu.RLock()
	id := s2.open[key("item:1", "f")]
	stubValNil := id != "" && s2.events[id] != nil && s2.events[id].Value == nil
	_, hasOffset := s2.offsets[id]
	s2.mu.RUnlock()
	if id == "" {
		t.Fatal("expected an open fact for item:1")
	}
	if !stubValNil {
		t.Fatal("payload should be offloaded (Value nil) in RAM after lazy replay")
	}
	if !hasOffset {
		t.Fatal("a disk offset should be recorded for the offloaded payload")
	}

	// Reading it back must hydrate the payload from disk.
	got := s2.Current("item:1", "")
	if len(got) != 1 {
		t.Fatalf("expected 1 current fact for item:1, got %d", len(got))
	}
	if got[0].Value == nil || fmt.Sprint(got[0].Value["price_cents"]) != "100" || got[0].Value["note"] != "n1" {
		t.Fatalf("lazy read returned wrong payload: %#v", got[0].Value)
	}

	// History hydrates too.
	h := s2.History("item:2", "")
	if len(h) != 1 || h[0].Value == nil || fmt.Sprint(h[0].Value["price_cents"]) != "200" {
		t.Fatalf("history hydrate failed: %#v", h)
	}
}
