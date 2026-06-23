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
	"os"
	"path/filepath"
	"sync"

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
	dir string
	cap int

	mu          sync.Mutex
	man         *segment.Manifest
	cache       map[string][]byte // path@root -> decompressed bytes
	order       []string          // LRU order, oldest first
	hits        int64
	misses      int64
	decomp      int64
	bytesCached int64
}

func newArchiveReader(dir string, capSegs int) *archiveReader {
	if capSegs <= 0 {
		capSegs = 32
	}
	return &archiveReader{dir: dir, cap: capSegs, cache: map[string][]byte{}}
}

// manifest returns the cached manifest, reading it once.
func (a *archiveReader) manifest() (*segment.Manifest, error) {
	a.mu.Lock()
	defer a.mu.Unlock()
	if a.man != nil {
		return a.man, nil
	}
	mb, err := os.ReadFile(filepath.Join(a.dir, "manifest.json"))
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

	raw, err := os.ReadFile(filepath.Join(a.dir, filepath.FromSlash(e.Path)))
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

// tailBytes reads the mutable tail (never cached).
func (a *archiveReader) tailBytes() ([]byte, error) {
	man, err := a.manifest()
	if err != nil {
		return nil, err
	}
	return os.ReadFile(archiveTailPath(a.dir, man))
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
