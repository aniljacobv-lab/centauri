# Centauri Code Review — 2026-07-01

> **STATUS UPDATE (same day):** every finding below — B1-B8, S1-S10, and all nits (including the six catalog entries and the repo-wide gofmt stragglers) — has been **FIXED** and verified: `go vet ./...` clean, `go test ./...` all green, `-race` on store/api/proc/architect green, `gofmt -l` empty, Python SDK 15/15. Fix fallout caught and resolved: the S3 injection hardening initially broke DDL-generated procedures feeding numeric strings into number-typed fields (`TestDDLApplyAndGuards`); resolved by letting strings that are single numeric/boolean literals splice bare (still one token — injection-safe), with regression test `TestSubstitutionNumericStringSplicesBare`. New tests added across store (torn tail, lazy legal hold, snapshot stability, duplicate ingest, batch hygiene), api (end-to-end mask on /v1/query, body limits, ctx keys, stream-only ?token=), and ceql (17 malformed-AST cases).

**Scope:** internal/store, internal/model, internal/ceql, internal/proc, internal/api, internal/mcp, cmd/centauri. Follow-up to the 2026-06-18 review.
**Verification:** Python SDK tests pass (15/15). `go vet`/`go test` could not run in this sandbox (no Go toolchain, network blocked) — run locally before acting on this. All bug-severity findings below were hand-verified against source; file:line cited.

## Status of the 2026-06-18 review

- Issue 1 (ACL bypass via ?db=) — **FIXED** (auth resolves ACL store from ?db=, api.go:440-446; handleACL writes via dbOr, api.go:1651).
- Issue 2 (named envs opened without Lock) — **FIXED** (all named-env opens pass `Options{Lock:true}`: api.go:587, 676, 708, 1542).
- Issue 7 (chainExtend not factored) — **FIXED** (`writeApplyNotify`, store.go:571).
- Issue 8 (IngestForeign lock discipline) — **FIXED** (documented contract, sync.go:23-28).
- Issue 3 (apply mutates shared *Event) — **open in substance** (see B5).
- Issue 4 (hydrate I/O under lock) — **partially fixed** (hot paths off-lock via hydrateAllSafe; Trace/Context/Similar etc. still under lock, now documented).

## Bugs (verified)

### B1. Replay admits a torn-tail record — next append corrupts the log
`internal/store/store.go:335-364`. `commit()` writes `record+'\n'` in one Write; a crash can persist the JSON but not the `\n`. Replay parses the trimmed final line and **applies it** (line 349), skips `chainExtend` (the `'\n'` guard at 359), and still advances `off` — so `good == fi.Size()` and OpenOptions does not truncate (283). The next Append writes directly after the unterminated record, producing `{...}{...}\n` on one physical line; the *next* open then fails with "corrupt record not at tail". Also breaks chain/size coherence (`Integrity()` false-mismatch).
**Fix:** in `replay`, require the trailing `\n` *before* `s.apply(&r)`; a parseable-but-unterminated final line is a torn tail — return the pre-line offset so the caller truncates. (`WriteArchive` at archive.go:83 already does this correctly.)

### B2. Torn-tail truncation happens before the single-writer lock is acquired (TOCTOU)
`internal/store/store.go:283-307` (truncate at 285, `acquireLock` at 300); same order in `archive_open.go:81-104`. A second process opening the store while a live writer is mid-commit can read a partial tail, `Truncate()` bytes the writer has (or is about to have) durably committed — and only *then* fail the lock and exit. The writer continues at its own offset: hole/overlap, in-memory size/chain diverges from disk.
**Fix:** acquire the lock immediately after `os.OpenFile`, before checkpoint load / replay / truncate; release on every error path.

### B3. Legal holds silently unenforced after restart with LazyPayloads
`internal/store/store.go:653-673`. `heldLocked` skips events with `e.Value == nil` (659). In lazy mode replay offloads every payload (`ev.Value = nil`, 350-357), so after reopen every `hold:*` fact is skipped and RETIRE proceeds under an active hold. Holds created in the current process still work, so tests miss it (`legalhold_test.go` never does lazy+reopen).
**Fix:** hydrate `hold:*` candidates in `heldLocked` (caller holds the write lock; holds are few). Add a lazy+reopen test.

### B4. Field masking (VPD) never enforced on /v1/query
`internal/api/api.go:1292-1322` vs 1356-1369. `maskResult` is wired only into `handleSQL` — but scoped tokens are restricted *to* `/v1/query` (api.go:447-449), where `handleQuery` checks `scopeAllows` and then returns results unmasked. Net: an ACL policy's `mask` field is a no-op for exactly the principals it exists to constrain; masked fields (SSNs, salaries) return in clear text.
**Fix:** capture `pol.Mask` at the ctxScope check in `handleQuery` and call `maskResult` before `writeJSON`; extend `maskResult` to the `"context"` result kind (nested events). Add an end-to-end scoped-token test.

### B5. Context key collision: request ID vs OIDC subject
`internal/api/api.go:99,102`. `ctxOIDCSubject` and `ctxRequestID` are both `ctxKey = 3`. `verifyOIDC` overwrites the correlation ID with the JWT `sub`; `RequestID()` then returns the SSO subject (both strings — assertion silently passes).
**Fix:** `const ctxRequestID ctxKey = 4`.

### B6. Panic on attacker-controlled AST via POST /v1/query {"ast":...}
`internal/api/api.go:1266` executes a client-supplied AST with no validation; `internal/ceql/exec.go:645-647` `case "not": evalExpr(x.Kids[0], e)` indexes unchecked — `{"where":{"op":"not"}}` panics (index out of range); `{"op":"and","kids":[null]}` nil-derefs. Reachable by the least-privileged scoped token. net/http recovers per request, so it's a 500/DoS lever, not a crash.
**Fix:** add a `Query.Validate()` walk before Execute (logical ops ≥1 non-nil kid, "not" exactly 1), or guard in `evalExpr`.

