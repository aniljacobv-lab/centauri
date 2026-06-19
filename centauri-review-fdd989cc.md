# Centauri Code Review Notes

**Review date:** 2026-06-18  
**Scope:** Entire core codebase (internal/model, store/*, ceql/*, api, catalog, architect, cmd/centauri, demo, plus SDK and Airbyte connector samples) + local uncommitted diff in review-diff.tmp (demo hierarchy seeding).  
**Process:** Read diff first, CLAUDE.md in full, all listed key files (store.go full via chunks, integrity, checkpoint, ship, model, ceql, main, api chunks, catalog, architect, demo full, plus supporting files via list/grep), inspected commit/apply/replay/IngestRaw/Append paths for the 5 invariants, state discipline, write-then-apply, hash chain coverage (incl. IngestRaw), demo changes, lock discipline, error paths, races, leaks, auth/input, tests. Ran `go vet ./...` (clean) + targeted `go test` (pass).  
**Rules followed:** Correctness > style. Specific file:line. No code fixes. Only real issues.

## Summary

The core engine is high-quality, append-only, and largely obeys the documented invariants and "zero Go deps" rule. Replay determinism, write-then-apply, and hash-chain coverage are carefully implemented across commit/replay/IngestRaw (with identical chainExtend paths and apply-only state mutation). The demo diff adds a substantial retail merchandise × location hierarchy seeding (with denormalized facts + causal links for rollups) plus test assertions; it is functionally correct and exercises GROUP BY + history + disagreements on the new grain. Minor issues exist around multi-environment ACL/locking, some best-effort IO without leaks, and a few nits on pointer mutation and lock hold times. No critical data-loss, race, or invariant-violation bugs found in the paths exercised by seed/demo/replication. Tests (including updated demo_test) pass; vet clean.

## Issues

### Issue 1 -- Severity: bug
- File: C:\Users\anilj\centauri\internal\api\api.go:84
- Description: lookupACL (and thus scope/ACL enforcement in auth at ~264, 1050) unconditionally uses the hardcoded default `s.st.Current("acl:"+...)`. Named databases selected via ?db= (resolved via dbOr/byName) are never subject to ACL prefix policies or read-only scoping from acl:* facts. handleACL (1379) also always does `s.st.Append` bypassing db().
- Suggestion: Route ACL lookup and writes through the request's resolved store (or make ACLs environment-scoped and store them per-log). Update auth middleware and handleACL to honor ?db=.
- Status: open

### Issue 2 -- Severity: bug
- File: C:\Users\anilj\centauri\internal\api\api.go:383
- File: C:\Users\anilj\centauri\internal\api\api.go:472
- File: C:\Users\anilj\centauri\internal\api\api.go:1248
- Description: byName (for named envs), handleCreateDB clone path, and handleArchitectApply all call `store.Open(path)` (or equivalent) with default Options (no Lock). In contrast, main serve/desktop/sync explicitly pass Lock:true (main.go:196,236). External writers (or synced-folder peers) can concurrently open named *.log files, risking log corruption. Single-writer lock is only for the default DB.
- Suggestion: Consistently pass `store.Options{Lock: true}` (or make Lock configurable per-env) for all file-backed Open calls in api.go and architect apply paths.
- Status: open

### Issue 3 -- Severity: suggestion
- File: C:\Users\anilj\centauri\internal\store\store.go:404
- Description: apply() mutates `*model.Event` in place for ActivationTime (`te.ActivationTime = ...`) and (via supersede path) SupersededBy/EffectiveEnd on pointers stored in s.events. Model comment (model.go:6) states "Events are immutable once appended"; only SupersededBy/EffectiveEnd are documented as post-set. ActivationTime is a post-fact lifecycle bit, but the shared pointer means a reader holding an *Event before an Activate sees the update without re-query. No crash, but violates "immutable" expectation for callers.
- Suggestion: Document the two post-append mutation points explicitly (or return defensive copies on all query paths that could observe lifecycle fields).
- Status: open

### Issue 4 -- Severity: suggestion
- File: C:\Users\anilj\centauri\internal\store\store.go:706
- File: C:\Users\anilj\centauri\internal\store\store.go:731
- Description: hydrate() (and callers under RLock: Current/AsOf/History/...) perform f.ReadAt while the RWMutex is held when LazyPayloads=true. Disk I/O under read lock can stall writers or other readers on slow media/IO.
- Suggestion: Release lock before ReadAt (snapshot offset+len first), or document the contention trade-off. (Lazy is opt-in for >RAM datasets.)
- Status: open

### Issue 5 -- Severity: nit
- File: C:\Users\anilj\centauri\internal\demo\demo.go:155 (and surrounding connect/put in the added hierarchy block ~130-258)
- Description: connect() (pure-link Append) and the many put() calls in the retail hierarchy seeding swallow subsequent errors via firstErr guard and early return in closure. Stats increments are best-effort after first error. Hierarchy links are attached only to the final skuID event for price-history grains (Fashion/Grocery cases do 3 sequential Appends + connect only on last). Old superseded price events have no ROLLS_UP_TO/AT_STORE links.
- Suggestion: For demo data the effect is harmless (links are illustrative), but if links are meant to attach to the "grain entity" consider linking the subject pattern or all versions. Consider surfacing partial-seed errors more loudly in Result.
- Status: open

### Issue 6 -- Severity: nit
- File: C:\Users\anilj\centauri\internal\api\api.go:539 (handleDemoClear), 449-469 (clone error paths), and similar manual close/remove sites
- Description: Several file/resource paths use explicit Close + Remove on error instead of defer + named returns or cleanup funcs. Correct in current code, but fragile to future edits (e.g. early returns after partial write in clone).
- Suggestion: Centralize temp-file cleanup or use defer patterns more consistently for readability.
- Status: open

### Issue 7 -- Severity: nit
- File: C:\Users\anilj\centauri\internal\store\ship.go:118
- File: C:\Users\anilj\centauri\internal\store\store.go:300
- Description: chainExtend (and Buf) occurs after apply in replay but after write+size in commit/IngestRaw. Functionally identical (replay re-hashes the exact bytes from disk; commit/Ingest extend over bytes just written). Invariant 4 holds, but the three paths are not textually identical (order + Lazy offload side-effect).
- Suggestion: Extract a single "write line + extend chain + (maybe apply)" helper used by commit, IngestRaw, and replay to make coverage obvious and reduce drift risk.
- Status: open

### Issue 8 -- Severity: suggestion
- File: C:\Users\anilj\centauri\internal\store\sync.go:23 (IngestForeign)
- Description: Dedup snapshot under RLock then unlock + IngestRaw (which re-locks + writes). Safe only because callers (follow/sync) hold the exclusive file lock (main.go). Direct s.events read bypasses any future accessor. Correct today but lock discipline is subtle.
- Suggestion: Add a package-level comment or exported "single-writer assumed" guard; consider an internal snapshot method under the same lock.
- Status: open

(End of issues list. No additional high-severity correctness, race, leak, or invariant violations found after exhaustive path inspection and test runs. The 5 CLAUDE invariants are obeyed in the store write/replay/chain paths.)

## Other Observations (non-issues)

- Diff (review-diff.tmp) + resulting demo.go/demo_test.go: hierarchy seeding is complete, assertions pass, uses only public Append + store APIs, no direct state, no new deps. New suggestions exercise GROUP BY over denormalized hierarchy fields + HISTORY + grain + slice. Correct.
- Zero deps: go.mod empty require block; all Go sources use only stdlib + internal/ (confirmed via grep + go build/vet).
- Invariant coverage: apply() exclusively owns index mutations; commit/IngestRaw do write+fsync+chain+apply; replay applies then chains on disk lines; IngestRaw identical to primary bytes; checkpoint load + reset documented; nothing mutates log bytes.
- Lock discipline: all core mutators use Lock + writable(); queries RLock; notifications non-blocking under lock; named envs are the outlier noted above.
- Error handling + resources: torn-tail recovery, rollback on partial write, best-effort checkpoints/watchers, explicit closes on error paths. No leaks observed.
- Security/input: subtle.ConstantTimeCompare for tokens, subject prefix scoping, CeQL parse before exec, schema validation in Append, no injection vectors.
- Test/demo: updated assertions match the new seed data; full demo + store tests pass.

## Verdict

Core is solid and invariant-respecting. Primary actionable items are the multi-db ACL/locking gaps in the API layer (high impact for users of named environments + tokens). Demo changes are correct. Recommend addressing Issues 1-2 before broader multi-db/ACL use.

**Review file written to:** C:\Users\anilj\centauri\centauri-review-fdd989cc.md
