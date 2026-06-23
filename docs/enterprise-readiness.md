# Enterprise readiness — an honest capability matrix

Centauri aims to be an enterprise-grade store for facts that must never be
silently changed: append-only, cryptographically tamper-evident, compressed, and
queryable across both time axes from cold disk. This page states plainly what it
**does** and **does not** do today. Documentation honesty is policy here — we do
not inflate a ✗ to a ✓. The same matrix is shown live in the Tablespace Console
(`serve -lazy-index`, then open the dashboard).

## What it does

| Capability | Status | Notes |
|---|---|---|
| Immutable append-only log | ✓ | Nothing is ever erased; an "update" appends a superseding fact, a "delete" is a RETIRE marker. |
| Cryptographic tamper-evidence | ✓ | Per-line SHA-256 hash chain across the whole log + a per-segment Merkle root; `verify` recomputes both. |
| Compressed cold tier | ✓ | Sealed segments are flate-compressed (typically 5–10× on cold data); zone-map stats and the Merkle root/chain head stay uncompressed in the manifest so pruning and verification need no decompression. |
| Bi-temporal time travel | ✓ | `AS OF` (valid time) and `AS KNOWN AT` (transaction time), answered from cold segments. |
| Crypto-erasure ("right to be forgotten") | ✓ | Destroying a per-segment AES-256-GCM key makes payloads unreadable while the hash chain stays intact — delete data without breaking the audit trail. |
| Scales beyond RAM | ✓ | The lazy index keeps only the current fact per subject + small zone maps resident; history/asof/search/trace stream zone-map-pruned segments from disk. |
| Online integrity verification | ✓ | `centauri seal` verifies after sealing; the dashboard `Verify` button / `/v1/verify` checks segments + chain with no downtime. |
| Fast restart on large archives | ✓ | A Merkle-validated pointer-checkpoint (`lazy.ckpt`) lets restart replay only the tail + newly-sealed segments. |
| Hot segment caching | ✓ | An LRU of decompressed segments keeps repeat queries in RAM; hit/miss/decompression counts are on the dashboard. |
| Secondary index — equality over current state | ✓ | A resident field index makes `WHERE field = value` over current facts a sub-linear map lookup (high-cardinality fields fall back to scan, same as the in-RAM engine). |
| Single zero-dependency binary | ✓ | Go stdlib only; no third-party runtime. |
| Native TLS / HTTPS | ✓ | `-tls-cert`/`-tls-key` on `serve` and `serve -lazy-index` — no reverse proxy required (one is still fine). |
| Token auth on data routes (both modes) | ✓ | Normal `serve` gates every `/v1/*` route behind `-token` (admin) / `-read-token` (read-only); `serve -lazy-index` now does the same on its read routes (closing an earlier bypass). The dashboard, `/v1/version`, health probes, and `/metrics` stay open by design (no fact data). |
| Prometheus metrics + health probes | ✓ | `/metrics` (text exposition), `/livez`, `/readyz` on **both** the normal `serve` and `serve -lazy-index` paths — Prometheus/Grafana scraping and Kubernetes liveness/readiness in either deployment mode. The lazy `/metrics` adds segment-cache gauges; the normal one exposes store counters + build info. |

## What it does not do (yet)

| Capability | Status | Notes |
|---|---|---|
| SQL / JDBC / ODBC | ✗ | Query language is CeQL (text, JSON AST, and REST). No SQL wire protocol. |
| Multi-statement ACID transactions | ✗ | Writes are single-fact or batch appends; there is no interactive `BEGIN…COMMIT` MVCC transaction model. |
| Concurrent multi-writer OLTP | ✗ | A single-writer lock serialises writes. Multi-master ingestion converges deterministically; it is not concurrent OLTP. |
| Index for arbitrary historical / range `WHERE` | partial | Equality over *current* state is indexed (above); cold *history* and *range* predicates use zone-map pruning + segment scans — there is no persisted B-tree/inverted index for arbitrary cold predicates yet. |
| Replication / HA failover | partial | Log shipping and durable CDC slots exist; automatic leader election / failover does not. |
| Role hierarchies / fine-grained RBAC | partial | Scoped read tokens (RLS) exist; there is no full role hierarchy, OIDC/JWT/LDAP integration, token expiry/rotation, or column masking. |
| At-rest encryption of the hot tier | partial | Sealed segments support per-segment AES-256-GCM (crypto-erasure); the hot tail, manifest, and `lazy.ckpt` are not encrypted — use volume/disk encryption for those. |
| External secrets / KMS integration | ✗ | Model credentials come from environment variables (`auth_env`); no Vault / cloud Secrets Manager / KMS envelope encryption. |
| Rate limiting / quotas / admission control | partial | `serve -lazy-index` has a global concurrency cap (`-max-concurrency`, HTTP 429) and a per-request timeout (`-query-timeout`, HTTP 503) to protect against heavy cold scans / SEARCH. The write path and per-tenant quotas are not yet limited. |
| Structured logging / OpenTelemetry traces | ✗ | Logs are line-oriented (`log`/`fmt`); `/metrics` is exposed, but there are no structured logs, trace spans, or correlation IDs. |
| Object-store cold tier (S3/GCS) | ✗ | Segments are portable files; tiers are manual directories. No native object-store backend yet. |
| Automated retention / legal hold | ✗ | Retention is manual (`RETIRE`); no scheduled purging or legal-hold policy engine. |
| Automatic failover / leader election | ✗ | Log shipping (`follow`) + CDC slots (`sync`) exist; HA orchestration is external. |

## How to read this

The ✓ rows are the deliberate differentiators — **compressed *and* cryptographically
tamper-proof cold storage with bi-temporal time travel from a single binary** is a
combination mainstream databases and ledger databases do not offer together. The
✗/partial rows are the honest cost of that focus: Centauri is a system of record
for auditable facts, not a drop-in replacement for a general-purpose OLTP RDBMS.

See also [design-tablespaces.md](design-tablespaces.md) for the storage engine and
the staged plan that closes the `partial` rows.
