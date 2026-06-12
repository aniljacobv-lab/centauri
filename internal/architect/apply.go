package architect

import (
	"fmt"

	"github.com/proxima360/centauri/internal/model"
	"github.com/proxima360/centauri/internal/proc"
	"github.com/proxima360/centauri/internal/store"
)

// Apply builds the blueprint inside st: schemas, procedures, samples —
// and the genesis lineage itself, stored as ordinary facts so the
// database forever remembers why it is shaped the way it is.
func Apply(st *store.Store, bp *Blueprint, answers map[string]string, now int64) error {
	// 1. Schemas.
	for _, sc := range bp.Schemas {
		s := &model.Schema{SchemaID: sc.ID, Title: sc.Title,
			Fields: map[string]model.FieldDef{}}
		for _, f := range sc.Fields {
			s.Fields[f.Name] = model.FieldDef{Type: f.Type, Required: f.Required,
				Min: f.Min, Unit: f.Unit}
		}
		if err := st.PutSchema(now, s); err != nil {
			return fmt.Errorf("schema %s: %w", sc.ID, err)
		}
	}
	// 2. Procedures.
	for _, src := range bp.Procedures {
		if _, err := proc.Save(st, src, now); err != nil {
			return fmt.Errorf("procedure: %w", err)
		}
	}
	// 3. Samples (validated by the schemas they were generated for).
	var events []*model.Event
	for _, s := range bp.Samples {
		events = append(events, &model.Event{
			Subject: s.Subject, Facet: s.Facet, Type: model.Observed,
			Value: s.Value, SchemaID: s.SchemaID,
			Provenance: model.SystemFeed, Confidence: 1.0,
			SourceSystem: "GENESIS", SourceRef: "genesis:samples",
		})
	}
	// 4. Genesis lineage: requirement, answers, decisions — as facts.
	mk := func(subject string, value map[string]any) *model.Event {
		return &model.Event{
			Subject: subject, Facet: "genesis", Type: model.Observed,
			Value: value, Provenance: model.HumanEntry, Confidence: 1.0,
			SourceSystem: "GENESIS", SourceRef: "genesis:interview",
		}
	}
	events = append(events,
		mk("blueprint:requirement", map[string]any{
			"text": bp.Description, "domain": bp.Signals.Domain}),
		mk("blueprint:decisions", map[string]any{
			"schemas": len(bp.Schemas), "procedures": len(bp.Procedures),
			"watches": len(bp.Watches), "queries": len(bp.Queries),
			"detected_money": bp.Signals.HasMoney, "detected_lifecycle": bp.Signals.HasLifecycle,
			"detected_multi_source": bp.Signals.HasMultiSrc, "detected_tenancy": bp.Signals.HasTenancy}),
		mk("blueprint:guide", map[string]any{"text": bp.Guide}),
	)
	for id, a := range answers {
		events = append(events, mk("blueprint:answer/"+sanitize(id), map[string]any{
			"question": id, "answer": a}))
	}
	qv := make([]any, len(bp.Queries))
	for i, q := range bp.Queries {
		qv[i] = q
	}
	events = append(events, mk("blueprint:starter-queries", map[string]any{"queries": qv}))

	if err := st.Append(now, events, nil); err != nil {
		return fmt.Errorf("genesis facts: %w", err)
	}
	return nil
}

func sanitize(s string) string {
	out := make([]rune, 0, len(s))
	for _, r := range s {
		if (r >= 'a' && r <= 'z') || (r >= '0' && r <= '9') || r == '-' || r == '_' {
			out = append(out, r)
		} else {
			out = append(out, '-')
		}
	}
	return string(out)
}
