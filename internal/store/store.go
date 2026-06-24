// Package store implements Centauri's v0.1 storage engine.
//
// Design: an append-only JSONL log on disk (the only durable truth) plus
// in-memory indexes rebuilt on open. This is deliberately the simplest
// engine that preserves Centauri's semantics — immutability, atomic
// supersession, bi-temporal reads, causal traversal — behind the Engine
// interface, so swapping in Pebble/RocksDB later changes nothing above
// this package. State is a cache; the log is truth.
//
// Durability model:
//   - Every commit is written as one contiguous byte slice and fsynced
//     before in-memory indexes are updated (unless Options.NoSync).
//   - A crash mid-write leaves a torn tail; Open detects it, truncates
//     back to the last complete record, and recovers. Corruption that is
//     NOT at the tail (bit rot, manual edits) fails Open loudly — we never
//     silently skip records that good data depends on.
//   - If a write fails mid-commit, the store rolls the file back to the
//     pre-commit offset; memory is never updated with unflushed state.
package store

import (
	"bufio"
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"sort"
	"sync"
	"time"

	"github.com/proxima360/centauri/internal/model"
)

// ErrClosed is returned by writes after Close.
var ErrClosed = errors.New("store: closed")

// ErrFailed is returned after a write error that could not be rolled
// back. The on-disk log may have a torn tail; reopening recovers it.
var ErrFailed = errors.New("store: disabled after unrecoverable write error; reopen to recover")

// record is one line in the log: exactly one of the pointers is set.
type record struct {
	Event      *model.Event      `json:"event,omitempty"`
	Link       *model.CausalLink `json:"link,omitempty"`
	Enrichment *model.Enrichment `json:"enrichment,omitempty"`
	Schema     *model.Schema     `json:"schema,omitempty"`
	// Supersession marker: applied to a prior event at append time.
	Supersede *supersedeOp `json:"supersede,omitempty"`
}

func (r *record) empty() bool {
	return r.Event == nil && r.Link == nil && r.Enrichment == nil && r.Schema == nil && r.Supersede == nil
}

type supersedeOp struct {
	EventID      string `json:"event_id"`
	SupersededBy string `json:"superseded_by"`
	EffectiveEnd int64  `json:"effective_end"`
	RecordedTime int64  `json:"recorded_time"` // when the supersession was recorded (bi-temporal!)
}

// supersessionNote lets bi-temporal reads know WHEN we learned an event
// was superseded — so "as known at T" can ignore supersessions recorded
// after T.
type supersessionNote struct {
	recordedTime int64
}

// Options tunes durability.
type Options struct {
	// NoSync skips the per-commit fsync. Use only for bulk loads (seed)
	// where the whole load can be redone; Close still syncs once.
	NoSync bool
	// Lock acquires an exclusive single-writer lock next to the data file,
	// so a second writer (e.g. another device syncing the same folder)
	// can't open it and corrupt the log. Safe default for serve/desktop.
	Lock bool
	// LazyPayloads keeps event value maps on disk instead of in RAM: on
	// replay, only lightweight metadata is indexed and the payload is read
	// back (via ReadAt) on demand. Breaks the "whole dataset in RAM" limit
	// at the cost of a disk read + decode per cold value. Opt-in; OFF keeps
	// the original all-in-memory behavior exactly.
	LazyPayloads bool
	// GroupCommit funnels concurrent Append calls through one committer that
	// coalesces a burst into a single fsync (higher write throughput under
	// concurrency). The chain, ordering, and durability are unchanged. Opt-in;
	// OFF keeps the original inline append path exactly.
	GroupCommit bool
	// CheckpointEvery, if > 0, writes the recovery checkpoint on this cadence
	// while serving (not only on Close), so crash recovery replays only the log
	// since the last checkpoint — bounded recovery time regardless of uptime.
	// Applies to a plain log store (see AutoSealBytes for archive mode).
	CheckpointEvery time.Duration
	// AutoSealBytes, if > 0, seals the appendable tail into a new compressed
	// segment once it grows past this many bytes (archive-backed stores only).
	// Keeps the hot log — and therefore open/recovery time — bounded no matter
	// how much total history accumulates.
	AutoSealBytes int64
}

// Store is Centauri's storage engine.
type Store struct {
	mu     sync.RWMutex
	path   string
	f      *os.File
	size   int64 // committed file size; rollback target on write error
	opts      Options
	closed    bool
	failed    bool
	lockPath  string // single-writer lock marker; "" when unlocked
	archiveDir string // non-empty when opened via OpenArchive (segments + tail)

	events map[string]*model.Event // event_id -> event (Value nil when offloaded)
	// offsets is the on-disk location {byteOffset, length} of each event's
	// record, used to ReadAt its payload when LazyPayloads is on.
	offsets map[string][2]int64
	// bySubjectFacet holds event ids sorted by EffectiveTime ascending.
	bySubjectFacet map[string][]string        // subject|facet -> ordered event ids
	open           map[string]string          // subject|facet -> current (non-superseded) event id
	pending        map[string]map[string]bool // facet -> set of event ids with ActivationTime == 0
	causalOut      map[string][]model.CausalLink
	causalIn       map[string][]model.CausalLink
	byRef          map[string][]string            // source_ref -> event ids
	enrichments    map[string][]*model.Enrichment // target_event -> enrichments
	supersededAt   map[string]supersessionNote    // event_id -> when supersession was recorded
	subjects       map[string]bool
	// subjectFacets indexes which facets exist per subject so facet-less
	// reads (Current/AsOf/History "OF subject") enumerate just that
	// subject's facets instead of scanning every (subject|facet) stream.
	subjectFacets map[string]map[string]struct{}
	// fieldIndex is a secondary equality index over STRING value fields:
	// field -> value -> event ids that ever held it (current-ness is
	// verified on read, so it can stay append-only). cappedFields are
	// fields whose distinct values exceeded the cap; queries on them fall
	// back to a scan, keeping results correct.
	fieldIndex   map[string]map[string][]string
	cappedFields map[string]bool
	schemas      map[string][]*model.Schema // schema_id -> versions ascending
	vectors        map[string][]float32       // event_id -> latest embedding

	subs    map[int]chan *model.Event // watch subscribers
	nextSub int

	// chainHash is the tamper-evidence hash chain head (integrity.go).
	chainHash [32]byte

	// Group commit (opt-in via Options.GroupCommit). When enabled, concurrent
	// Append calls are funnelled through writeQ to a single committer goroutine
	// that coalesces a burst of appends into ONE fsync + one commit() — so write
	// throughput under concurrency is bounded by batch size, not by one fsync per
	// caller. The single chain, ordering, and write-then-apply are unchanged
	// (commit/writeApplyNotify are reused verbatim). writeQ == nil means the
	// feature is off and Append runs inline exactly as before.
	writeQ        chan *commitReq
	committerDone chan struct{}
	qmu           sync.RWMutex // guards qClosed and the send-vs-close race on writeQ
	qClosed       bool

	// Background maintenance: periodic checkpointing and/or auto-sealing.
	maintStop     chan struct{}
	maintDone     chan struct{}
	maintStopOnce sync.Once
}

