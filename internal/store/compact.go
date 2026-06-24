package store

// Compaction merges consecutive sealed segments into fewer, larger ones —
// reining in the segment proliferation that frequent auto-sealing causes (fewer
// files, a smaller manifest, faster sequential scans). It is NOT garbage
// collection: nothing is erased, every line is preserved in its exact order, so
// the merged segment's lines are byte-identical to the concatenation of its
// inputs and the cross-segment hash chain is unchanged (a merged segment simply
// inherits its last input's chain head). VerifyArchive after compaction
// reproduces the same chain head — checked here as a safety net.
//
// Because each merge group is independent, the groups run in parallel; only the
// final manifest swap is sequential. Offline: it takes the single-writer lock,
// so it refuses to run against a live server.

import (
	"bytes"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"sync"

	"github.com/proxima360/centauri/internal/model"
	"github.com/proxima360/centauri/internal/segment"
)

// maxSegID returns the largest segment id in the manifest (0 if none). Used to
// hand out monotonic ids that never collide, even after compaction.
func maxSegID(man *segment.Manifest) int {
	m := 0
	for _, e := range man.Segments {
		if e.ID > m {
			m = e.ID
		}
	}
	return m
}

// CompactArchive merges every run of `groupSize` consecutive sealed segments
// into one, returning the segment count before and after.
func CompactArchive(dir string, groupSize int) (before, after int, err error) {
	if groupSize < 2 {
		return 0, 0, fmt.Errorf("compact: group size must be >= 2")
	}
	mb, err := os.ReadFile(filepath.Join(dir, "manifest.json"))
	if err != nil {
		return 0, 0, err
	}
	man, err := segment.ParseManifest(mb)
	if err != nil {
		return 0, 0, err
	}
	tail := man.Tail
	if tail == "" {
		tail = "current.log"
	}
	// Exclusive access — refuse if a server holds the archive.
	lp, err := acquireLock(filepath.Join(dir, tail))
	if err != nil {
		return 0, 0, fmt.Errorf("compact: archive is in use (a server may be running): %w", err)
	}
	defer releaseLock(lp)

	// Re-read under the lock.
	if mb, err = os.ReadFile(filepath.Join(dir, "manifest.json")); err != nil {
		return 0, 0, err
	}
	if man, err = segment.ParseManifest(mb); err != nil {
		return 0, 0, err
	}
	segs := man.Segments
	before = len(segs)
	if before < 2 {
		return before, before, nil
	}

	// Plan: split into consecutive groups; assign fresh ids (above any existing,
	// so the new merged files never overwrite a live one) to merged groups.
	type group struct {
		entries  []segment.Entry
		mergedID int // 0 = pass through unchanged
	}
	nextID := maxSegID(man) + 1
	var groups []group
	for i := 0; i < len(segs); i += groupSize {
		end := i + groupSize
		if end > len(segs) {
			end = len(segs)
		}
		g := segs[i:end]
		if len(g) == 1 {
			groups = append(groups, group{entries: g, mergedID: 0})
		} else {
			groups = append(groups, group{entries: g, mergedID: nextID})
			nextID++
		}
	}

	// Merge groups in parallel; each produces one manifest entry.
	out := make([]segment.Entry, len(groups))
	errs := make([]error, len(groups))
	sem := make(chan struct{}, runtime.GOMAXPROCS(0))
	var wg sync.WaitGroup
	for gi := range groups {
		wg.Add(1)
		go func(gi int) {
			defer wg.Done()
			sem <- struct{}{}
			defer func() { <-sem }()
			g := groups[gi]
			if g.mergedID == 0 {
				out[gi] = g.entries[0] // passthrough: keep file + entry
				return
			}
			out[gi], errs[gi] = mergeSegments(dir, g.mergedID, g.entries)
		}(gi)
	}
	wg.Wait()
	for _, e := range errs {
		if e != nil {
			return before, 0, e
		}
	}

	// Atomic manifest swap to the merged set, then GC the merged-away originals.
	newMan := &segment.Manifest{Version: man.Version, Tail: man.Tail, Segments: out}
	if err := writeManifestAtomic(dir, newMan); err != nil {
		return before, 0, err
	}
	_, _ = GCArchive(dir) // remove the now-unreferenced original segment files

	// Safety net: the chain head must be exactly what it was before.
	if _, _, verr := VerifyArchive(dir); verr != nil {
		return before, len(out), fmt.Errorf("compact: post-compaction verify FAILED: %w", verr)
	}
	return before, len(out), nil
}

// mergeSegments writes the concatenation (in order) of the given segments as one
// new segment with id `newID`, preserving line order so the chain head is the
// last input's chain head.
func mergeSegments(dir string, newID int, entries []segment.Entry) (segment.Entry, error) {
	var lines [][]byte
	var events []*model.Event
	for _, e := range entries {
		data, err := os.ReadFile(filepath.Join(dir, filepath.FromSlash(e.Path)))
		if err != nil {
			return segment.Entry{}, err
		}
		if e.Compressed {
			if data, err = segment.Decompress(data); err != nil {
				return segment.Entry{}, fmt.Errorf("segment %d: decompress: %w", e.ID, err)
			}
		}
		for len(data) > 0 {
			i := bytes.IndexByte(data, '\n')
			var ln []byte
			if i < 0 {
				ln, data = data, nil
			} else {
				ln, data = data[:i+1], data[i+1:]
			}
			lines = append(lines, ln)
			if t := bytes.TrimSpace(ln); len(t) > 0 {
				var r record
				if json.Unmarshal(t, &r) == nil && r.Event != nil {
					events = append(events, r.Event)
				}
			}
		}
	}
	var buf bytes.Buffer
	for _, ln := range lines {
		buf.Write(ln)
	}
	comp, err := segment.Compress(buf.Bytes())
	if err != nil {
		return segment.Entry{}, err
	}
	rel := fmt.Sprintf("segments/%08d.seg", newID)
	if err := writeFileSync(filepath.Join(dir, filepath.FromSlash(rel)), comp); err != nil {
		return segment.Entry{}, err
	}
	last := entries[len(entries)-1]
	return segment.Entry{
		ID: newID, Path: rel, Bytes: int64(len(comp)), Records: int64(len(lines)),
		ChainHead:  last.ChainHead, // chain is preserved: same lines, same order
		MerkleRoot: segment.MerkleRootHex(lines),
		Tier:       "local", Compressed: true,
		Zones: segment.ComputeZones(events),
	}, nil
}
