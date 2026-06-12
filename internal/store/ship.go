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
	for off, line := range bytes.Split(b[:len(b)-1], []byte{'\n'}) {
		trimmed := bytes.TrimSpace(line)
		if len(trimmed) == 0 {
			continue
		}
		var r record
		if err := json.Unmarshal(trimmed, &r); err != nil || r.empty() {
			return fmt.Errorf("ingest: bad record in chunk (line %d): %v", off, err)
		}
		recs = append(recs, &r)
	}
	if _, err := s.f.Write(b); err != nil {
		s.rollback()
		return fmt.Errorf("ingest: write: %w", err)
	}
	if !s.opts.NoSync {
		if err := s.f.Sync(); err != nil {
			s.rollback()
			return fmt.Errorf("ingest: fsync: %w", err)
		}
	}
	s.size += int64(len(b))
	s.chainExtendBuf(b) // identical bytes ⇒ identical chain as the primary
	for _, r := range recs {
		s.apply(r)
	}
	for _, r := range recs {
		if r.Event == nil {
			continue
		}
		for _, ch := range s.subs {
			select {
			case ch <- r.Event:
			default:
			}
		}
	}
	return nil
}
