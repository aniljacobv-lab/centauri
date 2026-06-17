package store

import (
	"fmt"
	"path/filepath"
	"testing"

	"github.com/proxima360/centauri/internal/model"
)

// Changes must return committed facts in order, and resume cleanly from a
// saved cursor — the contract a CDC consumer relies on.
func TestChangesResumeFromCursor(t *testing.T) {
	p := filepath.Join(t.TempDir(), "c.log")
	s, err := OpenOptions(p, Options{})
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	defer s.Close()
	put := func(i int, now int64) {
		e := &model.Event{Subject: fmt.Sprintf("item:%d", i), Facet: "f", Type: model.Observed,
			Value: map[string]any{"price_cents": i * 100}, Provenance: model.SystemFeed, Confidence: 1}
		if err := s.Append(now, []*model.Event{e}, nil); err != nil {
			t.Fatalf("append: %v", err)
		}
	}
	put(1, 1000)
	put(2, 2000)
	put(3, 3000)

	evs, cur, err := s.Changes(0)
	if err != nil {
		t.Fatalf("changes: %v", err)
	}
	if len(evs) != 3 {
		t.Fatalf("got %d events, want 3", len(evs))
	}
	if evs[0].Subject != "item:1" || evs[2].Subject != "item:3" {
		t.Fatalf("events out of commit order: %s … %s", evs[0].Subject, evs[2].Subject)
	}
	if cur != s.LogSize() {
		t.Fatalf("cursor %d != log size %d", cur, s.LogSize())
	}
	// Resuming from the cursor yields nothing new.
	evs2, cur2, err := s.Changes(cur)
	if err != nil {
		t.Fatalf("changes resume: %v", err)
	}
	if len(evs2) != 0 || cur2 != cur {
		t.Fatalf("resume returned %d events (cursor %d); want 0 and stable cursor", len(evs2), cur2)
	}
	// A new commit is picked up from the saved cursor — only the new fact.
	put(4, 4000)
	evs3, _, err := s.Changes(cur)
	if err != nil {
		t.Fatalf("changes after append: %v", err)
	}
	if len(evs3) != 1 || evs3[0].Subject != "item:4" {
		t.Fatalf("resume after append got %v, want only item:4", evs3)
	}
	// Full payloads are present (read from the log, not offloaded RAM).
	if got := fmt.Sprint(evs3[0].Value["price_cents"]); got != "400" {
		t.Fatalf("payload missing/wrong: %s", got)
	}
}

// A cursor past the committed size is a clear error (log replaced/truncated).
func TestChangesBeyondEnd(t *testing.T) {
	p := filepath.Join(t.TempDir(), "c.log")
	s, err := OpenOptions(p, Options{})
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	defer s.Close()
	if _, _, err := s.Changes(1 << 20); err == nil {
		t.Fatal("expected an error for an offset beyond the log")
	}
}
