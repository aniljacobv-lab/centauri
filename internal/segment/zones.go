package segment

import (
	"math"
	"sort"

	"github.com/proxima360/centauri/internal/model"
)

// Zone maps: cheap per-segment summary stats computed once at seal time. A query
// intersects its predicate with each segment's zones and SKIPS segments that
// provably can't match — Postgres BRIN / Apache Doris "data skipping", in one
// binary. Correctness is by construction: zones only ever EXCLUDE segments that
// cannot match; a query that can't be pruned simply scans (same result, slower).

const maxFieldValues = 64 // beyond this a string field is "capped" (can't prune by value)

// FieldStat summarizes one field's values across a segment.
type FieldStat struct {
	Numeric bool     `json:"numeric"`
	Min     float64  `json:"min"`
	Max     float64  `json:"max"`
	Values  []string `json:"values,omitempty"` // distinct low-cardinality strings
	Capped  bool     `json:"capped,omitempty"` // too many distinct values to track
}

// Zones is a segment's summary: time ranges, subject namespaces, and field stats.
type Zones struct {
	EffMin   int64                `json:"eff_min"`
	EffMax   int64                `json:"eff_max"`
	RecMin   int64                `json:"rec_min"`
	RecMax   int64                `json:"rec_max"`
	Subjects []string             `json:"subjects,omitempty"` // distinct namespaces
	Fields   map[string]FieldStat `json:"fields,omitempty"`
}

// namespaceOf returns the prefix of a subject before the first ':' or '/'.
func namespaceOf(s string) string {
	for i := 0; i < len(s); i++ {
		if s[i] == ':' || s[i] == '/' {
			return s[:i]
		}
	}
	return s
}

// ComputeZones builds the zone map for a segment's events.
func ComputeZones(events []*model.Event) Zones {
	z := Zones{Fields: map[string]FieldStat{}, EffMin: math.MaxInt64, RecMin: math.MaxInt64}
	nsSet := map[string]bool{}
	seen := false
	for _, e := range events {
		if e == nil {
			continue
		}
		seen = true
		if e.EffectiveTime < z.EffMin {
			z.EffMin = e.EffectiveTime
		}
		if e.EffectiveTime > z.EffMax {
			z.EffMax = e.EffectiveTime
		}
		if e.RecordedTime < z.RecMin {
			z.RecMin = e.RecordedTime
		}
		if e.RecordedTime > z.RecMax {
			z.RecMax = e.RecordedTime
		}
		nsSet[namespaceOf(e.Subject)] = true
		for k, v := range e.Value {
			fs := z.Fields[k]
			switch n := v.(type) {
			case float64:
				updateNum(&fs, n)
			case int:
				updateNum(&fs, float64(n))
			case int64:
				updateNum(&fs, float64(n))
			case string:
				updateStr(&fs, n)
			}
			z.Fields[k] = fs
		}
	}
	if !seen {
		z.EffMin, z.RecMin = 0, 0
	}
	for ns := range nsSet {
		z.Subjects = append(z.Subjects, ns)
	}
	sort.Strings(z.Subjects)
	if len(z.Fields) == 0 {
		z.Fields = nil
	}
	return z
}

func updateNum(fs *FieldStat, n float64) {
	if !fs.Numeric {
		fs.Numeric, fs.Min, fs.Max = true, n, n
		return
	}
	if n < fs.Min {
		fs.Min = n
	}
	if n > fs.Max {
		fs.Max = n
	}
}

func updateStr(fs *FieldStat, s string) {
	if fs.Capped {
		return
	}
	for _, v := range fs.Values {
		if v == s {
			return
		}
	}
	if len(fs.Values) >= maxFieldValues {
		fs.Capped, fs.Values = true, nil
		return
	}
	fs.Values = append(fs.Values, s)
}

// --- pruning predicates: each returns true if the segment MAY match (safe) ---

// MayContainEffectiveAt: a segment can only contribute to an AS OF t query if it
// holds at least one fact effective at or before t.
func (z Zones) MayContainEffectiveAt(t int64) bool { return z.EffMin <= t }

// MayContainKnownBy: facts recorded after knownAt weren't known yet; skip a
// segment whose earliest record is already after knownAt. knownAt<=0 = now.
func (z Zones) MayContainKnownBy(knownAt int64) bool {
	if knownAt <= 0 {
		return true
	}
	return z.RecMin <= knownAt
}

// MayContainNamespace: skip a segment that holds no subjects in this namespace.
func (z Zones) MayContainNamespace(ns string) bool {
	if ns == "" || ns == "*" || len(z.Subjects) == 0 {
		return true
	}
	for _, s := range z.Subjects {
		if s == ns {
			return true
		}
	}
	return false
}

// MayMatchNumber: prune by a numeric predicate (op is >,>=,<,<=,=) on a field.
func (z Zones) MayMatchNumber(field, op string, val float64) bool {
	fs, ok := z.Fields[field]
	if !ok || !fs.Numeric {
		return true // no stat — can't prune, so keep
	}
	switch op {
	case ">":
		return fs.Max > val
	case ">=":
		return fs.Max >= val
	case "<":
		return fs.Min < val
	case "<=":
		return fs.Min <= val
	case "=", "==":
		return val >= fs.Min && val <= fs.Max
	}
	return true
}

// MayMatchString: prune by string equality on a low-cardinality field.
func (z Zones) MayMatchString(field, val string) bool {
	fs, ok := z.Fields[field]
	if !ok || fs.Capped || len(fs.Values) == 0 {
		return true
	}
	for _, v := range fs.Values {
		if v == val {
			return true
		}
	}
	return false
}
