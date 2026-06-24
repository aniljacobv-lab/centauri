package store

// Parallel segment verification — the concrete payoff of immutable, independent
// segments. Each segment carries its own Merkle root over its own lines, so
// recomputing and checking it depends on nothing else: the work fans out across
// all CPU cores, one segment per worker. (The cross-segment hash chain in
// VerifyArchive is the opposite — chain_i = H(chain_{i-1} ‖ line_i) — so it must
// be folded sequentially; that's why a single log can't be verified in parallel
// but a pile of segments can.)
//
// VerifyArchiveParallel is a fast integrity "scrub": it catches any tampered or
// bit-rotted byte inside any segment, scaling with cores. It does NOT check the
// cross-segment chain (segment ordering / continuity) — run VerifyArchive for
// that authoritative, sequential check.

import (
	"bytes"
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"sync"

	"github.com/proxima360/centauri/internal/segment"
)

// VerifyArchiveParallel recomputes every segment's Merkle root concurrently and
// checks it against the manifest. Returns the total record count and the first
// integrity error found (if any).
func VerifyArchiveParallel(destDir string) (records int64, err error) {
	b, err := os.ReadFile(filepath.Join(destDir, "manifest.json"))
	if err != nil {
		return 0, err
	}
	man, err := segment.ParseManifest(b)
	if err != nil {
		return 0, err
	}
	segs := man.Segments
	if len(segs) == 0 {
		return 0, nil
	}

	workers := runtime.GOMAXPROCS(0)
	if workers > len(segs) {
		workers = len(segs)
	}
	if workers < 1 {
		workers = 1
	}

	jobs := make(chan segment.Entry)
	type result struct {
		records int64
		err     error
	}
	results := make(chan result, len(segs))

	var wg sync.WaitGroup
	for i := 0; i < workers; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for e := range jobs {
				n, verr := verifySegmentMerkle(destDir, e)
				results <- result{records: n, err: verr}
			}
		}()
	}
	go func() {
		for _, e := range segs {
			jobs <- e
		}
		close(jobs)
	}()
	go func() {
		wg.Wait()
		close(results)
	}()

	for r := range results {
		if r.err != nil && err == nil {
			err = r.err // any error means corruption; report the first seen
		}
		records += r.records
	}
	return records, err
}

// verifySegmentMerkle decompresses one segment and checks its Merkle root.
func verifySegmentMerkle(destDir string, e segment.Entry) (int64, error) {
	data, err := os.ReadFile(filepath.Join(destDir, filepath.FromSlash(e.Path)))
	if err != nil {
		return 0, err
	}
	if e.Compressed {
		if data, err = segment.Decompress(data); err != nil {
			return 0, fmt.Errorf("segment %d: decompress: %w", e.ID, err)
		}
	}
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
		return 0, fmt.Errorf("segment %d: Merkle root mismatch (tampering or bit rot?)", e.ID)
	}
	return int64(len(lines)), nil
}
