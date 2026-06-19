package store

import (
	"fmt"
	"sort"

	"github.com/proxima360/centauri/internal/model"
)

// ContextBundle is everything a model needs to reason about a subject,
// assembled in one query: the facts (current, or as believed at a past
// moment — decision replay), their history, causal chains, open
// disagreements between facets with a suggested resolution, unactivated
// distributions (wedges), AI enrichments, and the schemas that make the
// values self-describing. The bundle IS the prompt context.
type ContextBundle struct {
	Subject   string `json:"subject"`
	AsKnownAt int64  `json:"as_known_at,omitempty"` // 0 = now

	Facts   []*model.Event `json:"facts"`             // one per facet
	History []*model.Event `json:"history,omitempty"` // most recent first

	Disagreements []FieldDisagreement              `json:"disagreements,omitempty"`
	Pending       []*model.Event                   `json:"pending,omitempty"` // distributed, unactivated (as known then)
	Enrichments   map[string][]*model.Enrichment   `json:"enrichments,omitempty"`
	Causes        map[string][]TraceNode           `json:"causes,omitempty"` // fact id -> inbound chain
	Schemas       []*model.Schema                  `json:"schemas,omitempty"`
	Confidence    ConfidenceSummary                `json:"confidence"`
}

// FieldDisagreement reports facets that disagree on one field, plus the
// claim the store would trust most (provenance rank, then confidence,
// then recency) — a suggestion, not a verdict.
type FieldDisagreement struct {
	Field    string       `json:"field"`
	Claims   []FieldClaim `json:"claims"`
	Resolved FieldClaim   `json:"resolved"`
}

// FieldClaim is one facet's claim about a field's value.
type FieldClaim struct {
	Facet      string           `json:"facet"`
	Value      any              `json:"value"`
	EventID    string           `json:"event_id"`
	Provenance model.Provenance `json:"provenance"`
	Confidence float64          `json:"confidence"`
	RecordedAt int64            `json:"recorded_at"`
}

// ConfidenceSummary aggregates the confidence of the bundle's facts.
type ConfidenceSummary struct {
	Min  float64 `json:"min"`
	Mean float64 `json:"mean"`
}

// provenanceRank orders how much each ingestion path is trusted when
// facets disagree: physical verification beats human entry beats feeds
// beats inference.
func provenanceRank(p model.Provenance) int {
	switch p {
	case model.ScanVerified:
		return 4
	case model.HumanEntry:
		return 3
	case model.SystemFeed:
		return 2
	case model.AIInferred:
		return 1
	}
	return 0
}

