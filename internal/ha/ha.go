// Package ha provides lease-based leader election for automatic failover. One
// node holds a time-bound lease and is the writable PRIMARY; the others are
// read-only replicas that follow it. If the primary dies, its lease expires and
// a replica acquires the lease and promotes itself — no operator action, no
// external coordinator.
//
// Zero third-party dependencies. Correctness rests on one primitive: an atomic
// compare-and-swap on a single shared lease record (the standard leader-election
// primitive — etcd, Consul, Dynamo, and Postgres all expose it). The LeaseStore
// interface is that primitive; a node plugs in whichever shared store it has.
// Two implementations ship here: MemLeaseStore (single process / tests) and
// FileLeaseStore, which uses atomic O_EXCL file creation on a shared POSIX
// filesystem (NFS/EBS-multi-attach/a k8s shared volume).
//
// Safety. Split-brain (two writable primaries forking the hash chain) is
// prevented two ways: (1) the lease carries a monotonic Epoch — a fencing token
// that strictly increases on every acquisition; and (2) a leader stops accepting
// writes the instant it cannot confirm a non-expired lease (CanWrite returns
// false within one renewal interval of expiry), so a paused/partitioned old
// leader fences itself out before a new one is safe to write. As with all
// lease-based schemes this assumes bounded clock skew across nodes.
package ha

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sync"
	"time"
)

// ErrConflict is returned by CompareAndSwap when the stored version no longer
// matches the expected one (another node wrote first).
var ErrConflict = errors.New("ha: lease version conflict")

// Lease is the single shared leadership record.
type Lease struct {
	Holder    string `json:"holder"`     // node id of the current leader ("" = vacant)
	Addr      string `json:"addr"`       // leader's advertised base URL (followers replicate from it)
	Epoch     uint64 `json:"epoch"`      // monotonic fencing token; +1 on every acquisition
	ExpiresAt int64  `json:"expires_at"` // UnixNano; leader must renew before this
}

// LeaseStore is the shared source of truth. Get returns the current lease and an
// opaque version (0 = no record yet). CompareAndSwap atomically replaces the
// record only if the stored version still equals expected, returning the new
// version, or ErrConflict if another writer won the race.
type LeaseStore interface {
	Get() (Lease, uint64, error)
	CompareAndSwap(expected uint64, next Lease) (uint64, error)
}

// Role is a node's current election state.
type Role int

const (
	Follower Role = iota
	Leader
)

func (r Role) String() string {
	if r == Leader {
		return "leader"
	}
	return "follower"
}

// Config configures an Elector.
type Config struct {
	NodeID string        // this node's unique id
	Addr   string        // this node's advertised base URL (used when it leads)
	TTL    time.Duration // lease lifetime (default 10s)
	Renew  time.Duration // election tick / renewal cadence, must be << TTL (default TTL/3)
	Store  LeaseStore

	Clock func() time.Time // injectable wall clock (tests); nil = time.Now

	OnPromote func(epoch uint64) // called when this node becomes leader
	OnDemote  func()             // called when this node loses leadership
	OnLeader  func(addr string)  // called (on a follower) when the leader address changes
	Logf      func(string, ...any)
}

// Elector runs the election loop and exposes the node's role.
type Elector struct {
	cfg Config

	mu         sync.Mutex
	role       Role
	epoch      uint64
	expiry     int64 // our lease expiry (UnixNano); meaningful while Leader
	leaderAddr string
}

func New(cfg Config) *Elector {
	if cfg.TTL <= 0 {
		cfg.TTL = 10 * time.Second
	}
	if cfg.Renew <= 0 {
		cfg.Renew = cfg.TTL / 3
	}
	return &Elector{cfg: cfg, role: Follower}
}

func (e *Elector) now() int64 {
	if e.cfg.Clock != nil {
		return e.cfg.Clock().UnixNano()
	}
	return time.Now().UnixNano()
}

func (e *Elector) ttl() int64 { return int64(e.cfg.TTL) }

func (e *Elector) logf(f string, a ...any) {
	if e.cfg.Logf != nil {
		e.cfg.Logf(f, a...)
	}
}

