// Local-LLM helpers shared by SEARCH (semantic), ASK (RAG), and NL→CeQL. They
// reuse the same registered-model facts and Infer hook as ENRICH, so everything
// runs against whatever local model server (e.g. Ollama) the user registered —
// no new dependency, no cloud. Each helper degrades gracefully: if no suitable
// model is registered or the call fails, it returns empty and the caller falls
// back to the non-LLM path (keyword search, rule-based NL, the knowledge base).
package ceql

import (
	"fmt"
	"os"
	"sort"
	"strings"

	"github.com/proxima360/centauri/internal/model"
	"github.com/proxima360/centauri/internal/store"
)

// findModel returns the config of the first registered model whose kind matches
// any of `kinds` (e.g. "embedding", or "chat"/"vision"), or nil — so SEARCH/ASK
// can use a locally-registered model without the user naming one.
func findModel(st *store.Store, kinds ...string) map[string]any {
	for _, s := range st.Subjects() {
		if !strings.HasPrefix(s, "model:") {
			continue
		}
		cur := st.Current(s, "config")
		if len(cur) == 0 {
			continue
		}
		k, _ := cur[0].Value["kind"].(string)
		for _, want := range kinds {
			if k == want {
				return cur[0].Value
			}
		}
	}
	return nil
}

// embedQuery embeds free text with a locally-registered embedder, or returns nil
// if none is registered / the call fails (caller falls back to keyword search).
func embedQuery(st *store.Store, text string) []float32 {
	cfg := findModel(st, "embedding")
	if cfg == nil {
		return nil
	}
	endpoint, _ := cfg["endpoint"].(string)
	if endpoint == "" {
		return nil
	}
	var token string
	if env, _ := cfg["auth_env"].(string); env != "" {
		token = os.Getenv(env)
	}
	mid, _ := cfg["model"].(string)
	res, err := Infer(InferRequest{Endpoint: endpoint, Kind: "embedding", Model: mid, AuthToken: token, Input: text})
	if err != nil {
		return nil
	}
	return res.Vector
}

// chatLLM sends system+user text to a locally-registered chat/vision model and
// returns its reply, or "" if none is registered / it fails. Used by ASK (RAG)
// and NL→CeQL.
func chatLLM(st *store.Store, system, user string) string {
	cfg := findModel(st, "chat", "vision")
	if cfg == nil {
		return ""
	}
	endpoint, _ := cfg["endpoint"].(string)
	if endpoint == "" {
		return ""
	}
	var token string
	if env, _ := cfg["auth_env"].(string); env != "" {
		token = os.Getenv(env)
	}
	mid, _ := cfg["model"].(string)
	res, err := Infer(InferRequest{Endpoint: endpoint, Kind: "chat", Model: mid, AuthToken: token,
		Prompt: system, Input: user, TimeoutSecs: 300})
	if err != nil {
		return ""
	}
	return strings.TrimSpace(res.Text)
}

// stripFence removes a Markdown code fence (```…``` with an optional ceql/sql
// language tag) that LLMs often wrap output in.
func stripFence(s string) string {
	s = strings.TrimSpace(s)
	if strings.HasPrefix(s, "```") {
		s = strings.TrimSpace(strings.TrimPrefix(s, "```"))
		s = strings.TrimPrefix(s, "ceql")
		s = strings.TrimPrefix(s, "sql")
		if i := strings.LastIndex(s, "```"); i >= 0 {
			s = s[:i]
		}
	}
	return strings.TrimSpace(s)
}

// TranslateNLAI converts plain English to CeQL: deterministic rules first (fast,
// precise), then a locally-registered LLM for anything the rules don't cover.
// The model's output is re-parsed before being returned, so a suggestion is
// never syntactically invalid; if no LLM is registered it just returns the rule
// result (or its error).
func TranslateNLAI(st *store.Store, text string, now int64) (*Translation, error) {
	if tr, err := TranslateNL(text, now); err == nil && tr != nil && strings.TrimSpace(tr.CeQL) != "" {
		return tr, nil
	}
	if findModel(st, "chat", "vision") == nil {
		return TranslateNL(text, now)
	}
	sys := "Translate the user's request into ONE CeQL query. Output only the query, no prose, no code fence. " +
		"CeQL examples: FACTS OF item:* WHERE price_cents > 700 | HISTORY OF item:1 | " +
		"SEARCH 'text' OF asset:* | FACTS OF item:1 AS OF YESTERDAY | DISAGREE ON price_cents | " +
		"FACTS namespace, COUNT(*) OF * GROUP BY namespace | MATCH item:* CAUSES order:*"
	out := stripFence(chatLLM(st, sys, text))
	if out == "" {
		return TranslateNL(text, now)
	}
	if _, err := Parse(out, now); err != nil {
		return nil, fmt.Errorf("the local model produced an invalid query (%v): %s", err, out)
	}
	return &Translation{CeQL: out, Note: "translated by local LLM"}, nil
}

