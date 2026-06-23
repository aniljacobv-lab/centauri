package store

// Disk-scan causal trace for the lazy/archive path: walk the lineage of an event
// (its causes or its effects) by streaming the archive's Link records, without a
// resident causal index. A causal graph is inherently edge-sized, so this builds
// the adjacency (O(edges)) and materializes ONLY the events that are link
// endpoints (not the whole corpus) via a second pass — keeping it well below a
// full in-RAM replay. Mirrors Store.Trace's walk (same inbound/outbound edges,
// depth, and first-seen dedupe). Reads go through the cached archiveReader.

import (
	"bytes"
	"encoding/json"

	"github.com/proxima360/centauri/internal/model"
)

// forEachArchiveRecordR streams every record in an archive (all segments + tail)
// via the cached reader, calling fn once per parsed record.
func forEachArchiveRecordR(a *archiveReader, fn func(r *record)) error {
	man, err := a.manifest()
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
		raw, err := a.segmentBytes(e)
		if err != nil {
			return err
		}
		feed(raw)
	}
	if tb, err := a.tailBytes(); err == nil {
		feed(tb)
	}
	return nil
}

func traceR(a *archiveReader, eventID, direction string, maxDepth int) ([]TraceNode, error) {
	if maxDepth <= 0 {
		maxDepth = 16
	}
	effect := direction == "effect"

	causalIn := map[string][]model.CausalLink{}
	causalOut := map[string][]model.CausalLink{}
	linked := map[string]bool{}
	if err := forEachArchiveRecordR(a, func(r *record) {
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
	if err := forEachArchiveRecordR(a, func(r *record) {
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

// ScanTrace walks the causal graph from eventID over an archive. direction is
// "cause" (inbound — what led to this) or "effect" (outbound — what this led to).
func ScanTrace(dir, eventID, direction string, maxDepth int) ([]TraceNode, error) {
	return traceR(newArchiveReader(dir, 0), eventID, direction, maxDepth)
}
