package ceql

import (
	"strings"
	"testing"
	"time"

	"github.com/proxima360/centauri/internal/model"
)

func embedFact(st interface {
	AddEnrichment(*model.Enrichment) error
}, id string, vec []float32) {
	_ = st.AddEnrichment(&model.Enrichment{EnrichmentID: model.NewID(), TargetEvent: id,
		Kind: model.EmbeddingKind, Result: map[string]any{"vector": vec}, Confidence: 1, CreatedAt: 1})
}

// Semantic SEARCH must surface a fact whose embedding matches the query even
// with zero keyword overlap.
func TestSemanticSearch(t *testing.T) {
	st := newStore(t)
	run(t, st, "PUT model:emb FACET config SET endpoint='http://x', kind='embedding'", 1000)
	a := &model.Event{EventID: "ev-a", Subject: "item:A", Facet: "f", Type: model.Observed,
		Value: map[string]any{"note": "alpha"}, Provenance: model.SystemFeed, Confidence: 1}
	b := &model.Event{EventID: "ev-b", Subject: "item:B", Facet: "f", Type: model.Observed,
		Value: map[string]any{"note": "beta"}, Provenance: model.SystemFeed, Confidence: 1}
	if err := st.Append(1100, []*model.Event{a, b}, nil); err != nil {
		t.Fatal(err)
	}
	embedFact(st, "ev-a", []float32{1, 0, 0})
	embedFact(st, "ev-b", []float32{0, 1, 0})

	old := Infer
	defer func() { Infer = old }()
	Infer = func(r InferRequest) (InferResult, error) {
		if r.Kind == "embedding" {
			return InferResult{Vector: []float32{1, 0, 0}}, nil // close to ev-a
		}
		return InferResult{}, nil
	}
	res := run(t, st, "SEARCH 'zzz' OF item:*", 1200) // 'zzz' matches no keyword
	hits, _ := res["hits"].([]map[string]any)
	if len(hits) == 0 {
		t.Fatalf("semantic search found nothing: %v", res["note"])
	}
	if top, _ := hits[0]["event"].(*model.Event); top == nil || top.Subject != "item:A" {
		t.Fatalf("top hit = %#v, want item:A by vector similarity", hits[0]["event"])
	}
}

// Keyword SEARCH must also see AI-derived enrichment text (vision descriptions).
func TestEnrichmentTextSearchable(t *testing.T) {
	st := newStore(t)
	c := &model.Event{EventID: "ev-c", Subject: "item:C", Facet: "f", Type: model.Observed,
		Value: map[string]any{"x": 1}, Provenance: model.SystemFeed, Confidence: 1}
	if err := st.Append(1000, []*model.Event{c}, nil); err != nil {
		t.Fatal(err)
	}
	_ = st.AddEnrichment(&model.Enrichment{EnrichmentID: model.NewID(), TargetEvent: "ev-c",
		Kind: "vision", Result: map[string]any{"description": "supercalifragilistic expialidocious"},
		Confidence: 1, CreatedAt: 1000})
	res := run(t, st, "SEARCH 'supercalifragilistic' OF item:*", 1100)
	if hits, _ := res["hits"].([]map[string]any); len(hits) == 0 {
		t.Fatalf("enrichment description should be keyword-searchable: %v", res["note"])
	}
}

// NL→CeQL falls through to the local LLM when the rules can't translate, and the
// model's output is validated before being returned.
func TestNL2QViaLLM(t *testing.T) {
	st := newStore(t)
	run(t, st, "PUT model:chat FACET config SET endpoint='http://x', kind='chat'", 1000)
	old := Infer
	defer func() { Infer = old }()
	Infer = func(r InferRequest) (InferResult, error) {
		if r.Kind == "chat" {
			return InferResult{Text: "```ceql\nFACTS OF item:*\n```"}, nil
		}
		return InferResult{}, nil
	}
	tr, err := TranslateNLAI(st, "show me documents about inventory problems", time.Now().UnixMicro())
	if err != nil {
		t.Fatalf("LLM translation: %v", err)
	}
	if tr.CeQL != "FACTS OF item:*" {
		t.Fatalf("translated CeQL = %q, want 'FACTS OF item:*'", tr.CeQL)
	}
}

// ASK answers from the user's own data (RAG) when the KB doesn't know, citing
// the facts it used.
func TestRAGAsk(t *testing.T) {
	st := newStore(t)
	run(t, st, "PUT model:chat FACET config SET endpoint='http://x', kind='chat'", 1000)
	d := &model.Event{EventID: "ev-d", Subject: "item:widget", Facet: "f", Type: model.Observed,
		Value: map[string]any{"note": "the widget costs 500 cents"}, Provenance: model.SystemFeed, Confidence: 1}
	if err := st.Append(1100, []*model.Event{d}, nil); err != nil {
		t.Fatal(err)
	}
	old := Infer
	defer func() { Infer = old }()
	Infer = func(r InferRequest) (InferResult, error) {
		if r.Kind == "chat" {
			return InferResult{Text: "The widget costs 500 cents [ev-d]."}, nil
		}
		return InferResult{}, nil // no embedder: retrieval falls back to keyword
	}
	res := run(t, st, "ASK 'how much is the widget'", 1200)
	if res["grounded"] != true {
		t.Fatalf("expected a grounded RAG answer, got %v", res)
	}
	if ans, _ := res["answer"].(string); !strings.Contains(ans, "500") {
		t.Fatalf("answer = %q, want it to mention 500", res["answer"])
	}
}
