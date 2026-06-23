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
| Single zero-dependency binary | ✓ | Go stdlib only; no third-party runtime. |

## What it does not do (yet)

| Capability | Status | Notes |
|---|---|---|
| SQL / JDBC / ODBC | ✗ | Query language is CeQL (text, JSON AST, and REST). No SQL wire protocol. |
| Multi-statement ACID transactions | ✗ | Writes are single-fact or batch appends; there is no interactive `BEGIN…COMMIT` MVCC transaction model. |
| Concurrent multi-writer OLTP | ✗ | A single-writer lock serialises writes. Multi-master ingestion converges deterministically; it is not concurrent OLTP. |
| Disk secondary index for arbitrary `WHERE` | partial | Zone-map pruning + segment scans cover current/`AS OF`/range/keyword cheaply; there is no persisted B-tree/inverted index for arbitrary cold predicates yet. |
| Replication / HA failover | partial | Log shipping and durable CDC slots exist; automatic leader election / failover does not. |
| Role hierarchies / fine-grained RBAC | partial | Scoped read tokens (RLS) exist; there is no full role hierarchy or column masking. |

## How to read this

The ✓ rows are the deliberate differentiators — **compressed *and* cryptographically
tamper-proof cold storage with bi-temporal time travel from a single binary** is a
combination mainstream databases and ledger databases do not offer together. The
✗/partial rows are the honest cost of that focus: Centauri is a system of record
for auditable facts, not a drop-in replacement for a general-purpose OLTP RDBMS.

See also [design-tablespaces.md](design-tablespaces.md) for the storage engine and
the staged plan that closes the `partial` rows.
