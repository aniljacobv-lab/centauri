package ceql

import (
	"fmt"
	"sort"

	"github.com/proxima360/centauri/internal/store"
)

// execMatch runs a causal pattern query over the WHY graph: for every event
// whose subject matches the "from" pattern, it walks causal links (outbound
// for CAUSES, inbound for CAUSED BY), optionally filtered to one link type,
// and reports the events it reaches whose subject matches the "to" pattern.
// This is the set-level companion to WHY/EFFECTS (which start from one known
// event): "which intents triggered a register flip?", "what corrected this
// item?" — questions a row store simply can't express.
func execMatch(st *store.Store, q *Query) (map[string]any, error) {
	if q.MatchTo == "" {
		return nil, fmt.Errorf("MATCH needs a target pattern, e.g. MATCH item:* CAUSES register:*")
	}
	depth := q.Depth
	if depth <= 0 {
		depth = 3
	}
	toRe := globRe(q.MatchTo)
	rows := []map[string]any{}
	seen := map[string]bool{}
	for _, subj := range matchSubjects(st, q.Subject) {
		if isBookkeeping(subj) {
			continue
		}
		for _, fe := range st.History(subj, "") {
			for _, n := range st.TraceVia(fe.EventID, q.Dir, depth, q.Via) {
				if n.Depth == 0 || n.Event == nil {
					continue // skip the origin event itself
				}
				if !toRe.MatchString(n.Event.Subject) {
					continue
				}
				key := fe.EventID + ">" + n.Event.EventID
				if seen[key] {
					continue
				}
				seen[key] = true
				rows = append(rows, map[string]any{
					"from": fe.Subject, "from_event": fe.EventID,
					"to": n.Event.Subject, "to_event": n.Event.EventID,
					"hops": n.Depth, "via": string(n.Link),
				})
			}
		}
	}
	sort.Slice(rows, func(i, j int) bool {
		if rows[i]["from"] != rows[j]["from"] {
			return rows[i]["from"].(string) < rows[j]["from"].(string)
		}
		if rows[i]["to"] != rows[j]["to"] {
			return rows[i]["to"].(string) < rows[j]["to"].(string)
		}
		return rows[i]["hops"].(int) < rows[j]["hops"].(int)
	})
	if q.Limit > 0 && len(rows) > q.Limit {
		rows = rows[:q.Limit]
	}
	verb := "causes"
	if q.Dir == "cause" {
		verb = "caused by"
	}
	return map[string]any{
		"kind": "match", "rows": rows, "count": len(rows),
		"note": fmt.Sprintf("%d causal path(s): %s %s %s", len(rows), q.Subject, verb, q.MatchTo),
	}, nil
}
