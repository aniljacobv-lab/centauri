// Replication slots: durable, named CDC consumer positions — Centauri's take
// on PostgreSQL replication slots. A slot remembers how far a consumer has
// confirmed processing, so it can disconnect and resume without missing or
// replaying changes. Slots are stored as ordinary facts (subject
// "slot:<name>", facet "cdc"), so they're durable, replicated, bi-temporal,
// and queryable like everything else. MinSlotCursor is the hook a future log
// compactor will respect: never discard log a slot hasn't consumed.
package store

import (
	"fmt"
	"strings"

	"github.com/proxima360/centauri/internal/model"
)

// SlotInfo is a slot's current confirmed cursor (a byte offset into the log).
type SlotInfo struct {
	Name   string `json:"name"`
	Cursor int64  `json:"cursor"`
}

func slotSubject(name string) string { return "slot:" + name }

// SlotCursor returns a slot's confirmed cursor, or 0 if the slot is unknown.
func (s *Store) SlotCursor(name string) int64 {
	cur := s.Current(slotSubject(name), "cdc")
	if len(cur) == 0 {
		return 0
	}
	return asInt64(cur[0].Value["cursor"])
}

// AdvanceSlot confirms a slot's position by appending a superseding fact. It
// never moves a slot backwards (a late/duplicate ack is ignored), so the
// confirmed position is monotonic.
func (s *Store) AdvanceSlot(now int64, name string, cursor int64) error {
	if name == "" {
		return fmt.Errorf("slot: name is required")
	}
	if cursor < 0 {
		return fmt.Errorf("slot: cursor must be >= 0")
	}
	if cursor < s.SlotCursor(name) {
		return nil // monotonic: ignore acks that would rewind the slot
	}
	e := &model.Event{
		Subject: slotSubject(name), Facet: "cdc", Type: model.Observed,
		Value:      map[string]any{"cursor": cursor, "updated_at": now},
		Provenance: model.SystemFeed, Confidence: 1.0, SourceSystem: "CDC",
	}
	return s.Append(now, []*model.Event{e}, nil)
}

// Slots lists every slot with its confirmed cursor.
func (s *Store) Slots() []SlotInfo {
	out := []SlotInfo{}
	for _, subj := range s.Subjects() {
		if !strings.HasPrefix(subj, "slot:") {
			continue
		}
		cur := s.Current(subj, "cdc")
		if len(cur) == 0 {
			continue
		}
		out = append(out, SlotInfo{Name: strings.TrimPrefix(subj, "slot:"),
			Cursor: asInt64(cur[0].Value["cursor"])})
	}
	return out
}

// MinSlotCursor is the lowest confirmed cursor across all slots (0 if there
// are none). A log compactor must not discard bytes below this offset.
func (s *Store) MinSlotCursor() int64 {
	slots := s.Slots()
	if len(slots) == 0 {
		return 0
	}
	min := slots[0].Cursor
	for _, sl := range slots[1:] {
		if sl.Cursor < min {
			min = sl.Cursor
		}
	}
	return min
}

// asInt64 coerces the numeric shapes a value arrives in (int64 from a live
// write, float64 after a JSON round-trip).
func asInt64(v any) int64 {
	switch n := v.(type) {
	case int64:
		return n
	case int:
		return int64(n)
	case float64:
		return int64(n)
	case float32:
		return int64(n)
	}
	return 0
}
