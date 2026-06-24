# Recommendations: Making Centauri More Like Oracle (Based on Latest Codeset)

**Date:** 2026-06-23  
**Latest codeset highlights** (from current `main`, `enterprise-readiness.md`, sharding, group commit, etc.):
- Write scaling: `serve -shards N` (parallel appends across independent shard logs with subject-hashed routing + group commit for coalesced fsyncs).
- Admission control: `-max-concurrency`, `-query-timeout`, `-max-concurrency-per-db` on normal + lazy paths (exempts streams/health).
- Retention + legal hold: `centauri retention` + `hold:*` facts (RETIRE-based, never erases history/chain).
- Lean read-only SQL: `/v1/sql` (SELECT subset with AS OF transpiles to CeQL).
- Observability parity: Prometheus `/metrics`, `/livez`, `/readyz` + structured `slog` + correlation IDs on **both** normal serve and `serve -lazy-index`.
- Native TLS (`-tls-cert`/`-tls-key`), token auth on data routes for both modes.
- Tablespaces/lazy index: compressed Merkle+chain segments, zone pruning, LRU cache, crypto-erasure, scales beyond RAM.
- Core invariants preserved: append-only (nothing erased), replay determinism via `apply()`, write-then-apply, hash chain, zero Go deps, single binary.

Centauri is **deliberately not** a general-purpose OLTP RDBMS like Oracle. It is a specialized, immutable, bi-temporal, causal event/audit store ("the flight recorder beside your operational stores"). See `enterprise-readiness.md` and `design-own-your-data.md` for the honest positioning:

- Strengths (now stronger with latest): tamper-evidence, bi-temporal + causal queries, crypto-erasure, cold data scaling via tablespaces, zero-deps simplicity.
- Explicit gaps vs. Oracle: no full SQL wire protocol, no multi-statement ACID transactions, no concurrent multi-writer OLTP, partial HA/failover, partial RBAC, no object-store backend yet, etc.

**Goal of these recommendations**: Make Centauri *more Oracle-like in its lane* — enterprise-grade durability, compliance, scalability (within append-only model), usability, and operations — while respecting invariants. Leverage the latest sharding/group-commit/admission/SQL/observability as a foundation. Do not turn it into a mutable OLTP clone (that would break the core value).

Recommendations are prioritized, actionable, and reference current code paths. They build on the staged plan in `design-tablespaces.md` and `ROADMAP.md`.

## 1. Query Language & Ecosystem (Highest ROI for "Oracle Feel")

Oracle strength: Full SQL + wire protocols (JDBC/ODBC), rich ecosystem.

**Current progress (latest)**: Lean read-only SQL SELECT (with `AS OF` / `FOR SYSTEM_TIME AS OF`, GROUP BY, etc.) at `POST/GET /v1/sql` transpiles to CeQL. Auto-routed in Studio/dashboard. Still read-only; writes use CeQL/REST. No wire protocol.

**Recommendations**:
- **Evolve `/v1/sql` into a production-grade read layer**.
  - Expand transpiler (`internal/ceql` or new `sql` package) for more SQL:2011 features, cross-subject joins (where safe), window functions.
  - Add "virtual tables" or views backed by CeQL for common patterns.
  - Expose schema introspection that feels SQL-like.

- **Add thin SQL wire protocol compatibility (without full parser)**.
  - Implement a minimal Postgres wire protocol shim (or JDBC bridge) that accepts a subset of SELECT and translates to `/v1/sql` or direct CeQL executor.
  - This allows BI tools (Tableau, Power BI, DBeaver) to connect directly.
  - Keep writes strictly via existing API (preserves append-only model).
  - Location: New `internal/sqlwire` or contrib package (stdlib only).

- **Oracle-style stored procedures + debugging**.
  - Enhance CePL (already strong) with step-debugger in dashboard (ROADMAP item).
  - Add "packages", better error context, and PII-aware execution.

- **Ecosystem integrations**.
  - Official Grafana datasource (builds on existing `/metrics`).
  - Improved Airbyte/Spark/Flink readers that understand shards + tablespaces.
  - "Why Centauri" comparison table in dashboard (keep brutally honest, like the matrix).

**Why this makes it Oracle-like**: Developers and analysts get a familiar entry point. BI/ETL tools integrate without custom code.

**Effort/impact**: Medium-high. Leverages existing transpiler and executor. Start with expanding SELECT coverage.

## 2. Scalability, HA & Architecture (Leverage Sharding Foundation)

Oracle strength: Sharding, RAC-style clustering, Data Guard failover, high concurrency.

