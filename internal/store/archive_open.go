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

	"github.com/proxima360/centauri/internal/model"
	"github.com/proxima360/centauri/internal/segment"
)

// OpenArchive opens a store backed by dir/manifest.json + dir/segments/* and an
// appendable dir/current.log tail.
func OpenArchive(dir string, opts Options) (*Store, error) {
	mb, err := os.ReadFile(filepath.Join(dir, "manifest.json"))
	if err != nil {
		return nil, fmt.Errorf("archive: read manifest: %w", err)
	}
	man, err := segment.ParseManifest(mb)
	if err != nil {
		return nil, fmt.Errorf("archive: parse manifest: %w", err)
	}
	tailName := man.Tail
	if tailName == "" {
		tailName = "current.log"
	}
	tail := filepath.Join(dir, tailName)
	s := newStore(tail, opts)
	s.archiveDir = dir
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

// Seal rolls the current appendable tail into a new compressed, Merkle-rooted,
// zone-mapped segment and atomically switches the manifest to a FRESH empty tail
// generation. Crash-safe by construction: the only durable switch is one atomic
// manifest rename — before it, replay uses the old tail (the new segment is an
// ignored orphan); after it, replay uses the new (empty) tail plus the new
// segment. A crash can only leave a harmless orphan file, never a gap or a
// duplicate. Only valid on an archive-backed store, and (for now) not with
// LazyPayloads, since the tail's offloaded payloads would dangle after the seal.
func (s *Store) Seal() error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.archiveDir == "" {
		return fmt.Errorf("store: Seal requires an archive-backed store (open with OpenArchive)")
	}
	if s.opts.LazyPayloads {
		return fmt.Errorf("store: Seal is not yet supported with LazyPayloads")
	}
	if err := s.writable(); err != nil {
		return err
	}
	if err := s.f.Sync(); err != nil {
		return err
	}
	data, err := os.ReadFile(s.path) // the current tail
	if err != nil {
		return err
	}
	if len(data) == 0 {
		return nil // nothing to seal
	}

	// Parse the tail's lines (for Merkle) and events (for zone maps).
	var lines [][]byte
	var events []*model.Event
	for rest := data; len(rest) > 0; {
		i := bytes.IndexByte(rest, '\n')
		var ln []byte
		if i < 0 {
			ln, rest = rest, nil
		} else {
			ln, rest = rest[:i+1], rest[i+1:]
		}
		lines = append(lines, ln)
		if t := bytes.TrimSpace(ln); len(t) > 0 {
			var r record
			if json.Unmarshal(t, &r) == nil && r.Event != nil {
				events = append(events, r.Event)
			}
		}
	}

	// Read the live manifest (only Seal writes it, under this lock).
	mb, err := os.ReadFile(filepath.Join(s.archiveDir, "manifest.json"))
	if err != nil {
		return err
	}
	man, err := segment.ParseManifest(mb)
	if err != nil {
		return err
	}

	comp, err := segment.Compress(data)
	if err != nil {
		return err
	}
	id := len(man.Segments) + 1
	segRel := fmt.Sprintf("segments/%08d.seg", id)
	segDir := filepath.Join(s.archiveDir, "segments")
	if err := os.MkdirAll(segDir, 0o755); err != nil {
		return err
	}
	if err := writeFileSync(filepath.Join(s.archiveDir, filepath.FromSlash(segRel)), comp); err != nil {
		return err
	}

	// Fresh empty tail generation (a NEW filename — never reuse the sealed one).
	newTail := fmt.Sprintf("current.%08d.log", id)
	newF, err := os.OpenFile(filepath.Join(s.archiveDir, newTail), os.O_CREATE|os.O_RDWR|os.O_TRUNC, 0o644)
	if err != nil {
		return err
	}

	man.Segments = append(man.Segments, segment.Entry{
		ID: id, Path: segRel, Bytes: int64(len(comp)), Records: int64(len(lines)),
		ChainHead:  hex.EncodeToString(s.chainHash[:]), // the chain already covers this tail
		MerkleRoot: segment.MerkleRootHex(lines),
		Tier:       "local", Compressed: true,
		Zones: segment.ComputeZones(events),
	})
	man.Tail = newTail

	// THE atomic switch. After this rename the new state is durable; before it,
	// the old state is intact.
	if err := writeManifestAtomic(s.archiveDir, man); err != nil {
		newF.Close()
		return err
	}

	// In-process cleanup (reconstructable from the manifest if we crash here).
	oldPath := s.path
	_ = s.f.Close()
	s.f = newF
	s.path = filepath.Join(s.archiveDir, newTail)
	s.size = 0
	_ = os.Remove(oldPath) // the sealed tail's bytes now live in the segment
	return nil
}

// writeFileSync writes data and fsyncs before returning, so the file is durable
// before anything (e.g. the manifest) references it.
func writeFileSync(path string, data []byte) error {
	f, err := os.OpenFile(path, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, 0o644)
	if err != nil {
		return err
	}
	if _, err := f.Write(data); err != nil {
		f.Close()
		return err
	}
	if err := f.Sync(); err != nil {
		f.Close()
		return err
	}
	return f.Close()
}

// writeManifestAtomic writes the manifest to a temp file (fsynced) then renames
// it over manifest.json — an atomic replace on the same filesystem.
func writeManifestAtomic(dir string, m *segment.Manifest) error {
	b, err := m.JSON()
	if err != nil {
		return err
	}
	tmp := filepath.Join(dir, "manifest.json.tmp")
	if err := writeFileSync(tmp, b); err != nil {
		return err
	}
	return os.Rename(tmp, filepath.Join(dir, "manifest.json"))
}
