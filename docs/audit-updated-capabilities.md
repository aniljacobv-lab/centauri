# Centauri Code Audit — Updated Code with Expanded Capabilities

**Audit date:** 2026-06-23 (post recent enterprise + scaling features)  
**Scope:** Latest code state including:
- Write scaling (`serve -shards N`, parallel appends, subject-hashed routing, routed reads).
- Group commit (`-group-commit` for coalesced fsyncs under concurrency).
- Hot-path admission control (normal serve + lazy, with exemptions).
- Retention + legal hold (RETIRE-based, `hold:*` facts).
- Lean read-only SQL (`/v1/sql` transpiler to CeQL).
- Full observability parity (metrics/health on normal serve, structured `slog` + correlation IDs on both modes).
- Auth/TLS/docs clarifications.
- Existing tablespaces/lazy-index, native TLS, lazy auth, etc.

**Methods:** 
- Git history of last ~15 commits + diff of changed files (`cmd/centauri/main.go`, `internal/api/*`, `internal/shard/*`, `internal/retention/*`, `internal/store/store.go`, docs).
- Full test run + `go vet ./...` (all pass).
- Targeted reads of new code: `internal/shard/shard.go`, `api/shard.go`, `retention/retention.go`, SQL handler, admission wiring in `api.go`, group commit in `store.go`, updated `enterprise-readiness.md`.
- Grep for invariants (apply, chain, write-then-apply), cross-shard, auth, limits.
- Cross-check against prior audits (`enterprise-review-rerun.md`, `enterprise-deployment-gaps.md`, previous enterprise-readiness gaps).
- Verification of CLAUDE.md invariants, zero-deps, documentation honesty.

**Overall verdict:** Strong, targeted expansion of enterprise capabilities while staying true to the append-only, tamper-evident, bi-temporal core. Many prior gaps (write throughput under load, admission, retention, SQL front-door, observability parity, auth on all paths) are now closed or significantly advanced. New features introduce honest, documented limitations (e.g., sharding scope). Code is clean, tested, and respects invariants. No critical bugs found.

---

## Summary of New/Expanded Capabilities

### Write Scaling & Throughput
- **`serve -shards N`**: Partitions subjects across N independent shard logs (each with own chain, lock, committer). Writes to different subjects run in parallel (~N× throughput). Deterministic routing by FNV-1a hash of subject (same subject/facet always on same shard). Point reads route to owner; global ops limited.
  - `/v1/append`: parallel per-shard.
  - Routed `current`/`history`/`asof`/`subjects` (union).
  - `/v1/query` full on single shard for concrete subjects.
  - `/v1/shards`: distribution info.
- **`-group-commit`**: Opt-in coalescing of concurrent `Append` into one fsync/batch (higher write concurrency without changing durability, chain, or write-then-apply). Reuses core `commit()` path. Queue + committer goroutine.
- **Admission control now on hot path**: `-max-concurrency` (429), `-query-timeout` (503) apply to normal serve (writes + queries). Exempts streams (`/v1/watch`, `/v1/changes`, `/v1/log`) and health/metrics. Complements lazy version.

### Enterprise Compliance & Operations
- **Retention + legal hold**: `centauri retention -pattern '...' -older-than N [-apply]`. RETIREs (supersedes with `retired:true` — history preserved, chain intact). Legal holds via `hold:<name>` facts (pattern + active flag) that skip matching subjects. Dry-run by default.
- **Lean read-only SQL**: `POST/GET /v1/sql` with subset SELECT (WHERE, GROUP BY, HAVING, ORDER BY, LIMIT + `AS OF` / `FOR SYSTEM_TIME AS OF`). Transpiles to CeQL AST + executor. Auto-routed in Studio/dashboard. Subject to scoped tokens.
- **Observability parity**:
  - `/metrics`, `/livez`, `/readyz` on **normal serve** (plus lazy-specific segment cache metrics).
  - Structured `log/slog` + `X-Request-ID` correlation on both modes (`-log-format json|text`, `-log-level`).
- **Auth/TLS/docs**: Token auth clarified for both modes. Native TLS (`-tls-cert`/`-tls-key`) everywhere. `deploy.md` + matrix updated.

### Retained/Enhanced Core (Tablespaces Era)
- Compressed, Merkle + chain tamper-evident segments.
- Lazy index (RAM ~ live subjects).
- Crypto-erasure on sealed.
- Bi-temporal + causal + search over cold.
- Zero-deps, single binary.

---

## Positives

