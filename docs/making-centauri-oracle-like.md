# Making Centauri "Like Oracle" — Realistic Recommendations

**Context and Positioning (Critical First Step)**

Centauri is **not** intended to be a general-purpose OLTP RDBMS like Oracle. From the project's own documentation:

- It is a **bi-temporal, causal, append-only event database** — the "flight recorder" that sits *beside* your operational stores (Oracle, Postgres, etc.).
- Core strengths (from `enterprise-readiness.md` and `README.md`):
  - Immutable append-only log (updates = superseding facts; deletes = RETIRE markers).
  - Cryptographic tamper-evidence (SHA-256 chain + per-segment Merkle roots).
  - Bi-temporal queries (`AS OF` + `AS KNOWN AT`).
  - Causal lineage (WHY, EFFECTS, MATCH).
  - Crypto-erasure for compliance.
  - Scales to cold/disk data via tablespaces (compressed segments + lazy index + zone maps).
  - Zero Go dependencies, single static binary.
- Explicit honest gaps vs. Oracle (from `enterprise-readiness.md`):
  - No SQL / JDBC / ODBC.
  - No multi-statement ACID transactions (writes are append batches).
  - No concurrent multi-writer OLTP (single-writer lock).
  - Limited indexing for arbitrary historical/range queries (zone pruning + scans for cold data).
  - Replication/HA is partial (log shipping + CDC; no auto-failover or leader election).
  - Limited RBAC and governance.
  - Partial at-rest encryption (sealed segments only).
  - No object-store backend for cold tier yet.
  - No automated retention/legal-hold.
  - Etc.

**The "Oracle-grade" positioning from the design docs** (see `design-own-your-data.md`):

> "Oracle-grade memory for *what happened, when, why, and how much to trust it* — without the license, and on infrastructure you control."

**Recommendation #0 (Do this first):** Do **not** try to turn Centauri into a drop-in Oracle replacement. That would violate its core invariants (nothing ever erased, replay determinism through `apply()`, write-then-apply, hash chain, zero-deps). Instead, make it **Oracle-like in its niche**:
- Enterprise-grade durability, compliance, and auditability for immutable facts.
- Production-hardened operations (observability, security, scalability within the model).
- Ecosystem friendliness so it can sit in real Oracle-centric enterprises.

Trying to add full MVCC, cost-based optimizer, clustering consensus, etc., would bloat the code and dilute what makes Centauri unique (and easy to verify).

---

## Prioritized Recommendations

Group by category. Each includes:
- Why it makes it more "Oracle-like".
- Concrete steps / how-to (respecting CLAUDE.md invariants, zero-deps, append-only model).
- Effort level and dependencies on existing code (e.g., tablespaces, LazyIndex, CeQL).
- Status relative to current code (as of latest tablespaces + enterprise updates like native TLS, lazy auth, metrics/health on lazy path, admission control on lazy).

### 1. Durability, Integrity & Compliance (Where Centauri Can Be *More* Oracle-Like Than Oracle)

Oracle has strong durability (redo logs, Data Guard), but Centauri already has cryptographic tamper-evidence that Oracle does not.

**Recommendations:**
- **Complete hot-tier at-rest encryption + full crypto-erasure** (already in ROADMAP v0.4 and partially in sealed segments).
  - Extend AES-256-GCM (see `internal/segment/segment.go: Seal/Open`) to the hot tail and manifest.
  - Per-subject or per-namespace keys (so destroying a key erases readability of a subject without breaking the chain).
  - Store keys outside the DB (file, KMS integration later).
  - How: In `store.go` commit path, encrypt payloads before writing (but keep hash chain over ciphertext for integrity). Update `LazyPayloads` and archive paths.
  - Oracle-like: GDPR "right to be forgotten" + full audit trail (Centauri already does this better for sealed data).
- **Automated retention / legal-hold policies**.
  - Add `RETIRE` with policies (e.g., "retain 7 years, then auto-RETIRE unless legal hold").
  - Expose via CeQL or admin API. Use WATCH for alerts.
  - How: New `policy` facts + a background `proc` (CePL) that scans and RETIREs. Leverage existing `store.Retire` logic.
- **Object-store cold tier** (S3/GCS backend — see `docs/object-store-backend.md` and `design-own-your-data.md` for full design).
  - Make sealed segments first-class objects in *your* bucket.
  - Manifest as object; only tail local until sealed.
  - Use stdlib `net/http` + minimal SigV4 (no SDKs).
  - How: Introduce `SegmentStore` interface (LocalDir + ObjectStore impls). Update `archiveReader`, `WriteArchive`, `LazyIndex` to be backend-agnostic. Sealing uploads after local write + conditional manifest update.
  - Oracle-like: Cheap, durable, scalable cold storage with the same verification.