**Current progress (latest)**:
- `serve -shards N`: Parallel writes (~N× throughput) across independent per-shard logs/chains/locks. Deterministic subject routing. Routed reads. Explicit limits: no cross-shard atomic batches, no cross-shard causal links (rejected), no wildcard/global CeQL/SEARCH fan-out (use single-store).
- `-group-commit`: Coalesces concurrent appends into one fsync (better write concurrency under load).
- Admission control (global + per-`?db=`) prevents noisy neighbors.
- Tablespaces + lazy index for cold data (RAM scales with live subjects).
- Read replicas via `follow` + durable CDC slots (`sync`).

**Recommendations**:
- **Mature sharding into a production "Oracle sharding"-like experience**.
  - Add cross-shard fan-out + merge for SEARCH/aggregates/causal trace (background or on-demand coordinator).
  - Support cross-shard causal links (lightweight eventually-consistent linking via a coordinator log).
  - Add rebalancing / shard addition tooling (migrate subjects while preserving lineage).
  - Per-shard replication + unified client-side routing layer.
  - Expose shard health in `/metrics` and new `/v1/shards` (already partially there).
  - In `internal/shard/shard.go` and `api/shard.go`: Extend `Set` for fan-out.

- **Add automatic failover / leader election on top of existing replication**.
  - Simple lease-based leader election (using existing lock primitives or etcd/consul sidecar, but prefer stdlib where possible).
  - Use `/livez`/`/readyz` + chain head comparison for detection.
  - "Promote" command that reconfigures followers.
  - HA orchestration can be external (Kubernetes operator style) — document patterns.

- **True multi-writer convergence (design already exists)**.
  - Multiple independent writers (or shards) converge deterministically (union by event_id + re-order by recorded_time).
  - Builds on `ship.go` / merge logic. Make it first-class for geo-distributed or multi-master setups.

- **Object-store cold tier (still a gap, high priority)**.
  - Each sealed segment as immutable object in S3/GCS/R2 (your bucket).
  - Manifest as object; only hot tail local until sealed + uploaded.
  - Signed HTTP via stdlib `net/http` (no SDKs). See `design-own-your-data.md` for interface sketch (`SegmentStore`).
  - Combine with sharding (shards can tier to object store).
  - Update `WriteArchive`, `archiveReader`, `LazyIndex` to be backend-agnostic.
  - This gives cheap, durable, scalable cold storage (Oracle-like partitioning + ILM).

- **Admission control as resource governor**.
  - Already strong with per-db caps. Extend to query cost budgets, result size limits, and per-tenant CPU/disk quotas (track in sharded mode).

**Why Oracle-like**: Horizontal write scale + resilience. Shards give "sharded database" feel; object store + tablespaces give tiered storage.

**Effort/impact**: High for full cross-shard + object store. Start with fan-out and object store backend.

## 3. Transactions & Consistency Model

Oracle strength: Full ACID, multi-statement transactions, MVCC, serializable isolation.

**Current**:
- Writes are atomic batches (single-fact or multi via `Append`).
- Group commit improves batching.
- Bi-temporal + causal links provide "what was true" + "why".
- No interactive `BEGIN...COMMIT` MVCC (by design — immutable model).
- Per-shard atomicity in sharded mode; no cross-shard atomic transactions.

**Recommendations**:
- **Make batches feel more "transactional" (Oracle-like for immutable data)**.
  - Add explicit "transaction" context or batch ID that ties related appends (even in group commit).
  - Client-visible "commit" marker fact for atomic multi-subject groups.
  - Leverage existing supersession for "rollback" semantics (RETIRE the batch).
  - In sharded mode: Opt-in coordinated commit (2PC-like via causal links or external coordinator) for cross-shard batches.

- **Snapshot isolation via bi-temporal machinery**.
  - `AS KNOWN AT` already gives read snapshots. Expose as "SET TRANSACTION ISOLATION LEVEL" sugar in SQL/CeQL.
  - Add "read committed" vs "snapshot" modes for long-running queries.

