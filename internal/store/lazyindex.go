package store

// LazyIndex is the disk-backed, RAM-scaling read path (design-tablespaces.md,
// approach A). Opening it streams an archive's segments + tail and keeps in RAM
// only the *current* fact per (subject, facet) — superseded and historical
// events are dropped as they're passed. So RAM scales with the number of live
// subjects, not the total number of events: you can query a database far larger
// than memory. Current is answered from the resident pointer; History and AsOf
// stream the zone-map-pruned segments from disk (ScanHistory / ScanAsOf).
//
// A pointer-checkpoint (lazy.ckpt) makes restart O(tail) instead of O(total):
// it persists the non-superseded facts and the set of (immutable) segments
// already folded in, so reopen seeds from it and replays only segments added
// since + the always-fresh tail. Re-applying the tail over the checkpoint is
// idempotent (events key by id; supersede markers re-delete), so the result is
// identical to a full rebuild — a test asserts this.
//
// It is standalone (it does not change the in-RAM Store), so it carries no
// regression risk; `serve -lazy-index` mounts it via api.LazyRoutes.

import (
	"bytes"
	"encoding/json"
	"os"
	"path/filepath"

	"github.com/proxima360/centauri/internal/model"
	"github.com/proxima360/centauri/internal/segment"
)

const lazyCheckpointName = "lazy.ckpt"

// LazyIndex answers reads over an archive while holding only the current facts.
type LazyIndex struct {
	dir     string
	open    map[string]*model.Event             // subject|facet -> current (beats-max) fact
	facets  map[string]map[string]bool          // subject -> facets that have a current fact
	live    map[string]map[string]*model.Event  // key -> id -> non-superseded event (for checkpoint + tail merge)
	idKey   map[string]string                   // event id -> key (to drop on supersession)
	covered map[string]string                   // segment path -> merkle root already folded into live
}

// lazyCheckpoint is the on-disk pointer-checkpoint. Covered maps each folded
// segment path to its Merkle root, so a reused path whose content changed (e.g.
// a re-archived log) invalidates the checkpoint instead of seeding stale state.
type lazyCheckpoint struct {
	Covered map[string]string         `json:"covered"`
	Live    map[string][]*model.Event `json:"live"`
}

// applyRecordsLazy folds one buffer of log records into the live sets: keep
// non-superseded, non-lifecycle events (by id), and drop on supersede markers.
// Idempotent w.r.t. re-applying the same bytes.
func applyRecordsLazy(data []byte, live map[string]map[string]*model.Event, idKey map[string]string) {
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
		case r.Supersede != nil:
			id := r.Supersede.EventID
			if k, ok := idKey[id]; ok {
				delete(live[k], id)
				delete(idKey, id)
			}
		}
	}
}

// OpenLazyIndex builds (or restores) the current-pointer index. It seeds from a
// pointer-checkpoint when present and still consistent with the manifest, then
// replays only the segments not yet folded in plus the tail.
func OpenLazyIndex(dir string) (*LazyIndex, error) {
	mb, err := os.ReadFile(filepath.Join(dir, "manifest.json"))
	if err != nil {
		return nil, err
	}
	man, err := segment.ParseManifest(mb)
	if err != nil {
		return nil, err
	}

	li := &LazyIndex{
		dir:     dir,
		live:    map[string]map[string]*model.Event{},
		idKey:   map[string]string{},
		covered: map[string]string{},
	}

	// Current segment identities (path -> Merkle root) for checkpoint validation.
	segRoot := map[string]string{}
	for _, e := range man.Segments {
		segRoot[e.Path] = e.MerkleRoot
	}

	// Seed from the checkpoint only if every segment it folded in still exists in
	// the manifest with the SAME content (matching Merkle root). Otherwise (a
	// segment was compacted away or a path was rewritten) fall back to a full
	// rebuild.
	if ck, _ := loadLazyCheckpoint(dir); ck != nil {
		stale := false
		for p, root := range ck.Covered {
			if segRoot[p] != root {
				stale = true
				break
			}
		}
		if !stale {
			for k, evs := range ck.Live {
				m := map[string]*model.Event{}
				for _, e := range evs {
					m[e.EventID] = e
					li.idKey[e.EventID] = k
				}
				li.live[k] = m
			}
			for p, root := range ck.Covered {
				li.covered[p] = root
			}
		}
	}

	// Fold in any segments not already covered (immutable, so safe to skip once folded).
	for _, e := range man.Segments {
		if _, ok := li.covered[e.Path]; ok {
			continue
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
		applyRecordsLazy(raw, li.live, li.idKey)
		li.covered[e.Path] = e.MerkleRoot
	}

	// The tail is mutable/appendable, so it is never covered — always replayed.
	if tb, err := os.ReadFile(archiveTailPath(dir, man)); err == nil {
		applyRecordsLazy(tb, li.live, li.idKey)
	}

	li.resolve()
	return li, nil
}

// resolve recomputes the current fact per key (deterministic beats-max) and the
// per-subject facet set from the live sets.
func (li *LazyIndex) resolve() {
	li.open = map[string]*model.Event{}
	li.facets = map[string]map[string]bool{}
	for k, m := range li.live {
		var best *model.Event
		for _, e := range m {
			if beats(e, best) {
				best = e
			}
		}
		if best != nil {
			li.open[k] = best
			if li.facets[best.Subject] == nil {
				li.facets[best.Subject] = map[string]bool{}
			}
			li.facets[best.Subject][best.Facet] = true
		}
	}
}

// SaveCheckpoint atomically persists the current pointer state so the next open
// replays only the tail (+ any segments sealed since). Tail records are not
// covered, so they are always replayed — re-applying them is idempotent.
func (li *LazyIndex) SaveCheckpoint() error {
	ck := lazyCheckpoint{Covered: map[string]string{}, Live: map[string][]*model.Event{}}
	for p, root := range li.covered {
		ck.Covered[p] = root
	}
	for k, m := range li.live {
		for _, e := range m {
			ck.Live[k] = append(ck.Live[k], e)
		}
	}
	b, err := json.Marshal(ck)
	if err != nil {
		return err
	}
	tmp := filepath.Join(li.dir, lazyCheckpointName+".tmp")
	if err := writeFileSync(tmp, b); err != nil {
		return err
	}
	return os.Rename(tmp, filepath.Join(li.dir, lazyCheckpointName))
}

func loadLazyCheckpoint(dir string) (*lazyCheckpoint, error) {
	b, err := os.ReadFile(filepath.Join(dir, lazyCheckpointName))
	if err != nil {
		return nil, err
	}
	var ck lazyCheckpoint
	if err := json.Unmarshal(b, &ck); err != nil {
		return nil, err
	}
	return &ck, nil
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

// Search ranks the resident current facts with keyword BM25 — served from RAM,
// no disk read. (Cold-segment search has no vector/causal signals; this is the
// keyword surface.)
func (li *LazyIndex) Search(query string, limit int) []SearchHit {
	events := make([]*model.Event, 0, len(li.open))
	for _, e := range li.open {
		events = append(events, e)
	}
	return rankEventsBM25(events, query, limit)
}

// Trace walks the causal lineage of an event ("cause" or "effect") by scanning
// Link records from disk.
func (li *LazyIndex) Trace(eventID, direction string, maxDepth int) ([]TraceNode, error) {
	return ScanTrace(li.dir, eventID, direction, maxDepth)
}

// Keys reports the number of resident current-fact pointers — the in-RAM
// footprint, which scales with live subjects, not total events.
func (li *LazyIndex) Keys() int { return len(li.open) }
