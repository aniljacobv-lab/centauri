package ceql

import (
	"fmt"
	"math"
	"regexp"
	"sort"
	"strings"
	"time"

	"github.com/proxima360/centauri/internal/model"
	"github.com/proxima360/centauri/internal/store"
)

// IsWrite reports whether executing q mutates the database (used by the
// API to enforce read-only follower mode).
func (q *Query) IsWrite() bool {
	// EXPLAIN never writes — it only shows the plan of its inner query.
	// RUN counts as a write because procedures may PUT.
	// ASK may append a kb_gap fact on a miss, so it counts as a write.
	// SNAPSHOT appends a marker; ROLLBACK appends reversion facts.
	// DIFF is read-only.
	return q.Kind == KPut || q.Kind == KDefineSchema || q.Kind == KRun || q.Kind == KAsk ||
		q.Kind == KSnapshot || q.Kind == KRollback || q.Kind == KEnrich
}

// Execute runs a CeQL query against a store. now is the server clock
// (UnixMicro). The result is JSON-shaped: every kind returns a map with
// a "kind" key the dashboard and agents can switch on.
func Execute(st *store.Store, q *Query, now int64) (map[string]any, error) {
	switch q.Kind {
	case KExplain:
		return execExplain(st, q, now)
	case KStats:
		return map[string]any{"kind": "stats", "stats": st.Stats()}, nil
	case KSubjects:
		subs := st.Subjects()
		if q.Subject != "" {
			re := globRe(q.Subject)
			var out []string
			for _, s := range subs {
				if re.MatchString(s) {
					out = append(out, s)
				}
			}
			subs = out
		}
		if q.Limit > 0 && len(subs) > q.Limit {
			subs = subs[:q.Limit]
		}
		return map[string]any{"kind": "subjects", "subjects": subs}, nil
	case KSchemas:
		return map[string]any{"kind": "schemas", "schemas": st.Schemas()}, nil
	case KSchema:
		versions := st.SchemaVersions(q.SchemaID)
		if len(versions) == 0 {
			return nil, fmt.Errorf("unknown schema %q", q.SchemaID)
		}
		return map[string]any{"kind": "schemas", "schemas": versions}, nil
	case KDefineSchema:
		sc := &model.Schema{SchemaID: q.SchemaID, Title: q.SchemaTitle,
			Fields: map[string]model.FieldDef{}}
		for _, f := range q.SchemaFields {
			sc.Fields[f.Name] = model.FieldDef{Type: f.Type, Required: f.Required,
				Min: f.Min, Max: f.Max, Unit: f.Unit}
		}
		if err := st.PutSchema(now, sc); err != nil {
			return nil, err
		}
		return map[string]any{"kind": "schema_defined", "schema": sc.Ref(),
			"version": sc.Version}, nil
	case KPut:
		return execPut(st, q, now)
	case KPending:
		var older int64
		if q.OlderDays > 0 {
			older = now - int64(q.OlderDays)*24*int64(time.Hour/time.Microsecond)
		}
		evs := st.Pending(q.Facet, older)
		return map[string]any{"kind": "events", "events": evs,
			"note": fmt.Sprintf("%d pending (distributed, never activated) on facet %s", len(evs), q.Facet)}, nil
	case KDisagree:
		return map[string]any{"kind": "disagreements", "field": q.Field,
			"disagreements": st.Disagreements(q.Field)}, nil
	case KWhy:
		return map[string]any{"kind": "trace", "direction": "cause",
			"trace": st.Trace(q.EventID, "cause", q.Depth)}, nil
	case KEffects:
		return map[string]any{"kind": "trace", "direction": "effect",
			"trace": st.Trace(q.EventID, "effect", q.Depth)}, nil
	case KSimilar:
		min := -1.0
		if q.MinScore != nil {
			min = *q.MinScore
		}
		hits := st.SimilarToEvent(q.EventID, q.TopK, min)
		return map[string]any{"kind": "similar", "hits": hits}, nil
	case KContext:
		return map[string]any{"kind": "context",
			"context": st.Context(q.Subject, q.KnownAt, q.Limit, 0)}, nil
	case KWatch:
		// A watch is a standing query; the executor returns the channel
		// parameters and the caller (dashboard, SDK) opens the stream.
		return map[string]any{"kind": "watch", "subject": q.Subject,
			"facet": q.Facet, "type": q.EvType,
			"note": "open /v1/watch with these filters to receive the stream"}, nil
	case KRun:
		// Procedures live a layer above the query engine (internal/proc);
		// the API and MCP layers intercept KRun before calling Execute.
		return nil, fmt.Errorf("RUN is handled by the server layer — POST the query to /v1/query")
	case KProfile:
		return execProfile(st, q)
	case KShape:
		return execShape(st, q)
	case KConsistency:
		return execConsistency(st, q)
	case KCycles:
		return execCycles(st, q)
	case KDrift:
		return execDrift(st, q)
	case KSearch:
		return execSearch(st, q)
	case KAsk:
		return execAsk(st, q, now)
	case KSnapshot:
		return execSnapshot(st, q, now)
	case KRollback:
		return execRollback(st, q, now)
	case KDiff:
		return execDiff(st, q)
	case KMatch:
		return execMatch(st, q)
	case KEnrich:
		return execEnrich(st, q, now)
	case KFacts, KHistory:
		return execRead(st, q)
	}
	return nil, fmt.Errorf("unknown query kind %q", q.Kind)
}

