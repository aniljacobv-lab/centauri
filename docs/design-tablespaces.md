# Design: tablespaces — disk-backed, compressed, tamper-proof storage

**Status:** storage core built &amp; tested (`internal/segment`); the offline
archiver (`WriteArchive`/`VerifyArchive`, `centauri archive`) and the
**run-on-an-archive** path (`OpenArchive`: replay compressed, chain-verified
segments + an appendable tail; `serve -data <archive-dir>`) and **online
crash-safe sealing** (`Seal` / `centauri seal`: roll the tail into a new
compressed segment via one atomic manifest switch to a fresh tail generation)
and **crash-orphan GC** (`GCArchive`, run by `centauri seal`) are built &amp;
tested. Remaining: a disk-backed index so RAM &lt; total data (the big scaling
win). · **Builds on:** [design-segmentation.md](design-segmentation.md),
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