// commitReq is one queued append awaiting the group committer.
type commitReq struct {
	now    int64
	events []*model.Event
	links  []model.CausalLink
	done   chan error // buffered (cap 1)
}

// maxGroupCommit bounds how many queued appends one fsync coalesces.
const maxGroupCommit = 256

func key(subject, facet string) string { return subject + "|" + facet }

// beats reports whether fact a should be "current" over b: the greatest
// effective_time, then recorded_time, then event_id (a stable, replica-
// independent tiebreaker). This is a deterministic last-write-wins register
// (cf. Cassandra cell timestamps); it makes the current-fact pointer a pure
// function of the set of facts, independent of apply order, so replicas that
// ingest the same facts in different orders agree. See docs/design-sync.md.
func beats(a, b *model.Event) bool {
	if a == nil {
		return false
	}
	if b == nil {
		return true
	}
	if a.EffectiveTime != b.EffectiveTime {
		return a.EffectiveTime > b.EffectiveTime
	}
	if a.RecordedTime != b.RecordedTime {
		return a.RecordedTime > b.RecordedTime
	}
	return a.EventID > b.EventID
}

// recomputeOpen sets open[k] to the deterministic current fact (the beats-max)
// among the non-superseded, non-lifecycle events for a subject/facet, or clears
// it when none remain. Called when a fact's eligibility changes (supersession).
func (s *Store) recomputeOpen(k string) {
	var best *model.Event
	for _, id := range s.bySubjectFacet[k] {
		e := s.events[id]
		if e == nil || e.SupersededBy != "" || e.Type == model.Activated {
			continue
		}
		if beats(e, best) {
			best = e
		}
	}
	if best == nil {
		delete(s.open, k)
	} else {
		s.open[k] = best.EventID
	}
}

// Open opens (or creates) a Centauri store at path and rebuilds indexes
// by replaying the log. A torn final record (crash mid-write) is
// truncated away; corruption anywhere else fails loudly.
func Open(path string) (*Store, error) { return OpenOptions(path, Options{}) }

// newStore returns a Store with all indexes initialized (no file opened yet).
// Shared by OpenOptions (single log) and OpenArchive (segments + tail) so the
// in-memory index layout can never drift between the two open paths.
func newStore(path string, opts Options) *Store {
	return &Store{
		path:           path,
		opts:           opts,
		events:         map[string]*model.Event{},
		offsets:        map[string][2]int64{},
		bySubjectFacet: map[string][]string{},
		open:           map[string]string{},
		pending:        map[string]map[string]bool{},
		causalOut:      map[string][]model.CausalLink{},
		causalIn:       map[string][]model.CausalLink{},
		byRef:          map[string][]string{},
		enrichments:    map[string][]*model.Enrichment{},
		supersededAt:   map[string]supersessionNote{},
		subjects:       map[string]bool{},
		subjectFacets:  map[string]map[string]struct{}{},
		fieldIndex:     map[string]map[string][]string{},
		cappedFields:   map[string]bool{},
		schemas:        map[string][]*model.Schema{},
		vectors:        map[string][]float32{},
		subs:           map[int]chan *model.Event{},
	}
}

// OpenOptions is Open with explicit durability options.
func OpenOptions(path string, opts Options) (*Store, error) {
	s := newStore(path, opts)
	f, err := os.OpenFile(path, os.O_CREATE|os.O_RDWR, 0o644)
	if err != nil {
		return nil, err
	}
	// A checkpoint (written on Close) lets Open replay only the log tail.
	// It is an optimization, never truth: any mismatch — including a tail
	// replay that fails — falls back to full replay of the log.
	start := s.tryLoadCheckpoint(f)
	good, err := s.replay(f, start)
	if err != nil && start > 0 {
		s.resetState()
		start = 0
		good, err = s.replay(f, 0)
	}
	if err != nil {
		f.Close()
		return nil, err
	}
	fi, err := f.Stat()
	if err != nil {
		f.Close()
		return nil, err
	}
	if good < fi.Size() {
		// Torn tail from a crash mid-write: discard the partial record.
		if err := f.Truncate(good); err != nil {
			f.Close()
			return nil, fmt.Errorf("store: truncate torn tail: %w", err)
		}
		if err := f.Sync(); err != nil {
			f.Close()
			return nil, err
		}
	}
	if _, err := f.Seek(good, io.SeekStart); err != nil {
		f.Close()
		return nil, err
	}
	s.f = f
	s.size = good
	if opts.Lock {
		lp, lerr := acquireLock(path)
		if lerr != nil {
			f.Close()
			return nil, lerr
		}
		s.lockPath = lp
	}
	// Start the group committer only after replay has reconstructed state.
	if opts.GroupCommit {
		s.writeQ = make(chan *commitReq, 4096)
		s.committerDone = make(chan struct{})
		go s.commitLoop()
	}
	s.startMaintenance() // periodic checkpoint (plain log)
	return s, nil
}