// Context assembles a ContextBundle for subject. knownAt==0 means "as of
// now"; a past knownAt replays the decision context — facts, wedges and
// disagreements exactly as the system believed them at that moment.
// historyLimit caps the timeline (0 = 20). minConfidence filters facts.
func (s *Store) Context(subject string, knownAt int64, historyLimit int, minConfidence float64) *ContextBundle {
	s.mu.RLock()
	defer s.mu.RUnlock()
	if historyLimit <= 0 {
		historyLimit = 20
	}

	b := &ContextBundle{
		Subject:     subject,
		AsKnownAt:   knownAt,
		Enrichments: map[string][]*model.Enrichment{},
		Causes:      map[string][]TraceNode{},
	}

	// Facts: current open events, or the bi-temporal belief at knownAt.
	var facts []*model.Event
	if knownAt > 0 {
		facts = s.asOfLocked(subject, "", knownAt, knownAt)
	} else {
		facts = s.currentLocked(subject)
	}
	facts = s.hydrateAll(facts) // *Locked now returns raw; values read below
	for _, e := range facts {
		if e.Confidence >= minConfidence {
			b.Facts = append(b.Facts, e)
		}
	}

	// Full timeline as known at knownAt (most recent first). Pending is
	// derived from the FULL list — wedges are old by nature and must not
	// fall off the history cap.
	var full []*model.Event
	for _, fc := range s.facetsFor(subject, "") {
		for _, id := range s.bySubjectFacet[key(subject, fc)] {
			e := s.events[id]
			if knownAt > 0 && e.RecordedTime > knownAt {
				continue
			}
			full = append(full, s.hydrate(e))
		}
	}
	sort.Slice(full, func(i, j int) bool { return full[i].EffectiveTime > full[j].EffectiveTime })

	// Disagreements across the bundle's facts, with a suggested winner.
	b.Disagreements = disagreementsAmong(b.Facts)

	// Pending wedges: distributions not yet activated — and, in replay
	// mode, not yet activated or superseded AS KNOWN AT knownAt. The
	// supersededAt index records when we LEARNED of each supersession.
	for _, e := range full {
		if e.Type != model.Distributed {
			continue
		}
		if knownAt > 0 {
			if e.ActivationTime != 0 && e.ActivationTime <= knownAt {
				continue // already activated by then
			}
			if note, ok := s.supersededAt[e.EventID]; ok && note.recordedTime <= knownAt {
				continue // already superseded by then
			}
		} else if e.ActivationTime != 0 || e.SupersededBy != "" {
			continue
		}
		b.Pending = append(b.Pending, e)
	}

	hist := full
	if len(hist) > historyLimit {
		hist = hist[:historyLimit]
	}
	b.History = hist

	// Enrichments and causal chains for each fact.
	for _, e := range b.Facts {
		if ens := s.enrichments[e.EventID]; len(ens) > 0 {
			b.Enrichments[e.EventID] = append([]*model.Enrichment{}, ens...)
		}
		if chain := s.causeChainLocked(e.EventID, 3); len(chain) > 0 {
			b.Causes[e.EventID] = chain
		}
	}

	// Schemas referenced by any fact, so values are self-describing.
	seenSchema := map[string]bool{}
	for _, e := range b.Facts {
		if e.SchemaID != "" && !seenSchema[e.SchemaID] {
			seenSchema[e.SchemaID] = true
			if sc := s.schemaLatestLocked(e.SchemaID); sc != nil {
				b.Schemas = append(b.Schemas, sc)
			}
		}
	}

	// Confidence summary.
	if len(b.Facts) > 0 {
		min, sum := 1.0, 0.0
		for _, e := range b.Facts {
			if e.Confidence < min {
				min = e.Confidence
			}
			sum += e.Confidence
		}
		b.Confidence = ConfidenceSummary{Min: min, Mean: sum / float64(len(b.Facts))}
	}
	return b
}

// disagreementsAmong finds fields where the given events disagree and
// ranks the claims.
func disagreementsAmong(events []*model.Event) []FieldDisagreement {
	byField := map[string][]FieldClaim{}
	for _, e := range events {
		for f, v := range e.Value {
			byField[f] = append(byField[f], FieldClaim{
				Facet: e.Facet, Value: v, EventID: e.EventID,
				Provenance: e.Provenance, Confidence: e.Confidence,
				RecordedAt: e.RecordedTime,
			})
		}
	}
	var fields []string
	for f := range byField {
		fields = append(fields, f)
	}
	sort.Strings(fields)

	var out []FieldDisagreement
	for _, f := range fields {
		claims := byField[f]
		vals := map[string]bool{}
		for _, c := range claims {
			vals[fmt.Sprint(c.Value)] = true
		}
		if len(vals) < 2 {
			continue
		}
		best := claims[0]
		for _, c := range claims[1:] {
			if rankClaim(c, best) {
				best = c
			}
		}
		out = append(out, FieldDisagreement{Field: f, Claims: claims, Resolved: best})
	}
	return out
}

// rankClaim reports whether a should be trusted over b.
func rankClaim(a, b FieldClaim) bool {
	if ra, rb := provenanceRank(a.Provenance), provenanceRank(b.Provenance); ra != rb {
		return ra > rb
	}
	if a.Confidence != b.Confidence {
		return a.Confidence > b.Confidence
	}
	return a.RecordedAt > b.RecordedAt
}

// causeChainLocked walks inbound causal edges. Caller holds s.mu.
func (s *Store) causeChainLocked(eventID string, maxDepth int) []TraceNode {
	var out []TraceNode
	seen := map[string]bool{eventID: true}
	var walk func(id string, depth int)
	walk = func(id string, depth int) {
		if depth > maxDepth {
			return
		}
		for _, l := range s.causalIn[id] {
			if seen[l.From] {
				continue
			}
			seen[l.From] = true
			if e, ok := s.events[l.From]; ok {
				out = append(out, TraceNode{Event: s.hydrate(e), Link: l.Type, Depth: depth})
			}
			walk(l.From, depth+1)
		}
	}
	walk(eventID, 1)
	return out
}
