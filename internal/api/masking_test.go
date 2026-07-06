package api

import (
	"io"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"

	"github.com/aniljacobv-lab/centauri/internal/model"
	"github.com/aniljacobv-lab/centauri/internal/store"
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

// maskResult must also cover the nested-event result kinds: context bundles,
// similarity hits, and diff before/after value maps.
func TestMaskResultContextSimilarDiff(t *testing.T) {
	mask := []string{"ssn"}
	e := &model.Event{Subject: "user:1", Value: map[string]any{"name": "Ann", "ssn": "x"}}

	// context: facts/history/pending events + per-field disagreements
	b := &store.ContextBundle{
		Subject: "user:1",
		Facts:   []*model.Event{e},
		History: []*model.Event{e},
		Pending: []*model.Event{e},
		Disagreements: []store.FieldDisagreement{{
			Field:    "ssn",
			Claims:   []store.FieldClaim{{Facet: "a", Value: "x"}},
			Resolved: store.FieldClaim{Facet: "a", Value: "x"},
		}, {
			Field:    "name",
			Claims:   []store.FieldClaim{{Facet: "a", Value: "Ann"}},
			Resolved: store.FieldClaim{Facet: "a", Value: "Ann"},
		}},
	}
	res := map[string]any{"kind": "context", "context": b}
	maskResult(res, mask)
	got := res["context"].(*store.ContextBundle)
	for name, evs := range map[string][]*model.Event{
		"facts": got.Facts, "history": got.History, "pending": got.Pending} {
		if evs[0].Value["ssn"] != "***" {
			t.Fatalf("context %s not masked: %v", name, evs[0].Value["ssn"])
		}
		if evs[0].Value["name"] != "Ann" {
			t.Fatalf("context %s unmasked field changed: %v", name, evs[0].Value["name"])
		}
	}
	if got.Disagreements[0].Claims[0].Value != "***" || got.Disagreements[0].Resolved.Value != "***" {
		t.Fatalf("masked-field disagreement not redacted: %+v", got.Disagreements[0])
	}
	if got.Disagreements[1].Claims[0].Value != "Ann" {
		t.Fatalf("unmasked-field disagreement changed: %+v", got.Disagreements[1])
	}
	// the store-owned originals must be untouched
	if e.Value["ssn"] != "x" || b.Disagreements[0].Claims[0].Value != "x" {
		t.Fatal("original bundle/event mutated by masking")
	}

	// similar hits
	sres := map[string]any{"kind": "similar", "hits": []store.SimilarHit{{Event: e, Score: 0.9}}}
	maskResult(sres, mask)
	hit := sres["hits"].([]store.SimilarHit)[0]
	if hit.Event.Value["ssn"] != "***" || hit.Score != 0.9 {
		t.Fatalf("similar hit not masked correctly: %+v", hit)
	}
	if e.Value["ssn"] != "x" {
		t.Fatal("original event mutated via similar masking")
	}

	// diff before/after maps are the events' own value maps — clone + redact
	before := map[string]any{"ssn": "x", "name": "Ann"}
	after := map[string]any{"ssn": "y"}
	row := map[string]any{"subject": "user:1", "facet": "f", "change": "changed",
		"before": before, "after": after}
	dres := map[string]any{"kind": "diff", "changes": []map[string]any{row}}
	maskResult(dres, mask)
	if row["before"].(map[string]any)["ssn"] != "***" || row["after"].(map[string]any)["ssn"] != "***" {
		t.Fatalf("diff not masked: %v", row)
	}
	if row["before"].(map[string]any)["name"] != "Ann" {
		t.Fatalf("diff unmasked field changed: %v", row)
	}
	if before["ssn"] != "x" || after["ssn"] != "y" {
		t.Fatal("store-owned value maps mutated by diff masking")
	}
}

// End-to-end: a scoped token with mask=["ssn"] hits POST /v1/query (the ONLY
// endpoint scoped tokens may use) and must get redacted results — for plain
// FACTS reads and for nested CONTEXT bundles. The admin token sees raw values.
func TestQueryMaskingEndToEnd(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "main.log")
	st, err := store.OpenOptions(path, store.Options{})
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	srv := NewWithOptions(st, Options{Token: "admin", DataPath: path})
	defer srv.Close()
	ts := httptest.NewServer(srv.Routes())
	defer ts.Close()

	post := func(p, tok, body string) (int, string) {
		t.Helper()
		req, err := http.NewRequest("POST", ts.URL+p, strings.NewReader(body))
		if err != nil {
			t.Fatal(err)
		}
		req.Header.Set("Authorization", "Bearer "+tok)
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		defer resp.Body.Close()
		b, _ := io.ReadAll(resp.Body)
		return resp.StatusCode, string(b)
	}

	e := &model.Event{Subject: "user:1", Facet: "hr", Type: model.Observed,
		Value:         map[string]any{"name": "Ann", "ssn": "123-45-6789"},
		EffectiveTime: 1000, Provenance: model.HumanEntry, Confidence: 1}
	if err := st.Append(1000, []*model.Event{e}, nil); err != nil {
		t.Fatal(err)
	}

	if c, b := post("/v1/acl", "admin", `{"token":"scoped","prefixes":["user:"],"mask":["ssn"]}`); c != 200 {
		t.Fatalf("acl register = %d: %s", c, b)
	}

	for _, q := range []string{"FACTS OF user:*", "CONTEXT FOR user:1"} {
		code, body := post("/v1/query", "scoped", `{"q":"`+q+`"}`)
		if code != 200 {
			t.Fatalf("scoped query %q = %d: %s", q, code, body)
		}
		if strings.Contains(body, "123-45-6789") {
			t.Fatalf("ssn leaked to scoped token on %q:\n%s", q, body)
		}
		if !strings.Contains(body, "***") {
			t.Fatalf("no redaction marker in %q result:\n%s", q, body)
		}
		if !strings.Contains(body, "Ann") {
			t.Fatalf("unmasked field missing from %q result:\n%s", q, body)
		}
	}

	// admin still sees the raw value (and the store wasn't mutated)
	code, body := post("/v1/query", "admin", `{"q":"FACTS OF user:*"}`)
	if code != 200 || !strings.Contains(body, "123-45-6789") {
		t.Fatalf("admin query = %d, want raw ssn:\n%s", code, body)
	}
}