func execPut(st *store.Store, q *Query, now int64) (map[string]any, error) {
	if len(q.Set) == 0 {
		return nil, fmt.Errorf("PUT needs SET field=value, ... — a fact with no values says nothing")
	}
	if strings.ContainsRune(q.Subject, '*') {
		return nil, fmt.Errorf("PUT writes one subject; wildcards are for reading")
	}
	facet := q.Facet
	if facet == "" {
		facet = "source"
	}
	prov := model.Provenance(q.Provenance)
	if prov == "" {
		prov = model.HumanEntry
	}
	conf := 1.0
	if q.Confidence != nil {
		conf = *q.Confidence
	}
	e := &model.Event{
		Subject: q.Subject, Facet: facet, Type: model.EventType(q.EvType),
		Value: q.Set, EffectiveTime: q.Effective,
		Provenance: prov, Confidence: conf,
		SourceSystem: "CEQL", SourceRef: q.Ref, SchemaID: q.SchemaID,
	}
	if err := st.Append(now, []*model.Event{e}, nil); err != nil {
		return nil, err
	}
	return map[string]any{"kind": "put", "event_id": e.EventID,
		"subject": e.Subject, "facet": e.Facet}, nil
}

// gatherEvents collects and WHERE-filters the candidate events for
// FACTS / HISTORY / PROFILE, honoring wildcards and time travel.
func gatherEvents(st *store.Store, q *Query) ([]*model.Event, error) {
	var events []*model.Event
	subjects := []string{q.Subject}
	if strings.ContainsRune(q.Subject, '*') {
		re := globRe(q.Subject)
		subjects = subjects[:0]
		for _, s := range st.Subjects() {
			if re.MatchString(s) {
				subjects = append(subjects, s)
			}
		}
	}
	for _, subj := range subjects {
		switch {
		case q.Kind == KHistory:
			events = append(events, st.History(subj, q.Facet)...)
		case q.AsOf > 0 || q.KnownAt > 0:
			at := q.AsOf
			if at == 0 {
				at = q.KnownAt // "AS KNOWN AT k" alone means: the truth at k, as known at k
			}
			events = append(events, st.AsOf(subj, q.Facet, at, q.KnownAt)...)
		default:
			events = append(events, st.Current(subj, q.Facet)...)
		}
	}
	if q.Where != nil {
		kept := events[:0]
		for _, e := range events {
			ok, err := evalExpr(q.Where, e)
			if err != nil {
				return nil, err
			}
			if ok {
				kept = append(kept, e)
			}
		}
		events = kept
	}
	return events, nil
}

