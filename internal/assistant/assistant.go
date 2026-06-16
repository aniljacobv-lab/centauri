// Package assistant gives Centauri a self-learning knowledge base. The
// knowledge lives in kb.json — the single source of truth, shared with the
// website assistant — and is seeded into the store as kb:<slug> facts
// (facet "knowledge"), so it's versioned and bi-temporal like everything
// else. The CeQL `ASK '<question>'` statement (internal/ceql/assistant.go)
// retrieves the best answer with BM25 and records misses as kb_gap:<slug>
// facts that an agent can later answer over MCP. To extend the assistant,
// just edit kb.json — no code changes.
package assistant

import (
	_ "embed"
	"encoding/json"
	"strings"

	"github.com/proxima360/centauri/internal/model"
	"github.com/proxima360/centauri/internal/store"
)

//go:embed kb.json
var kbJSON []byte

// Entry is one knowledge fact. JSON keys mirror docs/kb.json.
type Entry struct {
	Slug     string `json:"slug"`
	Question string `json:"q"`
	Tags     string `json:"t"`
	Answer   string `json:"a"`
}

var entries []Entry

// Entries returns the embedded knowledge base (parsed once).
func Entries() []Entry {
	if entries == nil {
		_ = json.Unmarshal(kbJSON, &entries)
	}
	return entries
}

// Seed appends the knowledge base into st as kb:<slug> facts.
func Seed(st *store.Store, now int64) (int, error) {
	var events []*model.Event
	for _, e := range Entries() {
		if e.Slug == "" {
			continue
		}
		events = append(events, &model.Event{
			Subject: "kb:" + e.Slug,
			Facet:   "knowledge",
			Type:    model.Observed,
			Value: map[string]any{
				"question": e.Question,
				"answer":   e.Answer,
				"tags":     e.Tags,
			},
			Provenance:   model.SystemFeed,
			Confidence:   1.0,
			SourceSystem: "ASSISTANT_KB",
		})
	}
	if err := st.Append(now, events, nil); err != nil {
		return 0, err
	}
	return len(events), nil
}

// SeedIfEmpty seeds the knowledge base when the kb:* count differs from the
// embedded set (reseeds on a new build; same-subject entries supersede
// their old versions, keeping history like everything else).
func SeedIfEmpty(st *store.Store, now int64) (int, error) {
	have := 0
	for _, s := range st.Subjects() {
		if strings.HasPrefix(s, "kb:") {
			have++
		}
	}
	if have == len(Entries()) {
		return have, nil
	}
	return Seed(st, now)
}
