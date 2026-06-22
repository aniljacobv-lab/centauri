package store

// Disk-scan causal trace for the lazy/archive path: walk the lineage of an event
// (its causes or its effects) by streaming the archive's Link records, without a
// resident causal index. A causal graph is inherently edge-sized, so this builds
// the adjacency (O(edges)) and materializes ONLY the events that are link
// endpoints (not the whole corpus) via a second pass — keeping it well below a
// full in-RAM replay. Mirrors Store.Trace's walk (same inbound/outbound edges,
// depth, and first-seen dedupe).

import (
	"bytes"
	"encoding/json"
	"os"
	"path/filepath"

	"github.com/proxima360/centauri/internal/model"
	"github.com/proxima360/centauri/internal/segment"
)

// forEachArchiveRecord streams every record in an archive (all segments + tail),
// calling fn once per parsed record.
func forEachArchiveRecord(dir string, fn func(r *record)) error {
	mb, err := os.ReadFile(filepath.Join(dir, "manifest.json"))
	if err != nil {
		return err
	}
	man, err := segment.ParseManifest(mb)
	if err != nil {
		return err
	}
	feed := func(data []byte) {
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
			if json.Unmarshal(t, &r) == nil {
				fn(&r)
			}
		}
	}
	for _, e := range man.Segments {
		raw, err := os.ReadFile(filepath.Join(dir, filepath.FromSlash(e.Path)))
		if err != nil {
			return err
		}
		if e.Compressed {
			if raw, err = segment.Decompress(raw); err != nil {
				return err
			}
		}
		feed(raw)
	}
	if tb, err := os.ReadFile(archiveTailPath(dir, man)); err == nil {
		feed(tb)
	}
	return nil
}

// ScanTrace walks the causal graph from eventID over an archive. direction is
// "cause" (inbound — what led to this) or "effect" (outbound — what this led to).
func ScanTrace(dir, eventID, direction string, maxDepth int) ([]TraceNode, error) {
	if maxDepth <= 0 {
		maxDepth = 16
	}
	effect := direction == "effect"

	causalIn := map[string][]model.CausalLink{}
	causalOut := map[string][]model.CausalLink{}
	linked := map[string]bool{}
	if err := forEachArchiveRecord(dir, func(r *record) {
		if r.Link == nil {
			return
		}
		l := *r.Link
		causalOut[l.From] = append(causalOut[l.From], l)
		causalIn[l.To] = append(causalIn[l.To], l)
		linked[l.From] = true
		linked[l.To] = true
	}); err != nil {
		return nil, err
	}

	// Second pass: materialize only the events that are link endpoints.
	nodeEvents := map[string]*model.Event{}
	if err := forEachArchiveRecord(dir, func(r *record) {
		if r.Event != nil && linked[r.Event.EventID] {
			nodeEvents[r.Event.EventID] = r.Event
		}
	}); err != nil {
		return nil, err
	}

	var out []TraceNode
	seen := map[string]bool{eventID: true}
	var walk func(id string, depth int)
	walk = func(id string, depth int) {
		if depth > maxDepth {
			return
		}
		edges := causalIn[id]
		if effect {
			edges = causalOut[id]
		}
		for _, l := range edges {
			nb := l.From
			if effect {
				nb = l.To
			}
			if seen[nb] {
				continue
			}
			seen[nb] = true
			if e, ok := nodeEvents[nb]; ok {
				out = append(out, TraceNode{Event: e, Link: l.Type, Depth: depth})
			}
			walk(nb, depth+1)
		}
	}
	walk(eventID, 1)
	return out, nil
}
