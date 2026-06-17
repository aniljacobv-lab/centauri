package store

import (
	"path/filepath"
	"testing"

	"github.com/proxima360/centauri/internal/model"
)

// The subjectFacets index must give the same facet-less read results as the
// old full-scan, across the live store, a reopen (checkpoint or replay), and
// lazy mode (forced full replay).
func TestSubjectFacetsReadPaths(t *testing.T) {
	p := filepath.Join(t.TempDir(), "c.log")
	s, err := OpenOptions(p, Options{})
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	put := func(subj, facet string, price int, now int64) {
		e := &model.Event{Subject: subj, Facet: facet, Type: model.Observed,
			Value: map[string]any{"price_cents": price}, Provenance: model.SystemFeed, Confidence: 1}
		if err := s.Append(now, []*model.Event{e}, nil); err != nil {
			t.Fatalf("append: %v", err)
		}
	}
	put("item:1", "register", 100, 1000)
	put("item:1", "shelf", 110, 1100)
	put("item:2", "register", 200, 1200)

	if got := s.Current("item:1", ""); len(got) != 2 {
		t.Fatalf("item:1 current facets = %d, want 2", len(got))
	}
	if got := s.History("item:1", ""); len(got) != 2 {
		t.Fatalf("item:1 history = %d, want 2", len(got))
	}
	if got := s.Current("item:1", "shelf"); len(got) != 1 || got[0].Facet != "shelf" {
		t.Fatalf("explicit-facet read wrong: %#v", got)
	}
	if got := s.Current("item:2", ""); len(got) != 1 {
		t.Fatalf("item:2 facets = %d, want 1 (must not see item:1)", len(got))
	}
	if got := s.AsOf("item:1", "", 1100, 1100); len(got) != 2 {
		t.Fatalf("item:1 AsOf facets = %d, want 2", len(got))
	}
	if err := s.Close(); err != nil {
		t.Fatalf("close: %v", err)
	}

	// Reopen — exercises whichever of {checkpoint load, full replay} runs.
	s2, err := OpenOptions(p, Options{})
	if err != nil {
		t.Fatalf("reopen: %v", err)
	}
	if got := s2.Current("item:1", ""); len(got) != 2 {
		t.Fatalf("after reopen item:1 current = %d, want 2", len(got))
	}
	if got := s2.History("item:1", ""); len(got) != 2 {
		t.Fatalf("after reopen item:1 history = %d, want 2", len(got))
	}
	if got := s2.Current("item:2", ""); len(got) != 1 {
		t.Fatalf("after reopen item:2 = %d, want 1", len(got))
	}
	s2.Close()

	// Lazy mode forces a full replay; the index must rebuild via apply().
	s3, err := OpenOptions(p, Options{LazyPayloads: true})
	if err != nil {
		t.Fatalf("reopen lazy: %v", err)
	}
	defer s3.Close()
	if got := s3.Current("item:1", ""); len(got) != 2 {
		t.Fatalf("lazy item:1 current = %d, want 2", len(got))
	}
}
