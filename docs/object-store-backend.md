# Object Store Backend for Centauri Tablespaces

**Status:** Planned (described in design docs; not yet implemented in code). Current implementation is strictly local-filesystem based.

**Goal:** Allow sealed, immutable segments to live in user-owned object storage (S3, GCS, Azure Blob, Cloudflare R2, MinIO, etc.) while keeping the hot appendable tail local. This enables cheap, durable, scalable cold storage without requiring local disk for historical data.

## Current Architecture (File-based)

Centauri has moved from a single growing `.log` file to **segmented tablespaces**:

- The log is split into immutable **sealed segments** (compressed with flate, ~100k records or configurable size).
- A small `manifest.json` acts as the catalog (ordered list of segments + current tail name).
- Each `segment.Entry` contains:
  - Path, bytes, record count
  - ChainHead (for overall hash chain)
  - MerkleRoot (for per-segment tamper proof)
  - Tier, Compressed, Encrypted flags
  - Zones (min/max effective/recorded time, namespaces, field stats for pruning)
- Hot tail is the current appendable file (uncompressed).
- Sealing is online and crash-safe: write new segment, atomically rewrite manifest with new tail generation.

Core packages:
- `internal/segment/` : Compress/Decompress, Seal/Open (AES-256-GCM crypto-erasure), Manifest + Entry + Zones + Merkle.
- `internal/store/archive.go`, `archive_reader.go`, `scan.go`, etc.: WriteArchive, archiveReader (LRU of decompressed segments), zone-pruned scans (ScanHistory, ScanAsOf, etc.).
- LazyIndex (`lazyindex.go`) keeps only current facts + covered segments in RAM; history/asof/search/trace stream from disk via the reader.
- All reads go through `archiveReader.segmentBytes(e)` which does local `os.ReadFile` + decompress + cache.

Segments are **content-addressed** by `path@merkle-root` for safe caching.

**Tiers today:** Hard-coded to local directories. `Tier` field in Entry ("local" | "warm" | "cold").

## Planned ObjectStore Backend (from design)

See `docs/design-own-your-data.md` and `docs/design-tablespaces.md`:

A small pluggable `SegmentStore` interface so the engine is backend-agnostic:

```go
// Proposed (not yet in code)
type SegmentStore interface {
    // Manifest operations
    LoadManifest() (*segment.Manifest, error)
    SaveManifest(m *segment.Manifest) error

    // Segment data
    ReadSegment(e segment.Entry) ([]byte, error)  // returns compressed bytes
    WriteSegment(name string, data []byte) error  // for sealing

    ListSegments() ([]segment.Entry, error) // or rely on manifest
    DeleteSegment(path string) error        // for compaction / crypto-erasure cleanup?
}
```

Backends:
- **LocalDir** (current): `os.*` + `filepath` on a directory.
- **SyncedFolder**: LocalDir + lock + idle sync hints.
- **ObjectStore**: S3 / GCS / etc.

For ObjectStore (design quote):
> "your *own* S3 / GCS / Azure Blob / R2 bucket: each sealed segment is one immutable object, the manifest object is the index of record; only the open segment is local until sealed, then uploaded. Signed HTTP PUT/GET via stdlib `net/http` (no third-party SDK required for the basic verbs)."

Only sealed segments go to the object store. The active tail stays local until sealed (then uploaded + manifest updated).

## Detailed Design for Object Store Backend

### 1. Storage Layout in Bucket
- `manifest.json` (the single source of truth for segment order and metadata).
- `segments/00000001.seg`, `segments/00000002.seg`, ... (compressed + optionally encrypted bytes).
- Optional: `tails/` or versioned tails during sealing.
- Keys should be content-addressable where possible (e.g. include merkle root in name for safety).

The `Path` field in `segment.Entry` becomes the object key (relative or full).

### 2. Sealing Flow (Hybrid Local + Remote)
1. Write records to local tail (as today).
2. When sealing threshold hit:
   - Compress + (optionally) Seal with key.
   - Compute Merkle + Zones.
   - Write the segment bytes **locally first** (for atomicity + verification).
   - Upload the segment object via signed PUT.
   - Atomically update `manifest.json` (new segment entry + new tail name).
   - Optionally delete local copy of the now-sealed segment (or keep as cache).
3. The manifest update must be atomic or use conditional writes / versioning.

Only the open tail + in-flight seal work is local.

### 3. Read Path (LazyIndex + archiveReader)
The `archiveReader` (or a new backend-abstracted reader) will:
- Load manifest (from local cache or direct GET of manifest object).
- For a query: use `Manifest.SelectAsOf(...)` to prune by zones.
- For each needed segment:
  - Check local cache / temp dir.
  - If not present: signed GET of the object → verify Merkle root against Entry → decompress (and decrypt if Encrypted) → LRU cache.
- Tail is always read locally.
- Cache key: `path@merkle-root` (immutable guarantee).

Range requests are possible for very large segments, but full-segment fetch + decompress is simpler and matches current design.

### 4. Authentication / Signing (Zero-Deps Constraint)
Critical: **no AWS SDK, no google.golang.org/cloud**, etc.

- Use `net/http` directly.
- Implement minimal signing:
  - For **S3-compatible** (AWS S3, MinIO, R2, Backblaze, etc.): AWS Signature Version 4 (SigV4). Pure-Go implementations exist in ~150-250 LOC (canonical request, string-to-sign, HMAC-SHA256).
  - For **GCS**: HMAC (if enabled) or OAuth2 (more complex; use workload identity or service account JSON with stdlib crypto).
  - Azure: Shared Key or SAS tokens.
