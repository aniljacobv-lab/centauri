// Bidirectional sync support: ingesting facts received from a peer replica.
// See docs/design-sync.md. The key property is echo-safety — a fact that
// syncs A→B must not bounce back B→A — which falls out of Centauri's
// globally-unique, immutable event ids: anything we already hold is skipped.
package store

import (
	"bytes"
	"encoding/json"

	"github.com/proxima360/centauri/internal/model"
)

// IngestForeign appends facts received from a peer during sync. It skips any
// event whose id we already hold (dedup — this is the echo protection), and
// preserves each event's original id and recorded_time so bi-temporal reads
// stay correct across replicas. It reuses the verified IngestRaw write / apply
// / hash-chain path. Returns the number of new facts stored.
//
// Convergent for disjoint or single-origin subjects; multi-master writes to
// the SAME subject need the deterministic current-fact rule (design-sync.md).
func (s *Store) IngestForeign(events []*model.Event) (int, error) {
	s.mu.RLock()
	var buf bytes.Buffer
	seen := map[string]bool{}
	n := 0
	for _, e := range events {
		if e == nil || e.EventID == "" || seen[e.EventID] {
			continue
		}
		if _, have := s.events[e.EventID]; have {
			continue // already hold this fact — don't echo it
		}
		seen[e.EventID] = true
		b, err := json.Marshal(&record{Event: e})
		if err != nil {
			s.mu.RUnlock()
			return 0, err
		}
		buf.Write(b)
		buf.WriteByte('\n')
		n++
	}
	s.mu.RUnlock()
	if n == 0 {
		return 0, nil
	}
	// IngestRaw re-acquires the lock; safe because sync holds the single
	// writer lock, so nothing else writes between the dedup snapshot and here.
	if err := s.IngestRaw(buf.Bytes()); err != nil {
		return 0, err
	}
	return n, nil
}
