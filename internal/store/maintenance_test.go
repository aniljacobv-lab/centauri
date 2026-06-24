package store

import (
	"fmt"
	"path/filepath"
	"testing"
	"time"

	"github.com/proxima360/centauri/internal/model"
)

// Auto-seal rolls a grown tail into a segment, keeping the hot log bounded while
// all data stays readable.
func TestAutoSealBoundsTail(t *testing.T) {
	dir := t.TempDir()
	logp := filepath.Join(dir, "src.log")
	st, err := OpenOptions(logp, Options{})
	if err != nil {
		t.Fatal(err)
	}
	mk := func(now int64, subj string, v int) {
		e := &model.Event{Subject: subj, Facet: "f", Type: model.Observed, EffectiveTime: now,
			Provenance: model.SystemFeed, Confidence: 1, Value: map[string]any{"v": v}}
		if err := st.Append(now, []*model.Event{e}, nil); err != nil {
			t.Fatal(err)
		}
	}
	mk(1000, "item:1", 1)
	st.Close()

	arch := filepath.Join(dir, "arch")
	if _, err := WriteArchive(logp, arch, 100); err != nil {
		t.Fatal(err)
	}

	a, err := OpenArchive(arch, Options{Lock: true, AutoSealBytes: 1}) // tiny threshold
	if err != nil {
		t.Fatal(err)
	}
	defer a.Close()

	e := &model.Event{Subject: "item:2", Facet: "f", Type: model.Observed, EffectiveTime: 2000,
		Provenance: model.SystemFeed, Confidence: 1, Value: map[string]any{"v": 2}}
	if err := a.Append(2000, []*model.Event{e}, nil); err != nil {
		t.Fatal(err)
	}

	a.maybeAutoSeal() // tail >= 1 byte → seal

	size := func() int64 { a.mu.RLock(); defer a.mu.RUnlock(); return a.size }
	if size() != 0 {
		t.Fatalf("tail not sealed (size=%d)", size())
	}
	// Both the original (segment) and the just-sealed subject remain current.
	if c := a.Current("item:1", "f"); len(c) != 1 {
		t.Fatalf("item:1 lost: %v", c)
	}
	if c := a.Current("item:2", "f"); len(c) != 1 {
		t.Fatalf("item:2 lost after seal: %v", c)
	}
}

// Periodic checkpointing must fire while serving and Close must stop the
// maintenance loop cleanly (no deadlock); the data reopens correctly.
func TestPeriodicCheckpointLifecycle(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "l.log")
	st, err := OpenOptions(path, Options{CheckpointEvery: 10 * time.Millisecond})
	if err != nil {
		t.Fatal(err)
	}
	for i := 0; i < 5; i++ {
		e := &model.Event{Subject: fmt.Sprintf("item:%d", i), Facet: "f", Type: model.Observed,
			EffectiveTime: int64(1000 + i), Provenance: model.SystemFeed, Confidence: 1,
			Value: map[string]any{"v": i}}
		if err := st.Append(int64(1000+i), []*model.Event{e}, nil); err != nil {
			t.Fatal(err)
		}
	}
	time.Sleep(50 * time.Millisecond) // let the periodic checkpoint fire
	if err := st.Close(); err != nil { // must not deadlock with the maintenance loop
		t.Fatal(err)
	}

	st2, err := OpenOptions(path, Options{})
	if err != nil {
		t.Fatal(err)
	}
	defer st2.Close()
	if got := len(st2.Subjects()); got != 5 {
		t.Fatalf("after reopen subjects = %d, want 5", got)
	}
}