- **Stronger backup/DR tooling** (build on existing `centauri backup`, `verify`, `seal`).
  - Point-in-time restore from archive + specific chain head.
  - Automated "export to object store + verify" jobs.

**Current status:** Strong foundation (hash chain, Merkle, crypto-erasure on sealed, tablespaces for cold). Hot-tier encryption and object store are the big missing pieces for Oracle-grade compliance/durability.

### 2. High Availability, Scalability & Operations (The Biggest "Not Oracle" Gaps)

Oracle excels at clustering (Real Application Clusters), Data Guard, sharding, resource management.

**Recommendations (stay true to single-active-writer model):**
- **Improve replication/HA story** (currently partial: `follow`, `sync`, CDC slots).
  - Add automatic failover orchestration (external or simple built-in leader election for read replicas).
  - Promote "multi-master via deterministic merge" (already designed — see design docs on "Git for your data").
  - How: Enhance `syncPeer` / `follow` with health checks and automatic switch. Add a simple `centauri promote` command. Use the existing single-writer lock + merge primitives.
  - For true HA: Recommend running multiple `follow` replicas + external load balancer / proxy (with the new native TLS + health probes).
- **Full admission control, rate limiting, and resource management** (partial today — only on lazy path via `-max-concurrency`, `-query-timeout`, `WithLimits`).
  - Extend `internal/api/limits.go` + `WithLimits` to the normal serve path (writes + heavy queries).
  - Add per-`?db=` (named DB / tenant) quotas.
  - How: Wrap more handlers in `main.go` and `api.go`. Add write-side semaphore outside `store.Append` lock. Expose via `/metrics`.
  - Oracle-like: Protect SLAs, noisy-neighbor protection.
- **Better multi-tenancy isolation**.
  - Fix/ complete named DBs (`?db=`) + tablespaces (ensure `Lock:true`, ACL scoping, resource limits).
  - How: Build on recent auth improvements in lazy path. Add per-DB stats and limits.
- **Object store + tiered storage** (see above) for horizontal scaling of cold data.
- **Clustering / sharding** — *Explicitly out of scope per ROADMAP and design* (single active writer, no consensus). Instead:
  - Recommend "federation": multiple independent Centauri instances + external merge tool.
  - Or read-scaling via many `follow` replicas.
  - Long-term (if ever): Read replicas with consistent hashing on subjects, but keep writes single-writer or sharded by namespace.

**Current status:** Tablespaces + lazy index give excellent "scale with disk" for reads. Replication is manual. Single-writer is fundamental (and a feature for simplicity + correctness).

### 3. Query Language, Transactions & Ecosystem (The "Developer Experience" Gap)

Oracle = SQL, JDBC, full ACID txns, rich ecosystem.

**Recommendations (without breaking append-only model):**
- **SQL compatibility layer** (not full replacement for CeQL).
  - Provide a JDBC/ODBC driver or Postgres wire protocol shim that translates a *subset* of SQL to CeQL.
  - Focus on: SELECT with time travel (`AS OF`), basic aggregates, WHERE on current facts.
  - How: New `internal/sql` or contrib package (or use an existing zero-dep parser + translator). Expose via a separate port or the existing API.
  - Oracle-like: Easier adoption for teams that already use BI tools, JDBC apps.
  - Keep CeQL as the native powerful language (bi-temporal, causal, trust operators).
- **"ACID-like" batch transactions**.
  - Already have batched `Append`. Make it feel more transactional:
    - Client-side "transaction" ID that groups appends.
    - Atomic multi-subject batches with internal supersession.
    - Optional "commit marker" fact.
  - Add support for "savepoints" via causal links.
  - How: Enhance `Append` API and CePL to support explicit transaction boundaries (still all appends).
  - Never promise interactive `BEGIN/COMMIT` with isolation levels that allow updates-in-place (violates invariants).
- **Better historical + range indexing**.
  - Extend the current field index (current state only) and zone maps.
  - Add persisted indexes for common historical patterns (e.g., time + subject range).
  - How: In the lazy path, use segment-level indexes or B-tree-like structures on zone data (still stdlib).
  - Oracle-like: Predictable performance for "what changed last quarter" queries.