### B7. Query results share *Event pointers that apply() later mutates (race)
`store.go:481-537` mutates `ActivationTime`/`SupersededBy`/`EffectiveEnd` on stored pointers under s.mu; with LazyPayloads off, `Current`/`History`/`AsOf`/`Trace`/`ByRef` return those pointers and API handlers marshal them **after** releasing the RLock. Concurrent supersession writing a string field while the marshaler reads it is a data race under the Go memory model (torn string header possible). `hydrateAllSafe` (store.go:973-1007) already copies under lock — but only in lazy mode.
**Fix:** make the shallow copy unconditional in query paths, or supersede-by-replacement (store a fresh *Event instead of mutating).

### B8. Seal/manifest swap lacks directory fsync — crash can silently lose sealed records
`internal/store/archive_open.go:229-274`. Sequence: write+fsync segment → rename manifest (file fsynced, directory not) → remove old tail. The unlink can become durable while the rename does not; recovery reads the old manifest whose tail no longer exists, `OpenArchive` O_CREATEs an empty tail and opens **without error**; `GCArchive` would then delete the orphan segment holding the data. Same gap in `CompactArchive` (compact.go:128-132).
**Fix:** fsync the archive dir after segment write and after manifest rename, before removing the old tail (skip on Windows).

## Suggestions

- **S1. HTTP server has no timeouts** (`cmd/centauri/main.go:1920-1925`): zero-value `http.Server` → slowloris. Set `ReadHeaderTimeout: 10s`, `IdleTimeout: 2m` (no WriteTimeout — would break SSE).
- **S2. Unbounded JSON request bodies**: all JSON handlers decode r.Body uncapped (only asset uploads are limited). One shared decode helper with `http.MaxBytesReader(w, r.Body, 8<<20)`.
- **S3. CePL `${}` substitution is textual CeQL injection** (`internal/proc/proc.go:336-347`): a string arg like `x' , retired=true` reshapes the generated statement. Matters because mcp.go:125-128 pitches `proc_*` tools as the confinement mechanism for scoped principals. Quote/escape substituted strings or substitute into the AST.
- **S4. Checkpoint-restore diverges from replay for vectors** (store.go:526-530 vs checkpoint.go:189-200): superseding embedding with unparseable vector → live apply keeps stale vector, checkpoint rebuild drops it. Delete from s.vectors in apply on parse failure (invariant-2 drift).
- **S5. Auto-seal breaks CDC/ship cursors and ignores slots**: Seal resets the tail (s.size=0) so follower offsets/Changes cursors error after each seal; `maybeAutoSeal` never consults `MinSlotCursor` despite slots.go's promise. Defer auto-seal while slots exist, or document mutual exclusion.
- **S6. IngestRaw has no duplicate detection** (ship.go:83-110): redelivered chunk double-applies (duplicate ids in indexes, forked follower chain). Check expected offset against s.size or reject existing ids.
- **S7. Group commit: failed validation pollutes shared batch state** (store.go:754-782, 855-883): `seen[id]` set and caller events mutated before a later event fails → spurious "duplicate event id" for the next request in the group. Validate into locals first.
- **S8. handleCreateDB clone copies the whole log under s.mu** (api.go:630-675): blocks byName (and thus auth for named DBs) for the copy duration. Copy to temp outside the lock, then lock/re-check/rename.
- **S9. `go ceql.AutoEmbed(...)` per append** (api.go:842-845): unbounded goroutine fan-out under write bursts; may touch the store after Close. Use one worker + bounded queue.
- **S10. VerifyChain swallows mid-file read errors** (integrity.go:74-76): I/O failure reported as `verified:false` (tampering). Return the read error distinctly.

## Nits

- `?token=` query-param auth accepted on all routes (api.go:421) — restrict to SSE/streaming paths (tokens land in proxy logs).
- Scoped-token auth silently falls back to default store when ?db= fails to resolve (api.go:441-445) — reject instead.
- Catalog drift (feature checklist): `HAVING`, `OFFSET`, `EXISTS`, `MEDIAN`, `STDDEV`, `LISTAGG` have parser+executor+textbook coverage but no `catalog.go` entries.
- `Close()` writes a plain-log checkpoint even for archive stores; nothing reads it and GC doesn't clean it.
- `AdvanceSlot` (slots.go:44-52) check-then-append isn't atomic — concurrent acks can rewind the cursor.
- `archiveReader.segmentBytes` (archive_reader.go:116-121): concurrent misses double-insert into the LRU (inflated bytesCached, over-eviction).
- Repo hygiene: `review-diff.tmp` is git-tracked; large runtime artifacts (`centauri.log` 44 MB, `.checkpoint` 65 MB, `centauri.exe`) sit in the working tree — ignored, but worth relocating data files out of the repo root.

## Recommended order

1. **B1 + B2** — both are small fixes in the recovery path and both can corrupt or lose committed data.
2. **B4 + B6 + B5** — security-adjacent API fixes, each a few lines, reachable by low-privilege callers.
3. **B3, B8** — compliance (legal hold) and archive durability.
4. **S1 + S2** — mechanical HTTP hardening.
5. Then the rest as time permits.

## Verdict

The big items from the June review (multi-db ACL scoping, named-env locking, chain-path factoring) are fixed, and the core invariants hold in the normal write path. The remaining bug-severity issues cluster in two places: **crash-recovery edges** (B1, B2, B8) and the **scoped-token security surface** (B4, B5, B6). None require design changes — all are localized fixes. Run `go vet ./... && go test ./...` locally; the sandbox couldn't.