// replay applies every complete record from byte offset start and
// returns the byte offset just past the last good one. An unparseable
// region is tolerated only if it is the tail of the file (torn write);
// if any complete record follows it, that is real corruption and replay
// fails.
func (s *Store) replay(f *os.File, start int64) (int64, error) {
	if _, err := f.Seek(start, io.SeekStart); err != nil {
		return 0, err
	}
	rd := bufio.NewReaderSize(f, 1<<20)
	off := start
	for {
		line, rerr := rd.ReadBytes('\n')
		if rerr != nil && rerr != io.EOF {
			return 0, rerr
		}
		trimmed := bytes.TrimSpace(line)
		if len(trimmed) > 0 {
			var r record
			if jerr := json.Unmarshal(trimmed, &r); jerr != nil || r.empty() {
				later, lerr := hasCompleteRecordAfter(rd)
				if lerr != nil {
					// A read error past the corrupt region: we cannot
					// prove this is a torn tail, so refuse to truncate.
					return 0, fmt.Errorf("store: corrupt record at byte offset %d and read error while scanning tail: %v / %v", off, jerr, lerr)
				}
				if later {
					return 0, fmt.Errorf("store: corrupt record at byte offset %d (not at tail; refusing to skip): %v", off, jerr)
				}
				return off, nil // torn tail: caller truncates
			}
			s.apply(&r)
			if s.opts.LazyPayloads && r.Event != nil {
				// Offload the payload: keep metadata indexed, drop the
				// value map, and remember where to read it back from.
				if ev := s.events[r.Event.EventID]; ev != nil {
					ev.Value = nil
					s.offsets[r.Event.EventID] = [2]int64{off, int64(len(line))}
				}
			}
		}
		if len(line) > 0 && line[len(line)-1] == '\n' {
			// Read-side mirror of writeApplyNotify: apply-then-chainExtend over
			// the exact on-disk bytes reproduces the writers' chain (invariant 4).
			s.chainExtend(line) // tamper-evidence chain covers every kept line
		}
		off += int64(len(line))
		if rerr == io.EOF {
			return off, nil
		}
	}
}

// hasCompleteRecordAfter reports whether any parseable record remains in
// the reader — used to distinguish a torn tail from mid-file corruption.
// A non-EOF read error is returned so the caller fails Open rather than
// truncating data it could not inspect.
func hasCompleteRecordAfter(rd *bufio.Reader) (bool, error) {
	for {
		line, err := rd.ReadBytes('\n')
		trimmed := bytes.TrimSpace(line)
		if len(trimmed) > 0 {
			var r record
			if json.Unmarshal(trimmed, &r) == nil && !r.empty() {
				return true, nil
			}
		}
		if err == io.EOF {
			return false, nil
		}
		if err != nil {
			return false, err
		}
	}
}

// Close syncs and closes the log, writing a checkpoint so the next Open
// can skip full replay. Safe to call more than once.
func (s *Store) Close() error {
	// Stop the group committer first, WITHOUT holding s.mu — it needs s.mu to
	// flush its in-flight group. Closing writeQ (under qmu, so no send races the
	// close) makes commitLoop drain the remaining queued appends and exit.
	if s.writeQ != nil {
		s.qmu.Lock()
		stop := !s.qClosed
		if stop {
			s.qClosed = true
			close(s.writeQ)
		}
		s.qmu.Unlock()
		if stop {
			<-s.committerDone
		}
	}
	// Stop background maintenance before taking s.mu (it acquires s.mu itself).
	s.stopMaintenance()
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.closed {
		return nil
	}
	s.closed = true
	for id, ch := range s.subs {
		close(ch)
		delete(s.subs, id)
	}
	serr := s.f.Sync()
	if serr == nil && !s.failed {
		// Best-effort: a missing/stale checkpoint only costs replay time.
		_ = s.writeCheckpoint()
	}
	cerr := s.f.Close()
	releaseLock(s.lockPath)
	if serr != nil {
		return serr
	}
	return cerr
}

