// Change Data Capture: the consumer-friendly side of the append-only log.
// Where ReadLog/IngestRaw ship raw bytes between nodes, Changes returns
// parsed fact events plus a resumable cursor, so an application can tail
// the database without coordination: save the cursor, process the events,
// call again with the cursor to receive only what's new — no missed or
// duplicated changes.
package store

import (
	"bytes"
	"encoding/json"
	"fmt"

	"github.com/proxima360/centauri/internal/model"
)

// Changes returns the fact events committed at or after byte offset `from`,
// in commit (log) order, and the cursor to resume from. Read-only: it uses
// an independent file handle (via ReadLog) so it never disturbs the writer,
// and it returns full payloads even when LazyPayloads is on, because it
// reads the log rather than the in-RAM (offloaded) events.
//
// One call returns at most a record-aligned ~4 MiB slice of the log; keep
// calling with the returned cursor until it equals LogSize(). A `from`
// beyond the committed size is an error (the log was replaced/truncated).
func (s *Store) Changes(from int64) (events []*model.Event, nextOffset int64, err error) {
	b, err := s.ReadLog(from)
	if err != nil {
		return nil, from, err
	}
	if len(b) == 0 {
		return nil, from, nil
	}
	for _, line := range bytes.Split(b[:len(b)-1], []byte{'\n'}) {
		trimmed := bytes.TrimSpace(line)
		if len(trimmed) == 0 {
			continue
		}
		var r record
		if err := json.Unmarshal(trimmed, &r); err != nil {
			return nil, from, fmt.Errorf("changes: bad record near offset %d: %w", from, err)
		}
		if r.Event != nil {
			events = append(events, r.Event)
		}
	}
	return events, from + int64(len(b)), nil
}
