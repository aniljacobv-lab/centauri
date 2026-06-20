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
	// Union of both: a semantic-only hit (no keyword overlap) still scores via
	// its vector term — missing keys read as 0 from the normalized maps.
	for k := range bn {
		out[k] = alpha*bn[k] + (1-alpha)*vn[k]
	}
	for k := range vn {
		if _, seen := out[k]; !seen {
			out[k] = alpha*bn[k] + (1-alpha)*vn[k]
		}
	}
	return out
}

// provWeight ranks how much a fact's origin is trusted — physical
// verification beats human entry beats a feed beats inference.
func provWeight(p model.Provenance) float64 {
	switch p {
	case model.ScanVerified:
		return 1.0
	case model.HumanEntry:
		return 0.85
	case model.SystemFeed:
		return 0.70
	case model.AIInferred:
		return 0.50
	}
	return 0.60
}

// combineSignals folds Centauri-native signals into a base relevance score
// (BM25, or the hybrid keyword+vector blend, already in [0,1]). Keyword /
// semantic relevance stays dominant; the extra signals — recency, trust
// (confidence × provenance), and causal centrality — mainly order near-ties.
// This is information a plain inverted index (Postgres FTS, Elasticsearch)
// has no way to use: how fresh a fact is, how much it's trusted, and how
// central it is in the causal graph. Returns the final score plus each
// normalized signal so callers can show *why* a hit ranked where it did.
func combineSignals(st *store.Store, events []*model.Event, base map[int]float64) (final, rec, trust, cen map[int]float64) {
	recRaw, trustRaw, cenRaw := map[int]float64{}, map[int]float64{}, map[int]float64{}
	for d := range base {
		e := events[d]
		t := e.EffectiveTime
		if t == 0 {
			t = e.RecordedTime
		}
		recRaw[d] = float64(t)
		trustRaw[d] = e.Confidence * provWeight(e.Provenance)
		cenRaw[d] = float64(st.CausalDegree(e.EventID))
	}
	rec, trust, cen = minMax(recRaw), minMax(trustRaw), minMax(cenRaw)
	final = map[int]float64{}
	for d, b := range base {
		final[d] = 0.70*b + 0.12*rec[d] + 0.12*trust[d] + 0.06*cen[d]
	}
	return final, rec, trust, cen
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
		// Search the AI-derived text (vision descriptions, tags, summaries) too,
		// not just the raw fact values — so analysed images/docs are findable.
		docs[i] = append(docText(e), enrichTokens(st, e.EventID)...)
	}
	ranked := rankBM25(docs, qterms)

	hybrid := q.EventID != "" // SIMILAR TO <event>
	semantic := false         // query-embedding semantic search
	bm := map[int]float64{}
	for _, r := range ranked {
		bm[r.Doc] = r.Score
	}
	alpha := q.Alpha
	if alpha <= 0 || alpha > 1 {
		alpha = 0.5
	}
	var base map[int]float64
	vec := map[int]float64{}
	switch {
	case hybrid:
		qv := st.Vector(q.EventID)
		if len(qv) == 0 {
			return nil, fmt.Errorf("SIMILAR TO %s: that event has no embedding to compare", q.EventID)
		}
		for _, r := range ranked {
			vec[r.Doc] = cosine32(qv, st.Vector(events[r.Doc].EventID))
		}
		base = blendHybrid(bm, vec, alpha)
	default:
		// Semantic: embed the query with a locally-registered embedder and rank
		// every candidate that has an embedding (not just keyword hits). Falls
		// back to pure BM25 if no embedder is registered or the call fails.
		if qv := embedQuery(st, q.Text); len(qv) > 0 {
			semantic = true
			for d := range events {
				if ev := st.Vector(events[d].EventID); len(ev) > 0 {
					vec[d] = cosine32(qv, ev)
				}
			}
			base = blendHybrid(bm, vec, alpha)
		} else {
			base = minMax(bm)
		}
	}
	// Fold in the signals only a temporal, causal store has: freshness,
	// trust, and causal centrality. Relevance still dominates.
	score, rec, trust, cen := combineSignals(st, events, base)

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
		m := map[string]any{"event": events[d], "score": score[d],
			"bm25": bm[d], "relevance": base[d],
			"recency": rec[d], "trust": trust[d], "centrality": cen[d]}
		useVec := hybrid || semantic
		if useVec {
			m["vector"] = vec[d]
		}
		hits = append(hits, m)
	}
	kind := "keyword"
	switch {
	case hybrid:
		kind = "hybrid keyword+vector"
	case semantic:
		kind = "semantic + keyword"
	}
	note := fmt.Sprintf("%d match(es) for %q across %d documents — ranked by %s relevance + recency + trust + causal centrality",
		len(hits), q.Text, len(events), kind)
	return map[string]any{
		"kind": "search", "query": q.Text, "terms": qterms,
		"hybrid": hybrid || semantic, "hits": hits, "note": note,
	}, nil
}
