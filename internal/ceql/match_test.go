package ceql

import (
	"testing"

	"github.com/proxima360/centauri/internal/model"
	"github.com/proxima360/centauri/internal/store"
)

// link item:1 --TRIGGERED--> register:1 and return the store.
func causalPair(t *testing.T) *store.Store {
	t.Helper()
	st := newStore(t)
	a := &model.Event{Subject: "item:1", Facet: "f", Type: model.Observed,
		Value: map[string]any{"price_cents": 100}, Provenance: model.SystemFeed,
		Confidence: 1, EventID: model.NewID()}
	b := &model.Event{Subject: "register:1", Facet: "f", Type: model.Observed,
		Value: map[string]any{"price_cents": 100}, Provenance: model.SystemFeed,
		Confidence: 1, EventID: model.NewID()}
	link := model.CausalLink{From: a.EventID, To: b.EventID, Type: model.Triggered}
	if err := st.Append(1000, []*model.Event{a, b}, []model.CausalLink{link}); err != nil {
		t.Fatalf("append: %v", err)
	}
	return st
}

func rowsOf(t *testing.T, res map[string]any) []map[string]any {
	t.Helper()
	rows, ok := res["rows"].([]map[string]any)
	if !ok {
		t.Fatalf("result has no rows: %v", res)
	}
	return rows
}

func TestMatchCauses(t *testing.T) {
	st := causalPair(t)
	rows := rowsOf(t, run(t, st, "MATCH item:* CAUSES register:*", 9999))
	if len(rows) != 1 {
		t.Fatalf("got %d rows, want 1", len(rows))
	}
	r := rows[0]
	if r["from"] != "item:1" || r["to"] != "register:1" || r["via"] != "TRIGGERED" || r["hops"] != 1 {
		t.Fatalf("unexpected row: %#v", r)
	}
}

func TestMatchCausedBy(t *testing.T) {
	st := causalPair(t)
	rows := rowsOf(t, run(t, st, "MATCH register:* CAUSED BY item:*", 9999))
	if len(rows) != 1 || rows[0]["from"] != "register:1" || rows[0]["to"] != "item:1" {
		t.Fatalf("caused-by wrong: %#v", rows)
	}
}

func TestMatchViaFilter(t *testing.T) {
	st := causalPair(t)
	if rows := rowsOf(t, run(t, st, "MATCH item:* CAUSES register:* VIA TRIGGERED", 9999)); len(rows) != 1 {
		t.Fatalf("VIA TRIGGERED should match, got %d", len(rows))
	}
	if rows := rowsOf(t, run(t, st, "MATCH item:* CAUSES register:* VIA CORRECTS", 9999)); len(rows) != 0 {
		t.Fatalf("VIA CORRECTS should not match, got %d", len(rows))
	}
}

func TestMatchParse(t *testing.T) {
	ok := []string{
		"MATCH item:* CAUSES register:*",
		"MATCH item:1 CAUSED BY *",
		"MATCH item:* CAUSES register:* VIA TRIGGERED DEPTH 4 LIMIT 10",
	}
	for _, c := range ok {
		if _, err := Parse(c, 0); err != nil {
			t.Errorf("parse %q: %v", c, err)
		}
	}
	for _, c := range []string{"MATCH item:*", "MATCH item:* register:*"} {
		if _, err := Parse(c, 0); err == nil {
			t.Errorf("expected parse error for %q", c)
		}
	}
}
