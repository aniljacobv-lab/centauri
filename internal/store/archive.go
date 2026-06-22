package store

// Offline log → sealed-segment archiver: the first, non-destructive step toward
// disk-scaling "tablespaces". It reads an existing log and writes a manifest
// plus compressed, Merkle-rooted, zone-mapped segment files — without touching
// the live engine's write/replay/chain paths (so the invariants are untouched).
// The archive's hash chain is computed with the SAME algorithm as the engine, so
// VerifyArchive's final head equals the live store's ChainHead — proving the
// sealed, compressed copy is byte-faithful and tamper-evident. See
// docs/design-tablespaces.md. Reading/replaying live FROM segments is a later,
// separately-verified slice.

import (
	"bufio"
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"path/filepath"

	"github.com/proxima360/centauri/internal/model"
	"github.com/proxima360/centauri/internal/segment"
)

// WriteArchive seals srcLog into destDir/manifest.json + destDir/segments/*.seg,
// breaking segments at ~maxRecords complete lines (<=0 picks a default). It is
// non-destructive: srcLog is only read. A torn final record (crash mid-write) is
// skipped, exactly as replay would truncate it.
func WriteArchive(srcLog, destDir string, maxRecords int) (*segment.Manifest, error) {
	if maxRecords <= 0 {
		maxRecords = 100000
	}
	f, err := os.Open(srcLog)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	segDir := filepath.Join(destDir, "segments")
	if err := os.MkdirAll(segDir, 0o755); err != nil {
		return nil, err
	}

	man := &segment.Manifest{Version: 1}
	var chain [32]byte // running chain head, identical algorithm to the engine
	segID := 0
	var lines [][]byte
	var raw bytes.Buffer
	var events []*model.Event

	seal := func() error {
		if len(lines) == 0 {
			return nil
		}
		segID++
		comp, err := segment.Compress(raw.Bytes())
		if err != nil {
			return err
		}
		name := fmt.Sprintf("%08d.seg", segID)
		if err := os.WriteFile(filepath.Join(segDir, name), comp, 0o644); err != nil {
			return err
		}
		man.Segments = append(man.Segments, segment.Entry{
			ID: segID, Path: "segments/" + name,
			Bytes: int64(len(comp)), Records: int64(len(lines)),
			ChainHead:  hex.EncodeToString(chain[:]),
			MerkleRoot: segment.MerkleRootHex(lines),
			Tier:       "local", Compressed: true,
			Zones: segment.ComputeZones(events),
		})
		lines, events = nil, nil
		raw.Reset()
		return nil
	}

	rd := bufio.NewReader(f)
	for {
		line, rerr := rd.ReadBytes('\n')
		if len(line) > 0 && line[len(line)-1] == '\n' { // complete record only
			h := sha256.New()
			h.Write(chain[:])
			h.Write(line)
			copy(chain[:], h.Sum(nil))
			lc := append([]byte(nil), line...)
			lines = append(lines, lc)
			raw.Write(lc)
			if trimmed := bytes.TrimSpace(lc); len(trimmed) > 0 {
				var r record
				if json.Unmarshal(trimmed, &r) == nil && r.Event != nil {
					events = append(events, r.Event)
				}
			}
			if len(lines) >= maxRecords {
				if err := seal(); err != nil {
					return nil, err
				}
			}
		}
		if rerr == io.EOF {
			break
		}
		if rerr != nil {
			return nil, rerr
		}
	}
	if err := seal(); err != nil {
		return nil, err
	}
	b, err := man.JSON()
	if err != nil {
		return nil, err
	}
	if err := os.WriteFile(filepath.Join(destDir, "manifest.json"), b, 0o644); err != nil {
		return nil, err
	}
	return man, nil
}

// VerifyArchive walks an archive in order, decompresses each segment, recomputes
// its Merkle root (tamper-evidence) and the running hash chain (continuity
// across segments), and checks both against the manifest. Returns the final
// chain head and total record count — the head equals the source log's
// ChainHead when the archive is faithful.
func VerifyArchive(destDir string) (head string, records int64, err error) {
	b, err := os.ReadFile(filepath.Join(destDir, "manifest.json"))
	if err != nil {
		return "", 0, err
	}
	man, err := segment.ParseManifest(b)
	if err != nil {
		return "", 0, err
	}
	var chain [32]byte
	for _, e := range man.Segments {
		data, err := os.ReadFile(filepath.Join(destDir, filepath.FromSlash(e.Path)))
		if err != nil {
			return "", 0, err
		}
		if e.Compressed {
			if data, err = segment.Decompress(data); err != nil {
				return "", 0, fmt.Errorf("segment %d: decompress: %w", e.ID, err)
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
		if got := segment.MerkleRootHex(lines); got != e.MerkleRoot {
			return "", 0, fmt.Errorf("segment %d: Merkle root mismatch (tampering?)", e.ID)
		}
		for _, ln := range lines {
			h := sha256.New()
			h.Write(chain[:])
			h.Write(ln)
			copy(chain[:], h.Sum(nil))
		}
		if hex.EncodeToString(chain[:]) != e.ChainHead {
			return "", 0, fmt.Errorf("segment %d: chain head mismatch (tampering or gap?)", e.ID)
		}
		records += int64(len(lines))
	}
	return hex.EncodeToString(chain[:]), records, nil
}
