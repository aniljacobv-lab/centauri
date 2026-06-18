package ceql

import "testing"

// ENRICH runs the model once, stores the result as an enrichment, and on a
// second run skips it (cached) — the whole point of persisting inference.
func TestEnrichEmbeddingCaches(t *testing.T) {
	st := newStore(t)
	run(t, st, "PUT model:emb FACET config SET endpoint='http://x', kind='embedding'", 1000)
	run(t, st, "PUT item:1 SET note='late markdown'", 1100)

	old := Infer
	defer func() { Infer = old }()
	calls := 0
	Infer = func(r InferRequest) (InferResult, error) {
		calls++
		return InferResult{Vector: []float32{0.1, 0.2, 0.3}}, nil
	}

	res := run(t, st, "ENRICH item:* USING emb", 1200)
	if res["enriched"] != 1 {
		t.Fatalf("first run enriched = %v, want 1", res["enriched"])
	}
	res2 := run(t, st, "ENRICH item:* USING emb", 1300)
	if res2["enriched"] != 0 || res2["cached"] != 1 {
		t.Fatalf("second run = %v, want enriched 0 / cached 1", res2)
	}
	if calls != 1 {
		t.Fatalf("Infer called %d times, want 1 (cache must skip)", calls)
	}
}

// chat ENRICH sends the ON field and stores the text under AS.
func TestEnrichChatOnField(t *testing.T) {
	st := newStore(t)
	run(t, st, "PUT model:sum FACET config SET endpoint='http://x', kind='chat', model='m', prompt='Summarize:'", 1000)
	run(t, st, "PUT item:1 SET note='hello world'", 1100)

	old := Infer
	defer func() { Infer = old }()
	var gotInput string
	Infer = func(r InferRequest) (InferResult, error) {
		gotInput = r.Input
		return InferResult{Text: "a summary"}, nil
	}
	res := run(t, st, "ENRICH item:1 USING sum ON note AS summary", 1200)
	if res["enriched"] != 1 {
		t.Fatalf("enriched = %v, want 1", res["enriched"])
	}
	if gotInput != "hello world" {
		t.Fatalf("ON note should send only that field; model saw %q", gotInput)
	}
}

func TestEnrichUnknownModel(t *testing.T) {
	st := newStore(t)
	p, _ := Parse("ENRICH item:* USING nope", 0)
	if _, err := Execute(st, p, 1000); err == nil {
		t.Fatal("expected an error for an unregistered model")
	}
}

func TestEnrichParse(t *testing.T) {
	for _, c := range []string{
		"ENRICH item:* USING emb",
		"ENRICH item:* FACET source USING m ON note AS summary LIMIT 10",
	} {
		if _, err := Parse(c, 0); err != nil {
			t.Errorf("parse %q: %v", c, err)
		}
	}
	if _, err := Parse("ENRICH item:*", 0); err == nil {
		t.Error("expected parse error: missing USING")
	}
}
