package store

import (
	"path/filepath"
	"testing"
)

func TestSlots(t *testing.T) {
	s, err := OpenOptions(filepath.Join(t.TempDir(), "c.log"), Options{})
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	defer s.Close()

	if c := s.SlotCursor("airbyte"); c != 0 {
		t.Fatalf("unknown slot cursor = %d, want 0", c)
	}
	if err := s.AdvanceSlot(1000, "airbyte", 420); err != nil {
		t.Fatalf("advance: %v", err)
	}
	if c := s.SlotCursor("airbyte"); c != 420 {
		t.Fatalf("slot cursor = %d, want 420", c)
	}
	// monotonic: a rewinding ack is ignored.
	if err := s.AdvanceSlot(1100, "airbyte", 100); err != nil {
		t.Fatalf("advance back: %v", err)
	}
	if c := s.SlotCursor("airbyte"); c != 420 {
		t.Fatalf("slot must not rewind: got %d, want 420", c)
	}
	// forward advance works.
	if err := s.AdvanceSlot(1200, "airbyte", 900); err != nil {
		t.Fatalf("advance fwd: %v", err)
	}
	if c := s.SlotCursor("airbyte"); c != 900 {
		t.Fatalf("slot cursor = %d, want 900", c)
	}
	// a second slot is independent; MinSlotCursor tracks the laggard.
	if err := s.AdvanceSlot(1300, "warehouse", 300); err != nil {
		t.Fatalf("advance slot2: %v", err)
	}
	if len(s.Slots()) != 2 {
		t.Fatalf("expected 2 slots, got %d", len(s.Slots()))
	}
	if m := s.MinSlotCursor(); m != 300 {
		t.Fatalf("MinSlotCursor = %d, want 300", m)
	}
}
