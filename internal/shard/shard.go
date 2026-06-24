// Package shard gives Centauri horizontal write scaling: it partitions subjects
// across N independent shard logs (each its own append-only chain, lock, and
// committer) and writes to different shards IN PARALLEL. One chain can't be
// appended concurrently (the hash chain is sequential), but N chains can — so
// writes to different subjects scale across shards, ~N× throughput, while each
// shard stays a fully ordered, tamper-evident log.
//
// Routing is by a stable hash of the subject, so a subject (and all its facets)
// always lands on the same shard — point reads route to exactly one shard.
//
// v1 scope / honest limits:
//   - A batch spanning multiple shards is NOT globally atomic; each shard commits
//     independently (a single-subject append is one shard, atomic).
//   - Explicit causal links must connect events within the same batch; a link
//     whose endpoints land on different shards is rejected (no cross-shard
//     causal graph yet). Auto-supersession links stay intra-shard by construction
//     (same subject/facet → same shard).
//   - Cross-shard SEARCH / aggregation fan-out is not built here yet.
package shard

import (
	"fmt"
	"hash/fnv"
	"os"
	"path/filepath"
	"sort"
	"sync"

	"github.com/proxima360/centauri/internal/model"
	"github.com/proxima360/centauri/internal/store"
)

// Set is a fixed-size group of shard stores.
type Set struct {
	dir    string
	n      int
	shards []*store.Store
}

// Open opens (or creates) n shard logs under dir as shard-000.log … shard-NNN.log.
func Open(dir string, n int, opts store.Options) (*Set, error) {
	if n < 1 {
		n = 1
	}
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return nil, err
	}
	set := &Set{dir: dir, n: n, shards: make([]*store.Store, n)}
	for i := 0; i < n; i++ {
		p := filepath.Join(dir, fmt.Sprintf("shard-%03d.log", i))
		st, err := store.OpenOptions(p, opts)
		if err != nil {
			set.Close()
			return nil, fmt.Errorf("shard %d: %w", i, err)
		}
		set.shards[i] = st
	}
	return set, nil
}

// N is the shard count.
func (s *Set) N() int { return s.n }

// ShardIndex is the deterministic shard for a subject (FNV-1a hash mod N).
func ShardIndex(subject string, n int) int {
	h := fnv.New32a()
	_, _ = h.Write([]byte(subject))
	return int(h.Sum32() % uint32(n))
}

// Shard returns the store that owns a subject.
func (s *Set) Shard(subject string) *store.Store { return s.shards[ShardIndex(subject, s.n)] }

// Append routes events to their subjects' shards and commits the per-shard groups
// in parallel. See package doc for atomicity / cross-shard-link limits.
func (s *Set) Append(now int64, events []*model.Event, links []model.CausalLink) error {
	// Assign ids up front so explicit links can be routed by their endpoints.
	if len(links) > 0 {
		for _, e := range events {
			if e != nil && e.EventID == "" {
				e.EventID = model.NewID()
			}
		}
	}
	idShard := map[string]int{}
	byEvents := map[int][]*model.Event{}
	for _, e := range events {
		if e == nil {
			return fmt.Errorf("shard: nil event")
		}
		idx := ShardIndex(e.Subject, s.n)
		byEvents[idx] = append(byEvents[idx], e)
		idShard[e.EventID] = idx
	}
	byLinks := map[int][]model.CausalLink{}
	for _, l := range links {
		fs, fok := idShard[l.From]
		ts, tok := idShard[l.To]
		switch {
		case fok && tok && fs == ts:
			byLinks[fs] = append(byLinks[fs], l) // both endpoints co-located → safe
		case fok && tok:
			return fmt.Errorf("shard: causal link %s->%s spans shards (%d vs %d) — not supported in sharded mode", l.From, l.To, fs, ts)
		default:
			return fmt.Errorf("shard: causal link %s->%s has an endpoint outside this batch, so co-location can't be verified — not supported in sharded mode", l.From, l.To)
		}
	}

	touched := map[int]struct{}{}
	for i := range byEvents {
		touched[i] = struct{}{}
	}
	for i := range byLinks {
		touched[i] = struct{}{}
	}

	var wg sync.WaitGroup
	errs := make([]error, s.n)
	for i := range touched {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			errs[i] = s.shards[i].Append(now, byEvents[i], byLinks[i])
		}(i)
	}
	wg.Wait()
	for i, err := range errs {
		if err != nil {
			return fmt.Errorf("shard %d: %w", i, err)
		}
	}
	return nil
}

// Current / History / AsOf route to the subject's owning shard.
func (s *Set) Current(subject, facet string) []*model.Event {
	return s.Shard(subject).Current(subject, facet)
}
func (s *Set) History(subject, facet string) []*model.Event {
	return s.Shard(subject).History(subject, facet)
}
func (s *Set) AsOf(subject, facet string, effectiveAt, knownAt int64) []*model.Event {
	return s.Shard(subject).AsOf(subject, facet, effectiveAt, knownAt)
}

// Subjects returns the union of subjects across all shards (collected in parallel).
func (s *Set) Subjects() []string {
	var mu sync.Mutex
	var all []string
	var wg sync.WaitGroup
	for _, st := range s.shards {
		wg.Add(1)
		go func(st *store.Store) {
			defer wg.Done()
			subs := st.Subjects()
			mu.Lock()
			all = append(all, subs...)
			mu.Unlock()
		}(st)
	}
	wg.Wait()
	sort.Strings(all)
	return all
}

// Close closes every shard, returning the first error.
func (s *Set) Close() error {
	var firstErr error
	for _, st := range s.shards {
		if st == nil {
			continue
		}
		if err := st.Close(); err != nil && firstErr == nil {
			firstErr = err
		}
	}
	return firstErr
}