// execRead handles FACTS and HISTORY: gather, filter, project/aggregate,
// order, paginate, and optionally attach causal chains.
func execRead(st *store.Store, q *Query) (map[string]any, error) {
	events, err := gatherEvents(st, q)
	if err != nil {
		return nil, err
	}

	// 3. Aggregate?
	if hasAgg(q.Fields) {
		return aggregate(q, events)
	}

	// 4. Order.
	if q.OrderBy != "" {
		key := q.OrderBy
		sort.SliceStable(events, func(i, j int) bool {
			if q.Desc {
				return lessVal(getField(events[j], key), getField(events[i], key))
			}
			return lessVal(getField(events[i], key), getField(events[j], key))
		})
	}

	// 5. Paginate.
	if q.Offset > 0 {
		if q.Offset >= len(events) {
			events = nil
		} else {
			events = events[q.Offset:]
		}
	}
	if q.Limit > 0 && len(events) > q.Limit {
		events = events[:q.Limit]
	}

	// 6. Project.
	out := map[string]any{"kind": "events"}
	if len(q.Fields) > 0 && !(len(q.Fields) == 1 && q.Fields[0].Name == "*") {
		cols := make([]string, len(q.Fields))
		for i, f := range q.Fields {
			cols[i] = f.Name
		}
		rows := make([][]any, len(events))
		for i, e := range events {
			row := make([]any, len(cols))
			for j, c := range cols {
				if strings.EqualFold(c, "rank") {
					// rank: position in the ordered result (1-based,
					// counting from the true top across OFFSET pages).
					row[j] = q.Offset + i + 1
					continue
				}
				row[j] = getField(e, c)
			}
			rows[i] = row
		}
		out = map[string]any{"kind": "rows", "columns": cols, "rows": rows}
	} else {
		out["events"] = events
	}

	// 7. WHY: attach causal chains keyed by event id.
	if q.Why {
		depth := q.Depth
		if depth <= 0 {
			depth = 3
		}
		// A fact's story runs in both directions: inbound edges are what
		// triggered it; outbound SUPERSEDES/CORRECTS edges are what it
		// replaced. WHY merges both so the chain is complete.
		why := map[string][]store.TraceNode{}
		for _, e := range events {
			var chain []store.TraceNode
			if tr := st.Trace(e.EventID, "cause", depth); len(tr) > 1 {
				chain = append(chain, tr[1:]...)
			}
			if tr := st.Trace(e.EventID, "effect", depth); len(tr) > 1 {
				chain = append(chain, tr[1:]...)
			}
			if len(chain) > 0 {
				why[e.EventID] = chain
			}
		}
		out["why"] = why
	}
	return out, nil
}

func hasAgg(fields []Field) bool {
	for _, f := range fields {
		if f.Agg != "" {
			return true
		}
	}
	return false
}

