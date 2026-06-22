package store

// Disk-scan full-text SEARCH for the lazy/archive path: a single streaming pass
// over the zone-map-pruned segments + tail scores events with Okapi BM25 and
// returns the top-k — WITHOUT building a resident inverted index and without
// retaining every document. Only candidate docs (those containing at least one
// query term) are kept, so RAM scales with query selectivity, not corpus size.
//
// This is keyword BM25 only. The richer signals the in-RAM engine folds in
// (vector similarity, causal centrality, recency/trust weighting) need the full
// index and are not part of the cold-segment path. Tokenization matches the
// engine (lowercase, split on non-alphanumeric) so hits are consistent.

import (
	"math"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"unicode"

	"github.com/proxima360/centauri/internal/model"
	"github.com/proxima360/centauri/internal/segment"
)

const (
	searchK1 = 1.5  // term-frequency saturation
	searchB  = 0.75 // length normalization
)

// SearchHit is one ranked result.
type SearchHit struct {
	Event *model.Event `json:"event"`
	Score float64      `json:"score"`
}

// searchTokenize lowercases and splits on any non-alphanumeric run (matches the
// engine's tokenizer).
func searchTokenize(s string) []string {
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

// eventDocText is an event's searchable token stream: its subject plus every
// string-valued field (the same surface as the engine's docText).
func eventDocText(e *model.Event) []string {
	toks := searchTokenize(e.Subject)
	for _, v := range e.Value {
		if s, ok := v.(string); ok {
			toks = append(toks, searchTokenize(s)...)
		}
	}
	return toks
}

type searchCand struct {
	ev *model.Event
	tf map[string]int
	dl int
}

// rankEventsBM25 scores a set of events against the query with Okapi BM25 and
// returns the top-`limit` hits. Only candidate docs (>=1 query term) are
// retained; corpus stats (N, avgdl) use the full set.
func rankEventsBM25(events []*model.Event, query string, limit int) []SearchHit {
	// Deduplicate query terms.
	qset := map[string]bool{}
	for _, t := range searchTokenize(query) {
		qset[t] = true
	}
	if len(qset) == 0 || len(events) == 0 {
		return nil
	}

	var (
		n        int
		totalLen int
		df       = map[string]int{}
		cands    []searchCand
	)
	for _, e := range events {
		if e == nil {
			continue
		}
		toks := eventDocText(e)
		n++
		totalLen += len(toks)
		tf := map[string]int{}
		for _, tok := range toks {
			if qset[tok] {
				tf[tok]++
			}
		}
		if len(tf) == 0 {
			continue
		}
		for t := range tf {
			df[t]++
		}
		cands = append(cands, searchCand{ev: e, tf: tf, dl: len(toks)})
	}
	if n == 0 || len(cands) == 0 {
		return nil
	}
	avgdl := float64(totalLen) / float64(n)
	if avgdl == 0 {
		avgdl = 1
	}
	idf := map[string]float64{}
	for t := range qset {
		d := float64(df[t])
		idf[t] = math.Log(1 + (float64(n)-d+0.5)/(d+0.5))
	}

	hits := make([]SearchHit, 0, len(cands))
	for _, c := range cands {
		s := 0.0
		for t, f := range c.tf {
			ff := float64(f)
			s += idf[t] * (ff * (searchK1 + 1)) / (ff + searchK1*(1-searchB+searchB*float64(c.dl)/avgdl))
		}
		hits = append(hits, SearchHit{Event: c.ev, Score: s})
	}
	sort.SliceStable(hits, func(a, b int) bool {
		if hits[a].Score != hits[b].Score {
			return hits[a].Score > hits[b].Score
		}
		return hits[a].Event.EventID < hits[b].Event.EventID // stable tie-break
	})
	if limit <= 0 {
		limit = 20
	}
	if len(hits) > limit {
		hits = hits[:limit]
	}
	return hits
}

// ScanSearch streams the archive (current facts only) and returns the top-`limit`
// BM25 hits — standalone, no resident index required.
func ScanSearch(dir, query string, limit int) ([]SearchHit, error) {
	mb, err := os.ReadFile(filepath.Join(dir, "manifest.json"))
	if err != nil {
		return nil, err
	}
	man, err := segment.ParseManifest(mb)
	if err != nil {
		return nil, err
	}
	live := map[string]map[string]*model.Event{}
	idKey := map[string]string{}
	for _, e := range man.Segments {
		raw, err := os.ReadFile(filepath.Join(dir, filepath.FromSlash(e.Path)))
		if err != nil {
			return nil, err
		}
		if e.Compressed {
			if raw, err = segment.Decompress(raw); err != nil {
				return nil, err
			}
		}
		applyRecordsLazy(raw, live, idKey)
	}
	if tb, err := os.ReadFile(archiveTailPath(dir, man)); err == nil {
		applyRecordsLazy(tb, live, idKey)
	}
	var current []*model.Event
	for _, m := range live {
		var best *model.Event
		for _, e := range m {
			if beats(e, best) {
				best = e
			}
		}
		if best != nil {
			current = append(current, best)
		}
	}
	return rankEventsBM25(current, query, limit), nil
}
