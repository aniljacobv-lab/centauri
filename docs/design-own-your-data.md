# Own Your Data — Centauri storage design

**Principle:** your data never lives on our infrastructure. It lives on a device
or storage *you* control — a laptop, a server, a USB stick, a NAS, a synced
folder (OneDrive / Google Drive / Dropbox), or your own object-storage bucket.
No license, no per-core fees, no vendor that can hold your history hostage.

This is the differentiator: **the capabilities that matter for a system of
record — bi-temporal time travel, causal lineage, tamper-evidence, topology,
search, an agent interface — on infrastructure you own.**

---

## Where we are today

Centauri's durable form is already **a single append-only JSONL log** sealed by
a SHA-256 hash chain (`internal/store/store.go`, `integrity.go`), with in-memory
indexes rebuilt on startup by replay (or from a checkpoint). So it is *not*
RAM-only — the file is the source of truth. The two limits to lift:

1. **All event payloads sit in RAM**, so the working set must fit in memory.
2. **It's one growing file**, which is awkward for incremental cloud sync and
   archival.

## Target architecture (designed; staged for implementation)

### 1. Segmented log
Split the log into **immutable, size-capped segments** (e.g. 64 MB, or by month):
`seg-000001.log`, `seg-000002.log`, … plus a small `manifest.json` listing each
segment in order with its event count and the **hash-chain head at its seal**.
Appends go to the one open segment; when it fills, seal it (immutable) and open
the next.
*Why:* cloud sync only re-uploads the small open segment; cold segments archive
cleanly; files stay bounded.

### 2. Offset index + lazy reads (breaks the RAM ceiling, pure stdlib)
Keep only a lightweight index in RAM: `subject|facet → [(segment, offset,
length, key metadata)]`, hashes, counters — **not** the payloads. Read values on
demand with `os.File.ReadAt(offset)`; the OS page cache keeps hot data fast.
RAM then scales with the *number* of events × tens of bytes, not total payload
size. No `mmap` (which would need a third-party package and break the
zero-dependency rule) — `ReadAt` + the OS cache gets ~the same benefit.

### 3. Checkpoint = the index, not the data
`checkpoint.go` serializes the offset index + counters + chain head (small), so
restart is fast and verifiable without a full replay. `apply()` records offsets
instead of retaining payloads; `resetState()` clears the index. Replay
determinism (invariant #2) is preserved — state still flows only through
`apply()`.

### 4. Hash chain spans segments
The chain runs continuously across segments; each manifest entry records the
head at seal time, so `verify` walks segments in order. Keep `commit`, `replay`,
and `IngestRaw` byte-identical on the chain (invariant #4) and update the chain
tests.

### 5. Pluggable storage backends — all user-owned
A small `SegmentStore` interface (open/read/append/seal/list) so the engine
doesn't care *where* the bytes live:
- **LocalDir** — a folder of segments + manifest (default: desktop, server, USB, NAS).
- **SyncedFolder** — same, plus a single-writer lockfile and "sync when idle"
  guidance for OneDrive / Drive / Dropbox / iCloud.
- **ObjectStore** — your *own* S3 / GCS / Azure Blob / R2 bucket: each sealed
  segment is one immutable object, the manifest object is the index of record;
  only the open segment is local until sealed, then uploaded. Signed HTTP
  PUT/GET via stdlib `net/http` (no third-party SDK required for the basic verbs).

### 6. Multi-device safety + merge ("Git for your data")
- A **single-writer lock** in the folder/bucket prevents concurrent corruption
  (others open read-only).
- **Merge diverged copies:** because events are immutable with unique IDs and a
  hash chain, reconcile = find the shared chain prefix, union the disjoint events
  by `event_id`, re-order by `recorded_time`, re-seal and re-chain. Builds on the
  replication primitives already in `ship.go`.

### 7. Unchanged by design
Append-only, nothing erased, bi-temporal queries, CeQL, topology, BM25/vector
search, MCP — the engine swap is invisible to queries.

## Honest scope / non-goals
- Still **single active writer** (no multi-master consensus).
- The **index** must fit RAM (payloads need not) — fine for billions of small
  events on a normal box; not a petabyte OLAP warehouse.
- Not a distributed database. Replicas are readable copies, not a cluster.

## Suggested build order
1. **Offset index + `ReadAt`** (payloads leave RAM) — biggest win, lowest blast
   radius; touches `store.go`/`checkpoint.go` only, no format change.
2. **Segmentation + manifest** — enables sync and archival.
3. **SyncedFolder mode** (lock + idle sync) and the **merge** tool.
4. **ObjectStore backend** (your own bucket).

## Positioning vs. Oracle
| | Oracle | Centauri |
|---|---|---|
| Where data lives | Their licensing terms / their cloud | **Your** device, server, folder, or bucket |
| Cost | Per-core / per-seat licenses + support | **Free**, OSS license — pay only for hardware you own |
| Format | Proprietary | Open JSONL segments; `centauri export` any time |
| Lock-in | High | **None** — read, move, back up, or delete it yourself |
| Footprint | Heavy install | One static binary, zero dependencies |

"Oracle-grade memory for what happened, when, why, and how much to trust it —
without the license, and on infrastructure you control."
