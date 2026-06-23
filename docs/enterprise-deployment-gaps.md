# Centauri — Enterprise Deployment Gaps Review

**Date:** 2026-06-18  
**Reviewed code state:** Latest tablespaces feature set (sealed compressed segments, Merkle-verified cold storage, lazy disk-backed index, zone-map pruning, crypto-erasure, Tablespace Console, `serve -lazy-index`, archive/seal lifecycle, updated auth scoping, etc.)

**Context:** This review builds on previous analyses (concurrency, production readiness, AI/vision features) and focuses specifically on gaps for **enterprise deployment** (regulated environments, large organizations, 24/7 operations, multi-tenancy, compliance, cloud/K8s, high SLAs).

Centauri already ships a very honest self-assessment in [enterprise-readiness.md](enterprise-readiness.md) (also exposed live in the Tablespace Console). Tablespaces represent a major step forward for "data larger than RAM" use cases.

---

## Executive Summary

**Strengths (what tablespaces + core deliver well for enterprise):**
- Append-only + cryptographically tamper-evident storage (hash chain + per-segment Merkle)
- Compressed cold tier with zone-map pruning and LRU segment cache
- RAM usage scales with live subjects, not total events (via `LazyIndex`)
- Crypto-erasure (AES-256-GCM per sealed segment) for "right to be forgotten" while preserving audit trail
- Fast restarts via Merkle-validated pointer checkpoints (`lazy.ckpt`)
- Strong read paths over cold data (current, history, AS OF, SEARCH, causal trace)
- Honest documentation and a built-in Tablespace Console (storage inspector, one-click verify, cache metrics)
- Zero third-party Go dependencies + single binary

**Remaining gaps for true enterprise deployment** fall into several categories. Many are acknowledged in `enterprise-readiness.md` and `ROADMAP.md`, but they become more acute at scale, in regulated industries, or when operating in cloud/container environments.

---

## 1. Security & Data Protection

### No Built-in TLS / HTTPS
- Centauri always speaks plain HTTP.
- All deployment guidance (`deploy.md`, README) recommends a reverse proxy (Caddy, nginx, etc.) for TLS termination.
- No support for `--tls-cert`, `--tls-key`, mTLS, or ALPN inside the binary.

**Enterprise impact:** Many security scanners, compliance frameworks, and internal policies require the application itself to handle or at least support TLS termination. Running on `localhost:7771` behind a proxy adds operational surface.

### Partial At-Rest Encryption
- Sealed segments support per-segment AES-256-GCM keys (crypto-erasure works well).
- Hot tail, manifest, `lazy.ckpt`, and other metadata appear to have no encryption.
- No integration with external KMS, envelope encryption, or volume-level encryption hooks.

**Enterprise impact:** Regulated data (PII, financial, health) often requires encryption at rest for the entire dataset, not just archived portions.

### Authentication & Authorization Remains Basic
- Token-based only: admin bearer token + optional read token + scoped ACL facts (stored as `acl:<hash>`).
- Recent improvement: `lookupACL` now correctly takes the resolved store, making ACLs environment-scoped (previous multi-DB bug largely addressed).
- **No**:
  - Username/password, client certificates, or OIDC/JWT integration
  - Role-based access control (RBAC) hierarchies
  - Field/column-level masking
  - Token expiry, rotation, or service account concepts
  - Integration with corporate directories (LDAP, Okta, etc.)

**Enterprise impact:** Difficult to fit into existing IAM systems. Scoped tokens provide row-level security but lack the governance expected in large organizations.

### Lazy-Index / Archive Path Bypasses Auth Middleware
```go
// cmd/centauri/main.go
if cmd == "serve" && *lazyIndex {
    ...
    log.Fatal(http.ListenAndServe(*addr, api.LazyRoutes(li)))  // no auth wrapper
}
```
Normal `serve` uses `api.NewWithOptions(st, api.Options{Token: ..., ReadToken: ...})`.

**Enterprise impact:** Even read-only archives may contain sensitive data. The high-scale read path has weaker protection than the normal path.

### Secrets Management
- Model credentials (for vision/LLM) only via `auth_env` (plain environment variables).
- No support for HashiCorp Vault, AWS Secrets Manager, Kubernetes secrets injection, or encrypted config files.

### Other Security Notes
- Image/PDF asset uploads for vision go through a simple handler with size limits.
- External model servers (Ollama) are trusted without model provenance or signing.

---

## 2. High Availability, Disaster Recovery & Scalability

### Fundamental Single-Writer Architecture
- All writes (including `AddEnrichment`, `PutSchema`, `Append`) still serialize behind one `RWMutex` + fsync (or `IngestRaw` for replication).
- Tablespaces dramatically improve the **read** path for cold data but do not change the write model.

**Enterprise impact:** Limits throughput and makes true high-availability writes difficult.

### Replication & Failover Are Manual / Polling-Based
- `follow` (log shipping read replicas)
- `sync` (bidirectional CDC with slots)
- `backup` + `archive` + `verify` + chain-head recording (strong)
- No automatic leader election, synchronous replication, or failover orchestration.

**Enterprise impact:** Recovery time objectives (RTO) depend on operator intervention or external tooling.

