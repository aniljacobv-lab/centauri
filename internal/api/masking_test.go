package api

import (
	"testing"

	"github.com/proxima360/centauri/internal/model"
)

func TestMaskEventClonesAndRedacts(t *testing.T) {
	e := &model.Event{Subject: "user:1", Facet: "f",
		Value: map[string]any{"name": "Ann", "ssn": "123-45-6789"}}
	cp := maskEvent(e, map[string]bool{"ssn": true})

	if cp.Value["ssn"] != "***" {
		t.Fatalf("ssn not redacted in copy: %v", cp.Value["ssn"])
	}
	if cp.Value["name"] != "Ann" {
		t.Fatalf("unmasked field changed: %v", cp.Value["name"])
	}
	// The store's original event must be untouched (events are shared pointers).
	if e.Value["ssn"] != "123-45-6789" {
		t.Fatalf("original event was mutated! %v", e.Value["ssn"])
	}
}

func TestMaskResultEventsAndRows(t *testing.T) {
	// events result
	e := &model.Event{Subject: "user:1", Value: map[string]any{"name": "Ann", "ssn": "x"}}
	res := map[string]any{"kind": "events", "events": []*model.Event{e}}
	maskResult(res, []string{"ssn"})
	out := res["events"].([]*model.Event)
	if out[0].Value["ssn"] != "***" {
		t.Fatalf("events result not masked: %v", out[0].Value["ssn"])
	}
	if e.Value["ssn"] != "x" {
		t.Fatal("original event mutated via result masking")
	}

	// rows result (query-local, edited in place)
	rows := map[string]any{"kind": "rows",
		"columns": []string{"name", "ssn"}, "rows": [][]any{{"Ann", "x"}}}
	maskResult(rows, []string{"ssn"})
	r := rows["rows"].([][]any)
	if r[0][1] != "***" || r[0][0] != "Ann" {
		t.Fatalf("rows masking wrong: %v", r[0])
	}

	// empty mask is a no-op
	res2 := map[string]any{"kind": "events", "events": []*model.Event{e}}
	maskResult(res2, nil)
	if res2["events"].([]*model.Event)[0].Value["ssn"] != "x" {
		t.Fatal("empty mask should not change anything")
	}
}
