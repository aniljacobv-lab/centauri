package store

// archiveReader is the cached read layer under the lazy/archive query path. The
// naive scan re-reads and re-decompresses every segment on every query; that is
// correct but pays full I/O + inflate cost each time. archiveReader keeps the
// manifest and an LRU of decompressed segment buffers resident, so repeat
// queries (the interactive dashboard case) hit RAM instead of disk. Segments are
// immutable and content-addressed (keyed by path@merkle-root), so a cached
// buffer is always valid; the mutable tail is never cached. It also records hit/
// miss/decompression counts for the dashboard's performance panel.

import (
	"bytes"
	"fmt"
	"sync"

	"github.com/proxima360/centauri/internal/objstore"
	"github.com/proxima360/centauri/internal/segment"
)

// CacheStats is the segment-cache scorecard surfaced on the dashboard.
type CacheStats struct {
	CachedSegments int   `json:"cached_segments"`
	Capacity       int   `json:"capacity"`
	Hits           int64 `json:"hits"`
	Misses         int64 `json:"misses"`
	Decompressions int64 `json:"decompressions"`
	BytesCached    int64 `json:"bytes_cached"`
}

type archiveReader struct {
	bk     objstore.SegmentStore // local dir or S3-compatible
	verify bool                  // recompute Merkle on each fetch (untrusted/remote backend)
	cap    int

	mu          sync.Mutex
	man         *segment.Manifest
	cache       map[string][]byte // path@root -> decompressed bytes
	order       []string          // LRU order, oldest first
	hits        int64
	misses      int64
	decomp      int64
	bytesCached int64
}

// newArchiveReader reads a local archive directory (trusted, no per-read Merkle).
func newArchiveReader(dir string, capSegs int) *archiveReader {
	return newArchiveReaderBackend(objstore.NewLocalStore(dir), capSegs, false)
}

// newArchiveReaderBackend reads through any SegmentStore. verify recomputes each
// segment's Merkle root on fetch — essential for untrusted (object-store) bytes.
func newArchiveReaderBackend(bk objstore.SegmentStore, capSegs int, verify bool) *archiveReader {
	if capSegs <= 0 {
		capSegs = 32
	}
	return &archiveReader{bk: bk, verify: verify, cap: capSegs, cache: map[string][]byte{}}
}

// manifest returns the cached manifest, reading it once.
func (a *archiveReader) manifest() (*segment.Manifest, error) {
	a.mu.Lock()
	defer a.mu.Unlock()
	if a.man != nil {
		return a.man, nil
	}
	mb, err := a.bk.Get("manifest.json")
	if err != nil {
		return nil, err
	}
	man, err := segment.ParseManifest(mb)
	if err != nil {
		return nil, err
	}
	a.man = man
	return man, nil
}

// reload drops the cached manifest (call after the archive is re-sealed).
func (a *archiveReader) reload() {
	a.mu.Lock()
	a.man = nil
	a.mu.Unlock()
}

// segmentBytes returns a segment's decompressed bytes, from cache when possible.
func (a *archiveReader) segmentBytes(e segment.Entry) ([]byte, error) {
	ck := e.Path + "@" + e.MerkleRoot
	a.mu.Lock()
	if b, ok := a.cache[ck]; ok {
		a.hits++
		a.touchLocked(ck)
		a.mu.Unlock()
		return b, nil
	}
	a.misses++
	a.mu.Unlock()

	raw, err := a.bk.Get(e.Path)
	if err != nil {
		return nil, err
	}
	if e.Compressed {
		if raw, err = segment.Decompress(raw); err != nil {
			return nil, err
		}
		a.mu.Lock()
		a.decomp++
		a.mu.Unlock()
	}
	if a.verify {
		if err := verifyMerkle(raw, e); err != nil {
			return nil, err
		}
	}
	a.mu.Lock()
	a.cache[ck] = raw
	a.bytesCached += int64(len(raw))
	a.order = append(a.order, ck)
	a.evictLocked()
	a.mu.Unlock()
	return raw, nil
}

func (a *archiveReader) touchLocked(ck string) {
	for i, k := range a.order {
		if k == ck {
			a.order = append(a.order[:i], a.order[i+1:]...)
			break
		}
	}
	a.order = append(a.order, ck)
}

func (a *archiveReader) evictLocked() {
	for len(a.order) > a.cap {
		old := a.order[0]
		a.order = a.order[1:]
		if b, ok := a.cache[old]; ok {
			a.bytesCached -= int64(len(b))
			delete(a.cache, old)
		}
	}
}

// tailBytes reads the appendable tail (never cached). For a local archive this
// is the live tail file; for a remote (read-only) archive it's the tail object.
func (a *archiveReader) tailBytes() ([]byte, error) {
	man, err := a.manifest()
	if err != nil {
		return nil, err
	}
	tail := man.Tail
	if tail == "" {
		tail = "current.log"
	}
	return a.bk.Get(tail)
}

// verifyMerkle recomputes a segment's Merkle root over its (decompressed) lines
// and checks it against the manifest entry — detecting any tampered/corrupt byte
// from untrusted storage.
func verifyMerkle(data []byte, e segment.Entry) error {
	var lines [][]byte
	for len(data) > 0 {
		i := bytes.IndexByte(data, '\n')
		if i < 0 {
			lines = append(lines, data)
			data = nil
		} else {
			lines = append(lines, data[:i+1])
			data = data[i+1:]
		}
	}
	if segment.MerkleRootHex(lines) != e.MerkleRoot {
		return fmt.Errorf("segment %d: Merkle root mismatch on fetch (tampering or corruption?)", e.ID)
	}
	return nil
}

// Stats snapshots the cache counters.
func (a *archiveReader) Stats() CacheStats {
	a.mu.Lock()
	defer a.mu.Unlock()
	return CacheStats{
		CachedSegments: len(a.cache),
		Capacity:       a.cap,
		Hits:           a.hits,
		Misses:         a.misses,
		Decompressions: a.decomp,
		BytesCached:    a.bytesCached,
	}
}