### Limited Horizontal Scale & Multi-Tenancy Isolation
- Named databases + tablespace "tiers" (directories) exist.
- Still a single process/binary — no sharding, no tenant-level resource isolation at the kernel level.
- Noisy-neighbor risk during cold segment scans, decompression, or LLM inference.

**Enterprise impact:** Challenging for true multi-tenant SaaS or large internal shared services.

### Cold Storage Tied to Filesystem Semantics
- Segments are portable files with manifests, but production use of object stores (S3, GCS, etc.) is aspirational in design docs, not implemented.
- Tiers (`-tier warm=...`) are currently manual directory management.

---

## 3. Observability, Monitoring & Logging

### Good Custom Endpoints in Lazy Mode, But Not Standard
- `/v1/lazy/stats`, `/v1/segments`, `/v1/verify`, `/v1/cache`
- Tablespace Console dashboard with inspector + metrics
- `centauri doctor`

**Missing:**
- Standard Prometheus `/metrics` endpoint
- OpenTelemetry traces / metrics
- Structured logging (uses `log.` and `fmt` throughout)
- Correlation IDs across requests (especially important for cold scans + LLM calls)

**Enterprise impact:** Hard to integrate into existing monitoring stacks (Datadog, Prometheus + Grafana, ELK, etc.).

### Limited Visibility into Resource Usage
- LRU cache stats and decompression counts are exposed in lazy mode.
- No built-in visibility into disk IOPS during cold scans, memory pressure, or per-tenant usage.

### LLM / Vision Calls Add New Observability Needs
- Long-running inference (default 5 minutes, configurable) happens synchronously in query/enrich paths.
- Failures are gracefully degraded but often silent.

---

## 4. Compliance & Governance

### Strong Core Audit Properties
- Everything is a fact with provenance, recorded time, effective time, and confidence.
- Tamper evidence + Merkle proofs on cold segments.
- Crypto-erasure preserves the audit trail.

### Gaps
- No separate application-level audit log (the database *is* the audit).
- No automated retention policies, legal holds, or scheduled purging (only manual `RETIRE`).
- Scoped tokens exist, but no field-level masking, PII tagging, or classification enforcement (ROADMAP lists this as future work).
- No built-in support for "prove exactly what was known at time T" beyond raw CeQL queries.

---

## 5. Operations & Deployment

### Configuration & Lifecycle Management
- Almost entirely CLI flags + a few environment variables (`CENTAURI_TOKEN`, model `auth_env`).
- No config file format or dynamic reload.
- Tablespace manifests and pointer checkpoints add new operational artifacts that must be managed during upgrades/backups.

### Container / Orchestration Readiness
- Dockerfile and examples exist (Docker Compose, Fly, systemd).
- No built-in Kubernetes liveness/readiness probes (though `/v1/verify` or doctor can be adapted).
- Ollama sidecar management (`ensureOllama`, managed process kill) adds complexity in orchestrated environments.

### Rate Limiting, Quotas & Backpressure
- Explicitly absent (documented in `deploy.md`).
- A burst of writers, watchers, or heavy `ASK`/`SEARCH` queries can impact the entire process.

### Backup & Restore for Tablespaces
- Strong primitives (`archive`, `seal`, `backup`, `verify`).
- End-to-end orchestration, tier migration, and retention of sealed segments are left to the operator.

---

## 6. Other Notable Gaps

| Category              | Gap                                                                 | Impact |
|-----------------------|---------------------------------------------------------------------|--------|
| **Performance SLAs**  | No query timeouts, result size limits, or admission control        | Long cold scans or LLM calls can starve other work |
| **Upgrades**          | No documented migration path for manifest / checkpoint formats     | Risk during tablespace feature adoption |
| **Cost & Storage**    | No built-in tiering policies or object-store backend               | Operators must script movement of sealed segments |
| **Testing**           | Strong unit + concurrency tests, but limited public chaos / long-running endurance testing for enterprise SLAs | |

---

## Prioritized Recommendations

1. **Security quick wins**
   - Apply the standard auth middleware to `LazyRoutes` (even if read-only).
   - Add native TLS support flags.
   - Document and improve secret handling for models.

2. **Observability**
   - Add a Prometheus-compatible metrics endpoint exposing cache, segment, and query stats.
   - Move to structured logging.

3. **Operational hardening for tablespaces**
   - Better documentation and tooling for tier management, compaction, and backup of archives.
   - Add liveness/readiness endpoints that work in both normal and lazy modes.

4. **Rate limiting & resource controls**
   - Simple per-endpoint or global concurrency limits + query timeouts.

5. **Longer-term enterprise features** (aligns with ROADMAP)
   - Deeper RBAC / governance pack
   - Automatic failover or clearer guidance on using external orchestration for HA
   - Object-store backend for cold tiers
   - Query admission control and per-tenant isolation

---

## References

- [enterprise-readiness.md](enterprise-readiness.md) — Current honest capability matrix
- [design-tablespaces.md](design-tablespaces.md) — Architecture of the new cold storage layer
- [deploy.md](deploy.md) — Existing deployment recipes (already calls out many limitations)
- [ROADMAP.md](ROADMAP.md) — Planned governance and multi-tenancy work

---

**Note:** This document captures gaps as of the tablespaces-era code. Many items are known and tracked. The tablespaces work itself is a significant positive step toward enterprise-scale auditable cold storage.

*Generated from code review of the current repository state.*