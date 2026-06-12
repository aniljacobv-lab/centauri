package ceql

import (
	"fmt"
	"sort"
	"strings"

	"github.com/proxima360/centauri/internal/store"
)

// FieldProfile summarizes one value field across the profiled facts.
type FieldProfile struct {
	Name     string         `json:"name"`
	Type     string         `json:"type"` // dominant type: number/string/bool/mixed
	Coverage float64        `json:"coverage"` // share of facts carrying the field, 0..1
	Distinct int            `json:"distinct"` // capped at 1000 ("1000+")
	Min      *float64       `json:"min,omitempty"`
	Max      *float64       `json:"max,omitempty"`
	Avg      *float64       `json:"avg,omitempty"`
	Top      []TopValue     `json:"top,omitempty"` // most common values (strings/bools)
}

// TopValue is one frequent value.
type TopValue struct {
	Value string `json:"value"`
	Count int    `json:"count"`
}

// execProfile answers "what does my data look like?": shape, coverage,
// ranges, and distributions over the matching facts — Centauri's data
// profiler, time-travel aware like every other read.
func execProfile(st *store.Store, q *Query) (map[string]any, error) {
	events, err := gatherEvents(st, q)
	if err != nil {
		return nil, err
	}

	subjects := map[string]bool{}
	facets := map[string]int{}
	namespaces := map[string]int{}
	type fstat struct {
		present  int
		numbers  []float64
		counts   map[string]int // value -> count (strings/bools)
		distinct map[string]bool
		types    map[string]int
	}
	fields := map[string]*fstat{}

	for _, e := range events {
		subjects[e.Subject] = true
		facets[e.Facet]++
		ns := e.Subject
		if i := strings.IndexByte(ns, ':'); i > 0 {
			ns = ns[:i]
		}
		namespaces[ns]++
		for k, v := range e.Value {
			f := fields[k]
			if f == nil {
				f = &fstat{counts: map[string]int{}, distinct: map[string]bool{}, types: map[string]int{}}
				fields[k] = f
			}
			f.present++
			if len(f.distinct) < 1000 {
				f.distinct[fmtCellStr(v)] = true
			}
			if n, ok := toNum(v); ok {
				if _, isBool := v.(bool); !isBool {
					f.types["number"]++
					f.numbers = append(f.numbers, n)
					continue
				}
			}
			switch v.(type) {
			case bool:
				f.types["bool"]++
				f.counts[fmtCellStr(v)]++
			case string:
				f.types["string"]++
				f.counts[fmtCellStr(v)]++
			default:
				f.types["other"]++
			}
		}
	}

	var profiles []FieldProfile
	for name, f := range fields {
		p := FieldProfile{Name: name, Distinct: len(f.distinct)}
		if len(events) > 0 {
			p.Coverage = float64(f.present) / float64(len(events))
		}
		// dominant type
		best, bestN, total := "", 0, 0
		for t, n := range f.types {
			total += n
			if n > bestN {
				best, bestN = t, n
			}
		}
		if bestN < total {
			p.Type = "mixed (" + best + ")"
		} else {
			p.Type = best
		}
		if len(f.numbers) > 0 {
			mn, mx, sum := f.numbers[0], f.numbers[0], 0.0
			for _, n := range f.numbers {
				if n < mn {
					mn = n
				}
				if n > mx {
					mx = n
				}
				sum += n
			}
			avg := sum / float64(len(f.numbers))
			p.Min, p.Max, p.Avg = &mn, &mx, &avg
		}
		if len(f.counts) > 0 {
			type kv struct {
				v string
				n int
			}
			var all []kv
			for v, n := range f.counts {
				all = append(all, kv{v, n})
			}
			sort.Slice(all, func(i, j int) bool {
				if all[i].n != all[j].n {
					return all[i].n > all[j].n
				}
				return all[i].v < all[j].v
			})
			for i := 0; i < len(all) && i < 5; i++ {
				p.Top = append(p.Top, TopValue{Value: all[i].v, Count: all[i].n})
			}
		}
		profiles = append(profiles, p)
	}
	sort.Slice(profiles, func(i, j int) bool { return profiles[i].Coverage > profiles[j].Coverage ||
		(profiles[i].Coverage == profiles[j].Coverage && profiles[i].Name < profiles[j].Name) })

	return map[string]any{
		"kind":       "profile",
		"pattern":    q.Subject,
		"events":     len(events),
		"subjects":   len(subjects),
		"facets":     facets,
		"namespaces": namespaces,
		"fields":     profiles,
	}, nil
}

func fmtCellStr(v any) string {
	switch t := v.(type) {
	case string:
		return t
	case bool:
		if t {
			return "true"
		}
		return "false"
	}
	return fmt.Sprint(v)
}
