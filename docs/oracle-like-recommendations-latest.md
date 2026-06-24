# Making Centauri More Like Oracle — Recommendations Based on Latest Codeset (2026-06)

**Date:** 2026-06-23  
**Basis:** Latest codebase with expanded capabilities including:
- Write scaling via `serve -shards N` (parallel appends across independent shard logs with subject-hashed routing).
- `-group-commit` for coalesced fsyncs under concurrency.
- Hot-path admission control (`-max-concurrency`, `-query-timeout`, `-max-concurrency-per-db`).
- Retention + legal hold (`centauri retention`, `hold:*` facts).
- Lean read-only SQL SELECT transpiler at `/v1/sql` (with `AS OF` support).
- Full observability parity: Prometheus `/metrics`, `/livez`, `/readyz` on *both* normal serve and lazy-index paths.
- Structured logging (`slog`) + correlation IDs on both modes.
- Native TLS (`-tls-cert`/`-tls-key`), clarified token auth on data routes for both modes.
- Tablespaces/lazy index for cold data (compressed segments, zone maps, Merkle + chain, LRU cache, crypto-erasure).
- Existing bi-temporal, causal, tamper-evident append-only model.

**Important Context (from `enterprise-readiness.md`, `README.md`, `ROADMAP.md`, `design-own-your-data.md`)**

Centauri is **intentionally not** a general-purpose OLTP RDBMS like Oracle. Its core value proposition is:

- Immutable append-only log (updates = supersede; deletes = RETIRE; history never erased).
- Cryptographic tamper-evidence (per-line SHA-256 chain + per-segment Merkle).
- Bi-temporal queries (`AS OF` + `AS KNOWN AT`).
- Causal lineage (WHY / EFFECTS / MATCH).
- Crypto-erasure for compliance ("right to be forgotten" without breaking audit trail).
- Scales with disk (not just RAM) via tablespaces + lazy index.
- Zero Go dependencies, single static binary.
- "Oracle-grade memory for *what happened, when, why, and how much to trust it*" — the flight recorder / system-of-record alongside operational stores.

The latest `enterprise-readiness.md` is deliberately honest:
- **Strong/✓**: Immutable log, tamper-evidence, bi-temporal, crypto-erasure, scales beyond RAM, online verification, native TLS, token auth (both modes), Prometheus + health probes (both modes), structured logging + correlation IDs, admission control (including per-db).
- **Partial**: SQL (lean read-only SELECT subset via `/v1/sql`), write throughput under concurrency (via shards + group-commit), index for arbitrary historical queries (zone pruning + scans), replication/HA (log shipping + CDC; no auto-failover), RBAC (scoped tokens exist), at-rest encryption (sealed segments only), retention (RETIRE + holds, but partial automation).
- **✗ (by design or not yet)**: Full SQL wire protocol (JDBC/ODBC/pgwire), multi-statement ACID transactions, concurrent multi-writer OLTP, automatic failover/leader election, full role hierarchies/column masking, external KMS, object-store cold tier, OpenTelemetry traces, automated retention scheduler.

**Making it "like Oracle" does not mean turning it into a general-purpose OLTP database.** That would violate its invariants (append-only, replay determinism through `apply()`, write-then-apply, hash chain, zero-deps policy). Oracle strengths like full MVCC, cost-based optimizer for mutable data, RAC-style clustering, etc., are fundamentally misaligned.

**Realistic goal**: Make Centauri *Oracle-grade* as an **enterprise immutable event/audit/history store**:
- Oracle-like durability, compliance, auditability, and operations.
- Oracle-like usability and scalability *within its model* (e.g., sharding for writes, SQL front-door, per-tenant controls).
- Seamless coexistence with Oracle (as the "what really happened" layer).

This builds directly on the latest codeset (shards as scaling foundation, lean SQL as bridge, admission + retention as enterprise controls, full observability, tablespaces for cold data).

---

## Current Oracle-Like Progress (Latest Codeset)

**Scalability & Throughput (big step forward)**
- Sharding (`-shards N`): Parallel writes across independent per-shard logs/chains/locks. Subject-hashed deterministic routing. Routed reads. Group commit for fsync coalescing.
- Admission control (global + per-db) bounds heavy operations.
- Lazy + tablespaces already give excellent cold-data scaling.

**Compliance & Governance**
- Retention + legal holds (RETIRE-based, never erases history/chain; hold facts block retirement).
- Crypto-erasure on sealed segments.
- Tamper-evidence stronger than most RDBMS (Merkle + continuous chain).

**Usability & Ecosystem**
- Lean read-only SQL SELECT (with time travel) as familiar front-door.
- Native TLS, token auth (both modes), structured logs + correlation IDs.
- Full Prometheus + K8s health probes on normal + lazy paths.

**Durability**
- Write-then-apply + hash chain preserved even in sharded/group-commit paths.
- Online sealing + verification.