// apply updates in-memory indexes for one record (used on replay & write).
// Everything apply does must be derivable from the record alone, so that
// replay reconstructs exactly the state writes produced — any state set
// outside apply would be lost on restart.
func (s *Store) apply(r *record) {
	switch {
	case r.Event != nil:
		e := r.Event
		s.events[e.EventID] = e
		s.subjects[e.Subject] = true
		if s.subjectFacets[e.Subject] == nil {
			s.subjectFacets[e.Subject] = map[string]struct{}{}
		}
		s.subjectFacets[e.Subject][e.Facet] = struct{}{}
		s.indexEventFields(e)
		k := key(e.Subject, e.Facet)
		ids := s.bySubjectFacet[k]
		// insert keeping EffectiveTime ascending order
		i := sort.Search(len(ids), func(i int) bool {
			return s.events[ids[i]].EffectiveTime > e.EffectiveTime
		})
		ids = append(ids, "")
		copy(ids[i+1:], ids[i:])
		ids[i] = e.EventID
		s.bySubjectFacet[k] = ids
		if e.SupersededBy == "" && e.Type != model.Activated {
			// Lifecycle markers never own the current-state pointer;
			// only fact-bearing events do. The current fact is the
			// deterministic max by (effective, recorded, id) among
			// non-superseded events — last-write-wins, order-independent —
			// so the pointer converges across replicas regardless of
			// ingest order (Cassandra-style; see docs/design-sync.md).
			if cur := s.open[k]; cur == "" || beats(e, s.events[cur]) {
				s.open[k] = e.EventID
			}
		}
		if e.ActivationTime == 0 && e.Type == model.Distributed {
			if s.pending[e.Facet] == nil {
				s.pending[e.Facet] = map[string]bool{}
			}
			s.pending[e.Facet][e.EventID] = true
		}
		// Activation markers replay onto their target: set ActivationTime
		// and clear the wedge bit (idempotent on log replay).
		if e.Type == model.Activated {
			if tgt, ok := e.Value["activates"].(string); ok {
				if te, ok := s.events[tgt]; ok {
					te.ActivationTime = e.EffectiveTime
					if set, ok := s.pending[te.Facet]; ok {
						delete(set, tgt)
					}
				}
			}
		}
		if e.SourceRef != "" {
			s.byRef[e.SourceRef] = append(s.byRef[e.SourceRef], e.EventID)
		}
	case r.Supersede != nil:
		op := r.Supersede
		if e, ok := s.events[op.EventID]; ok {
			e.SupersededBy = op.SupersededBy
			e.EffectiveEnd = op.EffectiveEnd
			s.supersededAt[op.EventID] = supersessionNote{recordedTime: op.RecordedTime}
			// The superseded fact is no longer eligible to be current;
			// recompute the deterministic max over the remaining
			// non-superseded facts (restores the next-best, e.g. when a
			// correction carries an earlier effective time than the fact
			// it replaces). Order-independent → replica-convergent.
			s.recomputeOpen(key(e.Subject, e.Facet))
			if set, ok := s.pending[e.Facet]; ok {
				delete(set, op.EventID)
			}
		}
	case r.Link != nil:
		l := *r.Link
		s.causalOut[l.From] = append(s.causalOut[l.From], l)
		s.causalIn[l.To] = append(s.causalIn[l.To], l)
	case r.Enrichment != nil:
		en := r.Enrichment
		// Supersession of prior enrichments happens HERE so it is
		// re-derived on replay — it must never live only in memory.
		for _, prior := range s.enrichments[en.TargetEvent] {
			if prior.Kind == en.Kind && prior.SupersededBy == "" && prior.EnrichmentID != en.EnrichmentID {
				prior.SupersededBy = en.EnrichmentID
			}
		}
		s.enrichments[en.TargetEvent] = append(s.enrichments[en.TargetEvent], en)
		// Embeddings feed the similarity index; the latest wins (the
		// prior was just superseded above).
		if en.Kind == model.EmbeddingKind {
			if vec := parseVector(en.Result["vector"]); vec != nil {
				s.vectors[en.TargetEvent] = vec
			}
		}
	case r.Schema != nil:
		sc := r.Schema
		versions := s.schemas[sc.SchemaID]
		if len(versions) > 0 {
			versions[len(versions)-1].SupersededBy = sc.Ref()
		}
		s.schemas[sc.SchemaID] = append(versions, sc)
	}
}

// commit durably writes a batch of records, then applies them to memory.
// The batch is marshaled fully before any byte hits the file, written in
// one Write call, and fsynced. On failure the file is truncated back to
// the pre-commit offset so disk and memory stay consistent; if even that
// fails, the store disables writes (ErrFailed) rather than diverge.
// Callers must hold s.mu and have checked closed/failed.
func (s *Store) commit(recs []*record) error {
	var buf bytes.Buffer
	for _, r := range recs {
		b, err := json.Marshal(r)
		if err != nil {
			return fmt.Errorf("store: marshal record: %w", err)
		}
		buf.Write(b)
		buf.WriteByte('\n')
	}
	return s.writeApplyNotify(buf.Bytes(), recs)
}

// writeApplyNotify is the single durable-write path shared by commit and
// IngestRaw. `data` MUST be the complete, newline-terminated log line(s) whose
// parsed records are `recs`. It writes the bytes, fsyncs (unless NoSync),
// advances the tamper-evidence hash chain over the EXACT bytes written, then
// applies the records to the in-memory index and notifies watchers — in that
// order (write-then-apply; chain over committed bytes). Routing both writers
// through here is what keeps invariant 4 (the chain covers every committed line
// identically across commit and IngestRaw) true by construction. The replay
// path (see replay) is the read-side mirror: it does not write or fsync, but
// applies then chainExtends the same on-disk bytes, reproducing this chain.
// Caller holds s.mu (write).
func (s *Store) writeApplyNotify(data []byte, recs []*record) error {
	if _, err := s.f.Write(data); err != nil {
		s.rollback()
		return fmt.Errorf("store: write: %w", err)
	}
	if !s.opts.NoSync {
		if err := s.f.Sync(); err != nil {
			s.rollback()
			return fmt.Errorf("store: fsync: %w", err)
		}
	}
	s.size += int64(len(data))
	s.chainExtendBuf(data)
	for _, r := range recs {
		s.apply(r)
	}
	// Notify watchers. Non-blocking: a slow subscriber drops messages
	// rather than stalling commits; watchers are a cache-invalidation
	// signal, the log remains the truth.
	for _, r := range recs {
		if r.Event == nil {
			continue
		}
		for _, ch := range s.subs {
			select {
			case ch <- r.Event:
			default:
			}
		}
	}
	return nil
}

// rollback restores the file to the last committed offset after a failed
// write. Memory has not been touched, so on success the store remains
// fully usable; on failure it is fenced off.
func (s *Store) rollback() {
	if err := s.f.Truncate(s.size); err != nil {
		s.failed = true
		return
	}
	if _, err := s.f.Seek(s.size, io.SeekStart); err != nil {
		s.failed = true
		return
	}
	if err := s.f.Sync(); err != nil {
		// After a failed fsync the kernel may have dropped dirty pages;
		// we no longer know what is on disk. Fence writes; reopen recovers.
		s.failed = true
	}
}

func (s *Store) writable() error {
	if s.closed {
		return ErrClosed
	}
	if s.failed {
		return ErrFailed
	}
	return nil
}

func validType(t model.EventType) bool {
	switch t {
	case model.Intent, model.Distributed, model.Activated, model.Observed, model.Correction:
		return true
	}
	return false
}

