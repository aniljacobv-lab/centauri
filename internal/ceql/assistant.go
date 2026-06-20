// The self-learning assistant: ASK '<question>'. It answers from the
// knowledge base stored as kb:<slug> facts using BM25 (reusing the same
// ranker as SEARCH), and — when it can't answer confidently — records the
// question as a kb_gap:<slug> fact so an AI agent can answer it later
// (over MCP) by appending a new kb:<slug> fact. Centauri is the gateway
// and the memory: ask → search facts → miss → log gap → an agent fills
// it → answered from the database from then on.
package ceql

import (
	"fmt"
	"strings"

	"github.com/proxima360/centauri/internal/model"
	"github.com/proxima360/centauri/internal/store"
)

// askThreshold is the minimum BM25 score to count as a confident answer;
// below it the question is treated as unlearned and logged as a gap.
const askThreshold = 1.0

var askStop = map[string]bool{}

func init() {
	for _, w := range strings.Fields("the is it its a an of to do does did can could would should how what why where when which who with for and or but my me i you your our this that these those are was were be been on in at as by from will not no yes some any it's whats") {
		askStop[w] = true
	}
}

// slugify makes a stable kb_gap slug from a question.
func slugify(s string) string {
	toks := tokenize(s)
	if len(toks) > 6 {
		toks = toks[:6]
	}
	out := strings.Join(toks, "-")
	if out == "" {
		out = "question"
	}
	return out
}

func execAsk(st *store.Store, q *Query, now int64) (map[string]any, error) {
	question := strings.TrimSpace(q.Text)
	if question == "" {
		return nil, fmt.Errorf("ASK needs a quoted question, e.g. ASK 'does it scale?'")
	}
	// content terms only (drop stop-words so "can it do X" matches on X).
	var qterms []string
	seen := map[string]bool{}
	for _, t := range tokenize(question) {
		if len(t) > 1 && !askStop[t] && !seen[t] {
			seen[t] = true
			qterms = append(qterms, t)
		}
	}
	// gather the knowledge facts (current, one per kb:* subject).
	var kb []*model.Event
	for _, s := range st.Subjects() {
		if strings.HasPrefix(s, "kb:") {
			kb = append(kb, st.Current(s, "")...)
		}
	}
	if len(kb) > 0 && len(qterms) > 0 {
		docs := make([][]string, len(kb))
		for i, e := range kb {
			docs[i] = docText(e)
		}
		if r := rankBM25(docs, qterms); len(r) > 0 && r[0].Score >= askThreshold {
			best := kb[r[0].Doc]
			ans, _ := best.Value["answer"].(string)
			return map[string]any{
				"kind": "assistant", "question": question, "answer": ans,
				"source": best.Subject, "source_event": best.EventID,
				"score": r[0].Score, "learned": true,
			}, nil
		}
	}
	// KB miss → answer from the user's OWN data with a local LLM (RAG): retrieve
	// the most relevant facts, then have the model answer using only those, with
	// citations. Degrades to the gap-logging path if no chat model is registered.
	if ctx := retrieve(st, question, 6); len(ctx) > 0 && findModel(st, "chat", "vision") != nil {
		var b strings.Builder
		for _, e := range ctx {
			b.WriteString(contextLine(st, e))
			b.WriteByte('\n')
		}
		sys := "You answer questions about a user's database. Use ONLY the facts below. " +
			"Cite the [ids] you used in your answer. If the facts don't contain the answer, say you don't know."
		if ans := chatLLM(st, sys, "Facts:\n"+b.String()+"\nQuestion: "+question); ans != "" {
			ids := make([]string, len(ctx))
			for i, e := range ctx {
				ids[i] = e.EventID
			}
			return map[string]any{
				"kind": "assistant", "question": question, "answer": ans,
				"grounded": true, "sources": ids, "learned": false,
			}, nil
		}
	}

	// Miss: record the gap so an agent can answer it later.
	gap := "kb_gap:" + slugify(question)
	ev := &model.Event{
		Subject: gap, Facet: "assistant", Type: model.Observed,
		Value:        map[string]any{"question": question, "asked_at": now},
		Provenance:   model.SystemFeed, Confidence: 1.0, SourceSystem: "ASSISTANT",
	}
	_ = st.Append(now, []*model.Event{ev}, nil)
	return map[string]any{
		"kind": "assistant", "question": question, "answer": "", "learned": false,
		"gap":  gap,
		"note": "I haven't learned that yet — logged as " + gap + ". An agent can answer it, then PUT kb:<slug> SET answer='…' so it's answered from the database next time.",
	}, nil
}
