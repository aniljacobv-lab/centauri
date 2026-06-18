package ceql

import (
	"fmt"
	"strconv"
	"strings"
	"time"

	"github.com/proxima360/centauri/internal/model"
	"github.com/proxima360/centauri/internal/store"
)

// execExplain describes how a statement will run (its access path through
// Centauri's indexes), and — with ANALYZE — actually runs it and reports the
// row count and timing. Honest by design: Centauri has no cost-based
// optimizer, so the "plan" is the deterministic access path, not an estimate.
// ANALYZE never executes write statements (no side effects).
func execExplain(st *store.Store, q *Query, now int64) (map[string]any, error) {
	inner := q.Inner
	if inner == nil {
		return nil, fmt.Errorf("EXPLAIN needs a statement, e.g. EXPLAIN FACTS OF item:*")
	}
	out := map[string]any{
		"kind": "explain", "analyze": q.Analyze,
		"target": string(inner.Kind), "plan": planFor(inner), "ast": inner,
	}
	if q.Analyze {
		if inner.IsWrite() {
			out["note"] = "ANALYZE shows the plan only for write statements — it will not execute side effects."
			return out, nil
		}
		t0 := time.Now()
		res, err := Execute(st, inner, now)
		out["ms"] = float64(time.Since(t0).Microseconds()) / 1000.0
		if err != nil {
			out["error"] = err.Error()
		} else {
			out["rows"] = resultRows(res)
			out["executed"] = true
		}
	}
	return out, nil
}

func subjOrAll(s string) string {
	if s == "" || s == "*" {
		return "all subjects"
	}
	return s
}

// planFor returns the deterministic access path for a statement.
func planFor(q *Query) []string {
	switch q.Kind {
	case KFacts, KHistory:
		var s []string
		if q.Kind == KHistory {
			s = append(s, "history scan: full timeline for subject(s)")
		}
		if strings.ContainsRune(q.Subject, '*') {
			s = append(s, "wildcard subject scan: Subjects() + regex match")
		} else {
			s = append(s, "point lookup: open/bySubjectFacet index on "+q.Subject)
		}
		if q.Facet == "" {
			s = append(s, "facet enumeration: subjectFacets index (O(facets of subject))")
		} else {
			s = append(s, "facet filter: "+q.Facet)
		}
		if q.AsOf > 0 || q.KnownAt > 0 {
			s = append(s, "bi-temporal select: max effective ≤ as-of, recorded ≤ known-at")
		}
		if q.Where != nil {
			s = append(s, "filter: WHERE predicate")
		}
		if hasAgg(q.Fields) {
			s = append(s, "aggregate: GROUP BY / aggregate functions")
		}
		if q.OrderBy != "" {
			s = append(s, "sort: ORDER BY "+q.OrderBy)
		}
		if q.Limit > 0 {
			s = append(s, "limit "+strconv.Itoa(q.Limit))
		}
		if q.Why {
			s = append(s, "attach causal chains: WHY depth "+strconv.Itoa(q.Depth))
		}
		return s
	case KSearch:
		s := []string{"candidate gather: " + subjOrAll(q.Subject),
			"rank: BM25 single-pass (no persisted inverted index)",
			"multi-signal: + recency + trust + causal centrality"}
		if q.EventID != "" {
			s = append(s, "hybrid: blend cosine similarity over the vector index")
		}
		return s
	case KMatch:
		return []string{"causal traversal: TraceVia from " + subjOrAll(q.Subject) +
			" depth " + strconv.Itoa(q.Depth), "filter targets by pattern: " + q.MatchTo}
	case KShape, KConsistency, KCycles, KDrift:
		return []string{"topology: " + string(q.Kind) + " over the value cloud"}
	case KEnrich:
		return []string{"per-event model call: USING " + q.Using + " (cached as enrichment facts)"}
	case KContext:
		return []string{"context bundle: current facts + history + causes + disagreements + enrichments"}
	case KDiff:
		return []string{"compare bi-temporal snapshots at two points, per subject/facet"}
	default:
		return []string{string(q.Kind) + " execution"}
	}
}

// resultRows counts the rows a result carries, across the shapes Execute returns.
func resultRows(res map[string]any) int {
	if v, ok := res["events"].([]*model.Event); ok {
		return len(v)
	}
	if v, ok := res["rows"].([][]any); ok {
		return len(v)
	}
	if v, ok := res["hits"].([]map[string]any); ok {
		return len(v)
	}
	if v, ok := res["subjects"].([]string); ok {
		return len(v)
	}
	if v, ok := res["count"].(int); ok {
		return v
	}
	return 0
}