func aggregate(q *Query, events []*model.Event) (map[string]any, error) {
	groups := map[string][]*model.Event{}
	var keys []string
	keyOf := func(e *model.Event) string {
		if q.GroupBy == "" {
			return ""
		}
		return fmt.Sprint(getField(e, q.GroupBy))
	}
	for _, e := range events {
		k := keyOf(e)
		if _, ok := groups[k]; !ok {
			keys = append(keys, k)
		}
		groups[k] = append(groups[k], e)
	}
	sort.Strings(keys)

	var cols []string
	if q.GroupBy != "" {
		cols = append(cols, q.GroupBy)
	}
	for _, f := range q.Fields {
		if f.Agg == "" {
			if f.Name == q.GroupBy {
				continue // already the first column
			}
			return nil, fmt.Errorf("plain field %q next to aggregates needs GROUP BY %s", f.Name, f.Name)
		}
		cols = append(cols, f.Agg+"("+f.Name+")")
	}
	var rows [][]any
	for _, k := range keys {
		// HAVING: drop groups whose aggregates fail the conditions.
		keep := true
		for _, h := range q.Having {
			got, ok := toNum(aggValue(Field{Name: h.Field, Agg: h.Agg}, groups[k]))
			if !ok {
				keep = false
				break
			}
			switch h.Op {
			case "=":
				keep = got == h.Value
			case "!=":
				keep = got != h.Value
			case ">":
				keep = got > h.Value
			case ">=":
				keep = got >= h.Value
			case "<":
				keep = got < h.Value
			case "<=":
				keep = got <= h.Value
			}
			if !keep {
				break
			}
		}
		if !keep {
			continue
		}
		var row []any
		if q.GroupBy != "" {
			row = append(row, k)
		}
		for _, f := range q.Fields {
			if f.Agg == "" {
				continue
			}
			row = append(row, aggValue(f, groups[k]))
		}
		rows = append(rows, row)
	}
	return map[string]any{"kind": "rows", "columns": cols, "rows": rows}, nil
}

func aggValue(f Field, events []*model.Event) any {
	switch f.Agg {
	case "count":
		if f.Name == "*" {
			return len(events)
		}
		n := 0
		for _, e := range events {
			if getField(e, f.Name) != nil {
				n++
			}
		}
		return n
	case "listagg":
		seen := map[string]bool{}
		var vals []string
		for _, e := range events {
			if v := getField(e, f.Name); v != nil {
				s := fmt.Sprint(v)
				if !seen[s] {
					seen[s] = true
					vals = append(vals, s)
				}
			}
		}
		sort.Strings(vals)
		return strings.Join(vals, ", ")
	}
	var nums []float64
	for _, e := range events {
		if v, ok := toNum(getField(e, f.Name)); ok {
			nums = append(nums, v)
		}
	}
	if len(nums) == 0 {
		return nil
	}
	switch f.Agg {
	case "sum", "avg":
		s := 0.0
		for _, n := range nums {
			s += n
		}
		if f.Agg == "avg" {
			return s / float64(len(nums))
		}
		return s
	case "min":
		m := nums[0]
		for _, n := range nums[1:] {
			if n < m {
				m = n
			}
		}
		return m
	case "max":
		m := nums[0]
		for _, n := range nums[1:] {
			if n > m {
				m = n
			}
		}
		return m
	case "median":
		sort.Float64s(nums)
		mid := len(nums) / 2
		if len(nums)%2 == 1 {
			return nums[mid]
		}
		return (nums[mid-1] + nums[mid]) / 2
	case "stddev":
		mean := 0.0
		for _, n := range nums {
			mean += n
		}
		mean /= float64(len(nums))
		variance := 0.0
		for _, n := range nums {
			variance += (n - mean) * (n - mean)
		}
		return math.Sqrt(variance / float64(len(nums)))
	}
	return nil
}

// ---------------------------------------------------------------------
// field access & expression evaluation
// ---------------------------------------------------------------------

// getField resolves a name against an event: metadata first (subject,
// facet, type, provenance, trust/confidence, effective, recorded,
// event_id, pending), then the event's value map.
func getField(e *model.Event, name string) any {
	switch strings.ToLower(name) {
	case "subject":
		return e.Subject
	case "namespace":
		// The tenant/namespace is the subject's first segment:
		// "acme:order/42" -> "acme". Shared-schema multitenancy à la
		// PostgreSQL, but built into every query.
		if i := strings.IndexByte(e.Subject, ':'); i > 0 {
			return e.Subject[:i]
		}
		return e.Subject
	case "facet":
		return e.Facet
	case "type":
		return string(e.Type)
	case "provenance":
		return string(e.Provenance)
	case "trust", "confidence":
		return e.Confidence
	case "effective", "effective_time":
		return e.EffectiveTime
	case "recorded", "recorded_time":
		return e.RecordedTime
	case "event_id", "id":
		return e.EventID
	case "schema", "schema_id":
		return e.SchemaID
	case "ref", "source_ref":
		return e.SourceRef
	case "source", "source_system":
		return e.SourceSystem
	case "pending":
		return e.Type == model.Distributed && e.ActivationTime == 0
	case "superseded":
		return e.SupersededBy != ""
	}
	if e.Value == nil {
		return nil
	}
	return e.Value[name]
}

