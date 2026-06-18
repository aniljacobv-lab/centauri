# Design: log segmentation, manifests & zone maps

**Status:** design (not yet built) · **Companion to:** [design-own-your-data.md](design-own-your-data.md)

This captures the on-disk scaling design for Centauri's append-only log, so
the work is ready to pick up when we tackle "data far larger than RAM" and
cold/tiered storage. It folds in three proven, independently-validated ideas:

- **Postgres WAL segmentation + archiving** — split one ever-growing log into
  bounded, immutable segment files.
- **Apache Iceberg manifests** — a small index file listing each data file
  with its min/max column stats (validated again by Apache XTable, whose whole
  job is translating these manifests across formats).
- **Doris / BRIN zone maps** — per-block min/max so a scan *skips* blocks that
  can't match a predicate (data skipping).

Nothing here changes Centauri's semantics — immutability, bi-temporal reads,
the hash chain, replay determinism. It changes only *where bytes live* and
*which bytes a query must touch*.

---

## 1. The problem

Today the log is a single append-only file, fully replayed into RAM on open
(a checkpoint shortcuts this). That's simple and correct, but:

- **It grows forever.** "Nothing is erased" means the file only gets bigger.
- **One file, one tier.** No way to move cold history to cheap storage.
- **Cold start replays everything** below the checkpoint offset.
- **`AS OF` / range / field queries still consider all data**, even history
  that provably can't match (e.g. a 2026 query over 2019 segments).
- **`--lazy` keeps payloads on disk** but the *index* still must fit RAM and is
  built from the whole log.

## 2. Goals / non-goals

**Goals**
- Bound each on-disk file; allow old segments to live on cheap/cold storage
  (local dir → synced folder → object store) — see own-your-data doc.
- Let `AS OF`, time-range, and indexed-equality/range queries **skip whole
  segments** that can't contribute (data skipping).
- Preserve the tamper-evident hash chain *across* segment boundaries.
- Allow safe compaction/retention that never drops un-consumed CDC.

**Non-goals (for v1)**
- Distributed/sharded storage (still single-writer, single node + replicas).
- A new on-disk *record* format — segments hold the same JSONL records.
- Rewriting history. Compaction produces *new* segments; it never mutates
  committed bytes.

## 3. Overview

```
data/
  manifest.json              # the index: ordered segments + stats + chain heads
  segments/
    00000001.log             # sealed, immutable           (hot: local)
    00000002.log             # sealed, immutable           (cold: s3://… ref)
    ...
    current.log              # the open, appendable tail
  centauri.checkpoint        # in-memory index snapshot (unchanged role)
```

- The **active tail** (`current.log`) is the only writable file — single-writer
  lock as today. When it reaches a size/age threshold it is **sealed** into a
  numbered segment and a fresh tail starts.
- The **manifest** lists every segment in commit order with, per segment: byte
  range, record count, chain head at its end, and a **zone map** (see §5).
- Reads consult the manifest first to decide which segments to open.

## 4. Segments

- A segment is an immutable, record-aligned slice of the log (ends on `\n`).
- Sealing is a rename + manifest update; the bytes are identical to what was
  appended, so the **hash chain is unbroken** — the chain head recorded in the
  manifest for segment *N* equals the head at that offset of the old single
  log. `centauri verify` walks segments in order, exactly as it walks one file.
- Replication (`follow` / `ReadLog`) ships segments in order; a follower can
  fetch cold segments lazily.

## 5. Manifest + zone maps (the Iceberg/BRIN idea)

Each manifest entry carries cheap **summary stats** computed once at seal time:

```json
{
  "id": 17, "path": "segments/00000017.log", "bytes": 67108864,
  "records": 240131, "chain_head": "7f3a…",
  "tier": "local",
  "zones": {
    "effective_time": {"min": 1735689600000000, "max": 1738368000000000},
    "recorded_time":  {"min": 1735690000000000, "max": 1738400000000000},
    "subjects_prefix":{"item:": true, "order:": true},
    "fields": {
      "region": {"values": ["EU","US"]},        // low-cardinality: exact set
      "price_cents": {"min": 100, "max": 90000}  // numeric: min/max
    }
  }
}
```

**Pruning rule (data skipping).** Before opening segments for a query, intersect
the query's predicate with each segment's zone:

- `AS OF t` / `AS KNOWN AT t` / time ranges → skip segments whose
  `effective_time`/`recorded_time` range can't contain `t`.
- `WHERE region = 'EU'` → skip segments whose `region` value-set lacks `EU`
  (this is the `indexProbe` shape generalized to segments).
