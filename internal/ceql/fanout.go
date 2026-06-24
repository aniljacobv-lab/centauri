package ceql

// Cross-shard fan-out merge. In sharded mode a wildcard/global read runs on every
// shard in parallel (each shard answers over its own subjects), and the per-shard
// results are merged here — where the result shapes and the sort comparators
// already live. Supported: FACTS / HISTORY over a pattern (events or a plain
// projection) and SEARCH. NOT supported across shards: aggregates/GROUP BY
// (correct merging needs partial sums the executor doesn't expose), causal trace,
// and other global kinds — callers should reject those before fanning out.

import (
	"fmt"
	"sort"

	"github.com/proxima360/centauri/internal/model"
)

// FanOutSupported reports whether a query can be answered by per-shard execution
// + a merge. Concrete-subject queries don't fan out (route to one shard); this is
// for the wildcard/global case.
func FanOutSupported(q *Query) bool {
	switch q.Kind {
	case KSearch:
		return true
	case KFacts, KHistory:
		return !hasAgg(q.Fields) && q.GroupBy == ""
	}
	return false
}

// MergeShardResults combines per-shard results for a fan-out query into one
// result, re-applying global ORDER BY / OFFSET / LIMIT (or score ranking for
// SEARCH). results must all come from executing the SAME query q on each shard.
func MergeShardResults(q *Query, results []map[string]any) (map[string]any, error) {
	switch {
	case q.Kind == KSearch:
		return mergeSearch(q, results), nil
	case (q.Kind == KFacts || q.Kind == KHistory) && !hasAgg(q.Fields) && q.GroupBy == "":
		return mergeReads(q, results), nil
	}
	return nil, fmt.Errorf("query kind %q can't be merged across shards; use a concrete subject or single-store serve", q.Kind)
}

func mergeSearch(q *Query, results []map[string]any) map[string]any {
	var hits []map[string]any
	for _, r := range results {
		if h, ok := r["hits"].([]map[string]any); ok {
			hits = append(hits, h...)
		}
	}
	sort.SliceStable(hits, func(i, j int) bool { return scoreOf(hits[i]) > scoreOf(hits[j]) })
	limit := q.Limit
	if limit <= 0 {
		limit = 20
	}
	if len(hits) > limit {
		hits = hits[:limit]
	}
	return map[string]any{
		"kind": "search", "query": q.Text, "hits": hits,
		"note": fmt.Sprintf("merged across %d shards — note: BM25 scores are per-shard, so cross-shard ranking is approximate", len(results)),
	}
}

func scoreOf(hit map[string]any) float64 {
	if f, ok := hit["score"].(float64); ok {
		return f
	}
	return 0
}

func mergeReads(q *Query, results []map[string]any) map[string]any {
	projected := len(q.Fields) > 0 && !(len(q.Fields) == 1 && q.Fields[0].Name == "*")
	if projected {
		var cols []string
		var rows [][]any
		for _, r := range results {
			if c, ok := r["columns"].([]string); ok && cols == nil {
				cols = c
			}
			if rs, ok := r["rows"].([][]any); ok {
				rows = append(rows, rs...)
			}
		}
		if q.OrderBy != "" {
			if idx := colIndex(cols, q.OrderBy); idx >= 0 {
				sort.SliceStable(rows, func(i, j int) bool {
					if q.Desc {
						return lessVal(rows[j][idx], rows[i][idx])
					}
					return lessVal(rows[i][idx], rows[j][idx])
				})
			}
		}
		rows = pageRows(rows, q.Offset, q.Limit)
		return map[string]any{"kind": "rows", "columns": cols, "rows": rows}
	}

	var evs []*model.Event
	for _, r := range results {
		if e, ok := r["events"].([]*model.Event); ok {
			evs = append(evs, e...)
		}
	}
	if q.OrderBy != "" {
		sort.SliceStable(evs, func(i, j int) bool {
			if q.Desc {
				return lessVal(getField(evs[j], q.OrderBy), getField(evs[i], q.OrderBy))
			}
			return lessVal(getField(evs[i], q.OrderBy), getField(evs[j], q.OrderBy))
		})
	}
	if q.Offset > 0 {
		if q.Offset >= len(evs) {
			evs = nil
		} else {
			evs = evs[q.Offset:]
		}
	}
	if q.Limit > 0 && len(evs) > q.Limit {
		evs = evs[:q.Limit]
	}
	return map[string]any{"kind": "events", "events": evs,
		"note": fmt.Sprintf("merged across %d shards", len(results))}
}

func colIndex(cols []string, name string) int {
	for i, c := range cols {
		if c == name {
			return i
		}
	}
	return -1
}

func pageRows(rows [][]any, offset, limit int) [][]any {
	if offset > 0 {
		if offset >= len(rows) {
			return nil
		}
		rows = rows[offset:]
	}
	if limit > 0 && len(rows) > limit {
		rows = rows[:limit]
	}
	return rows
}
