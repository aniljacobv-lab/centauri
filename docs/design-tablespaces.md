# Design: tablespaces — disk-backed, compressed, tamper-proof storage

**Status:** storage core built &amp; tested (`internal/segment`); the offline
archiver (`WriteArchive`/`VerifyArchive`, `centauri archive`) and the
**run-on-an-archive** path (`OpenArchive`: replay compressed, chain-verified
segments + an appendable tail; `serve -data <archive-dir>`) and **online
crash-safe sealing** (`Seal` / `centauri seal`: roll the tail into a new
compressed segment via one atomic manifest switch to a fresh tail generation)
and **crash-orphan GC** (`GCArchive`, run by `centauri seal`) are built &amp;
tested. The **disk-backed index** is now reachable: `serve -lazy-index` opens an
archive holding only the current fact per subject in RAM (scales with live
subjects, not total events) and answers current/history/asof over HTTP, with a
Merkle-validated **pointer-checkpoint** (`lazy.ckpt`) that makes restart replay
only the tail + newly-sealed segments, plus keyword **SEARCH** (BM25, `/v1/search`)
and **causal trace** (`/v1/trace`) over cold data — so the lazy path now covers
the full read surface (current, history, asof, search, trace). Reads go through a
cached **archiveReader** (LRU of decompressed segments, so repeat queries hit
RAM), and `serve -lazy-index` ships a **Tablespace Console** dashboard (storage
inspector, one-click integrity verify, query console, cache metrics) plus an
honest [enterprise-readiness matrix](enterprise-readiness.md). ·
**Builds on:** [design-segmentation.md](design-segmentation.md),
[design-own-your-data.md](design-own-your-data.md)

The goal: make Centauri a full-fledged database that **scales with disk, not
RAM** — store as much as you have disk for — while staying a single
zero-dependency binary, and adding two things no mainstream DB combines:
**compression + cryptographic tamper-evidence** on cold data.

## The bottleneck and the fix

`--lazy` already keeps payloads on disk. What still scales with *total events* is
the **in-RAM index** (one metadata entry per event). The fix is to make RAM
scale with the **working set**:

1. **Segment the log** into sealed, immutable files + a small **manifest**.
2. **Zone maps** per segment (min/max times, namespaces, field stats) let a query
   **skip** segments that can't match (data skipping).
3. Keep only the **current-state pointer** (bounded by live subjects) and the
   **tiny zone maps** resident; serve `AS OF` / history as **pruned scans** of
   segments, demand-paged from disk via `mmap`.

So a decade of history on a 4 TB disk needs only live entities + zone maps in RAM.

## What's built now — `internal/segment` (stdlib only, fully tested)

The novel core is implemented and unit-tested, decoupled from the live store
(zero risk to the engine invariants):

- **Compression** — `Compress`/`Decompress` (flate, best). Sealed segments
  compress 5–10× on cold data; the hot tail stays uncompressed (appendable).
- **Tamper-proof** — `MerkleRoot` / `MerkleProof` / `VerifyProof`: each segment
  carries a Merkle root, so a single fact's inclusion is provable (and tampering
  detectable) in O(log n) without scanning the log. Domain-separated leaf/node
  hashing prevents second-preimage attacks.
- **Zone maps + pruning** — `ComputeZones` + `MayContainEffectiveAt` /
  `MayContainKnownBy` / `MayContainNamespace` / `MayMatchNumber` /
  `MayMatchString`. Predicates only ever *exclude* segments that cannot match;
  anything unprunable is scanned (same result, slower).
- **Crypto-erasure** — `NewKey` / `Seal` / `Open` (AES-256-GCM): payloads can be
  sealed per key; destroying a key makes them unreadable **while the hash chain
  stays intact** — GDPR-style "right to be forgotten" without breaking
  immutability or the audit trail.
- **Manifest** — `Manifest`/`Entry` + `SelectAsOf` (the catalog the engine reads
  first to decide which segments to open).

## Why this is unique

| Capability | Oracle / Postgres | Ledger DBs | **Centauri** |
|---|---|---|---|
| Compress cold data | ✓ | ✗ | ✓ (flate) |
| Cryptographic tamper-evidence | ✗ | ✓ | ✓ (hash chain + per-segment Merkle) |
| **Compressed *and* tamper-proof cold tier** | ✗ | ✗ | **✓** |
| Bi-temporal time-travel from cold disk | ✗ | ✗ | ✓ |
| Crypto-erasure (delete + keep audit) | ~ | ✗ | ✓ |
| Single zero-dependency binary | ✗ | ✗ | ✓ |

Compression keeps zone-map stats and the Merkle root/chain head **uncompressed in
the manifest**, so pruning and `verify` work without decompressing a thing.

## Tablespaces = tiers you point at

- **Tiers as directories:** `hot` (appendable tail, local SSD), `warm` (sealed
  segments on a big disk/NAS), `cold` (synced folder or bucket). A flag like
  `-tier warm=D:\centauri -tier cold=\\nas\archive` *is* adding a tablespace.
- **Portable, self-verifying segments:** each sealed segment is a content-
  addressed, compressed, Merkle-rooted file you can move between disks; `verify`
  walks them in manifest order.
