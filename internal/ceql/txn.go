package ceql

import (
	"fmt"
	"sort"
	"strings"

	"github.com/proxima360/centauri/internal/model"
	"github.com/proxima360/centauri/internal/store"
)

// Transactions: reversible, time-travel commits.
//
// Centauri never erases, so "rollback" cannot mean deletion. Instead it
// appends superseding *reversion* facts that restore each subject to the
// value it held at a chosen past point — and records the rollback itself
// as an auditable marker event. The consequences:
//
//   - you can rewind to ANY past commit, not just undo the last one;
//   - the revert is fully auditable (who/when/what) and itself reversible;
//   - AS OF / AS KNOWN AT still see the pre-rollback world unchanged.
//
// SNAPSHOT names a point; ROLLBACK returns to one (default: the last
// commit); DIFF shows what changed between two points so you can preview.

// execSnapshot records a named marker at the current commit so a later
// ROLLBACK TO SNAPSHOT 'name' can find it.
func execSnapshot(st *store.Store, q *Query, now int64) (map[string]any, error) {
	name := strings.TrimSpace(q.Name)
	if name == "" {
		return nil, fmt.Errorf("SNAPSHOT needs a name, e.g. SNAPSHOT 'before-import'")
	}
	e := &model.Event{
		Subject: "snapshot:" + name, Facet: "meta", Type: model.Observed,
		Value:      map[string]any{"at": now, "name": name},
		Provenance: model.SystemFeed, Confidence: 1.0, SourceSystem: "CEQL",
	}
	if err := st.Append(now, []*model.Event{e}, nil); err != nil {
		return nil, err
	}
	return map[string]any{
		"kind": "snapshot", "name": name, "at": now, "event_id": e.EventID,
		"note": fmt.Sprintf("snapshot %q taken — ROLLBACK TO SNAPSHOT '%s' to return here", name, name),
	}, nil
}

// execRollback restores matching subjects to a past point by appending
// reversion facts. Nothing is erased; the rollback is itself a fact.
func execRollback(st *store.Store, q *Query, now int64) (map[string]any, error) {
	at, target, err := resolveRollbackTarget(st, q)
	if err != nil {
		return nil, err
	}
	if at == 0 {
		return map[string]any{"kind": "rollback", "reverted": 0, "note": "nothing to roll back"}, nil
	}

	var revs []*model.Event
	changes := []map[string]any{}
	for _, subj := range matchSubjects(st, q.Subject) {
		if isBookkeeping(subj) {
			continue // never roll back snapshot/rollback markers themselves
		}
		past := byFacet(st.AsOf(subj, "", at, at)) // state as known at the target
		curr := byFacet(st.Current(subj, ""))      // state now
		for _, f := range facetUnion(past, curr) {
			pe, ce := past[f], curr[f]
			switch {
			case pe != nil && ce != nil:
				if !sameValues(pe.Value, ce.Value) {
					revs = append(revs, reversionEvent(subj, f, pe.Value))
					changes = append(changes, change(subj, f, "restored"))
				}
			case pe != nil && ce == nil:
				revs = append(revs, reversionEvent(subj, f, pe.Value))
				changes = append(changes, change(subj, f, "restored"))
			case pe == nil && ce != nil:
				if !isRetired(ce.Value) {
					revs = append(revs, reversionEvent(subj, f, map[string]any{"retired": true}))
					changes = append(changes, change(subj, f, "retired"))
				}
			}
		}
	}

	if len(revs) == 0 {
		return map[string]any{
			"kind": "rollback", "to": at, "target": target, "reverted": 0,
			"note": "already at that state — nothing to change",
		}, nil
	}

	// The rollback marker makes the revert auditable and links to every
	// reversion it produced, so WHY can trace a restored fact to it.
	marker := &model.Event{
		EventID: model.NewID(),
		Subject: fmt.Sprintf("rollback:%d", now), Facet: "meta", Type: model.Observed,
		Value: map[string]any{"to": at, "target": target, "count": len(revs),
			"pattern": q.Subject},
		Provenance: model.SystemFeed, Confidence: 1.0, SourceSystem: "CEQL",
	}
	links := make([]model.CausalLink, 0, len(revs))
	for _, r := range revs {
		links = append(links, model.CausalLink{From: marker.EventID, To: r.EventID, Type: model.Triggered})
	}
	batch := append([]*model.Event{marker}, revs...)
	if err := st.Append(now, batch, links); err != nil {
		return nil, err
	}
	return map[string]any{
		"kind": "rollback", "to": at, "target": target,
		"reverted": len(revs), "marker": marker.EventID, "changes": changes,
		"note": fmt.Sprintf("restored %d fact(s) to %s — history kept; this rollback is itself reversible", len(revs), target),
	}, nil
}