- **Addresses prior gaps directly**: Write scaling (group commit + shards), full-path admission, retention/legal hold, SQL front-door, observability on normal path, auth parity. Many items from `enterprise-review-rerun.md` and `enterprise-deployment-gaps.md` are now ✓ or advanced (see updated `enterprise-readiness.md`).
- **Respects invariants** (CLAUDE.md):
  - Sharding: Per-shard independent chains + `apply()`. Cross-shard links rejected early (preserves determinism). Auto-supersession stays intra-shard.
  - Group commit: Reuses `commit()` / `writeApplyNotify` verbatim; single chain ordering unchanged.
  - Retention: Pure RETIRE (supersede); never mutates bytes or breaks chain.
  - SQL: Read-only transpiler to CeQL; no writes.
- **Honest scoping**: Sharded mode explicitly limits global/wildcard ops and cross-shard atomics/causality (package docs + errors). Admission exempts long-lived streams. Matrix updated with nuances.
- **Code quality**: New packages (`shard`, `retention`) clean and isolated. Tests for new paths (including concurrency under shards/group-commit/admission). `slog` zero-dep. TLS/auth wired consistently.
- **Enterprise polish**: Retention is compliance-friendly (never erases). Admission bounds heavy LLM/cold queries. SQL lowers barrier without full wire protocol. Observability + structured logs ready for prod.
- **Tests/vet**: Full suite passes cleanly post-changes.

---

## Issues & Gaps (Still Present or New)

Categorized with severity, citations, descriptions. Focus on enterprise + core quality. No data-loss or invariant violations found.

### High-Impact Remaining / Partial
- **Sharding limitations (design trade-off, not bug)**:
  - Files: `internal/shard/shard.go:100-106` (rejects cross-shard links), `102` (batch co-location check), package doc.
  - Description: Multi-shard batches not atomic; explicit causal links across shards rejected; no cross-shard SEARCH/agg fan-out or global wildcard CeQL. `Subjects()` unions in parallel but scales with shards. Wildcard queries need single-store serve.
  - Suggestion: Document clearly (already good); add future cross-shard merge for SEARCH (fan-out + reduce). For now, recommend single-store for analytics/global views.
  - Enterprise impact: Great for write-heavy partitioned workloads (e.g., per-tenant or per-subject), but not full "Oracle sharding" transparency.

- **Object-store cold tier still ✗**:
  - Files: `docs/enterprise-readiness.md:45`, `design-own-your-data.md` (planned but unimplemented).
  - Description: Segments still filesystem-only. Tiers manual.
  - Suggestion: Implement `SegmentStore` abstraction (Local + Object) using stdlib HTTP + SigV4 for S3-compatible. See prior `docs/object-store-backend.md`.
  - Enterprise impact: High for cheap durable cold at scale.

- **Retention & legal hold partial**:
  - Files: `internal/retention/retention.go`, `cmd/centauri/main.go` (retention cmd), `enterprise-readiness.md:46`.
  - Description: Command + engine support exists (RETIRE + `hold:*` facts). No built-in scheduler, no crypto-erase action, no engine-level block on *manual* RETIRE during hold.
  - Suggestion: Add internal recurring task (via CePL/WATCH) + crypto-erase option + hold enforcement in write path.
  - Enterprise impact: Good start for policy enforcement; missing automation/completeness.

- **SQL still read-only + no wire protocol**:
  - Files: `internal/api/api.go:257` (`/v1/sql`), `ceql` transpiler, `enterprise-readiness.md:34`.
  - Description: Lean SELECT (incl. AS OF) works; writes use CeQL. No JDBC/ODBC/pgwire.
  - Suggestion: Keep as "front door"; add thin wire protocol later if needed. Enhance transpiler for more SQL:2011 features.
  - Enterprise impact: Lowers barrier; BI tools still need adapter.

- **Write throughput / multi-writer still constrained**:
  - Files: `enterprise-readiness.md:37` (shards + group-commit partial), `store.go` (single lock per shard).
  - Description: Shards + group-commit give real gains, but each shard is still single-writer; no cross-shard atomic txns.
  - Suggestion: (Already good) Per-shard parallelism + group. Future: more sophisticated batching or subject-aware queuing.

### Medium / Polish
- **Admission control still missing per-tenant quotas**:
  - Files: `internal/api/limits.go`, `api.go:276` (`WithLimitsExcept`), `enterprise-readiness.md:43`.
  - Description: Global concurrency/timeout now on hot path. No per-`?db=` or per-tenant.
  - Suggestion: Extend limits middleware with named-DB context.

