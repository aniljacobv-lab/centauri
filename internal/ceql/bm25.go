// Native full-text search for CeQL: a ranked BM25 (Okapi) inverted scan,
// plus hybrid keyword+vector ranking. Pure stdlib Go (the zero-dependency
// invariant) and read-only — no inverted index is persisted; query terms
// are scored in a single pass over the candidate events, which keeps it
// time-travel-aware for free (search the state AS OF / AS KNOWN AT).
//
// SEARCH '<text>' [OF <pattern>] [FACET f] [SIMILAR TO <event> [ALPHA a]]
//   [AS OF t] [AS KNOWN AT t] [LIMIT n]
package ceql

import (
	"fmt"
	"math"
	"sort"
	"strings"
	"unicode"

	"github.com/proxima360/centauri/internal/model"
	"github.com/proxima360/centauri/internal/store"
)

const (
	bm25K1 = 1.5  // term-frequency saturation
	bm25B  = 0.75 // length normalization
)

// tokenize lowercases and splits on any non-alphanumeric run.
func tokenize(s string) []string {
	var out []string
	var b strings.Builder
	for _, r := range strings.ToLower(s) {
		if unicode.IsLetter(r) || unicode.IsDigit(r) {
			b.WriteRune(r)
		} else if b.Len() > 0 {
			out = append(out, b.String())
			b.Reset()
		}
	}
	if b.Len() > 0 {
		out = append(out, b.String())
	}
	return out
}

func dedupeTerms(toks []string) []string {
	seen := map[string]bool{}
	var out []string
	for _, t := range toks {
		if !seen[t] {
			seen[t] = true
			out = append(out, t)
		}
	}
	return out
}

// bmScore is one BM25 result: the index of the document in the input
// slice, and its score.
type bmScore struct {
	Doc   int
	Score float64
}

// rankBM25 scores each document (a token slice) against the query terms
// using Okapi BM25, in a single pass. Documents containing no query term
// are omitted. Results are sorted best-first. Pure and deterministic.
func rankBM25(docs [][]string, qterms []string) []bmScore {
	n := len(docs)
	if n == 0 || len(qterms) == 0 {
		return nil
	}
	qset := map[string]bool{}
	for _, t := range qterms {
		qset[t] = true
	}
	df := map[string]int{}
	tfs := make([]map[string]int, n)
	total := 0
	for i, doc := range docs {
		total += len(doc)
		tf := map[string]int{}
		for _, tok := range doc {
			if qset[tok] {
				tf[tok]++
			}
		}
		for t := range tf {
			df[t]++
		}
		tfs[i] = tf
	}
	avgdl := float64(total) / float64(n)
	if avgdl == 0 {
		avgdl = 1
	}
	idf := map[string]float64{}
	for t := range qset {
		d := float64(df[t])
		idf[t] = math.Log(1 + (float64(n)-d+0.5)/(d+0.5))
	}
	var out []bmScore
	for i, tf := range tfs {
		if len(tf) == 0 {
			continue
		}
		dl := float64(len(docs[i]))
		s := 0.0
		for t, f := range tf {
			ff := float64(f)
			s += idf[t] * (ff * (bm25K1 + 1)) / (ff + bm25K1*(1-bm25B+bm25B*dl/avgdl))
		}
		out = append(out, bmScore{Doc: i, Score: s})
	}
	sort.Slice(out, func(a, b int) bool { return out[a].Score > out[b].Score })
	return out
}

// minMax normalizes values to [0,1]; a flat set maps to all-1.
func minMax(vals map[int]float64) map[int]float64 {
	out := map[int]float64{}
	if len(vals) == 0 {
		return out
	}
	lo, hi := math.Inf(1), math.Inf(-1)
	for _, v := range vals {
		if v < lo {
			lo = v
		}
		if v > hi {
			hi = v
		}
	}
	for k, v := range vals {
		if hi == lo {
			out[k] = 1
		} else {
			out[k] = (v - lo) / (hi - lo)
		}
	}
	return out
}