- `WHERE price_cents > 700` → skip segments whose `price_cents.max ≤ 700`.
- Subject pattern → skip segments whose namespace-prefix set can't match.

A segment that survives pruning is opened and scanned (or its in-RAM index used
if hot); the rest are never touched — the whole point.

> Correctness is by construction: zone maps only ever *exclude* segments that
> **cannot** match. A query that can't be safely pruned simply scans — same
> result as today, just slower. (Same safety property as the secondary index's
> scan fallback.)

Because superseding facts are appended later (in newer segments), a current
read still merges across surviving segments and picks the latest — exactly as
`open`/`bySubjectFacet` do now, but seeded only from un-pruned segments.

## 6. Compaction & retention

- **Compaction** merges small/old segments into larger ones (and can drop facts
  that are fully superseded *and* no longer needed for any retained `AS KNOWN
  AT` window). It writes **new** segments and updates the manifest atomically;
  originals are removed only after the new manifest is durable. Never mutates
  bytes in place.
- **Retention is gated by `MinSlotCursor()`** (already built): a compactor must
  not discard or relocate log below the lowest un-acked CDC slot, so no consumer
  ever misses changes. This is why replication slots came first.
- **Right-to-be-forgotten** is *not* TTL deletion (that breaks the chain and the
  audit story). It's **crypto-erasure**: payloads encrypted per subject;
  destroying a key renders those payloads unreadable while the chain stays
  intact. (Roadmap; noted here because segmentation is where it slots in.)

## 7. Tiered storage

- Segments carry a `tier` (`local` | `synced` | `object`). Hot (recent)
  segments stay local; cold ones move to a synced folder or a bring-your-own
  object-storage bucket (S3/GCS/Azure/R2) — see own-your-data doc.
- Zone-map pruning means cold segments are usually **not fetched at all**; only
  a query whose predicate overlaps a cold segment's zone pays the fetch.
- The object-store backend is a small `SegmentStore` interface
  (`Open(id) io.ReaderAt`, `Put`, `List`) with a `LocalDir` default — no
  third-party SDK in the core; a bucket backend is a separate, optional build.

## 8. Interaction with what already exists

| Component | Interaction |
|-----------|-------------|
| **Hash chain** (`integrity.go`) | Chain spans segments in order; per-segment head stored in the manifest; `verify` walks segments. Unchanged semantics. |
| **Checkpoint** | Still the in-RAM index snapshot; gains the manifest's covered offset. Cold start = load checkpoint + tail-replay the active segment only. |
| **`--lazy` payloads** | Generalizes: the offset index becomes `{segment, offset, len}`; payloads hydrate via the segment's `ReaderAt` (local or remote). |
| **Replication slots** | `MinSlotCursor` gates compaction/eviction (§6). |
| **Secondary index / `indexProbe`** | Same predicate analysis drives segment pruning; in-RAM index covers hot segments, zone maps cover cold ones. |
| **`follow` / `ReadLog`** | Ships sealed segments in order; follower fetches cold segments on demand. |
| **`merge`** | Still reconciles diverged logs; now emits a fresh segmented layout + manifest. |

## 9. Build order (incremental, test-first, invariant-preserving)

1. **Manifest + sealing, single tier.** Split the log into sealed segments +
   `current.log` + `manifest.json`; reads/replay/verify walk segments. No
   behavior change, just multiple files. (Heaviest, highest-risk slice — do it
   with the compiler in the loop; encode the hash-chain-across-segments
   invariant in tests.)
2. **Zone maps + pruning.** Compute per-segment stats at seal; prune on
   `AS OF`/range/equality. Tests assert *pruned result == unpruned result*.
3. **`SegmentStore` interface + lazy hydrate per segment.** Generalize the
   `--lazy` offset index to `{segment,offset,len}`.
4. **Compaction + retention gated by `MinSlotCursor`.**
5. **Object-store backend** (separate optional build; no core dep).
6. **Crypto-erasure** (separate track).

Each slice ships independently, default-off where it changes the on-disk
layout, with tests that prove identical query results to the pre-segmentation
engine.

## 10. Prior art

- **PostgreSQL** — WAL segment files + archiving; PITR by replaying segments.
- **Apache Iceberg / Hudi / Delta (and Apache XTable, which translates between
  them)** — manifests: immutable data files + per-file min/max stats + snapshot
  isolation. Our manifest is the same idea for an event log.
- **Apache Doris / PostgreSQL BRIN** — zone maps / block min-max for data
  skipping.
- **LSM-tree engines** — sealed immutable runs + background compaction;
  Centauri's "compaction never mutates, only supersedes" mirrors this without
  the read-amplification of merge-on-read deletes.
