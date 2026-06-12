package store

import (
	"encoding/json"
	"errors"
	"fmt"

	"github.com/proxima360/centauri/internal/model"
)

var validFieldTypes = map[string]bool{"number": true, "string": true, "bool": true, "any": true}

// PutSchema appends a new version of a schema. Versions are
// server-assigned and append-only: a new version supersedes the prior
// one (derived in apply, so it survives replay), and events already
// validated against old versions are untouched — the registry has the
// same bi-temporal discipline as the facts it describes.
func (s *Store) PutSchema(now int64, sc *model.Schema) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if err := s.writable(); err != nil {
		return err
	}
	if sc == nil {
		return errors.New("schema: nil")
	}
	if sc.SchemaID == "" {
		return errors.New("schema: schema_id is required")
	}
	if len(sc.Fields) == 0 {
		return errors.New("schema: at least one field is required")
	}
	for name, def := range sc.Fields {
		if name == "" {
			return errors.New("schema: empty field name")
		}
		if !validFieldTypes[def.Type] {
			return fmt.Errorf("schema: field %q has unknown type %q (number|string|bool|any)", name, def.Type)
		}
		if (def.Min != nil || def.Max != nil) && def.Type != "number" {
			return fmt.Errorf("schema: field %q: min/max only apply to number fields", name)
		}
		if def.Min != nil && def.Max != nil && *def.Min > *def.Max {
			return fmt.Errorf("schema: field %q: min > max", name)
		}
	}
	// Server-managed fields.
	sc.Version = len(s.schemas[sc.SchemaID]) + 1
	sc.CreatedAt = now
	sc.SupersededBy = ""
	return s.commit([]*record{{Schema: sc}})
}

// SchemaLatest returns the current version of a schema, or nil.
func (s *Store) SchemaLatest(id string) *model.Schema {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return s.schemaLatestLocked(id)
}

func (s *Store) schemaLatestLocked(id string) *model.Schema {
	versions := s.schemas[id]
	if len(versions) == 0 {
		return nil
	}
	return versions[len(versions)-1]
}

// SchemaVersions returns every version of a schema, ascending.
func (s *Store) SchemaVersions(id string) []*model.Schema {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return append([]*model.Schema{}, s.schemas[id]...)
}

// Schemas returns the latest version of every registered schema.
func (s *Store) Schemas() []*model.Schema {
	s.mu.RLock()
	defer s.mu.RUnlock()
	out := []*model.Schema{}
	for id := range s.schemas {
		out = append(out, s.schemaLatestLocked(id))
	}
	return out
}

// validateAgainstSchema checks an event's Value against the latest
// version of its declared schema. Caller holds s.mu.
func (s *Store) validateAgainstSchema(e *model.Event) error {
	sc := s.schemaLatestLocked(e.SchemaID)
	if sc == nil {
		return fmt.Errorf("unknown schema %q", e.SchemaID)
	}
	for name, def := range sc.Fields {
		v, present := e.Value[name]
		if !present {
			if def.Required {
				return fmt.Errorf("schema %s: required field %q missing", sc.Ref(), name)
			}
			continue
		}
		switch def.Type {
		case "number":
			n, ok := toFloat(v)
			if !ok {
				return fmt.Errorf("schema %s: field %q must be a number, got %T", sc.Ref(), name, v)
			}
			if def.Min != nil && n < *def.Min {
				return fmt.Errorf("schema %s: field %q = %v below min %v", sc.Ref(), name, n, *def.Min)
			}
			if def.Max != nil && n > *def.Max {
				return fmt.Errorf("schema %s: field %q = %v above max %v", sc.Ref(), name, n, *def.Max)
			}
		case "string":
			if _, ok := v.(string); !ok {
				return fmt.Errorf("schema %s: field %q must be a string, got %T", sc.Ref(), name, v)
			}
		case "bool":
			if _, ok := v.(bool); !ok {
				return fmt.Errorf("schema %s: field %q must be a bool, got %T", sc.Ref(), name, v)
			}
		case "any":
			// anything goes
		}
	}
	return nil
}

// toFloat accepts the numeric types JSON decoding and Go callers produce.
func toFloat(v any) (float64, bool) {
	switch n := v.(type) {
	case float64:
		return n, true
	case float32:
		return float64(n), true
	case int:
		return float64(n), true
	case int32:
		return float64(n), true
	case int64:
		return float64(n), true
	case json.Number:
		f, err := n.Float64()
		return f, err == nil
	}
	return 0, false
}
