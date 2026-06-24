package shard

import (
	"fmt"
	"sync"
	"testing"
	"time"

	"github.com/proxima360/centauri/internal/model"
	"github.com/proxima360/centauri/internal/store"
)

func openSet(t *testing.T, n int) *Set {
	t.Helper()
	set, err := Open(t.TempDir(), n, store.Options{})
	if err != nil {
		t.Fatal(err)
	}
	return set
}

func ev(subject string, v map[string]any) *model.Event {
	return &model.Event{Subject: subject, Facet: "f", Type: model.Observed,
		Value: v, Provenance: model.SystemFeed, Confidence: 1}
}

func TestRoutingIsDeterministic(t *testing.T) {
	set := openSet(t, 8)
	defer set.Close()
	a := set.Shard("item:42")
	b := set.Shard("item:42")
	if a != b {
		t.Fatal("same subject routed to different shards")
	}
	if ShardIndex("item:42", 8) != ShardIndex("item:42", 8) {
		t.Fatal("ShardIndex not stable")
	}
}

// Concurrent writes to distinct subjects land on (and only on) their owning
// shard, and nothing is lost.
func TestParallelAppendNoLossAndRouting(t *testing.T) {
	const n = 8
	set := openSet(t, n)
	defer set.Close()

	const writers, per = 16, 40
	var wg sync.WaitGroup
	errCh := make(chan error, writers*per)
	for w := 0; w < writers; w++ {
		wg.Add(1)
		go func(w int) {
			defer wg.Done()
			for i := 0; i < per; i++ {
				subj := fmt.Sprintf("item:%d", w*1000+i)
				if err := set.Append(time.Now().UnixMicro(), []*model.Event{ev(subj, map[string]any{"w": w})}, nil); err != nil {
					errCh <- err
					return
				}
			}
		}(w)
	}
	wg.Wait()
	close(errCh)
	for e := range errCh {
		t.Fatalf("parallel append: %v", e)
	}

	want := writers * per
	if got := len(set.Subjects()); got != want {
		t.Fatalf("subjects = %d, want %d", got, want)
	}
	// A subject is found on its owning shard and only there.
	subj := "item:5005"
	if c := set.Current(subj, "f"); len(c) != 1 {
		t.Fatalf("routed read of %s returned %d, want 1", subj, len(c))
	}
	owner := ShardIndex(subj, n)
	for i, st := range set.shards {
		got := len(st.Current(subj, "f"))
		if i == owner && got != 1 {
			t.Fatalf("owner shard %d missing %s", i, subj)
		}
		if i != owner && got != 0 {
			t.Fatalf("non-owner shard %d unexpectedly has %s", i, subj)
		}
	}
}

// All facts for one subject co-locate, so history is complete from one shard.
func TestSameSubjectColocates(t *testing.T) {
	set := openSet(t, 8)
	defer set.Close()
	now := time.Now().UnixMicro()
	if err := set.Append(now, []*model.Event{ev("order:1", map[string]any{"status": "placed"})}, nil); err != nil {
		t.Fatal(err)
	}
	if err := set.Append(now+1, []*model.Event{ev("order:1", map[string]any{"status": "shipped"})}, nil); err != nil {
		t.Fatal(err)
	}
	if h := set.History("order:1", "f"); len(h) != 2 {
		t.Fatalf("history = %d, want 2 (both versions on one shard)", len(h))
	}
}

// A causal link within a batch is accepted; one whose endpoints aren't in the
// batch is rejected (no cross-shard causal graph in v1).
func TestCausalLinkRouting(t *testing.T) {
	set := openSet(t, 8)
	defer set.Close()
	now := time.Now().UnixMicro()

	a := ev("order:1", map[string]any{"x": 1})
	a.EventID = "evA"
	b := ev("order:1", map[string]any{"x": 2})
	b.EventID = "evB"
	// Both endpoints in the batch (same subject → same shard): OK.
	if err := set.Append(now, []*model.Event{a, b}, []model.CausalLink{{From: "evA", To: "evB", Type: model.Triggered}}); err != nil {
		t.Fatalf("in-batch link should be accepted: %v", err)
	}
	// A link to an id not in this batch: rejected.
	c := ev("order:2", map[string]any{"x": 3})
	c.EventID = "evC"
	err := set.Append(now, []*model.Event{c}, []model.CausalLink{{From: "evC", To: "ghost", Type: model.Triggered}})
	if err == nil {
		t.Fatal("expected cross-shard / dangling causal link to be rejected")
	}
}