// Run drives the election until ctx is cancelled, at which point a leader
// releases its lease so a standby can take over immediately.
func (e *Elector) Run(ctx context.Context) {
	e.step()
	t := time.NewTicker(e.cfg.Renew)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			e.release()
			return
		case <-t.C:
			e.step()
		}
	}
}

// step performs one election decision: renew if we hold a live lease, acquire if
// it is vacant or expired, otherwise follow the current holder.
func (e *Elector) step() {
	lease, ver, err := e.cfg.Store.Get()
	if err != nil {
		// Can't confirm the lease — step down rather than risk split-brain.
		e.logf("ha: lease read failed: %v", err)
		e.demote()
		return
	}
	now := e.now()
	self := e.cfg.NodeID
	switch {
	case lease.Holder == self && lease.ExpiresAt > now:
		// We hold a live lease: renew it (keep the same epoch).
		e.tryAcquire(ver, Lease{Holder: self, Addr: e.cfg.Addr, Epoch: lease.Epoch, ExpiresAt: now + e.ttl()})
	case lease.Holder == "" || lease.ExpiresAt <= now:
		// Vacant or expired: try to take it, bumping the fencing epoch.
		e.tryAcquire(ver, Lease{Holder: self, Addr: e.cfg.Addr, Epoch: lease.Epoch + 1, ExpiresAt: now + e.ttl()})
	default:
		// Someone else holds a live lease: follow them.
		e.demote()
		e.setLeaderAddr(lease.Addr)
	}
}

func (e *Elector) tryAcquire(ver uint64, next Lease) {
	if _, err := e.cfg.Store.CompareAndSwap(ver, next); err != nil {
		// Lost the race (or store error): we are not the leader this round.
		e.demote()
		return
	}
	e.promote(next.Epoch, next.ExpiresAt, next.Addr)
}

func (e *Elector) promote(epoch uint64, expiry int64, addr string) {
	e.mu.Lock()
	was := e.role
	e.role, e.epoch, e.expiry, e.leaderAddr = Leader, epoch, expiry, addr
	e.mu.Unlock()
	if was != Leader {
		e.logf("ha: %s acquired leadership (epoch %d)", e.cfg.NodeID, epoch)
		if e.cfg.OnPromote != nil {
			e.cfg.OnPromote(epoch)
		}
	}
}

func (e *Elector) demote() {
	e.mu.Lock()
	was := e.role
	e.role = Follower
	e.mu.Unlock()
	if was == Leader {
		e.logf("ha: %s lost leadership", e.cfg.NodeID)
		if e.cfg.OnDemote != nil {
			e.cfg.OnDemote()
		}
	}
}

func (e *Elector) setLeaderAddr(addr string) {
	e.mu.Lock()
	changed := e.leaderAddr != addr
	e.leaderAddr = addr
	e.mu.Unlock()
	if changed && addr != "" {
		e.logf("ha: following leader at %s", addr)
		if e.cfg.OnLeader != nil {
			e.cfg.OnLeader(addr)
		}
	}
}

// release voluntarily gives up a held lease (on shutdown) so a standby promotes
// without waiting for the full TTL to elapse.
func (e *Elector) release() {
	e.mu.Lock()
	leader := e.role == Leader
	e.mu.Unlock()
	if !leader {
		return
	}
	lease, ver, err := e.cfg.Store.Get()
	if err != nil || lease.Holder != e.cfg.NodeID {
		return
	}
	// Tombstone: vacant holder, expired, epoch preserved so the next acquire bumps it.
	_, _ = e.cfg.Store.CompareAndSwap(ver, Lease{Epoch: lease.Epoch, ExpiresAt: 0})
	e.demote()
	e.logf("ha: %s released leadership", e.cfg.NodeID)
}

// CanWrite reports whether this node may accept writes right now: it must be the
// leader AND safely before its lease expiry (a renewal interval of headroom), so
// a stalled leader fences itself off before a successor can write.
func (e *Elector) CanWrite() bool {
	e.mu.Lock()
	defer e.mu.Unlock()
	return e.role == Leader && e.now() < e.expiry-int64(e.cfg.Renew)
}

func (e *Elector) IsLeader() bool {
	e.mu.Lock()
	defer e.mu.Unlock()
	return e.role == Leader
}