- **Observability / logging still evolving**:
  - Files: `enterprise-readiness.md:44` (no OTel traces yet), `api.go` / main for slog.
  - Description: Structured logs + IDs good; no full OTel spans. Internal logs mixed.
  - Suggestion: Add lightweight tracing (stdlib or minimal) for hot paths (append, query, shard dispatch, LLM).

- **Auth / RBAC still partial**:
  - Files: `api.go` (scoped tokens, `scopeAllows`), `enterprise-readiness.md:40`.
  - Description: Good token + RLS; no full roles, expiry, OIDC, column masking.
  - Suggestion: Build on existing ACL facts for governance pack (per ROADMAP).

- **At-rest encryption hot tier + KMS**:
  - Files: `enterprise-readiness.md:41-42`.
  - Description: Sealed only (via segment); hot/manifest via volume. Secrets = env only.
  - Suggestion: Hot-tier encryption + file/KMS secret support.

### Low / Nits
- **Docs drift risk**: `deploy.md` may need refresh for shards/SQL/admission (prior updates done for TLS/probes).
- **Sharded mode edge cases**: Union of subjects can be expensive at extreme scale (parallel but still). Test cross-shard link rejection thoroughly (already in tests).
- **New code paths**: Group commit + sharding add goroutines/queues — ensure Close paths drain cleanly (code looks good).

---

## Review of Updated `enterprise-readiness.md`

- **Honesty**: Excellent. New rows accurately reflect capabilities with caveats (e.g., SQL partial, shards "not provided: cross-shard atomic...", retention partial, admission ✓ with "per-tenant still not").
- **Updates from prior**: Added/expanded:
  - Token auth on both modes.
  - Metrics/health on both.
  - Structured logging.
  - Admission on hot + lazy.
  - Retention partial.
  - Write scaling details (group + shards).
- **Gaps in matrix**: Could call out "sharded mode limitations" more explicitly in a dedicated row if needed. Good cross-ref to design-tablespaces.
- **Live in console**: Matrix shown in lazy dashboard — ensure sharded mode has equivalent or note.

---

## Comparison to Prior Audit Gaps (from `enterprise-review-rerun.md` + `enterprise-deployment-gaps.md`)

**Addressed / Advanced**:
- Lazy auth bypass → closed.
- Observability (lazy-only) → now on normal + structured logs.
- Admission (lazy-only) → hot path + exemptions.
- TLS basic → native everywhere.
- Retention (absent) → command + holds (partial).
- Write scaling (absent) → shards + group-commit.
- SQL front-door (absent) → `/v1/sql`.
- Docs drift → several updates.

**Still open / partial** (as expected):
- Object store cold tier.
- Full multi-writer / HA failover / auto election.
- Full RBAC / KMS / hot encryption.
- Per-tenant quotas.
- OTel traces.
- Object store backend.
- Arbitrary historical indexes.
- SQL wire protocol.

New sharding introduces scoped limitations (honestly documented) but delivers real throughput wins within the model.

---

## Recommendations (Prioritized for Next Steps)

1. **Port object-store backend** (high enterprise impact for cold scale/cost). Start with S3-compatible + manifest abstraction (see prior design doc).

2. **Complete retention** (scheduler inside binary, crypto-erase action, hold enforcement on writes).

3. **Add OTel traces** (lightweight, alongside slog) for key paths (append, query, shard dispatch, LLM).

4. **Per-tenant quotas** in admission (extend limits with `?db=` context).

5. **Enhance sharded mode** (future cross-shard SEARCH fan-out + merge; document when to use single vs sharded).

6. **SQL evolution**: Expand transpiler; consider thin wire protocol if demand.

7. **Hot-tier encryption + secrets**: Finish v0.4 items.

8. **Update full deploy.md + examples** for new flags (`-shards`, `-group-commit`, retention, `/v1/sql`).

9. **Load/chaos tests**: Add for sharded + group-commit under mixed load + admission.

10. **Keep invariants front-of-mind**: Every new feature (shards, group, retention) correctly routes through per-shard `apply()` / chain.

**Testing note**: New concurrency tests (shards, group, admission) are present and passing. Run with `-race` on amd64 where possible.

This audit is objective, based on direct inspection. The updates represent excellent progress on enterprise capabilities without compromising the unique append-only / bi-temporal strengths.

Full raw findings preserved in this document for reference. Update the matrix + this audit as more lands. 

*Generated via code + doc review. No code changes made.*