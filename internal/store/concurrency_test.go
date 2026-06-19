package store

import (
	"fmt"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"github.com/proxima360/centauri/internal/model"
)

// TestConcurrentReadWriteWatch hammers the store with concurrent writers,
// readers, and watchers to shake out races and deadlocks in the lock
// discipline (Append/commit vs Current/History/AsOf/Disagreements vs
// Subscribe/Unsubscribe), including the off-lock lazy hydration path. Run with
// -race for the strongest signal: `go test -race ./internal/store`.
func TestConcurrentReadWriteWatch(t *testing.T) {
	for _, lazy := range []bool{false, true} {
		name := "eager"
		if lazy {
			name = "lazy"
		}
		t.Run(name, func(t *testing.T) {
			// NoSync: durability isn't what's under test here — lock discipline is.
			st, err := OpenOptions(filepath.Join(t.TempDir(), "c.log"), Options{LazyPayloads: lazy, NoSync: true})
			if err != nil {
				t.Fatalf("open: %v", err)
			}
			defer st.Close()

			const subjects = 10
			var work, watch sync.WaitGroup
			stop := make(chan struct{})

			// Watchers: drain the change feed until stopped.
			for w := 0; w < 2; w++ {
				watch.Add(1)
				go func() {
					defer watch.Done()
					id, ch := st.Subscribe(8)
					defer st.Unsubscribe(id)
					for {
						select {
						case <-stop:
							return
						case <-ch:
						}
					}
				}()
			}

			// Writers: many superseding appends on a small set of subjects —
			// stresses apply()/recomputeOpen and the single-writer lock.
			for wr := 0; wr < 4; wr++ {
				work.Add(1)
				go func(wr int) {
					defer work.Done()
					for i := 0; i < 200; i++ {
						subj := fmt.Sprintf("item:%d", i%subjects)
						e := &model.Event{
							Subject: subj, Facet: "f", Type: model.Observed,
							Value:      map[string]any{"n": wr*1000 + i},
							Provenance: model.SystemFeed, Confidence: 1,
						}
						if err := st.Append(time.Now().UnixMicro(), []*model.Event{e}, nil); err != nil {
							t.Errorf("append: %v", err)
							return
						}
					}
				}(wr)
			}

			// Readers: exercise every locked query path concurrently.
			for rd := 0; rd < 4; rd++ {
				work.Add(1)
				go func() {
					defer work.Done()
					for i := 0; i < 400; i++ {
						subj := fmt.Sprintf("item:%d", i%subjects)
						_ = st.Current(subj, "")
						_ = st.Current(subj, "f")
						_ = st.History(subj, "f")
						_ = st.HistoryN(subj, "f", 5)
						_ = st.AsOf(subj, "f", time.Now().UnixMicro(), 0)
						_ = st.Disagreements("n")
					}
				}()
			}

			work.Wait()
			close(stop)
			watch.Wait()

			// Sanity: every subject has exactly one current fact and a chain head.
			for i := 0; i < subjects; i++ {
				if cur := st.Current(fmt.Sprintf("item:%d", i), "f"); len(cur) != 1 {
					t.Fatalf("item:%d current = %d facts, want 1", i, len(cur))
				}
			}
			if head, _ := st.ChainHead(); head == "" {
				t.Fatalf("empty chain head after concurrent writes")
			}
		})
	}
}
