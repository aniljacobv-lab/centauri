# Centauri — Concurrency, Locking & Production Readiness Suggestions

**Generated:** 2026-06-18  
**Consolidated from:**
- Full codebase review (CLAUDE.md invariants, store paths, API, architect)
- Deep concurrency & write-lock analysis (RWMutex, fsync, LazyPayloads, replication, multi-DB)
- Prior review file: `centauri-review-fdd989cc.md`

**Scope:** All write paths (`Append`, `commit`, `Ingest*`, `Activate`, `AddEnrichment`, `PutSchema`), read paths, watches, named environments, replication (follow/sync), HTTP API, and durability model.

---

## Executive Summary

Centauri has a **solid, simple, and correct core** built around a single append-only log + replayable in-memory indexes protected by one `RWMutex`. The design deliberately favors simplicity and strong invariants over complex locking.

**However, in real-world production** (moderate-to-high write concurrency, mixed read/write workloads, named/multi-tenant DBs, replication, or LazyPayloads on real storage), several patterns become bottlenecks or correctness risks:

- Every normal write holds an **exclusive lock through fsync**.
- `LazyPayloads` performs disk I/O while holding read locks.
- Named DB / multi-environment features have incomplete isolation and locking.
- No rate limiting, write batching, or observability around contention.

This document collects **all actionable suggestions** (from both reviews) with priorities, concrete recommendations, and production impact.

---

## Prioritized Suggestions

### P0 — Critical (Correctness & Multi-Tenant Safety)

#### 1. ACL / Scoped Tokens Do Not Honor `?db=` Named Databases
- **Severity:** bug
- **Files:** `internal/api/api.go:84` (lookupACL), `~264`, `~1050` (auth), `~1379` (handleACL)
- **Description:** `lookupACL` and ACL enforcement always use the hardcoded default `s.st`. Requests using `?db=otherenv` bypass subject-prefix scoping and read-only policies. `handleACL` writes also go to the default store.
- **Suggestion:**
  ```go
  // In auth middleware and lookupACL:
  st, err := s.dbOr(...) // or s.byName(...)
  if err != nil { ... }
  pol, ok := lookupACLOnStore(st, got) // or make ACL facts live per-log
  ```
  Either route ACL reads/writes through the request-resolved store, or store ACL facts under a per-environment namespace.
- **Production Impact:** Complete failure of row-level security and read-only tokens when using named environments. High risk for multi-tenant or multi-app deployments.
- **Status:** open

#### 2. Named Environments Often Opened Without File-Level Writer Lock
- **Severity:** bug
- **Files:** `internal/api/api.go:383` (byName), `472` (handleCreateDB), `1248` (handleArchitectApply), `internal/architect/apply.go` paths, contrast with `cmd/centauri/main.go:196`
- **Description:** Several paths call `store.Open(path)` (or equivalent) with default `Options` (no `Lock`). Only the primary store in `serve` gets `Lock: true`.
- **Suggestion:** Always pass `store.Options{Lock: true}` (or make it configurable) when opening named DBs on disk:
  ```go
  st, err := store.OpenOptions(path, store.Options{
      Lock:         true,
      LazyPayloads: *lazy, // if applicable
  })
  ```
  Update `handleCreateDB`, `byName`, `handleArchitectApply`, and any clone paths. Consider adding a helper `openNamedDB(...)`.
- **Production Impact:** Risk of log corruption when two processes (or a follower + another writer) touch the same named `*.log` on shared storage, NAS, or synced folders.
- **Status:** open (note: recent read showed byName now passes Lock in some places — verify all call sites)

---

### P1 — High (Performance & Scalability)

#### 3. Disk I/O (ReadAt) Performed While Holding RLock in LazyPayloads Mode
- **Severity:** suggestion (high production impact)
- **Files:** `internal/store/store.go:698-717` (`hydrate`), `706`, `720-728` (`hydrateAll`), callers: `Current:731`, `AsOf:755`, `History:790`, `CurrentByField:1054`, etc.
- **Description:** When `LazyPayloads=true`, `hydrate()` does `s.f.ReadAt(...)` **while the caller still holds `RLock`**. This blocks writers (they must wait for all readers) and other readers on slow storage.
- **Suggestion (preferred):**
  ```go
  // In Current / asOfLocked / etc.
  s.mu.RLock()
  pos, ok := s.offsets[id]   // snapshot metadata
  evCopy := *s.events[id]    // shallow copy of metadata
  s.mu.RUnlock()

  if needsHydrate {
      buf := make([]byte, pos[1])
      s.f.ReadAt(buf, pos[0])  // no lock held
      // unmarshal and merge into evCopy
  }
  return evCopy
  ```
  Alternative (simpler): document the trade-off and recommend against LazyPayloads on high-contention workloads.
