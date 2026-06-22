package store

// Disk-backed read primitives: answer History / AsOf for a subject by reading
// ONLY the zone-map-pruned segments from disk (plus the tail) — no full in-RAM
// index. This is the foundation of the "scales with disk, not RAM" path: a query
// touches its working set, not the whole history. These mirror Store.History /
// Store.AsOf exactly (a test asserts equality), and are standalone (they don't
// change the live engine), so they're the safe building block to wire a lazy-
// index Store onto next. See docs/design-tablespaces.md.

import (
	"bytes"
	"encoding/json"
	"os"
	"path/filepath"
	"sort"

	"github.com/proxima360/centauri/internal/model"
	"github.com/proxima360/centauri/internal/segment"
)

func nsOf(s string) string {
	for i := 0; i < len(s); i++ {
		if s[i] == ':' || s[i] == '/' {
			return s[:i]
		}
	}
	return s
}

func archiveTailPath(dir string, man *segment.Manifest) string {
	t := man.Tail
	if t == "" {
		t = "current.log"
	}
	return filepath.Join(dir, t)
}

// scanSubjectEvents collects every event for subject (optionally one facet) from
// the segments that survive prune(zones), plus the tail. Non-event records
// (supersede markers, links) are ignored.
func scanSubjectEvents(dir, subject, facet string, prune func(segment.Zones) bool) ([]*model.Event, error) {
	mb, err := os.ReadFile(filepath.Join(dir, "manifest.json"))
	if err != nil {
		return nil, err
	}
	man, err := segment.ParseManifest(mb)
	if err != nil {
		return nil, err
	}
	var out []*model.Event
	collect := func(data []byte) {
		for len(data) > 0 {
			i := bytes.IndexByte(data, '\n')
			var ln []byte
			if i < 0 {
				ln, data = data, nil
			} else {
				ln, data = data[:i+1], data[i+1:]
			}
			t := bytes.TrimSpace(ln)
			if len(t) == 0 {
				continue
			}
			var r record
			if json.Unmarshal(t, &r) == nil && r.Event != nil &&
				r.Event.Subject == subject && (facet == "" || r.Event.Facet == facet) {
				out = append(out, r.Event)
			}
		}
	}
	for _, e := range man.Segments {
		if prune != nil && !prune(e.Zones) {
			continue // data skipping: this segment can't contribute
		}
		raw, err := os.ReadFile(filepath.Join(dir, filepath.FromSlash(e.Path)))
		if err != nil {
			return nil, err
		}
		if e.Compressed {
			if raw, err = segment.Decompress(raw); err != nil {
				return nil, err
			}
		}
		collect(raw)
	}
	if tb, err := os.ReadFile(archiveTailPath(dir, man)); err == nil { // tail is never pruned
		collect(tb)
	}
	return out, nil
}

// ScanHistory returns the full timeline for subject/facet, reading only segments
// in the subject's namespace + the tail. Mirrors Store.History.
func ScanHistory(dir, subject, facet string) ([]*model.Event, error) {
	ns := nsOf(subject)
	evs, err := scanSubjectEvents(dir, subject, facet, func(z segment.Zones) bool {
		return z.MayContainNamespace(ns)
	})
	if err != nil {
		return nil, err
	}
	sort.SliceStable(evs, func(i, j int) bool { return evs[i].EffectiveTime < evs[j].EffectiveTime })
	return evs, nil
}

// ScanAsOf answers the bi-temporal point query from disk, pruning segments by
// time + namespace zone maps. Mirrors Store.AsOf (same max-effective rule).
func ScanAsOf(dir, subject, facet string, effectiveAt, knownAt int64) ([]*model.Event, error) {
	ns := nsOf(subject)
	prune := func(z segment.Zones) bool {
		if effectiveAt > 0 && !z.MayContainEffectiveAt(effectiveAt) {
			return false
		}
		if !z.MayContainKnownBy(knownAt) {
			return false
		}
		return z.MayContainNamespace(ns)
	}
	evs, err := scanSubjectEvents(dir, subject, facet, prune)
	if err != nil {
		return nil, err
	}
	byFacet := map[string][]*model.Event{}
	for _, e := range evs {
		byFacet[e.Facet] = append(byFacet[e.Facet], e)
	}
	facets := make([]string, 0, len(byFacet))
	for f := range byFacet {
		facets = append(facets, f)
	}
	sort.Strings(facets)
	var out []*model.Event
	for _, f := range facets {
		if b := pickAsOf(byFacet[f], effectiveAt, knownAt); b != nil {
			out = append(out, b)
		}
	}
	return out, nil
}

// pickAsOf mirrors asOfLocked's selection: among non-lifecycle events recorded by
// knownAt and effective by effectiveAt, the one with the greatest effective time.
func pickAsOf(events []*model.Event, effectiveAt, knownAt int64) *model.Event {
	sort.SliceStable(events, func(i, j int) bool { return events[i].EffectiveTime < events[j].EffectiveTime })
	var best *model.Event
	for _, e := range events {
		if e.Type == model.Activated {
			continue
		}
		if knownAt > 0 && e.RecordedTime > knownAt {
			continue
		}
		if e.EffectiveTime > effectiveAt {
			continue
		}
		if best == nil || e.EffectiveTime >= best.EffectiveTime {
			best = e
		}
	}
	return best
}