- **Do not add full MVCC or in-place updates** — that breaks the append-only + tamper-evidence core. Instead, emphasize "immutable transactions with full lineage" as a differentiator (better audit than Oracle's flashback).

- **Stronger consistency for critical paths**.
  - Use group commit + fsync for durability.
  - In sharded: Allow users to declare "same-shard" requirements for atomicity.

**Why Oracle-like**: Developers get familiar transactional boundaries. Multi-shard coordination adds enterprise resilience.

**Effort/impact**: Medium. Build on existing `Append`, group commit, and causal links.

## 4. Security, Compliance & Governance

Oracle strength: TDE (encryption), VPD (fine-grained access), auditing, RBAC, legal hold, retention policies.

**Current (latest)**:
- Token auth (admin + read-only + scoped RLS via ACL facts) on data routes for both modes.
- Native TLS.
- Crypto-erasure (AES-GCM per sealed segment).
- Retention + legal hold (RETIRE + `hold:*` facts).
- Admission control (resource protection).
- Structured logs + correlation (basic auditing).

**Recommendations**:
- **Complete at-rest encryption + KMS integration**.
  - Hot tail + manifest (currently use volume encryption; add optional per-record AES like sealed segments).
  - Support external KMS for keys (expand `auth_env` pattern to `key_ref` or Vault integration via stdlib HTTP).
  - Per-tenant / per-shard keys for isolation.

- **Evolve RBAC / governance pack**.
  - Build on scoped tokens + per-db admission.
  - Add role hierarchy facts, field/column masking (apply in executor for read-token paths).
  - PII classification tags (as enrichments) + automatic enforcement/redaction.
  - Full audit of admin actions (leverage retention + new audit facts).

- **Mature retention + legal hold**.
  - Add stored policy scheduler inside the binary (run via CePL background task).
  - Crypto-erase as retention action (destroy key + RETIRE).
  - Engine-level hold enforcement (prevent manual RETIRE during active hold).
  - Integrate with sharding (per-shard retention).

- **Compliance features**.
  - "Prove this fact at time T" tooling (leverage bi-temporal + chain head export).
  - Automated e-discovery export (filtered by retention/hold).
  - Integration with external audit systems via structured logs + `/v1/changes`.

**Why Oracle-like**: Strong data protection and compliance story. Retention + erasure is already a differentiator.

**Effort/impact**: High for encryption/KMS + full RBAC. Retention is already partially there — extend it.

## 5. Observability, Operations & Performance

Oracle strength: Enterprise Manager, AWR, Data Guard, resource manager, cost-based optimizer.

**Current (latest)**:
- Prometheus `/metrics` + health probes on both modes.
- Structured logging + correlation IDs.
- Tablespace Console (inspector, verify, cache metrics).
- Admission control as basic resource governor.
- `centauri doctor`, seal/verify, backup tools.

**Recommendations**:
- **Add OpenTelemetry traces** (build on correlation IDs).
  - Trace spans for append/query/shard dispatch/LLM calls/segment reads.
  - Export via stdlib-compatible mechanism or sidecar.

- **Cost-based query planning + optimizer hints**.
  - Use zone maps + segment stats (already in manifest) + new statistics gathering.
  - Hints for "force hot path", "use specific shard", "AS OF vs current".
  - Expose query plans (enhance `EXPLAIN`).

- **Advanced operations**.
  - Per-shard + per-tenant backup/restore/PITR (using archives + chain heads).
  - Automated compaction / tier migration (hot → object store).
  - Resource manager (extend admission to CPU/disk quotas, query cost limits).
  - "AWR-like" performance views (leverage `/metrics` + new historical stats facts).

- **"Oracle-style" backup & recovery**.
  - Consistent snapshots across shards.
  - Point-in-time restore with legal-hold awareness.

**Why Oracle-like**: Production operators get familiar tools and visibility.

**Effort/impact**: Medium-high. Leverage existing metrics and tablespace console.

## 6. Overall Phased Roadmap (Leverage Latest as Base)

**Short-term (build immediately on current code)**:
- Expand SQL transpiler + add thin wire protocol.
- Cross-shard fan-out + basic object-store backend.
- OTel traces + per-tenant resource quotas.
- Hot-tier encryption + KMS hooks.
- Mature retention scheduler.

**Medium-term**:
- Full RBAC/governance + column masking.
- Automatic failover + multi-writer convergence tooling.
- Cost-based pruning + historical indexes.
- Sharding rebalance + cross-shard causal support.

**Long-term / aspirational**:
- Deeper ecosystem (JDBC driver, Spark connector with shard awareness).
- "Derived facts" / incremental materialized views (ROADMAP).
- Optional read-side sharding with global indexes (while keeping write model).

**Constraints (non-negotiable)**:
- Preserve append-only + never-erase model.
- Replay determinism (everything through `apply()`).
- Zero Go dependencies.
- Honest documentation (update `enterprise-readiness.md` matrix as features land).
- Single binary where possible (shards are already a great compromise).

**How to execute**:
- Small, testable PRs (use existing test patterns for sharding/admission).
- Update the live matrix in the dashboard and `enterprise-readiness.md`.
- Add "Oracle-like" comparison notes in README/Studio.
- Prioritize based on user pain (e.g., BI tools → SQL; compliance → retention/encryption; scale → sharding + object store).

This approach turns Centauri's unique strengths (immutability, bi-temporal, tamper-evidence, causal) into enterprise superpowers while adding Oracle-like surface area and scale via the latest sharding, SQL front-door, admission, and observability work. It stays true to the "flight recorder" positioning instead of becoming a mediocre Oracle clone.

See also:
- `docs/enterprise-readiness.md` (source of truth matrix)
- `docs/design-tablespaces.md`
- `ROADMAP.md`
- `docs/audit-updated-capabilities.md` (recent code review)

If you want implementation sketches for any item (e.g., wire protocol on shards, object store + sharding integration), a backlog PR list, or comparison table vs. specific Oracle features, let me know.