// Append atomically appends events and links. For each appended event,
// the previously-open event on the same (subject, facet) — including one
// appended earlier in this same batch — is superseded: its SupersededBy
// and EffectiveEnd are set exactly once, and a SUPERSEDES link is written
// — one batch, one consistent transition. Events' RecordedTime is set by
// the server clock; client values are ignored, which is what keeps
// "as known at" honest. SupersededBy/EffectiveEnd are likewise
// server-managed and reset on ingest.
func (s *Store) Append(now int64, events []*model.Event, links []model.CausalLink) error {
	// Group-commit path: hand off to the committer, which coalesces a burst of
	// concurrent appends into one fsync. writeQ is set once at Open and never
	// changes, so reading it without the lock is safe.
	if s.writeQ != nil {
		req := &commitReq{now: now, events: events, links: links, done: make(chan error, 1)}
		s.qmu.RLock()
		if s.qClosed {
			s.qmu.RUnlock()
			return ErrClosed
		}
		s.writeQ <- req // RLock held so Close can't close the channel mid-send
		s.qmu.RUnlock()
		return <-req.done
	}
	// Inline path (group commit off): identical to the original behaviour.
	s.mu.Lock()
	defer s.mu.Unlock()
	recs, err := s.prepareBatch(now, events, links, map[string]string{}, map[string]bool{})
	if err != nil {
		return err
	}
	return s.commit(recs)
}

// prepareBatch validates one append and builds its record batch, threading
// `overlay` (open-pointer changes accumulated within this commit group) and
// `seen` (event ids already taken in this group) so several appends committed
// together still chain and de-duplicate correctly. It mutates server-managed
// event fields (RecordedTime etc.). Caller holds s.mu; it does NOT write.
func (s *Store) prepareBatch(now int64, events []*model.Event, links []model.CausalLink, overlay map[string]string, seen map[string]bool) ([]*record, error) {
	if err := s.writable(); err != nil {
		return nil, err
	}
	if now <= 0 {
		return nil, errors.New("append: now must be a positive UnixMicro timestamp")
	}

	// Validate everything before writing anything: a batch is all-or-nothing.
	for i, e := range events {
		if e == nil {
			return nil, fmt.Errorf("append: event %d is nil", i)
		}
		if e.Subject == "" || e.Facet == "" {
			return nil, errors.New("append: event requires subject and facet")
		}
		if !validType(e.Type) {
			return nil, fmt.Errorf("append: event %d has unknown type %q", i, e.Type)
		}
		if !(e.Confidence >= 0 && e.Confidence <= 1) { // NaN-safe
			return nil, fmt.Errorf("append: event %d confidence %v outside [0,1]", i, e.Confidence)
		}
		if e.EventID == "" {
			e.EventID = model.NewID()
		}
		if _, dup := s.events[e.EventID]; dup || seen[e.EventID] {
			return nil, fmt.Errorf("append: duplicate event id %s (events are immutable)", e.EventID)
		}
		seen[e.EventID] = true
		if e.EffectiveTime <= 0 {
			e.EffectiveTime = now
		}
		// Server-managed fields: client values are ignored.
		e.RecordedTime = now
		e.SupersededBy = ""
		e.EffectiveEnd = 0
		if e.Type != model.Activated {
			e.ActivationTime = 0 // the wedge bit is set only via Activate
		}
		if e.SchemaID != "" {
			if err := s.validateAgainstSchema(e); err != nil {
				return nil, fmt.Errorf("append: event %d: %w", i, err)
			}
		}
	}
	for i, l := range links {
		if l.From == "" || l.To == "" || l.Type == "" {
			return nil, fmt.Errorf("append: link %d requires from, to, and type", i)
		}
	}

	var batch []*record
	// openNow overlays this batch's supersessions onto s.open (plus any earlier
	// appends in the same commit group) so events on the same (subject, facet)
	// chain correctly.
	openNow := func(k string) (string, bool) {
		if id, ok := overlay[k]; ok {
			return id, true
		}
		id, ok := s.open[k]
		return id, ok
	}
	for _, e := range events {
		k := key(e.Subject, e.Facet)
		// Crash-ordering invariant: the new event is logged BEFORE its
		// supersession marker. If a crash tears the batch between them,
		// replay sees the new event (which already overwrote the open
		// pointer) and merely misses the marker — the old, acknowledged
		// event is never orphaned by a marker pointing at a lost event.
		batch = append(batch, &record{Event: e})
		if prevID, ok := openNow(k); ok && e.Type != model.Activated {
			// Activation events update the same logical fact's facet
			// lifecycle; they do not supersede the distribution they fulfil.
			batch = append(batch, &record{Supersede: &supersedeOp{
				EventID:      prevID,
				SupersededBy: e.EventID,
				EffectiveEnd: e.EffectiveTime,
				RecordedTime: now,
			}})
			batch = append(batch, &record{Link: &model.CausalLink{
				From: e.EventID, To: prevID, Type: model.Supersedes,
			}})
		}
		if e.Type != model.Activated {
			overlay[k] = e.EventID
		}
	}
	for i := range links {
		batch = append(batch, &record{Link: &links[i]})
	}
	return batch, nil
}

// commitLoop is the group committer: it drains a burst of queued appends,
// prepares them in order (threading one overlay + seen set so they chain), then
// commits the merged records with a SINGLE fsync. Reuses commit() verbatim, so
// durability, the hash chain, and write-then-apply are unchanged.
func (s *Store) commitLoop() {
	for req := range s.writeQ {
		group := []*commitReq{req}
		// Opportunistically coalesce whatever is already queued (no waiting:
		// low load → groups of one; high load → big groups, fewer fsyncs).
		for len(group) < maxGroupCommit {
			select {
			case r := <-s.writeQ:
				group = append(group, r)
			default:
				goto run
			}
		}
	run:
		s.commitGroup(group)
	}
	close(s.committerDone)
}

func (s *Store) commitGroup(group []*commitReq) {
	s.mu.Lock()
	overlay := map[string]string{}
	seen := map[string]bool{}
	var merged []*record
	errs := make([]error, len(group))
	committed := false
	for i, r := range group {
		recs, err := s.prepareBatch(r.now, r.events, r.links, overlay, seen)
		if err != nil {
			errs[i] = err
			continue
		}
		merged = append(merged, recs...)
		committed = true
	}
	var cerr error
	if committed {
		cerr = s.commit(merged) // one write + one fsync + apply-all, in order
	}
	s.mu.Unlock()
	for i, r := range group {
		if errs[i] != nil {
			r.done <- errs[i] // per-request validation error
		} else {
			r.done <- cerr // shared commit result (nil, or a write/fsync error)
		}
	}
}

