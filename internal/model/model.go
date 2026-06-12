// Package model defines Centauri's core primitives.
//
// Centauri's atom is not a row but an Event: a fact that knows when it
// became true (EffectiveTime), when the system learned it (RecordedTime),
// which facet of reality it belongs to, what caused it, where it came
// from, and how much it can be trusted. Events are immutable once
// appended; the only fields ever set after append are SupersededBy and
// EffectiveEnd, written exactly once by the supersession logic.
package model

import (
	"crypto/rand"
	"encoding/hex"
	"fmt"
	"time"
)

// Provenance classifies how a fact entered the system.
type Provenance string

const (
	SystemFeed   Provenance = "SYSTEM_FEED"
	ScanVerified Provenance = "SCAN_VERIFIED"
	HumanEntry   Provenance = "HUMAN_ENTRY"
	AIInferred   Provenance = "AI_INFERRED"
)

// EventType describes the lifecycle stage a fact represents.
type EventType string

const (
	Intent      EventType = "INTENT"      // source-of-truth declares a future fact (e.g. RMS price change)
	Distributed EventType = "DISTRIBUTED" // fact fanned out to a facet's pipeline
	Activated   EventType = "ACTIVATED"   // facet acted on the fact (register flip, PDT confirm)
	Observed    EventType = "OBSERVED"    // fact observed in the world (scan, audit)
	Correction  EventType = "CORRECTION"  // supersedes an erroneous prior event
)

// Event is the immutable atom of Centauri.
type Event struct {
	EventID string `json:"event_id"` // time-ordered unique id (UUIDv7-style)
	Subject string `json:"subject"`  // what the fact is about, e.g. "item:123456/store:4412"
	Facet   string `json:"facet"`    // which reality: "source", "register", "pdt", "shelf", "storecentral"
	Type    EventType `json:"type"`

	// Value is the domain payload. v0.1 keeps it as a generic map;
	// a schema registry hardens this in v0.2.
	Value    map[string]any `json:"value"`
	SchemaID string         `json:"schema_id,omitempty"`

	// Bi-temporal core. All times are UnixMicro.
	EffectiveTime  int64 `json:"effective_time"`            // when true in the world
	EffectiveEnd   int64 `json:"effective_end,omitempty"`   // 0 = open-ended; set by supersession
	RecordedTime   int64 `json:"recorded_time"`             // when Centauri ingested it (server clock)
	ActivationTime int64 `json:"activation_time,omitempty"` // 0 = pending. The wedge bit.

	SupersededBy string `json:"superseded_by,omitempty"`

	Provenance   Provenance `json:"provenance"`
	Confidence   float64    `json:"confidence"`
	SourceSystem string     `json:"source_system"`
	SourceRef    string     `json:"source_ref,omitempty"`
}

// LinkType classifies a causal edge.
type LinkType string

const (
	Triggered     LinkType = "TRIGGERED"      // intent -> distribution
	DistributedAs LinkType = "DISTRIBUTED_AS" // intent -> per-facet copy
	ActivatedBy   LinkType = "ACTIVATED_BY"   // distribution -> activation
	Supersedes    LinkType = "SUPERSEDES"
	Corrects      LinkType = "CORRECTS"
	EnrichedFrom  LinkType = "ENRICHED_FROM"
)

// CausalLink is a directed edge in the lineage graph: From caused To.
type CausalLink struct {
	From string   `json:"from"`
	To   string   `json:"to"`
	Type LinkType `json:"type"`
}

// Enrichment is an AI-written fact about an event. Re-enrichment with a
// newer model supersedes rather than overwrites — same append-only
// discipline as events, which is what makes the dataset retroactively
// improvable ("data that appreciates").
type Enrichment struct {
	EnrichmentID string         `json:"enrichment_id"`
	TargetEvent  string         `json:"target_event"`
	Kind         string         `json:"kind"` // e.g. "wedge_risk", "anomaly"
	ModelID      string         `json:"model_id"`
	ModelVersion string         `json:"model_version"`
	Result       map[string]any `json:"result"`
	Confidence   float64        `json:"confidence"`
	CreatedAt    int64          `json:"created_at"`
	SupersededBy string         `json:"superseded_by,omitempty"`
}

// FieldDef describes one field of a schema: enough for a machine — or a
// model — to understand the field without a human explaining it.
type FieldDef struct {
	Type        string   `json:"type"` // "number", "string", "bool", "any"
	Required    bool     `json:"required,omitempty"`
	Min         *float64 `json:"min,omitempty"` // numbers only
	Max         *float64 `json:"max,omitempty"` // numbers only
	Unit        string   `json:"unit,omitempty"` // e.g. "cents", "celsius"
	Description string   `json:"description,omitempty"`
}

// Schema is a versioned, append-only description of an event Value.
// Schemas live in the log like everything else: a new version supersedes
// the prior one, and old events keep validating against the version that
// was current when they were written.
type Schema struct {
	SchemaID     string              `json:"schema_id"`
	Version      int                 `json:"version"` // server-assigned, 1-based
	Title        string              `json:"title,omitempty"`
	Description  string              `json:"description,omitempty"`
	Fields       map[string]FieldDef `json:"fields"`
	CreatedAt    int64               `json:"created_at"`
	SupersededBy string              `json:"superseded_by,omitempty"` // "id@vN"
}

// Ref returns the canonical "id@vN" reference for this schema version.
func (s *Schema) Ref() string { return fmt.Sprintf("%s@v%d", s.SchemaID, s.Version) }

// EmbeddingKind is the enrichment kind whose Result carries a "vector"
// array. The store maintains a similarity index over the latest
// embedding per event.
const EmbeddingKind = "embedding"

// NewID returns a time-ordered unique identifier: a UUIDv7-style id whose
// leading bits are the current UnixMicro timestamp. Time-sortable ids are
// cheap insurance for future log shipping and replication.
func NewID() string {
	var r [8]byte
	_, _ = rand.Read(r[:])
	return fmt.Sprintf("%016x-%s", time.Now().UnixMicro(), hex.EncodeToString(r[:]))
}
