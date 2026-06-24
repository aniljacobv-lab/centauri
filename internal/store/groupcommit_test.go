package store

import (
	"fmt"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"github.com/proxima360/centauri/internal/model"
)

// Many goroutines append concurrently through the group committer: nothing is
// lost, and the data survives a reopen (durability + replay determinism).
func TestGroupCommitConcurrentNoLoss(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "g.log")
	st, err := OpenOptions(path, Options{GroupCommit: true})
	if err != nil {
		t.Fatal(err)
	}

	const writers, perWriter = 16, 50
	var wg sync.WaitGroup
	errCh := make(chan error, writers*perWriter)
	for w := 0; w < writers; w++ {
		wg.Add(1)
		go func(w int) {
			defer wg.Done()
			for i := 0; i < perWriter; i++ {
				e := &model.Event{Subject: fmt.Sprintf("item:%d", w*1000+i), Facet: "f",
					Type: model.Observed, Value: map[string]any{"w": w, "i": i},
					Provenance: model.SystemFeed, Confidence: 1}
				if err := st.Append(time.Now().UnixMicro(), []*model.Event{e}, nil); err != nil {
					errCh <- err
					return
				}
			}
		}(w)
	}
	wg.Wait()
	close(errCh)
	for e := range errCh {
		t.Fatalf("concurrent append error: %v", e)
	}

	want := writers * perWriter
	if got := len(st.Subjects()); got != want {
		t.Fatalf("subjects = %d, want %d (lost writes under concurrency?)", got, want)
	}
	st.Close()

	// Reopen WITHOUT group commit and confirm everything replayed.
	st2, err := OpenOptions(path, Options{})
	if err != nil {
		t.Fatal(err)
	}
	defer st2.Close()
	if got := len(st2.Subjects()); got != want {
		t.Fatalf("after reopen subjects = %d, want %d", got, want)
	}
}

// Concurrent appends to the SAME (subject,facet) are serialized by the committer:
// every version is retained in history and exactly one is current.
func TestGroupCommitSameSubjectConverges(t *testing.T) {
	dir := t.TempDir()
	st, err := OpenOptions(filepath.Join(dir, "g.log"), Options{GroupCommit: true})
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()

	const n = 100
	var wg sync.WaitGroup
	for i := 0; i < n; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			e := &model.Event{Subject: "item:hot", Facet: "f", Type: model.Observed,
				Value: map[string]any{"seq": i}, Provenance: model.SystemFeed, Confidence: 1}
			if err := st.Append(time.Now().UnixMicro(), []*model.Event{e}, nil); err != nil {
				t.Errorf("append: %v", err)
			}
		}(i)
	}
	wg.Wait()

	if h := st.History("item:hot", "f"); len(h) != n {
		t.Fatalf("history = %d, want %d (all versions kept — nothing erased)", len(h), n)
	}
	if cur := st.Current("item:hot", "f"); len(cur) != 1 {
		t.Fatalf("current = %d, want exactly 1 (deterministic winner)", len(cur))
	}
}