// Activate marks a distributed event as activated by its facet at time t.
// This closes the wedge for that event. The mutation of the target event
// happens via apply (replaying the activation marker), never directly —
// so memory only changes after the record is durable.
func (s *Store) Activate(eventID string, t int64) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if err := s.writable(); err != nil {
		return err
	}
	if t <= 0 {
		return errors.New("activate: time must be a positive UnixMicro timestamp")
	}
	e, ok := s.events[eventID]
	if !ok {
		return fmt.Errorf("activate: unknown event %s", eventID)
	}
	if e.Type != model.Distributed {
		return fmt.Errorf("activate: event %s is %s, not DISTRIBUTED", eventID, e.Type)
	}
	if e.ActivationTime != 0 {
		return fmt.Errorf("activate: event %s already activated at %d", eventID, e.ActivationTime)
	}
	// Activation is itself an event in the log: a copy-on-write marker.
	// RecordedTime mirrors t so backdated activations (seed data) keep a
	// consistent timeline.
	act := &model.Event{
		EventID: model.NewID(), Subject: e.Subject, Facet: e.Facet,
		Type: model.Activated, Value: map[string]any{"activates": eventID},
		EffectiveTime: t, RecordedTime: t,
		Provenance: model.SystemFeed, Confidence: 1.0,
		SourceSystem: "FACET:" + e.Facet,
	}
	return s.commit([]*record{
		{Event: act},
		{Link: &model.CausalLink{From: eventID, To: act.EventID, Type: model.ActivatedBy}},
	})
}

// ---- Queries ----

// Current returns the open (non-superseded) event for subject, optionally
// filtered to one facet. O(1) per facet via the open index.
// hydrate returns an event with its payload loaded. When LazyPayloads is
// off (or the value is already present) it returns e unchanged. Otherwise
// it reads the record at the recorded offset and returns a copy with the
// value filled in, leaving the stored stub lean. Caller holds s.mu (R or W).
func (s *Store) hydrate(e *model.Event) *model.Event {
	if e == nil || !s.opts.LazyPayloads || e.Value != nil {
		return e
	}
	pos, ok := s.offsets[e.EventID]
	if !ok || s.f == nil {
		return e
	}
	buf := make([]byte, pos[1])
	if _, err := s.f.ReadAt(buf, pos[0]); err != nil {
		return e // best effort: metadata is still valid
	}
	var r record
	if json.Unmarshal(bytes.TrimSpace(buf), &r) != nil || r.Event == nil {
		return e
	}
	cp := *e
	cp.Value = r.Event.Value
	return &cp
}

// hydrateAll hydrates a slice in place-returning a new slice. Caller holds
// s.mu (this performs disk I/O under the lock when LazyPayloads is on; used by
// internal callers that already hold the lock and inspect values inline).
func (s *Store) hydrateAll(evs []*model.Event) []*model.Event {
	if !s.opts.LazyPayloads {
		return evs
	}
	for i, e := range evs {
		evs[i] = s.hydrate(e)
	}
	return evs
}

// hydrateAllSafe hydrates lazy stubs WITHOUT holding s.mu across the disk read,
// so a slow ReadAt can't stall writers (which need the exclusive lock). The
// caller must NOT hold s.mu. It briefly RLocks only to (a) snapshot each
// event's payload offset and (b) copy the event struct — the copy under lock
// avoids racing the post-append ActivationTime/SupersededBy mutations in
// apply(); the payload bytes on disk are immutable, so reading them off-lock is
// safe. Used by the hot read paths (Current/AsOf/History).
func (s *Store) hydrateAllSafe(evs []*model.Event) []*model.Event {
	if !s.opts.LazyPayloads {
		return evs
	}
	type job struct {
		idx      int
		off, ln  int64
		cp       *model.Event
	}
	var jobs []job
	s.mu.RLock()
	f := s.f
	for i, e := range evs {
		if e == nil || e.Value != nil {
			continue
		}
		if pos, ok := s.offsets[e.EventID]; ok && f != nil {
			cp := *e
			jobs = append(jobs, job{idx: i, off: pos[0], ln: pos[1], cp: &cp})
		}
	}
	s.mu.RUnlock()
	for _, j := range jobs {
		buf := make([]byte, j.ln)
		if _, err := f.ReadAt(buf, j.off); err != nil {
			continue // best effort: metadata copy is still valid
		}
		var r record
		if json.Unmarshal(bytes.TrimSpace(buf), &r) == nil && r.Event != nil {
			j.cp.Value = r.Event.Value
			evs[j.idx] = j.cp
		}
	}
	return evs
}

func (s *Store) Current(subject, facet string) []*model.Event {
	raw := func() []*model.Event {
		s.mu.RLock()
		defer s.mu.RUnlock()
		if facet != "" {
			var out []*model.Event
			if id, ok := s.open[key(subject, facet)]; ok {
				out = append(out, s.events[id])
			}
			return out
		}
		return s.currentLocked(subject)
	}()
	return s.hydrateAllSafe(raw) // disk read (lazy) happens off-lock
}

// AsOf answers the bi-temporal point query: for each facet of subject,
// the event that was effective at effectiveAt — as known at knownAt.
// knownAt==0 means "as known now".
//
// Correctness note: we do NOT rely on EffectiveEnd for belief queries,
// because EffectiveEnd is written by supersessions that may have been
// recorded AFTER knownAt. Instead we select, among events recorded by
// knownAt, the one with the greatest EffectiveTime <= effectiveAt whose
// supersession (if any) was also recorded by knownAt only if the
// superseding event qualifies. The simple max-effective rule below is
// equivalent for single-timeline facets.
func (s *Store) AsOf(subject, facet string, effectiveAt, knownAt int64) []*model.Event {
	raw := func() []*model.Event {
		s.mu.RLock()
		defer s.mu.RUnlock()
		return s.asOfLocked(subject, facet, effectiveAt, knownAt)
	}()
	return s.hydrateAllSafe(raw) // disk read (lazy) happens off-lock
}

