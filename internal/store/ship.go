// Log shipping: replication for an append-only store is just copying
// bytes. A primary serves its committed log via ReadLog; a follower
// appends those bytes with IngestRaw, replaying them into its own
// indexes. Time-ordered event ids and the single-writer log make this
// safe without coordination — the follower is a read replica that is
// exactly as far along as the bytes it has.
package store

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
)

// maxShipChunk caps one ReadLog response so followers stream in pieces.
const maxShipChunk = 4 << 20 // 4 MiB

// LogSize returns the committed size of the log in bytes.
func (s *Store) LogSize() int64 {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return s.size
}

// ReadLog returns committed log bytes from offset `from`, up to roughly
// maxShipChunk, always ending on a record boundary so a follower can
// never observe a torn record. An empty slice means "caught up". A from
// beyond the committed size is an error: the follower has diverged
// (e.g. the primary's log was replaced) and must not wait forever.
func (s *Store) ReadLog(from int64) ([]byte, error) {
	s.mu.RLock()
	size := s.size
	path := s.path
	s.mu.RUnlock()
	if from < 0 {
		return nil, errors.New("readlog: negative offset")
	}
	if from > size {
		return nil, fmt.Errorf("readlog: offset %d beyond committed size %d (log replaced? re-seed the follower)", from, size)
	}
	if from == size {
		return []byte{}, nil
	}
	// Read via an independent handle so shipping never disturbs the
	// writer's file position.
	f, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	n := size - from
	if n > maxShipChunk {
		n = maxShipChunk
	}
	for {
		buf := make([]byte, n)
		if _, err := io.ReadFull(io.NewSectionReader(f, from, n), buf); err != nil {
			return nil, err
		}
		if n == size-from {
			return buf, nil // committed log always ends on '\n'
		}
		// Trim back to the last complete record.
		if i := bytes.LastIndexByte(buf, '\n'); i >= 0 {
			return buf[:i+1], nil
		}
		// One record larger than the chunk: grow until its newline.
		n *= 2
		if n > size-from {
			n = size - from
		}
	}
}

// IngestRaw appends raw log bytes shipped from a primary. Every line is
// validated as a complete, parseable record BEFORE any byte is written —
// a malformed chunk is rejected whole. The follower's own commit path is
// bypassed (these records were already validated and ordered by the
// primary); they are written, synced, and applied verbatim.
func (s *Store) IngestRaw(b []byte) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if err := s.writable(); err != nil {
		return err
	}
	if len(b) == 0 {
		return nil
	}
	if b[len(b)-1] != '\n' {
		return errors.New("ingest: chunk does not end on a record boundary")
	}
	var recs []*record
	var keep bytes.Buffer
	keep.Grow(len(b))
	for off, line := range bytes.Split(b[:len(b)-1], []byte{'\n'}) {
		trimmed := bytes.TrimSpace(line)
		if len(trimmed) == 0 {
			keep.Write(line)
			keep.WriteByte('\n')
			continue
		}
		var r record
		if err := json.Unmarshal(trimmed, &r); err != nil || r.empty() {
			return fmt.Errorf("ingest: bad record in chunk (line %d): %v", off, err)
		}
		// Duplicate-delivery guard: a redelivered chunk (retry after a lost
		// ack) must never double-apply. Skipped lines are not written either,
		// so the chain keeps covering exactly the bytes on disk (invariant 4)
		// and a fully-duplicate chunk leaves the log byte-identical to the
		// primary's.
		if s.alreadyApplied(&r) {
			continue
		}
		keep.Write(line)
		keep.WriteByte('\n')
		recs = append(recs, &r)
	}
	if keep.Len() == 0 {
		return nil // every line already applied: duplicate delivery, ack as done
	}
	// Same durable-write path as commit (write→fsync→chain→apply→notify);
	// identical bytes ⇒ identical chain as the primary.
	return s.writeApplyNotify(keep.Bytes(), recs)
}

// alreadyApplied reports whether one shipped record is already reflected in
// the index, so IngestRaw can skip it on redelivery. Events are keyed by their
// immutable id; the other record kinds are matched on their full identifying
// content. Caller holds s.mu.
func (s *Store) alreadyApplied(r *record) bool {
	switch {
	case r.Event != nil:
		if r.Event.EventID == "" {
			return false
		}
		_, ok := s.events[r.Event.EventID]
		return ok
	case r.Supersede != nil:
		op := r.Supersede
		e, ok := s.events[op.EventID]
		return ok && e.SupersededBy == op.SupersededBy &&
			e.EffectiveEnd == op.EffectiveEnd &&
			s.supersededAt[op.EventID].recordedTime == op.RecordedTime
	case r.Link != nil:
		for _, l := range s.causalOut[r.Link.From] {
			if l == *r.Link {
				return true
			}
		}
		return false
	case r.Enrichment != nil:
		for _, en := range s.enrichments[r.Enrichment.TargetEvent] {
			if en.EnrichmentID == r.Enrichment.EnrichmentID {
				return true
			}
		}
		return false
	case r.Schema != nil:
		// Versions are 1-based and append-only on the primary: version N
		// exists iff at least N versions have been applied.
		return r.Schema.Version > 0 && len(s.schemas[r.Schema.SchemaID]) >= r.Schema.Version
	}
	return false
}
