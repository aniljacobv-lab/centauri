package ha

import (
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

func TestMemStoreCAS(t *testing.T) {
	m := &MemLeaseStore{}
	_, v, _ := m.Get()
	if v != 0 {
		t.Fatalf("empty version = %d, want 0", v)
	}
	v1, err := m.CompareAndSwap(0, Lease{Holder: "a", Epoch: 1})
	if err != nil || v1 != 1 {
		t.Fatalf("first CAS = %d, %v", v1, err)
	}
	if _, err := m.CompareAndSwap(0, Lease{Holder: "b"}); err != ErrConflict {
		t.Fatalf("stale CAS err = %v, want ErrConflict", err)
	}
	if _, err := m.CompareAndSwap(1, Lease{Holder: "a", Epoch: 1}); err != nil {
		t.Fatalf("renew CAS: %v", err)
	}
}

func TestFileStoreCAS(t *testing.T) {
	f, err := NewFileLeaseStore(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	if _, v, _ := f.Get(); v != 0 {
		t.Fatalf("empty version = %d", v)
	}
	v1, err := f.CompareAndSwap(0, Lease{Holder: "a", Epoch: 1, ExpiresAt: 100})
	if err != nil || v1 != 1 {
		t.Fatalf("CAS(0) = %d, %v", v1, err)
	}
	got, v, _ := f.Get()
	if v != 1 || got.Holder != "a" || got.Epoch != 1 {
		t.Fatalf("Get = %+v v=%d", got, v)
	}
	if _, err := f.CompareAndSwap(0, Lease{Holder: "b"}); err != ErrConflict {
		t.Fatalf("stale CAS err = %v, want ErrConflict", err)
	}
	if _, err := f.CompareAndSwap(1, Lease{Holder: "b", Epoch: 2, ExpiresAt: 200}); err != nil {
		t.Fatalf("CAS(1): %v", err)
	}
}

// TestFileStoreConcurrentAcquire: many goroutines contend for the same version;
// exactly one must win.
func TestFileStoreConcurrentAcquire(t *testing.T) {
	f, _ := NewFileLeaseStore(t.TempDir())
	const n = 16
	var wins int64
	var wg sync.WaitGroup
	for i := 0; i < n; i++ {
		wg.Add(1)
		go func(id int) {
			defer wg.Done()
			if _, err := f.CompareAndSwap(0, Lease{Holder: string(rune('A' + id))}); err == nil {
				atomic.AddInt64(&wins, 1)
			}
		}(i)
	}
	wg.Wait()
	if wins != 1 {
		t.Fatalf("winners = %d, want exactly 1", wins)
	}
}

// electorWith builds an Elector sharing a store and a controllable clock, and
// records promote/demote callbacks.
func electorWith(id, addr string, store LeaseStore, now *int64) (*Elector, *int32) {
	var promotions int32
	e := New(Config{
		NodeID: id, Addr: addr, TTL: 9 * time.Second, Renew: 3 * time.Second, Store: store,
		Clock:     func() time.Time { return time.Unix(0, atomic.LoadInt64(now)) },
		OnPromote: func(uint64) { atomic.AddInt32(&promotions, 1) },
		OnDemote:  func() { atomic.AddInt32(&promotions, -1) },
	})
	return e, &promotions
}

func TestElectionAndFailover(t *testing.T) {
	store := &MemLeaseStore{}
	now := int64(1_000_000_000) // 1s in nanos
	a, aLead := electorWith("A", "http://a", store, &now)
	b, bLead := electorWith("B", "http://b", store, &now)

	// A steps first and becomes leader at epoch 1.
	a.step()
	if !a.IsLeader() || a.Epoch() != 1 {
		t.Fatalf("A should lead at epoch 1, got leader=%v epoch=%d", a.IsLeader(), a.Epoch())
	}
	if atomic.LoadInt32(aLead) != 1 {
		t.Fatal("A OnPromote should have fired once")
	}

	// B sees a live lease held by A and stays a follower.
	b.step()
	if b.IsLeader() {
		t.Fatal("B must not lead while A's lease is live")
	}
	if !a.CanWrite() {
		t.Fatal("A should be writable right after acquiring")
	}

	// Time advances past the lease TTL without A renewing (A "crashed").
	atomic.AddInt64(&now, int64(10*time.Second))
	if a.CanWrite() {
		t.Fatal("A must fence itself off once its lease has expired")
	}

	// B now acquires the expired lease and the epoch is fenced forward.
	b.step()
	if !b.IsLeader() || b.Epoch() != 2 {
		t.Fatalf("B should take over at epoch 2, got leader=%v epoch=%d", b.IsLeader(), b.Epoch())
	}
	if atomic.LoadInt32(bLead) != 1 {
		t.Fatal("B OnPromote should have fired once")
	}

	// When the old leader A runs again, it sees B's live lease and demotes.
	a.step()
	if a.IsLeader() {
		t.Fatal("A must demote after B took over")
	}
	if atomic.LoadInt32(aLead) != 0 {
		t.Fatal("A OnDemote should have fired (net promotions back to 0)")
	}
}

func TestLeaderRenewKeepsEpoch(t *testing.T) {
	store := &MemLeaseStore{}
	now := int64(1_000_000_000)
	a, _ := electorWith("A", "http://a", store, &now)
	a.step() // epoch 1
	atomic.AddInt64(&now, int64(3*time.Second))
	a.step() // renew, still epoch 1
	if a.Epoch() != 1 {
		t.Fatalf("renew should keep epoch 1, got %d", a.Epoch())
	}
	if !a.IsLeader() || !a.CanWrite() {
		t.Fatal("A should still be a writable leader after renewing")
	}
}