// retrieve returns the top-k events most relevant to a question, ranked by the
// same hybrid (keyword BM25 + semantic vector) signal SEARCH uses — the
// retrieval half of RAG. Falls back to keyword-only if no embedder is present.
func retrieve(st *store.Store, question string, k int) []*model.Event {
	q := &Query{Subject: "*", Text: question}
	all, err := gatherEvents(st, q)
	if err != nil || len(all) == 0 {
		return nil
	}
	// Don't feed Centauri's own bookkeeping (models, ACLs, the knowledge base)
	// into the answer context — only the user's data.
	events := make([]*model.Event, 0, len(all))
	for _, e := range all {
		if strings.HasPrefix(e.Subject, "model:") || strings.HasPrefix(e.Subject, "kb:") ||
			strings.HasPrefix(e.Subject, "kb_gap:") || strings.HasPrefix(e.Subject, "acl:") ||
			strings.HasPrefix(e.Subject, "feedback:") || isBookkeeping(e.Subject) {
			continue
		}
		events = append(events, e)
	}
	if len(events) == 0 {
		return nil
	}
	docs := make([][]string, len(events))
	for i, e := range events {
		docs[i] = append(docText(e), enrichTokens(st, e.EventID)...)
	}
	bm := map[int]float64{}
	for _, r := range rankBM25(docs, dedupeTerms(tokenize(question))) {
		bm[r.Doc] = r.Score
	}
	vec := map[int]float64{}
	if qv := embedQuery(st, question); len(qv) > 0 {
		for d := range events {
			if ev := st.Vector(events[d].EventID); len(ev) > 0 {
				vec[d] = cosine32(qv, ev)
			}
		}
	}
	base := blendHybrid(bm, vec, 0.5)
	// Closed feedback loop: a user's rating of a source nudges it up or down in
	// future retrievals — bounded, so it re-ranks among relevant hits without
	// overriding relevance. Costs nothing until ratings exist.
	if fb := feedbackScores(st); len(fb) > 0 {
		for d := range base {
			if sc, ok := fb[events[d].EventID]; ok {
				base[d] += feedbackWeight * sc
			}
		}
	}
	order := make([]int, 0, len(base))
	for d := range base {
		order = append(order, d)
	}
	sort.Slice(order, func(a, b int) bool { return base[order[a]] > base[order[b]] })
	if k <= 0 {
		k = 6
	}
	out := []*model.Event{}
	for _, d := range order {
		if len(out) >= k {
			break
		}
		out = append(out, events[d])
	}
	return out
}

// contextLine renders one fact for an LLM context block: id, subject, compact
// values, and the AI description if present.
func contextLine(st *store.Store, e *model.Event) string {
	desc := ""
	for _, en := range st.EnrichmentsFor(e.EventID) {
		if en.SupersededBy == "" {
			if d, ok := en.Result["description"].(string); ok {
				desc = " — " + d
				break
			}
		}
	}
	return fmt.Sprintf("[%s] %s %v%s", e.EventID, e.Subject, e.Value, desc)
}

// enrichTokens returns searchable tokens from an event's enrichments (the vision
// description/tags, summaries, etc.) so SEARCH's keyword path sees AI-derived
// text, not just the raw fact values.
func enrichTokens(st *store.Store, eventID string) []string {
	var toks []string
	for _, en := range st.EnrichmentsFor(eventID) {
		if en.SupersededBy != "" {
			continue
		}
		for _, key := range []string{"description", "text", "summary"} {
			if s, ok := en.Result[key].(string); ok {
				toks = append(toks, tokenize(s)...)
			}
		}
		if tags, ok := en.Result["tags"].([]any); ok {
			for _, t := range tags {
				if s, ok := t.(string); ok {
					toks = append(toks, tokenize(s)...)
				}
			}
		}
	}
	return toks
}
