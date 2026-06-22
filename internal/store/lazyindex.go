package store

// LazyIndex is the disk-backed, RAM-scaling read path (design-tablespaces.md,
// approach A). Opening it streams an archive's segments + tail ONCE and keeps in
// RAM only the *current* fact per (subject, facet) — superseded and historical
// events are dropped as they're passed. So RAM scales with the number of live
// subjects, not the total number of events: you can query a database far larger
// than memory. Current is answered from the resident pointer; History and AsOf
// stream the zone-map-pruned segments from disk (ScanHistory / ScanAsOf).
//
// It is standalone (it does not change the in-RAM Store), so it carries no
// regression risk; wiring it behind `serve -data <archive>` is the next slice.

import (
	"bytes"
	"encoding/json"
	"os"
	"path/filepath"

	"github.com/proxima360/centauri/internal/model"
	"github.com/proxima360/centauri/internal/segment"
)

// LazyIndex answers reads over an archive while holding only the current facts.
type LazyIndex struct {
	dir    string
	open   map[string]*model.Event    // subject|facet -> current (non-superseded, beats-max) fact
	facets map[string]map[string]bool // subject -> facets (for Current with facet == "")
}

// OpenLazyIndex builds the current-pointer index by a single streaming pass over
// the archive — keeping only non-superseded facts, then resolving each
// (subject,facet) to its deterministic current fact (the same beats rule the
// in-RAM engine uses).
func OpenLazyIndex(dir string) (*LazyIndex, error) {
	li := &LazyIndex{dir: dir, open: map[string]*model.Event{}, facets: map[string]map[string]bool{}}
	live := map[string]map[string]*model.Event{} // key -> id -> event (non-superseded so far)
	idKey := map[string]string{}                 // event id -> key (to drop on supersession)

	process := func(data []byte) {
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
			if json.Unmarshal(t, &r) != nil {
				continue
			}
			switch {
			case r.Event != nil:
				e := r.Event
				if e.Type == model.Activated || e.SupersededBy != "" {
					continue
				}
				k := key(e.Subject, e.Facet)
				if live[k] == nil {
					live[k] = map[string]*model.Event{}
				}
				live[k][e.EventID] = e
				idKey[e.EventID] = k
				if li.facets[e.Subject] == nil {
					li.facets[e.Subject] = map[string]bool{}
				}
				li.facets[e.Subject][e.Facet] = true
			case r.Supersede != nil:
				id := r.Supersede.EventID
				if k, ok := idKey[id]; ok {
					delete(live[k], id)
					delete(idKey, id)
				}
			}
		}
	}

	mb, err := os.ReadFile(filepath.Join(dir, "manifest.json"))
	if err != nil {
		return nil, err
	}
	man, err := segment.ParseManifest(mb)
	if err != nil {
		return nil, err
	}
	for _, e := range man.Segments {
		raw, err := os.ReadFile(filepath.Join(dir, filepath.FromSlash(e.Path)))
		if err != nil {
			return nil, err
		}
		if e.Compressed {
			if raw, err = segment.Decompress(raw); err != nil {
				return nil, err
			}
		}
		process(raw)
	}
	if tb, err := os.ReadFile(archiveTailPath(dir, man)); err == nil {
		process(tb)
	}

	// Resolve each key to its current fact (deterministic max), then drop the
	// rest — only the current facts stay resident.
	for k, m := range live {
		var best *model.Event
		for _, e := range m {
			if beats(e, best) {
				best = e
			}
		}
		if best != nil {
			li.open[k] = best
		}
	}
	return li, nil
}

// Current returns the current fact(s) for a subject (one facet, or all when
// facet == "") — served from RAM, no disk read.
func (li *LazyIndex) Current(subject, facet string) []*model.Event {
	if facet != "" {
		if e := li.open[key(subject, facet)]; e != nil {
			return []*model.Event{e}
		}
		return nil
	}
	var out []*model.Event
	for f := range li.facets[subject] {
		if e := li.open[key(subject, f)]; e != nil {
			out = append(out, e)
		}
	}
	return out
}

// History / AsOf stream the pruned segments from disk (no full index needed).
func (li *LazyIndex) History(subject, facet string) ([]*model.Event, error) {
	return ScanHistory(li.dir, subject, facet)
}
func (li *LazyIndex) AsOf(subject, facet string, effectiveAt, knownAt int64) ([]*model.Event, error) {
	return ScanAsOf(li.dir, subject, facet, effectiveAt, knownAt)
}

// Keys reports the number of resident current-fact pointers — the in-RAM
// footprint, which scales with live subjects, not total events.
func (li *LazyIndex) Keys() int { return len(li.open) }