// blendHybrid combines normalized BM25 and vector scores: alpha toward
// keyword (1.0), 1-alpha toward semantic similarity (0.0).
func blendHybrid(bm, vec map[int]float64, alpha float64) map[int]float64 {
	bn, vn := minMax(bm), minMax(vec)
	out := map[int]float64{}
	for k := range bm {
		out[k] = alpha*bn[k] + (1-alpha)*vn[k]
	}
	return out
}

func cosine32(a, b []float32) float64 {
	if len(a) == 0 || len(b) == 0 || len(a) != len(b) {
		return 0
	}
	var dot, na, nb float64
	for i := range a {
		dot += float64(a[i]) * float64(b[i])
		na += float64(a[i]) * float64(a[i])
		nb += float64(b[i]) * float64(b[i])
	}
	if na == 0 || nb == 0 {
		return 0
	}
	return dot / (math.Sqrt(na) * math.Sqrt(nb))
}

// docText turns an event into its searchable token stream: the subject
// plus every string-valued field (the same surface as WHERE … MATCHES).
func docText(e *model.Event) []string {
	toks := tokenize(e.Subject)
	for _, v := range e.Value {
		if s, ok := v.(string); ok {
			toks = append(toks, tokenize(s)...)
		}
	}
	return toks
}

func execSearch(st *store.Store, q *Query) (map[string]any, error) {
	if strings.TrimSpace(q.Text) == "" {
		return nil, fmt.Errorf("SEARCH needs a quoted query, e.g. SEARCH 'late markdown'")
	}
	qterms := dedupeTerms(tokenize(q.Text))
	if len(qterms) == 0 {
		return nil, fmt.Errorf("no searchable terms in %q", q.Text)
	}
	if q.Subject == "" {
		q.Subject = "*"
	}
	events, err := gatherEvents(st, q)
	if err != nil {
		return nil, err
	}
	docs := make([][]string, len(events))
	for i, e := range events {
		docs[i] = docText(e)
	}
	ranked := rankBM25(docs, qterms)

	hybrid := q.EventID != ""
	bm := map[int]float64{}
	for _, r := range ranked {
		bm[r.Doc] = r.Score
	}
	var score map[int]float64
	vec := map[int]float64{}
	if hybrid {
		qv := st.Vector(q.EventID)
		if len(qv) == 0 {
			return nil, fmt.Errorf("SIMILAR TO %s: that event has no embedding to compare", q.EventID)
		}
		for _, r := range ranked {
			vec[r.Doc] = cosine32(qv, st.Vector(events[r.Doc].EventID))
		}
		alpha := q.Alpha
		if alpha <= 0 || alpha > 1 {
			alpha = 0.5
		}
		score = blendHybrid(bm, vec, alpha)
	} else {
		score = bm
	}

	order := make([]int, 0, len(score))
	for d := range score {
		order = append(order, d)
	}
	sort.Slice(order, func(a, b int) bool {
		if score[order[a]] != score[order[b]] {
			return score[order[a]] > score[order[b]]
		}
		return order[a] < order[b] // stable tie-break
	})
	limit := q.Limit
	if limit <= 0 {
		limit = 20
	}
	if len(order) > limit {
		order = order[:limit]
	}
	hits := make([]map[string]any, 0, len(order))
	for _, d := range order {
		m := map[string]any{"event": events[d], "score": score[d], "bm25": bm[d]}
		if hybrid {
			m["vector"] = vec[d]
		}
		hits = append(hits, m)
	}
	note := fmt.Sprintf("%d match(es) for %q across %d documents", len(hits), q.Text, len(events))
	if hybrid {
		note += " (hybrid keyword+vector)"
	}
	return map[string]any{
		"kind": "search", "query": q.Text, "terms": qterms,
		"hybrid": hybrid, "hits": hits, "note": note,
	}, nil
}