- Recommend starting with **S3-compatible only** (most common for "own bucket").
- Credentials: via env vars (`AWS_ACCESS_KEY_ID`, `AWS_SECRET_ACCESS_KEY`, or provider-specific), or IAM instance roles (for AWS: query metadata service with stdlib HTTP — no SDK needed).
- Never hard-code secrets.

The design explicitly says "Signed HTTP PUT/GET via stdlib `net/http`".

You will need to add a small `internal/objectstore/signer.go` or similar (stdlib only).

### 5. Atomicity & Consistency Challenges
Object stores are **eventually consistent** (especially LIST and overwrite).

Solutions:
- **Manifest as source of truth**: Always read the latest manifest (use ETag/If-None-Match or versioning).
- For sealing: Upload segment first (immutable), *then* do a conditional PUT on manifest (If-Match current ETag). Retry on conflict.
- Use bucket versioning + object lock where available for extra safety.
- On startup / reload: re-validate manifest against chain heads / merkle roots.
- Avoid relying on LIST for correctness; prefer reading manifest.

### 6. Integration Points
- Extend `archiveReader` (or introduce `backend` interface) to take a `SegmentStore`.
- `store.OpenArchive(dir string, ...)` → generalize to accept a backend.
- `WriteArchive` / `Seal` commands will need to know the target tier.
- LazyIndex remains unchanged (it talks to the reader).
- `segment.Entry.Tier` and `Path` will indicate remote location.
- Commands in `main.go`:
  ```bash
  centauri archive ... --tier cold=s3://my-bucket/centauri/
  centauri serve -data /local/hot -tier cold=s3://... -lazy-index
  ```
- Or flags like `-object-store s3://...`.

Manifest operations will need to support remote fetch/save.

### 7. Crypto-Erasure with Objects
- The per-segment AES key (from `segment.NewKey()`) can live:
  - Locally (user manages deletion).
  - In a separate KMS (user's own).
  - Or as an object with its own lifecycle (user deletes the key object).
- The hash chain / Merkle is computed over the *ciphertext*, so deleting the key leaves verifiable (but unreadable) history.

### 8. Performance & Caching
- High latency for first cold read → rely on LRU + OS page cache + application cache (same as today).
- Multipart uploads for large segments (>100MB).
- Prefetch / background warming for known hot segments.
- ETags / If-None-Match to avoid re-downloads.
- Cost control: track GET/PUT counts (add to `/metrics`).

Expected: 5-10x compression on cold data makes object storage very economical.

### 9. Security
- Use IAM roles / least-privilege policies (read-only for readers, write for sealer).
- Prefer instance metadata / workload identity over long-lived keys.
- TLS is already required (use the new native `-tls-*` or reverse proxy).
- Verify every downloaded segment (Merkle + chain) — untrusted storage is fine because of tamper evidence.

### 10. Commands & Operations
- `centauri archive -data local.log -to s3://bucket/prefix/`
- `centauri seal` (when running on archive) should support remote.
- `centauri verify` works over objects.
- `centauri tablespace-demo` extended for object tier.
- Migration: `centauri compact` or tier-move tool.
- GC / compaction of old segments while preserving CDC slots (`MinSlotCursor`).

### Challenges & Mitigations (stdlib only)
- **SigV4 implementation**: Non-trivial but bounded. There are well-known pure-Go snippets (canonical request hashing, etc.). Start with GET/PUT/HEAD. Test against MinIO.
- **Eventual consistency**: Design around manifest as authority + retries + content addressing.
- **No LIST dependency for correctness**: Prefer manifest-driven.
- **Streaming large objects**: `io.Copy` + http.
- **Testing**: Use MinIO in Docker for tests (already common pattern). Mock HTTP for unit tests.
- **Error handling**: Distinguish "object not found" vs auth vs network.
- **Presigned URLs** (optional): For client-side direct access in future.
- **Multi-cloud**: Abstract the signer (S3Signer, GCSSigner).

### Suggested Implementation Order
1. Define the `SegmentStore` (or `ArchiveBackend`) interface in `internal/segment` or `internal/store`.
2. Refactor current file code into `LocalDir` implementation (extract from `archive_reader.go` etc.).
3. Implement minimal S3-compatible client (`internal/objectstore/s3.go`):
   - Signer
   - PutObject, GetObject, HeadObject, GetObject with range (future)
   - Load/Save manifest (as JSON object)
4. Wire into `archiveReader` / `WriteArchive` / `OpenArchive` with tier awareness.
5. Add CLI flags and config for object store (endpoint, region, creds via env).
6. Update LazyIndex, sealing, verify, compaction paths.
7. Add tests (with MinIO).
8. Update docs (`design-tablespaces.md`, `enterprise-readiness.md`, `deploy.md`).

### Relation to Existing Code
- The segment/ package is already backend-agnostic (pure data formats).
- The store layer (`archive_*`, `scan.go`, `lazyindex.go`) is the place to abstract I/O.
- All tamper-evidence (chain, Merkle, zones) stays the same regardless of where bytes come from.
- Crypto-erasure works the same.

This keeps the "one binary, own your data" philosophy: the object store is *your* bucket, Centauri just speaks HTTP to it.

### Open Questions for Implementation
- Exact interface (how much to abstract vs. current dir-based reader).
- Support for "bring your own" via FUSE (rclone mount) as interim.
- How to handle the open tail in pure object store (append to object is hard; keep local tail always).
- Billing / egress cost visibility in metrics.
- Versioning of the backend protocol (manifest version already exists).

For a concrete starting point, the design in `docs/design-own-your-data.md` (section 5) is the authoritative spec.

This backend would close one of the major "✗" rows for true enterprise cold storage at scale.