func evalExpr(x *Expr, e *model.Event) (bool, error) {
	switch x.Op {
	case "and":
		for _, k := range x.Kids {
			ok, err := evalExpr(k, e)
			if err != nil || !ok {
				return false, err
			}
		}
		return true, nil
	case "or":
		for _, k := range x.Kids {
			ok, err := evalExpr(k, e)
			if err != nil {
				return false, err
			}
			if ok {
				return true, nil
			}
		}
		return false, nil
	case "not":
		ok, err := evalExpr(x.Kids[0], e)
		return !ok, err
	case "in":
		got := getField(e, x.Field)
		for _, v := range x.Values {
			if looseEq(got, v) {
				return true, nil
			}
		}
		return false, nil
	case "like":
		got := fmt.Sprint(getField(e, x.Field))
		pat, _ := x.Value.(string)
		return globRe(pat).MatchString(got), nil
	case "matches":
		// Full-text-ish search, case-insensitive. Field "any" (or "*")
		// scans the subject and every text value on the fact.
		needle := strings.ToLower(fmt.Sprint(x.Value))
		f := strings.ToLower(x.Field)
		if f == "any" || f == "*" || f == "text" {
			if strings.Contains(strings.ToLower(e.Subject), needle) {
				return true, nil
			}
			for _, v := range e.Value {
				if s, ok := v.(string); ok && strings.Contains(strings.ToLower(s), needle) {
					return true, nil
				}
			}
			return false, nil
		}
		got, ok := getField(e, x.Field).(string)
		return ok && strings.Contains(strings.ToLower(got), needle), nil
	case "=", "!=", ">", ">=", "<", "<=":
		got := getField(e, x.Field)
		res := compare(got, x.Value)
		switch x.Op {
		case "=":
			return res == 0 && got != nil, nil
		case "!=":
			return res != 0 || got == nil, nil
		case ">":
			return res > 0, nil
		case ">=":
			return res >= 0, nil
		case "<":
			return res < 0 && got != nil, nil
		case "<=":
			return res <= 0 && got != nil, nil
		}
	}
	return false, fmt.Errorf("unknown operator %q", x.Op)
}

func looseEq(a, b any) bool { return compare(a, b) == 0 && a != nil }

// compare: numbers compare numerically (JSON gives float64, Go callers
// may store ints), everything else as strings. Returns -1/0/1; nil
// compares as less-than-everything.
func compare(a, b any) int {
	if a == nil {
		if b == nil {
			return 0
		}
		return -1
	}
	an, aok := toNum(a)
	bn, bok := toNum(b)
	if aok && bok {
		switch {
		case an < bn:
			return -1
		case an > bn:
			return 1
		}
		return 0
	}
	return strings.Compare(fmt.Sprint(a), fmt.Sprint(b))
}

func lessVal(a, b any) bool { return compare(a, b) < 0 }

func toNum(v any) (float64, bool) {
	switch n := v.(type) {
	case float64:
		return n, true
	case float32:
		return float64(n), true
	case int:
		return float64(n), true
	case int64:
		return float64(n), true
	case bool:
		if n {
			return 1, true
		}
		return 0, true
	}
	return 0, false
}

// globRe compiles a *-wildcard pattern into an anchored regexp.
func globRe(pat string) *regexp.Regexp {
	parts := strings.Split(pat, "*")
	for i, p := range parts {
		parts[i] = regexp.QuoteMeta(p)
	}
	return regexp.MustCompile("^" + strings.Join(parts, ".*") + "$")
}