- **Production Impact:** Severe writer starvation and unpredictable latency when using Lazy mode with any real disk load or many concurrent readers.
- **Status:** open

#### 4. Every Normal Write Holds Exclusive Lock Through fsync
- **Severity:** suggestion
- **Files:** `internal/store/store.go:468-507` (`commit`), `482-486`, `Append:556`, `IngestRaw:84`, `Activate`, `AddEnrichment`, `PutSchema`
- **Description:** `Write` + `Sync()` + `apply()` + watcher notify all occur under `Lock()`. No batching across concurrent HTTP clients. `NewID()` and full validation also run under the lock.
- **Suggestions:**
  - Consider a small internal write queue / batcher for `Append` when `NoSync=false` (coalesce multiple events from different goroutines before one fsync).
  - Move non-mutating validation (subject/facet checks, type checks) outside the lock where safe; keep only the final dedup + commit under lock.
  - Expose or document a "bulk" mode more prominently.
- **Production Impact:** Write throughput is fundamentally limited by single-threaded fsync latency. On anything but the fastest local SSD, concurrent writers (multiple dashboards, agents, sync processes) will queue up.
- **Status:** open

#### 5. Long Operations Held Under RLock (History, Large Scans)
- **Severity:** suggestion
- **Files:** `internal/store/store.go:789` (History), `asOfLocked`, `CurrentByField`, `Disagreements`
- **Description:** `History` on a busy subject copies + sorts potentially thousands of events while holding the read lock. Similar for full-facet scans.
- **Suggestion:**
  - Add server-side pagination / limit to the store layer for `History`.
  - Or release the lock, do the sort/copy, and re-validate currency on return (with care for consistency).
  - Document that very large histories should be streamed via CDC / `/v1/changes` instead of `HISTORY`.
- **Production Impact:** One expensive analytical query can delay all writers and other readers.

---

### P2 — Medium (Robustness, Maintainability, Multi-DB)

#### 6. In-Place Mutation of `*model.Event` in `apply()`
- **Severity:** suggestion
- **Files:** `internal/store/store.go:404` (`te.ActivationTime = ...`), supersede path, `model.go:6`
- **Description:** `apply` mutates events that are already returned to callers (`ActivationTime`, `SupersededBy`, `EffectiveEnd`). The model docstring claims events are immutable after append.
- **Suggestion:** 
  - Explicitly document the two post-append mutation points.
  - Or return defensive copies (`cp := *e; cp.ActivationTime = ...; return &cp`) from all public query paths that can observe lifecycle fields.
- **Production Impact:** Callers holding references can observe surprising mutations without re-querying. Breaks "immutable fact" mental model.

#### 7. Subtle Lock Discipline in Replication (IngestForeign)
- **Severity:** suggestion
- **Files:** `internal/store/sync.go:23` (RLock dedup snapshot then unlock + IngestRaw)
- **Description:** Relies on external single-writer file lock being held by the caller. Direct map access bypasses normal accessors.
- **Suggestion:**
  - Add a clear package comment: `// IngestForeign assumes the caller holds the process-level writer lock (see main.go syncPeer).`
  - Consider an unexported `snapshotForDedup() []byte` helper that stays under one lock acquisition.
- **Production Impact:** Future maintainers or alternative sync mechanisms could introduce subtle races.

#### 8. Named DB Management Uses Plain Mutex + Lazy Opens
- **Severity:** suggestion
- **Files:** `internal/api/api.go:152` (`Server.mu sync.Mutex`), `byName:380`, `handleCreateDB:426`, `handleArchitectApply:1260`, `handleDemo*`
- **Description:** All named DB creation/open/list operations serialize on a single mutex. Architect apply does `store.Open` then registers under the lock.
- **Suggestions:**
  - Consider `sync.RWMutex` for `dbs` map (readers for normal `?db=` traffic, writer only on create/open).
  - Extract a helper `openOrGetNamedDB(name string) (*store.Store, error)`.
- **Production Impact:** Management operations (create DB, architect apply, demo clear) can briefly block query traffic on the server mutex.

#### 9. Resource Cleanup Is Manual and Error-Prone
- **Severity:** nit
- **Files:** `internal/api/api.go:539` (handleDemoClear), `449-469` (clone error paths), `handleArchitectApply:1254`
- **Description:** Many `f.Close(); os.Remove(...)` patterns instead of `defer` + named returns or cleanup helpers.
- **Suggestion:** Introduce a small `cleanup` func or use `defer` more consistently on error paths in create/clone flows.
- **Production Impact:** Low — mostly readability and future maintenance risk.