func (s *Store) asOfLocked(subject, facet string, effectiveAt, knownAt int64) []*model.Event {
	facets := s.facetsFor(subject, facet)
	out := []*model.Event{}
	for _, fc := range facets {
		ids := s.bySubjectFacet[key(subject, fc)]
		var best *model.Event
		for _, id := range ids {
			e := s.events[id]
			if e.Type == model.Activated {
				continue // lifecycle markers, not price-bearing facts
			}
			if knownAt > 0 && e.RecordedTime > knownAt {
				continue // we hadn't learned this yet
			}
			if e.EffectiveTime > effectiveAt {
				continue // not yet effective at that moment
			}
			if best == nil || e.EffectiveTime >= best.EffectiveTime {
				best = e
			}
		}
		if best != nil {
			out = append(out, best)
		}
	}
	return out // RAW; callers hydrate (AsOf off-lock, Context under lock)
}

// History returns the full ordered event timeline for subject/facet.
func (s *Store) History(subject, facet string) []*model.Event {
	return s.HistoryN(subject, facet, 0)
}

// HistoryN is History capped to the most recent `limit` events (limit<=0 =
// unlimited), in ascending effective-time order. The cap bounds memory and
// lock-hold time for subjects with very long histories; for streaming an
// entire large history prefer CDC (/v1/changes) over materializing it here.
func (s *Store) HistoryN(subject, facet string, limit int) []*model.Event {
	raw := func() []*model.Event {
		s.mu.RLock()
		defer s.mu.RUnlock()
		var out []*model.Event
		for _, fc := range s.facetsFor(subject, facet) {
			for _, id := range s.bySubjectFacet[key(subject, fc)] {
				out = append(out, s.events[id])
			}
		}
		sort.Slice(out, func(i, j int) bool { return out[i].EffectiveTime < out[j].EffectiveTime })
		if limit > 0 && len(out) > limit {
			out = out[len(out)-limit:] // keep the most recent `limit`
		}
		return out
	}()
	return s.hydrateAllSafe(raw) // disk read (lazy) happens off-lock
}

// Pending returns distributed-but-unactivated events on a facet older
// than olderThan (UnixMicro recorded time). olderThan==0 returns all.
// This is the wedge scan.
func (s *Store) Pending(facet string, olderThan int64) []*model.Event {
	s.mu.RLock()
	defer s.mu.RUnlock()
	out := []*model.Event{}
	for id := range s.pending[facet] {
		e := s.events[id]
		if olderThan == 0 || e.RecordedTime < olderThan {
			out = append(out, e)
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i].RecordedTime < out[j].RecordedTime })
	return s.hydrateAll(out)
}

// Disagreements returns subjects whose open facets disagree on field.
func (s *Store) Disagreements(field string) map[string][]*model.Event {
	s.mu.RLock()
	defer s.mu.RUnlock()
	out := map[string][]*model.Event{}
	for subject := range s.subjects {
		evs := s.hydrateAll(s.currentLocked(subject)) // values needed inline
		vals := map[string]bool{}
		for _, e := range evs {
			if v, ok := e.Value[field]; ok {
				vals[fmt.Sprint(v)] = true
			}
		}
		if len(vals) > 1 {
			out[subject] = evs
		}
	}
	return out
}

// currentLocked returns RAW (un-hydrated) open events; the caller hydrates
// (Current/AsOf do so off-lock; callers that read values inline — Disagreements,
// Context — hydrate under the lock they already hold).
func (s *Store) currentLocked(subject string) []*model.Event {
	var out []*model.Event
	for f := range s.subjectFacets[subject] {
		if id, ok := s.open[key(subject, f)]; ok {
			out = append(out, s.events[id])
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Facet < out[j].Facet })
	return out
}

// TraceNode is one hop in a causal walk.
type TraceNode struct {
	Event *model.Event   `json:"event"`
	Link  model.LinkType `json:"via,omitempty"`
	Depth int            `json:"depth"`
}

// Trace walks the causal graph from eventID. direction is "cause"
// (walk inbound: what led to this) or "effect" (walk outbound).
func (s *Store) Trace(eventID, direction string, maxDepth int) []TraceNode {
	s.mu.RLock()
	defer s.mu.RUnlock()
	var out []TraceNode
	seen := map[string]bool{}
	var walk func(id string, depth int, via model.LinkType)
	walk = func(id string, depth int, via model.LinkType) {
		if depth > maxDepth || seen[id] {
			return
		}
		seen[id] = true
		if e, ok := s.events[id]; ok {
			out = append(out, TraceNode{Event: s.hydrate(e), Link: via, Depth: depth})
		}
		if direction == "cause" {
			// inbound edges: From caused To==id, so the cause is From.
			for _, l := range s.causalIn[id] {
				walk(l.From, depth+1, l.Type)
			}
		} else {
			for _, l := range s.causalOut[id] {
				walk(l.To, depth+1, l.Type)
			}
		}
	}
	walk(eventID, 0, "")
	return out
}

// TraceVia is Trace restricted to causal edges of a single link type.
// via=="" or "*" follows every type (identical to Trace). Read-only.
// Powers the CeQL MATCH operator's VIA filter.
func (s *Store) TraceVia(eventID, direction string, maxDepth int, via string) []TraceNode {
	s.mu.RLock()
	defer s.mu.RUnlock()
	anyType := via == "" || via == "*"
	var out []TraceNode
	seen := map[string]bool{}
	var walk func(id string, depth int, edge model.LinkType)
	walk = func(id string, depth int, edge model.LinkType) {
		if depth > maxDepth || seen[id] {
			return
		}
		seen[id] = true
		if e, ok := s.events[id]; ok {
			out = append(out, TraceNode{Event: s.hydrate(e), Link: edge, Depth: depth})
		}
		if direction == "cause" {
			for _, l := range s.causalIn[id] {
				if anyType || string(l.Type) == via {
					walk(l.From, depth+1, l.Type)
				}
			}
		} else {
			for _, l := range s.causalOut[id] {
				if anyType || string(l.Type) == via {
					walk(l.To, depth+1, l.Type)
				}
			}
		}
	}
	walk(eventID, 0, "")
	return out
}

