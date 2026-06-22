package store

// OpenArchive runs the engine directly on a sealed-segment archive (the output
// of WriteArchive / `centauri archive`): it replays the compressed, Merkle-
// rooted segments in order, verifying the hash chain at every segment boundary,
// then opens an appendable, uncompressed tail (current.log) for new writes. The
// result is an ordinary read/write *Store — queries and Append work exactly as
// on a single log. The single-log Open/replay/commit paths are untouched, so
// this is purely additive. See docs/design-tablespaces.md.
//
// Note: segment payloads are replayed into RAM (Values present) even under
// LazyPayloads; only the tail offloads. A disk-backed/lazy index over segments
// (so RAM < total data) is a later slice; this one delivers compressed,
// tamper-verified at-rest storage you can run on.

import (
	"bytes"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"path/filepath"

	"github.com/proxima360/centauri/internal/segment"
)

// OpenArchive opens a store backed by dir/manifest.json + dir/segments/* and an
// appendable dir/current.log tail.
func OpenArchive(dir string, opts Options) (*Store, error) {
	tail := filepath.Join(dir, "current.log")
	s := newStore(tail, opts)
	s.archiveDir = dir

	mb, err := os.ReadFile(filepath.Join(dir, "manifest.json"))
	if err != nil {
		return nil, fmt.Errorf("archive: read manifest: %w", err)
	}
	man, err := segment.ParseManifest(mb)
	if err != nil {
		return nil, fmt.Errorf("archive: parse manifest: %w", err)
	}
	// Replay sealed segments in order, verifying the chain at each boundary.
	for _, e := range man.Segments {
		data, err := os.ReadFile(filepath.Join(dir, filepath.FromSlash(e.Path)))
		if err != nil {
			return nil, fmt.Errorf("archive: segment %d: %w", e.ID, err)
		}
		if e.Compressed {
			if data, err = segment.Decompress(data); err != nil {
				return nil, fmt.Errorf("archive: segment %d: decompress: %w", e.ID, err)
			}
		}
		if err := s.applyLines(data); err != nil {
			return nil, fmt.Errorf("archive: segment %d: %w", e.ID, err)
		}
		if e.ChainHead != "" && hex.EncodeToString(s.chainHash[:]) != e.ChainHead {
			return nil, fmt.Errorf("archive: segment %d chain head mismatch (tampering or a gap)", e.ID)
		}
	}

	// Open and replay the appendable tail (continues the chain from the segments).
	f, err := os.OpenFile(tail, os.O_CREATE|os.O_RDWR, 0o644)
	if err != nil {
		return nil, err
	}
	good, err := s.replay(f, 0)
	if err != nil {
		f.Close()
		return nil, err
	}
	fi, err := f.Stat()
	if err != nil {
		f.Close()
		return nil, err
	}
	if good < fi.Size() { // torn tail
		if err := f.Truncate(good); err != nil {
			f.Close()
			return nil, fmt.Errorf("archive: truncate torn tail: %w", err)
		}
		if err := f.Sync(); err != nil {
			f.Close()
			return nil, err
		}
	}
	if _, err := f.Seek(good, io.SeekStart); err != nil {
		f.Close()
		return nil, err
	}
	s.f = f
	s.size = good
	if opts.Lock {
		lp, lerr := acquireLock(tail)
		if lerr != nil {
			f.Close()
			return nil, lerr
		}
		s.lockPath = lp
	}
	return s, nil
}

// applyLines replays a buffer of complete log lines (from a decompressed
// segment) into the index and the hash chain — the segment-side equivalent of
// replay over a file, minus torn-tail handling (sealed segments are complete).
func (s *Store) applyLines(data []byte) error {
	for len(data) > 0 {
		i := bytes.IndexByte(data, '\n')
		var line []byte
		if i < 0 {
			line, data = data, nil
		} else {
			line, data = data[:i+1], data[i+1:]
		}
		trimmed := bytes.TrimSpace(line)
		if len(trimmed) == 0 {
			s.chainExtend(line) // keep the chain byte-exact even over blank lines
			continue
		}
		var r record
		if err := json.Unmarshal(trimmed, &r); err != nil || r.empty() {
			return fmt.Errorf("corrupt record in segment")
		}
		s.apply(&r)
		s.chainExtend(line)
	}
	return nil
}
