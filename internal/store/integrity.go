// Tamper evidence: every committed record extends a SHA-256 hash chain
// (h_n = SHA256(h_{n-1} || line)). The chain head is maintained on every
// commit, replayed identically on Open, shipped implicitly to followers
// (same bytes → same chain), and persisted in checkpoints. Editing any
// byte of history changes every subsequent hash — Oracle sells this as
// "blockchain tables"; in an append-only database it's one invariant.
//
// The chain head is a fingerprint of the entire history. Record it
// externally (a notebook, another system, a colleague's email) and
// `centauri verify` can prove the log is exactly the history that
// produced that fingerprint.
package store

import (
	"bufio"
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"os"
)

// chainExtend advances the hash chain over one raw log line (the line
// includes its trailing newline). Caller holds s.mu.
func (s *Store) chainExtend(line []byte) {
	h := sha256.New()
	h.Write(s.chainHash[:])
	h.Write(line)
	copy(s.chainHash[:], h.Sum(nil))
}

// chainExtendBuf advances the chain over a buffer of complete lines.
func (s *Store) chainExtendBuf(buf []byte) {
	for len(buf) > 0 {
		i := bytes.IndexByte(buf, '\n')
		if i < 0 {
			s.chainExtend(buf) // defensive; committed buffers end on '\n'
			return
		}
		s.chainExtend(buf[:i+1])
		buf = buf[i+1:]
	}
}

// ChainHead returns the current chain head (hex) and the byte offset it
// covers. Two stores with the same head hold byte-identical history.
func (s *Store) ChainHead() (string, int64) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return hex.EncodeToString(s.chainHash[:]), s.size
}

// VerifyChain re-reads a log file from disk and recomputes the chain
// independently of any in-memory state. Returns the head, the bytes
// covered, and the number of records.
func VerifyChain(path string) (head string, size int64, records int, err error) {
	f, err := os.Open(path)
	if err != nil {
		return "", 0, 0, err
	}
	defer f.Close()
	var chain [32]byte
	rd := bufio.NewReaderSize(f, 1<<20)
	for {
		line, rerr := rd.ReadBytes('\n')
		if len(line) > 0 && line[len(line)-1] == '\n' {
			h := sha256.New()
			h.Write(chain[:])
			h.Write(line)
			copy(chain[:], h.Sum(nil))
			size += int64(len(line))
			records++
		}
		if rerr != nil {
			break
		}
	}
	return hex.EncodeToString(chain[:]), size, records, nil
}

// Integrity compares the live chain against an independent re-read of
// the file. A mismatch means bytes on disk are not the bytes that were
// committed — tampering or corruption.
func (s *Store) Integrity() (map[string]any, error) {
	liveHead, liveSize := s.ChainHead()
	diskHead, diskSize, records, err := VerifyChain(s.path)
	if err != nil {
		return nil, err
	}
	ok := liveHead == diskHead && liveSize == diskSize
	return map[string]any{
		"verified":    ok,
		"chain_head":  liveHead,
		"log_bytes":   liveSize,
		"records":     records,
		"disk_head":   diskHead,
		"disk_bytes":  diskSize,
		"note": fmt.Sprintf("record the chain_head externally; any future "+
			"`centauri verify` reproducing it proves history is intact (%d records)", records),
	}, nil
}