// CausalEdges returns a copy of every causal link in the lineage graph
// (read-only). The topology engine uses it to detect cycles — a directed
// cycle here is a data-integrity alarm, since lineage must be acyclic.
func (s *Store) CausalEdges() []model.CausalLink {
	s.mu.RLock()
	defer s.mu.RUnlock()
	var out []model.CausalLink
	for _, ls := range s.causalOut {
		out = append(out, ls...)
	}
	return out
}

// ByRef resolves an outside-world id (sendnow batch, job run) to events.
func (s *Store) ByRef(ref string) []*model.Event {
	s.mu.RLock()
	defer s.mu.RUnlock()
	var out []*model.Event
	for _, id := range s.byRef[ref] {
		out = append(out, s.events[id])
	}
	return s.hydrateAll(out)
}

// AddEnrichment appends an AI-written fact and its lineage link, and
// supersedes any prior enrichment of the same kind on the same target.
// The supersession is derived inside apply, so it survives replay.
func (s *Store) AddEnrichment(en *model.Enrichment) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if err := s.writable(); err != nil {
		return err
	}
	if en == nil {
		return errors.New("enrich: enrichment is nil")
	}
	if en.Kind == "" {
		return errors.New("enrich: kind is required")
	}
	if !(en.Confidence >= 0 && en.Confidence <= 1) { // NaN-safe
		return fmt.Errorf("enrich: confidence %v outside [0,1]", en.Confidence)
	}
	if _, ok := s.events[en.TargetEvent]; !ok {
		return fmt.Errorf("enrich: unknown target event %s", en.TargetEvent)
	}
	if en.EnrichmentID == "" {
		en.EnrichmentID = model.NewID()
	}
	if en.CreatedAt == 0 {
		en.CreatedAt = time.Now().UnixMicro()
	}
	en.SupersededBy = "" // server-managed
	return s.commit([]*record{
		{Enrichment: en},
		{Link: &model.CausalLink{From: en.TargetEvent, To: en.EnrichmentID, Type: model.EnrichedFrom}},
	})
}

// EnrichmentsFor returns enrichments on an event (latest first).
func (s *Store) EnrichmentsFor(eventID string) []*model.Enrichment {
	s.mu.RLock()
	defer s.mu.RUnlock()
	ens := append([]*model.Enrichment{}, s.enrichments[eventID]...)
	sort.Slice(ens, func(i, j int) bool { return ens[i].CreatedAt > ens[j].CreatedAt })
	return ens
}

// Subjects lists all known subjects (for the demo UI).
func (s *Store) Subjects() []string {
	s.mu.RLock()
	defer s.mu.RUnlock()
	var out []string
	for sub := range s.subjects {
		out = append(out, sub)
	}
	sort.Strings(out)
	return out
}

// MaxRecordedTime returns the transaction-time (UnixMicro) of the most
// recently committed event, or 0 if the store is empty. It is the
// timestamp of the "last commit" and is used to target ROLLBACK TO LAST.
// Read-only: it never mutates state.
func (s *Store) MaxRecordedTime() int64 {
	s.mu.RLock()
	defer s.mu.RUnlock()
	var max int64
	for _, e := range s.events {
		if e.RecordedTime > max {
			max = e.RecordedTime
		}
	}
	return max
}

// maxIndexDistinct caps the distinct values a single field may hold in the
// secondary index. A field that exceeds it (e.g. a free-text note) is marked
// "capped" and its queries fall back to a scan — bounding index memory.
const maxIndexDistinct = 50000

// indexEventFields adds an event's STRING value fields to the secondary
// equality index. Append-only (current-ness is checked on read), so it never
// needs to undo work on supersession. Caller holds s.mu.
func (s *Store) indexEventFields(e *model.Event) {
	for fk, fv := range e.Value {
		sv, ok := fv.(string)
		if !ok {
			continue
		}
		if s.cappedFields[fk] {
			continue
		}
		m := s.fieldIndex[fk]
		if m == nil {
			m = map[string][]string{}
			s.fieldIndex[fk] = m
		}
		if _, seen := m[sv]; !seen && len(m) >= maxIndexDistinct {
			s.cappedFields[fk] = true
			delete(s.fieldIndex, fk) // too high-cardinality to index; scan instead
			continue
		}
		m[sv] = append(m[sv], e.EventID)
	}
}

// CurrentByField returns the current (open) events whose string field equals
// val, using the secondary index. ok=false means the field isn't usefully
// indexed (unknown or capped) and the caller must scan. Read-only.
func (s *Store) CurrentByField(field, val string) ([]*model.Event, bool) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	if s.cappedFields[field] {
		return nil, false
	}
	m, ok := s.fieldIndex[field]
	if !ok {
		return nil, false
	}
	var out []*model.Event
	for _, id := range m[val] {
		e := s.events[id]
		if e == nil || s.open[key(e.Subject, e.Facet)] != id {
			continue // not the current fact for its (subject, facet)
		}
		out = append(out, s.hydrate(e))
	}
	return out, true
}

// CausalDegree returns how many causal links touch an event (inbound +
// outbound) — a cheap centrality signal: hub facts that explain, or were
// explained by, many others rank as more significant. Read-only.
func (s *Store) CausalDegree(eventID string) int {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return len(s.causalIn[eventID]) + len(s.causalOut[eventID])
}

// Stats returns basic counters.
func (s *Store) Stats() map[string]int {
	s.mu.RLock()
	defer s.mu.RUnlock()
	pend := 0
	for _, set := range s.pending {
		pend += len(set)
	}
	links := 0
	for _, ls := range s.causalOut {
		links += len(ls)
	}
	return map[string]int{
		"events":   len(s.events),
		"subjects": len(s.subjects),
		"open":     len(s.open),
		"pending":  pend,
		"links":    links,
	}
}

func (s *Store) facetsFor(subject, facet string) []string {
	if facet != "" {
		return []string{facet}
	}
	fs := s.subjectFacets[subject]
	out := make([]string, 0, len(fs))
	for f := range fs {
		out = append(out, f)
	}
	sort.Strings(out)
	return out
}
