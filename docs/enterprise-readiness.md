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
| Structured request logging + correlation IDs | ✓ | stdlib `log/slog` request logs (`-log-format text\|json`, `-log-level`) — one line per request with method, path, status, bytes, duration, and an `X-Request-ID` correlation id (honoured inbound, echoed in the response), on both serve modes. Zero third-party deps. |

## What it does not do (yet)

| Capability | Status | Notes |
|---|---|---|
| SQL (read-only SELECT subset) | partial | A lean SQL `SELECT` (WHERE / GROUP BY / HAVING / ORDER BY / LIMIT, plus `AS OF` and SQL:2011 `FOR SYSTEM_TIME AS OF`) transpiles to CeQL at `POST/GET /v1/sql` — a familiar front door for SQL-speaking humans and LLMs. It is **not** a SQL **wire protocol**: there is no JDBC/ODBC/pgwire, so BI tools (Tableau/Power BI/DBeaver) cannot connect directly, and writes still use CeQL. |
| Multi-statement ACID transactions | ✗ | Writes are single-fact or batch appends; there is no interactive `BEGIN…COMMIT` MVCC transaction model. |
| Concurrent multi-writer OLTP | ✗ | A single-writer lock serialises writes, and the per-line hash chain is inherently sequential (line N+1 hashes line N) — so one log cannot be appended in parallel, by design. Multi-master ingestion converges deterministically; it is not concurrent OLTP. |
| Write throughput under concurrency | partial | Two mechanisms. (1) `-group-commit` coalesces concurrent appends into **one fsync** per batch on a single chain. (2) **`serve -shards N`** partitions subjects across N independent shard logs (each its own chain, lock, committer) and writes them in parallel over HTTP (~N× throughput) — `POST /v1/append` (parallel), routed `current`/`history`/`asof`, `/v1/subjects` (union), `/v1/shards` (distribution), and `/v1/query` for a concrete subject. Not provided: cross-shard atomic transactions, cross-shard causal links, and wildcard/global CeQL or SEARCH fan-out in sharded mode (use single-store serve for those). |
| Index for arbitrary historical / range `WHERE` | partial | Equality over *current* state is indexed (above); cold *history* and *range* predicates use zone-map pruning + segment scans — there is no persisted B-tree/inverted index for arbitrary cold predicates yet. |
| Replication / HA failover | partial | Log shipping and durable CDC slots exist; automatic leader election / failover does not. |
| Role hierarchies / fine-grained RBAC | partial | Scoped read tokens (RLS) exist; there is no full role hierarchy, OIDC/JWT/LDAP integration, token expiry/rotation, or column masking. |
| At-rest encryption of the hot tier | partial | Sealed segments support per-segment AES-256-GCM (crypto-erasure); the hot tail, manifest, and `lazy.ckpt` are not encrypted — use volume/disk encryption for those. |
| External secrets / KMS integration | ✗ | Model credentials come from environment variables (`auth_env`); no Vault / cloud Secrets Manager / KMS envelope encryption. |
| Admission control (concurrency + timeouts) | ✓ | `-max-concurrency` (HTTP 429) and `-query-timeout` (HTTP 503) apply to the normal `serve`/`desktop` hot path **and** `serve -lazy-index` — bounding heavy writes, queries, SEARCH, and synchronous LLM calls. Streaming endpoints (`/v1/watch`, `/v1/changes`, `/v1/log`) are exempt (never cut off, never hold a slot). Per-database/per-tenant quotas are still not enforced. |
| OpenTelemetry traces | ✗ | Request logs are structured with correlation IDs (above) and `/metrics` is exposed, but there are no OTel trace spans yet, and internal startup/error logs are still line-oriented (`log`/`fmt`). |
| Object-store cold tier (S3/GCS) | ✗ | Segments are portable files; tiers are manual directories. No native object-store backend yet. |
| Retention &amp; legal hold | partial | `centauri retention -pattern '…' -older-than N [-apply]` RETIREs stale subjects (history is kept — never erased; dry-run by default, schedule `-apply` for a recurring policy). A `hold:<name>` fact carrying a subject `pattern` puts matching subjects under a **legal hold** that retention skips. Not yet: a stored-policy scheduler inside the binary, a crypto-erase retention action, or engine-level hold enforcement against *manual* writes. |
| Automatic failover / leader election | ✗ | Log shipping (`follow`) + CDC slots (`sync`) exist; HA orchestration is external. |

## How to read this

The ✓ rows are the deliberate differentiators — **compressed *and* cryptographically
tamper-proof cold storage with bi-temporal time travel from a single binary** is a
combination mainstream databases and ledger databases do not offer together. The
✗/partial rows are the honest cost of that focus: Centauri is a system of record
for auditable facts, not a drop-in replacement for a general-purpose OLTP RDBMS.

See also [design-tablespaces.md](design-tablespaces.md) for the storage engine and
the staged plan that closes the `partial` rows.