func (e *Elector) Epoch() uint64 {
	e.mu.Lock()
	defer e.mu.Unlock()
	return e.epoch
}

// Status returns a JSON-friendly snapshot for a /v1/ha endpoint.
func (e *Elector) Status() map[string]any {
	e.mu.Lock()
	defer e.mu.Unlock()
	return map[string]any{
		"node":        e.cfg.NodeID,
		"role":        e.role.String(),
		"epoch":       e.epoch,
		"leader_addr": e.leaderAddr,
		"can_write":   e.role == Leader && e.now() < e.expiry-int64(e.cfg.Renew),
	}
}

// ---- MemLeaseStore: in-process store for a single node and for tests ----

type MemLeaseStore struct {
	mu    sync.Mutex
	lease Lease
	ver   uint64
}

func (m *MemLeaseStore) Get() (Lease, uint64, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.lease, m.ver, nil
}

func (m *MemLeaseStore) CompareAndSwap(expected uint64, next Lease) (uint64, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.ver != expected {
		return m.ver, ErrConflict
	}
	m.ver++
	m.lease = next
	return m.ver, nil
}

// ---- FileLeaseStore: shared-filesystem store using atomic O_EXCL creation ----

// FileLeaseStore stores each lease version as a separate file (lease.<version>)
// in a shared directory. CompareAndSwap creates the next version with
// O_CREATE|O_EXCL, which is atomic on a POSIX filesystem (including NFSv3+): only
// one node can create a given version, so only one node wins each round. The
// current version is the highest-numbered file; older ones are pruned. Requires
// a filesystem with atomic exclusive create (local disk, NFS, most shared
// volumes); for object stores without it, supply a CAS-capable LeaseStore.
type FileLeaseStore struct {
	dir string
}

func NewFileLeaseStore(dir string) (*FileLeaseStore, error) {
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return nil, err
	}
	return &FileLeaseStore{dir: dir}, nil
}

func (f *FileLeaseStore) path(v uint64) string {
	return filepath.Join(f.dir, fmt.Sprintf("lease.%020d", v))
}

func (f *FileLeaseStore) highest() uint64 {
	ents, err := os.ReadDir(f.dir)
	if err != nil {
		return 0
	}
	var max uint64
	for _, e := range ents {
		var v uint64
		if n, _ := fmt.Sscanf(e.Name(), "lease.%d", &v); n == 1 && v > max {
			max = v
		}
	}
	return max
}

func (f *FileLeaseStore) Get() (Lease, uint64, error) {
	v := f.highest()
	if v == 0 {
		return Lease{}, 0, nil
	}
	b, err := os.ReadFile(f.path(v))
	if err != nil {
		return Lease{}, 0, err
	}
	var l Lease
	if err := json.Unmarshal(b, &l); err != nil {
		return Lease{}, 0, err
	}
	return l, v, nil
}

func (f *FileLeaseStore) CompareAndSwap(expected uint64, next Lease) (uint64, error) {
	nv := expected + 1
	fd, err := os.OpenFile(f.path(nv), os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0o644)
	if err != nil {
		if os.IsExist(err) {
			return f.highest(), ErrConflict // another node already wrote this version
		}
		return 0, err
	}
	b, _ := json.Marshal(next)
	_, werr := fd.Write(b)
	cerr := fd.Close()
	if werr != nil {
		_ = os.Remove(f.path(nv))
		return 0, werr
	}
	if cerr != nil {
		return 0, cerr
	}
	// Guard against a stale `expected`: if a higher version exists (ours was
	// behind), our write is void — drop it and report the conflict.
	if f.highest() > nv {
		_ = os.Remove(f.path(nv))
		return f.highest(), ErrConflict
	}
	f.prune(nv)
	return nv, nil
}

func (f *FileLeaseStore) prune(keep uint64) {
	ents, err := os.ReadDir(f.dir)
	if err != nil {
		return
	}
	for _, e := range ents {
		var v uint64
		if n, _ := fmt.Sscanf(e.Name(), "lease.%d", &v); n == 1 && v < keep {
			_ = os.Remove(filepath.Join(f.dir, e.Name()))
		}
	}
}