- **Ecosystem integrations**.
  - JDBC driver (thin, over HTTP/JSON or wire protocol).
  - Grafana datasource (build on new `/metrics`).
  - Airbyte connector (already exists, improve it).
  - Spark/Flink readers for analytics.
  - How: Keep them thin (call the REST API or read archives directly).

**Current status:** CeQL is powerful for its domain. No SQL layer yet. Transactions are append batches.

### 4. Security, Governance & Multi-Tenancy (Enterprise Polish)

**Recommendations:**
- **Full RBAC + governance pack** (partially in ROADMAP).
  - Build on scoped tokens + ACL facts.
  - Add roles, field masking for read tokens, PII tags (as enrichments), scheduled checks.
  - How: New `governance` facts + CePL procedures. Extend auth middleware in `api.go`.
- **Secrets & KMS integration**.
  - Support external secret refs (Vault, cloud KMS) for model credentials and encryption keys (beyond `auth_env`).
- **At-rest encryption for hot tier + full KMS** (see durability section).
- **Audit / compliance tooling**.
  - First-class "prove this fact existed at time T" (leverage existing bi-temporal + chain).
  - Export for e-discovery.
  - Automated retention policies.

**Current status:** Good foundation (tokens, ACLs, TLS, metrics). Governance and full hot-tier encryption are next.

### 5. Observability, Performance & Operations

- **Full structured logging + OpenTelemetry** (currently line-oriented `log`/`fmt`).
  - Add `slog` (stdlib) or lightweight structured output.
  - Correlation IDs across queries, appends, LLM calls, segment reads.
- **Production hardening on the hot/write path** (admission control, limits — currently stronger on lazy).
- **Object store backend** (see above) + tiering policies.
- **Cost-based elements** (for CeQL, not full Oracle CBO) — statistics + hints for cold vs hot paths.
- **Backup/restore automation** and PITR using archives + chain heads.

---

## Realistic Phased Roadmap

**Phase 1 (v0.4 — "Enterprise Hardening", align with current ROADMAP):**
- Hot-tier encryption + crypto-erasure (full).
- Object store backend (start with S3-compatible).
- Governance pack basics.
- Admission control + limits on main path.
- SQL shim / JDBC (thin layer).
- Structured logging + full observability parity (metrics/health on normal serve too).
- Docs update: clearer "Oracle for immutable history" positioning.

**Phase 2 (v0.5+):**
- Improved HA (auto-failover for replicas, better merge tooling).
- Historical indexing improvements.
- Full RBAC + column masking.
- Retention policies + legal hold.
- Ecosystem: official Grafana + Spark connectors.

**Phase 3 (Longer term, if demand exists):**
- "Dynamic tables" / derived facts (already dreamed in ROADMAP).
- Better multi-tenancy isolation.
- Optional read-scaling sharding (by namespace/subject prefix).

**Things to *never* do (to preserve the product):**
- In-place updates or full MVCC that erases history.
- Third-party Go dependencies.
- Multi-writer clustering with consensus (violates simplicity and determinism).
- Become a general OLTP store (keep the "flight recorder" identity).

---

## How to Prioritize & Execute

1. **Start with the gaps that hurt enterprise users today**: object store, hot encryption, observability on the main path, rate limiting, docs.
2. **Leverage existing strengths**: Every new feature must flow through `apply()`, maintain the hash chain, and be replay-deterministic.
3. **Keep it simple**: Many "Oracle-like" features can be achieved by thin layers (SQL translator, governance procs) on top of the rock-solid immutable log.
4. **Measure against Oracle where it matters**: Durability (chain + Merkle > many RDBMS), audit (bi-temporal + causal), compliance (crypto-erasure), cost/ownership (your bucket, your binary).
5. **Use the existing honest matrix**: Update `enterprise-readiness.md` as you close rows. The live Tablespace Console is a great way to communicate progress.

**Bottom line:** You don't need to *become* Oracle. You can become the thing enterprises wish Oracle had: an incorruptible, queryable history layer that is cheap to run at scale, lives on infrastructure *they* control, and never silently forgets or rewrites the past.

This keeps Centauri's magic while making it production-ready in the environments where Oracle is common today.

See related docs for deeper dives:
- `docs/enterprise-readiness.md`
- `docs/enterprise-implementation-guide.md`
- `docs/object-store-backend.md`
- `docs/design-tablespaces.md`
- `ROADMAP.md`

If you want a detailed implementation plan for any specific item (e.g., object store + SQL layer together), code sketches, or prioritization for a particular version, just say the word.