// resolveRollbackTarget turns the ROLLBACK target into a (knownAt) time.
func resolveRollbackTarget(st *store.Store, q *Query) (int64, string, error) {
	switch {
	case q.Name != "":
		cur := st.Current("snapshot:"+q.Name, "meta")
		if len(cur) == 0 {
			return 0, "", fmt.Errorf("no snapshot named %q — take one with SNAPSHOT '%s'", q.Name, q.Name)
		}
		f, ok := toNum(cur[0].Value["at"])
		if !ok {
			return 0, "", fmt.Errorf("snapshot %q is missing its timestamp", q.Name)
		}
		return int64(f), fmt.Sprintf("snapshot '%s'", q.Name), nil
	case q.AsOf > 0:
		return q.AsOf, "a point in time", nil
	default: // LAST
		last := st.MaxRecordedTime()
		if last == 0 {
			return 0, "the last commit", nil
		}
		// State as known just before the most recent commit.
		return last - 1, "the last commit", nil
	}
}

// execDiff reports what changed for matching subjects between two points
// in valid-time. Read-only.
func execDiff(st *store.Store, q *Query) (map[string]any, error) {
	if q.From == 0 || q.To == 0 {
		return nil, fmt.Errorf("DIFF needs two times: DIFF OF item:* BETWEEN <t1> AND <t2>")
	}
	rows := []map[string]any{}
	for _, subj := range matchSubjects(st, q.Subject) {
		if isBookkeeping(subj) {
			continue
		}
		a := byFacet(st.AsOf(subj, q.Facet, q.From, q.From))
		b := byFacet(st.AsOf(subj, q.Facet, q.To, q.To))
		for _, f := range facetUnion(a, b) {
			ae, be := a[f], b[f]
			switch {
			case ae == nil && be != nil:
				rows = append(rows, diffRow(subj, f, "added", nil, be.Value))
			case ae != nil && be == nil:
				rows = append(rows, diffRow(subj, f, "removed", ae.Value, nil))
			case ae != nil && be != nil && !sameValues(ae.Value, be.Value):
				rows = append(rows, diffRow(subj, f, "changed", ae.Value, be.Value))
			}
		}
	}
	sort.Slice(rows, func(i, j int) bool {
		if rows[i]["subject"] != rows[j]["subject"] {
			return rows[i]["subject"].(string) < rows[j]["subject"].(string)
		}
		return rows[i]["facet"].(string) < rows[j]["facet"].(string)
	})
	return map[string]any{"kind": "diff", "from": q.From, "to": q.To,
		"changes": rows, "count": len(rows)}, nil
}

// --- helpers -------------------------------------------------------------

func matchSubjects(st *store.Store, pattern string) []string {
	if !strings.ContainsRune(pattern, '*') {
		return []string{pattern}
	}
	re := globRe(pattern)
	var out []string
	for _, s := range st.Subjects() {
		if re.MatchString(s) {
			out = append(out, s)
		}
	}
	return out
}

func isBookkeeping(subj string) bool {
	return strings.HasPrefix(subj, "snapshot:") || strings.HasPrefix(subj, "rollback:")
}

func byFacet(events []*model.Event) map[string]*model.Event {
	m := map[string]*model.Event{}
	for _, e := range events {
		m[e.Facet] = e
	}
	return m
}

func facetUnion(a, b map[string]*model.Event) []string {
	set := map[string]bool{}
	for f := range a {
		set[f] = true
	}
	for f := range b {
		set[f] = true
	}
	out := make([]string, 0, len(set))
	for f := range set {
		out = append(out, f)
	}
	sort.Strings(out)
	return out
}

// reversionEvent is a Correction that re-asserts an old value, superseding
// the current fact while keeping the whole history intact.
func reversionEvent(subject, facet string, val map[string]any) *model.Event {
	cp := make(map[string]any, len(val))
	for k, v := range val {
		cp[k] = v
	}
	return &model.Event{
		EventID: model.NewID(),
		Subject: subject, Facet: facet, Type: model.Correction,
		Value: cp, Provenance: model.SystemFeed, Confidence: 1.0,
		SourceSystem: "CEQL", SourceRef: "rollback",
	}
}

// sameValues reports whether two fact values are equal field-by-field.
func sameValues(a, b map[string]any) bool {
	if len(a) != len(b) {
		return false
	}
	for k, av := range a {
		bv, ok := b[k]
		if !ok || !looseEq(av, bv) {
			return false
		}
	}
	return true
}

func isRetired(v map[string]any) bool {
	if v == nil {
		return false
	}
	r, ok := v["retired"]
	return ok && (r == true || r == "true")
}

func change(subject, facet, action string) map[string]any {
	return map[string]any{"subject": subject, "facet": facet, "action": action}
}

func diffRow(subject, facet, kind string, before, after map[string]any) map[string]any {
	return map[string]any{"subject": subject, "facet": facet,
		"change": kind, "before": before, "after": after}
}