- **`compact`** merges/tiers old segments, gated by `MinSlotCursor` so no
  un-consumed CDC is dropped.

## Disk-backed index — the RAM-scaling slice (in progress)

Today `OpenArchive` replays all segments into RAM, so RAM still scales with total
events. The foundation for fixing that is built: **disk-scan read primitives**
(`ScanHistory`, `ScanAsOf` in `scan.go`) answer a query by reading only the
zone-map-pruned segments from disk + the tail, with no full in-RAM index — and a
test proves they match the in-RAM engine exactly.

**Chosen: approach A.** The core is now built &amp; tested — `LazyIndex`
(`lazyindex.go`): a single streaming pass over the archive keeps in RAM only the
*current* fact per `(subject,facet)` (resolved with the engine's `beats` rule),
dropping superseded/historical events as they pass. `Current` is served from RAM;
`History`/`AsOf` stream the pruned segments from disk. A test asserts RAM holds
one pointer per subject (not per event) and that `Current`/`History` match the
in-RAM engine.

It is now reachable over HTTP: **`serve -lazy-index`** (on an archive directory)
opens the `LazyIndex` and mounts a small read-only surface (`api.LazyRoutes`):
`/v1/current` from the resident pointer, `/v1/history` and `/v1/asof` streamed
from pruned segments, and `/v1/lazy/stats` reporting the resident-key footprint.
The full in-RAM `Server` is untouched (writes still use a normal `serve`).

Restart is now near-O(tail), not O(total): a **pointer-checkpoint** (`lazy.ckpt`,
written atomically on start and validated by each folded segment's Merkle root)
persists the resident facts + the segments already folded in, so reopen seeds
from it and replays only segments sealed since + the always-fresh tail.
Re-applying the tail over the checkpoint is idempotent, so the result is
byte-identical to a full rebuild (a test corrupts every segment after
checkpointing and shows `Current` still answers from the checkpoint).

**Keyword SEARCH** over cold data is now covered too: `LazyIndex.Search` ranks
the resident current facts with Okapi BM25 (and `store.ScanSearch` does the same
standalone over an archive, retaining only docs that contain a query term so RAM
scales with query selectivity, not corpus size); both are exposed at
`/v1/search`. This is the keyword surface only — vector similarity, causal
centrality and recency/trust weighting need the full in-RAM index.

**Causal trace** over cold data closes the loop: `LazyIndex.Trace` /
`store.ScanTrace` (exposed at `/v1/trace`) walk an event's lineage — its causes
(inbound edges) or effects (outbound) — by streaming the archive's `Link`
records. A causal graph is inherently edge-sized, so this builds the adjacency
and materializes only the events that are link endpoints (a second pass), staying
well under a full replay; the walk mirrors `Store.Trace` (same edges, depth,
first-seen dedupe). The lazy read path now covers current, history, asof, search,
and trace — the full read surface a database-larger-than-RAM needs.

The three approaches considered, by RAM/latency/complexity:

- **A — Lazy current-pointer index (chosen).** On open, scan segments once
  to build ONLY a compact `current` pointer per `(subject,facet)` + the zone
  maps; never hold superseded/historical facts in RAM.
  `Current` from RAM; `History`/`AsOf`/`SEARCH` use pruned
  segment scans. RAM ≈ distinct live subjects (not total events). Open is O(total)
  the first time but a persisted pointer-checkpoint makes restarts O(1). Moderate
  build; the natural fit for our zone-map + segment design.
- **B — On-disk B-tree/LSM index.** A real persistent index so even open is O(1)
  and RAM is bounded regardless of subject count. Biggest engineering and the
  highest blind-write risk (it's a storage engine); strongest at extreme scale.
- **C — Scan-only (no per-event index).** Keep ~zero index; every query is a
  zone-map-pruned segment scan from disk. Simplest, lowest RAM, slowest queries;
  good for archival/cold datasets queried occasionally.

## Staged wiring into the live engine (each ship-verified, test-first)

1. **Manifest + sealing (single tier).** Split the log into sealed segments +
   `manifest.json`; reads/replay/`verify` walk segments. No behavior change —
   just multiple files. *(Heaviest/riskiest slice; encode the
   hash-chain-across-segments invariant in tests.)*
2. **Zone-map pruning** on `AS OF`/range/equality. Tests assert *pruned == unpruned*.
3. **Compression of sealed segments** (the `internal/segment` core, now wired).
4. **`mmap` reads + tiers** — demand-paged segment reads (`syscall.Mmap`,
   build-tagged per OS); configurable per-tier directories.
5. **Compaction + retention** gated by replication slots.
6. **Crypto-erasure** (payloads sealed per subject) and **per-segment Merkle
   proofs** exposed via `verify`/API.
7. **Disk-backed secondary index** — the one large remaining piece, only if
   arbitrary `WHERE` over cold history at scale becomes a real need; until then
   zone-map-pruned scans cover current / `AS OF` / range cheaply.

Each slice ships independently, default-off where it changes on-disk layout, with
tests proving identical query results to today's engine — so the invariants
(nothing erased, replay determinism, write-then-apply, the hash chain, zero
deps) hold at every step.