**Gaps vs Oracle** (see honest matrix in `enterprise-readiness.md`): No full wire protocol, no interactive multi-statement ACID, no auto-HA/failover, partial RBAC/encryption, no object-store backend yet.

---

## Prioritized Recommendations

Recommendations are evolutionary, respect invariants, and build on latest code. Prioritized by enterprise impact + feasibility.

### 1. Query Language & Ecosystem (Highest usability win)

**Goal**: Make it feel more like Oracle for developers/analysts/BI tools without replacing CeQL.

- **Evolve the lean SQL transpiler into a fuller read layer + optional wire protocol**.
  - Current: `/v1/sql` (SELECT subset with `AS OF`/`FOR SYSTEM_TIME AS OF`, GROUP BY, etc.) → CeQL AST → executor. Good start.
  - Next: Expand transpiler for more SQL:2011 features, joins (cross-subject where safe), window functions.
  - Add a thin JDBC/ODBC or Postgres wire-protocol shim (in a separate process or contrib) that speaks the wire but translates to `/v1/sql` or direct CeQL. This allows Tableau, Power BI, DBeaver, etc.
  - Keep writes strictly CeQL (or REST) — this respects append-only model.
  - Leverage existing Studio/dashboard auto-routing.

- **Add Oracle-like "materialized views" / derived facts** (already in ROADMAP dreams).
  - Use CePL or a new `DERIVE` statement to maintain incremental summaries as ordinary facts (supersedable, with full lineage).
  - This gives Oracle-like denormalized query performance while keeping the log pure.

- **PL/SQL-like stored procedures evolution**.
  - CePL already exists. Add step-debugger (in progress per prior), better error context, more Oracle-style control structures.
  - Add "packages" / namespaces for procedures.

**How (leverage latest)**:
- Extend `ceql` package (ParseSQL, transpiler).
- Add wire protocol handler (new `internal/sqlwire` or similar; keep zero-deps by implementing minimal Postgres wire or using HTTP underneath).
- Update `enterprise-readiness.md` matrix as it advances from "partial" toward fuller read compatibility.

**Trade-off**: Don't promise full SQL DML that mutates in place.

### 2. Transactions, Consistency & Write Model (ACID-like within append-only)

**Goal**: Give Oracle-like transactional feel for batches without breaking immutability.

- **Strengthen batch "transactions" using latest group commit + sharding**.
  - Current append batches + group commit already provide atomicity within a shard.
  - Add client-visible "transaction" markers or batch IDs that tie related appends (even across a group commit).
  - For sharded: Explicitly document "single-shard atomicity by default". Add opt-in "coordinated commit" for multi-shard batches (2-phase via causal links where possible, or external coordinator).
  - Expose "transactional" append APIs that guarantee all-or-nothing within shard.

- **Add "savepoints" and "read committed" snapshot isolation** via existing bi-temporal machinery.
  - Use `AS KNOWN AT` for snapshot reads during long operations.
  - Leverage causal links for "nested" operations.

- **Never add in-place updates or full MVCC** — that would violate the append-only invariant. Instead, market "immutable transactions" as a feature (full audit of every change).

**How (leverage latest)**:
- Extend `store.Append` and shard routing logic to support explicit transaction context.
- Use group commit queue for batching.
- In `ceql`, add transactional query hints.

### 3. Scalability, HA & Sharding (Build directly on latest shards)

**Goal**: Oracle-like horizontal scale + resilience.

- **Mature the sharding model** (already a major step beyond single-node).
  - Add cross-shard SEARCH / aggregation fan-out + merge (background or on-demand).
  - Add cross-shard causal link support (via a lightweight coordinator or eventual consistency).
  - Per-shard replication (`follow` per shard) + unified client routing.
  - Add "rebalance" tooling for adding/removing shards.

- **Add automatic failover / leader election on top of existing replication**.
  - Build a simple lease-based leader for the "primary" writer (or per-shard).
  - Use existing `follow` + CDC slots + new health probes (`/livez`/`/readyz`).
  - Recommend (or ship) sidecar for orchestration (Kubernetes operator style).

- **True multi-writer via deterministic convergence** (design already exists).
  - Multiple writers (different shards or processes) converge via merge (union by event_id + re-order by recorded_time).
  - This is Oracle-like "eventual consistency" for history, but with cryptographic proof.

- **Object-store cold tier + tiered storage** (still a gap).
  - Make sealed segments first-class objects in S3/GCS (your bucket). Use signed HTTP via stdlib.
  - Combine with sharding (shards can point to object store for cold segments).
  - This gives Oracle-like "partitioning" across cheap storage.

**How (leverage latest)**:
- Extend `internal/shard` package.
- Add fan-out logic in `ceql/exec.go` or new query router.
- Use existing `listenMaybeTLS` + health endpoints for failover detection.
- See `docs/object-store-backend.md` for design.

### 4. Security, Compliance & Multi-Tenancy (Leverage retention + admission)

**Goal**: Oracle-like governance (VPD, TDE, auditing, RBAC).

