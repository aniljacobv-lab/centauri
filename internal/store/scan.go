package store

// Disk-backed read primitives: answer History / AsOf for a subject by reading
// ONLY the zone-map-pruned segments (plus the tail) — no full in-RAM index. This
// is the foundation of the "scales with disk, not RAM" path: a query touches its
// working set, not the whole history. The reader-based cores (…R) go through the
// cached archiveReader so repeat queries hit RAM; the public Scan* helpers wrap
// them with a one-shot reader. These mirror Store.History / Store.AsOf exactly
// (a test asserts equality). See docs/design-tablespaces.md.

import (
	"bytes"
	"encoding/json"
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

// scanSubjectEventsR collects every event for subject (optionally one facet) from
// the segments that survive prune(zones), plus the tail, using the cached reader.
// prune also reports how many segments were skipped vs scanned (data skipping).
func scanSubjectEventsR(a *archiveReader, subject, facet string, prune func(segment.Zones) bool) ([]*model.Event, error) {
	man, err := a.manifest()
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
		raw, err := a.segmentBytes(e)
		if err != nil {
			return nil, err
		}
		collect(raw)
	}
	if tb, err := a.tailBytes(); err == nil { // tail is never pruned
		collect(tb)
	}
	return out, nil
}

func historyR(a *archiveReader, subject, facet string) ([]*model.Event, error) {
	ns := nsOf(subject)
	evs, err := scanSubjectEventsR(a, subject, facet, func(z segment.Zones) bool {
		return z.MayContainNamespace(ns)
	})
	if err != nil {
		return nil, err
	}
	sort.SliceStable(evs, func(i, j int) bool { return evs[i].EffectiveTime < evs[j].EffectiveTime })
	return evs, nil
}

func asOfR(a *archiveReader, subject, facet string, effectiveAt, knownAt int64) ([]*model.Event, error) {
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
	evs, err := scanSubjectEventsR(a, subject, facet, prune)
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

// ScanHistory returns the full timeline for subject/facet, reading only segments
// in the subject's namespace + the tail. Mirrors Store.History.
func ScanHistory(dir, subject, facet string) ([]*model.Event, error) {
	return historyR(newArchiveReader(dir, 0), subject, facet)
}

// ScanAsOf answers the bi-temporal point query from disk, pruning segments by
// time + namespace zone maps. Mirrors Store.AsOf (same max-effective rule).
func ScanAsOf(dir, subject, facet string, effectiveAt, knownAt int64) ([]*model.Event, error) {
	return asOfR(newArchiveReader(dir, 0), subject, facet, effectiveAt, knownAt)
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