#### 10. Chain Extension Code Duplicated Across Paths
- **Severity:** nit
- **Files:** `internal/store/ship.go:118`, `store.go:300` (replay), `commit:489`
- **Description:** `chainExtendBuf` ordering and LazyPayloads side-effects differ textually between commit, IngestRaw, and replay, even though the invariant holds.
- **Suggestion:** Extract a small internal helper:
  ```go
  func (s *Store) writeAndExtend(recs []*record, data []byte) error { ... }
  ```
- **Production Impact:** Low — increases risk of future divergence.

---

### P3 — Polish & Operational

#### 11. No Rate Limiting, Concurrency Controls, or Backpressure
- **Files:** `internal/api/api.go` (all handlers), `cmd/centauri/main.go`
- **Suggestion:**
  - Add a simple semaphore or `golang.org/x/sync/semaphore` for max concurrent writes.
  - Consider `http.TimeoutHandler` or per-route timeouts.
  - Expose Prometheus-style metrics for lock wait time, fsync latency, active watchers, queued appends (via `expvar` or a lightweight collector).
- **Production Impact:** A misbehaving client or burst of agents can easily saturate the single writer.

#### 12. Validation and ID Generation Under Exclusive Lock
- **Files:** `internal/store/store.go:566-604` (Append validation loop), `581` (model.NewID inside lock)
- **Suggestion:**
  - Do cheap client-side validation before taking the lock.
  - Generate IDs before the lock when possible (client can supply, or pre-generate a batch).
- **Production Impact:** Wasted lock hold time on invalid or duplicate submissions.

#### 13. Watch Subscription Management Takes Exclusive Lock
- **Files:** `internal/store/watch.go:11,29`
- **Suggestion:** Consider an atomic map or separate lock for the `subs` map (very short critical section) so Subscribe/Unsubscribe don't contend with long-running commits.
- **Production Impact:** Minor, but adds tiny latency to every write when watches are being added/removed.

#### 14. History and Large Result Sets
- **Suggestion:** Add `LIMIT` / offset support at the store query layer (or document that callers should use CDC `/v1/changes` for large histories).
- **Production Impact:** Memory and lock hold time for analytical queries.

#### 15. Shutdown & Checkpoint Robustness
- **Files:** `internal/store/store.go:334` (Close), `main.go` signal handler
- **Suggestion:**
  - Make Close more resilient to partial failures.
  - Consider a background checkpoint goroutine on idle (optional).
- **Production Impact:** Faster restarts and lower chance of full replays after unclean shutdown.

#### 16. Improve Test Coverage for Concurrency
- **Suggestion:**
  - Add tests that run many concurrent `Append` + `Current` + `Watch` + `History` goroutines (use `errgroup` or `sync.WaitGroup`).
  - Add a stress test that enables `LazyPayloads` + mixed workload.
  - Since `-race` is not available on all platforms, consider running it in CI on amd64/linux.
- **Production Impact:** Catches issues before users do.

---

## Non-Code Recommendations

### Documentation
- Clearly document expected write throughput characteristics and the single-writer + fsync model.
- Add a "Production Considerations" section to README or a new `docs/production.md` (cover LazyPayloads, multi-DB, replication lag, RAM requirements).
- Document that `HISTORY` on high-event subjects is expensive.

### Deployment & Operations
- For production, prefer fast local NVMe or high-IOPS storage.
- Consider putting the data directory on a volume with `fdatasync` friendly settings.
- Run with `Lock: false` only when you control the single writer process.
- Monitor:
  - Write latency / fsync duration
  - Number of active watchers
  - Memory used by indexes vs `LazyPayloads` hits
  - Replication lag (via cursor / log size diff)

### Architecture Evolution (Future)
- Investigate a lock-free or sharded index design if write scaling becomes a hard requirement.
- Consider optional write-ahead batching or group commit.
- Evaluate whether the current model is sufficient for the target "flight recorder" use case vs high-velocity transaction workloads.

---

## What Is Already Excellent

- Write-then-apply + replay determinism is rigorously followed.
- Non-blocking watcher delivery is the right design.
- `ReadLog` correctly avoids holding locks during large IO.
- File-level lock (`acquireLock`) is safe and informative.
- `NoSync` option for bulk loads is well placed.
- Invariants around crash ordering and hash chain are clear and implemented.

---

## How to Use This Document

1. Start with P0 items (ACL + Lock for named DBs).
2. Tackle P1 items if you plan to use LazyPayloads or expect > low-hundreds of writes/sec.
3. Use the P2/P3 list as a backlog for robustness and maintainability.

**Next step suggestion:** Open issues or a design doc for items 1, 2, and 3 — they have the highest user-visible production impact.

---

*This file was generated automatically from review artifacts and code analysis. Update it as suggestions are implemented.*