- **Complete at-rest encryption + KMS integration**.
  - Hot tail + manifest (currently partial; use volume encryption or extend segment.Seal to tail).
  - Integrate external KMS for keys (model credentials already use `auth_env`; expand this).
  - Per-tenant keys for isolation.

- **Evolve RBAC / governance pack** (ROADMAP item).
  - Build on scoped tokens + new per-db admission.
  - Add role hierarchy facts, field-level masking (apply in executor for read-token queries).
  - PII classification tags + automatic enforcement.
  - Full audit logging of all admin actions (leverage structured logs + new facts).

- **Mature retention + legal hold**.
  - Add stored policy scheduler inside the binary (run via CePL or background task).
  - Add crypto-erase as a retention action (destroy key + RETIRE).
  - Engine-level hold enforcement (block manual RETIREs that would violate active holds).

- **Per-tenant isolation** (already advanced with `-max-concurrency-per-db` and `?db=`).
  - Add resource quotas (CPU, disk, query cost) per `?db=`.
  - Namespacing + tenant-specific encryption keys.

**How (leverage latest)**:
- Extend `retention` package + wire into `main.go` and store writes.
- Enhance auth middleware in `api.go` with masking.
- Use existing `segment` crypto for hot tier.

### 5. Performance & Query Optimizer

**Goal**: Oracle-like predictable performance.

- **Add cost-based elements for cold vs hot paths**.
  - Use zone maps + segment stats (already in manifest) to choose strategies.
  - Statistics gathering (PROFILE command already exists — make it automatic).
  - Hints for `AS OF` vs current, hot vs cold.

- **Better historical indexes**.
  - Extend current field index + zone pruning with persisted secondary structures for common range predicates on cold data.
  - Leverage sharding for parallel index builds.

- **Query parallelization** within shards or across (for sharded mode).

**How (leverage latest)**:
- Extend `ceql` planner / `exec.go`.
- Use tablespaces manifest stats.

### 6. Operations, Monitoring & Ecosystem

- **Full OpenTelemetry + advanced observability**.
  - Add trace spans (build on existing correlation IDs + slog).
  - Per-shard, per-tenant metrics.

- **Backup, restore, PITR**.
  - First-class support for sharded archives + object-store.
  - Point-in-time restore using chain heads + retention.

- **PL/SQL / CePL ecosystem**.
  - Debugger, packages, better error handling, more built-ins (already strong foundation).

- **"Why Centauri" table in dashboard** — keep it brutally honest (like the matrix).

---

## Phased Implementation Plan (Leveraging Latest)

**Phase 1 (near-term, build on current momentum)**:
- Expand SQL transpiler + add thin wire protocol shim.
- Mature sharding (fan-out, cross-shard links where safe).
- Complete hot-tier encryption + basic KMS.
- Add OTel traces + per-tenant quotas.
- Object-store backend (start S3-compatible).

**Phase 2**:
- Retention scheduler + crypto-erase actions.
- RBAC / governance pack (roles, masking).
- Cost-based pruning + better historical indexes.
- Automated failover guidance + simple lease leader.

**Phase 3 (longer-term)**:
- Fuller SQL compatibility layer.
- Multi-writer convergence tooling.
- Advanced sharding (rebalance, global indexes).
- Full ecosystem (official JDBC, Spark, etc.).

**Constraints to respect in all phases**:
- Append-only / never erase.
- Replay determinism (state only via `apply()`).
- Zero Go deps.
- Single binary where possible.
- Honest documentation (update `enterprise-readiness.md` and the live matrix).

---

## Summary Advice

1. **Double down on strengths** — Centauri's immutable + bi-temporal + causal model + tamper-evidence + tablespaces is already more "Oracle-like" for audit/compliance than Oracle itself in many regulated industries. Market it that way.

2. **Use latest code as launchpad** — Sharding + group commit = scale foundation. Lean SQL = adoption bridge. Admission + retention = enterprise controls. Observability = ops ready.

3. **Evolutionary, not revolutionary** — Add Oracle-like surface (SQL front-door, wire protocol, RBAC, failover) on top of the immutable core.

4. **Measure against Oracle where it matters** — Durability/audit (win), compliance (win with retention + erasure), cost of ownership (win), horizontal write scale (improving fast with shards), developer familiarity (improving with SQL).

5. **Keep the matrix honest** — This is your superpower. Enterprises will trust you more because of it.

The latest codeset has already closed a surprising number of gaps. Focus on the high-leverage items above (SQL ecosystem, object store + sharding maturity, compliance automation, full observability) and Centauri can become the "Oracle for immutable facts" in many organizations.

See also:
- `docs/enterprise-readiness.md` (live honest matrix)
- `docs/design-tablespaces.md`
- `ROADMAP.md`
- Previous implementation guides in this repo.

If you want a detailed design doc for any specific item (e.g., SQL wire protocol on top of shards, or object-store + sharding integration), code sketches, or prioritization for a specific version, let me know.