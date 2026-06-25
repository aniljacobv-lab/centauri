package store

import (
	"path/filepath"
	"testing"

	"github.com/proxima360/centauri/internal/model"
)

func TestLegalHoldBlocksRetire(t *testing.T) {
	st, err := OpenOptions(filepath.Join(t.TempDir(), "l.log"), Options{})
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()

	ev := func(subj, facet string, typ model.EventType, v map[string]any) *model.Event {
		return &model.Event{Subject: subj, Facet: facet, Type: typ, EffectiveTime: 1000,
			Provenance: model.SystemFeed, Confidence: 1, Value: v}
	}
	now := int64(1000)

	// Place a legal hold over user:*.
	if err := st.Append(now, []*model.Event{ev("hold:gdpr", "policy", model.Observed,
		map[string]any{"pattern": "user:*", "active": true})}, nil); err != nil {
		t.Fatal(err)
	}
	// A normal fact on a held subject is allowed (history may still grow).
	if err := st.Append(now, []*model.Event{ev("user:1", "f", model.Observed, map[string]any{"name": "a"})}, nil); err != nil {
		t.Fatalf("normal append under hold should be allowed: %v", err)
	}
	// RETIRE of the held subject is blocked.
	if err := st.Append(now, []*model.Event{ev("user:1", "f", model.Correction, map[string]any{"retired": true})}, nil); err == nil {
		t.Fatal("RETIRE of a held subject should be blocked")
	}

	// A subject NOT under hold can be retired freely.
	if err := st.Append(now, []*model.Event{ev("item:1", "f", model.Observed, map[string]any{"n": 1})}, nil); err != nil {
		t.Fatal(err)
	}
	if err := st.Append(now, []*model.Event{ev("item:1", "f", model.Correction, map[string]any{"retired": true})}, nil); err != nil {
		t.Fatalf("RETIRE of an unheld subject should be allowed: %v", err)
	}

	// Lift the hold (active=false), then the RETIRE is allowed.
	if err := st.Append(now, []*model.Event{ev("hold:gdpr", "policy", model.Correction,
		map[string]any{"pattern": "user:*", "active": false})}, nil); err != nil {
		t.Fatal(err)
	}
	if err := st.Append(now, []*model.Event{ev("user:1", "f", model.Correction, map[string]any{"retired": true})}, nil); err != nil {
		t.Fatalf("after lifting the hold, RETIRE should be allowed: %v", err)
	}
}
