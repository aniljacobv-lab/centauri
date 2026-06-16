package store

import (
	"bufio"
	"bytes"
	"encoding/json"
	"fmt"
	"os"
)

// MergeLogs reconciles append-only logs that diverged — e.g. the same data
// edited on two devices through a synced folder (OneDrive, Google Drive,
// Dropbox) — into one new log at outPath.
//
// It is offline and non-destructive: the inputs are never modified, and the
// result is a fresh file you can inspect and `centauri verify` before you
// trust it. Records are unioned and de-duplicated: history shared by the
// inputs collapses to one copy (the lines are byte-identical), and each
// branch's unique records are kept in their original order. Because events
// are immutable with unique ids and the hash chain is just a hash of the
// committed bytes, the merged log recomputes a valid chain on open.
//
// Returns the number of unique records written.
func MergeLogs(outPath string, inPaths ...string) (int, error) {
	if len(inPaths) == 0 {
		return 0, fmt.Errorf("merge: need at least one input log")
	}
	seen := map[string]bool{}
	var lines [][]byte
	for _, p := range inPaths {
		b, err := os.ReadFile(p)
		if err != nil {
			return 0, fmt.Errorf("merge: read %s: %w", p, err)
		}
		for n, line := range bytes.Split(b, []byte{'\n'}) {
			t := bytes.TrimSpace(line)
			if len(t) == 0 {
				continue
			}
			var r record
			if err := json.Unmarshal(t, &r); err != nil || r.empty() {
				return 0, fmt.Errorf("merge: %s line %d is not a valid record: %v", p, n+1, err)
			}
			key := string(t)
			if seen[key] {
				continue // shared history (byte-identical) collapses
			}
			seen[key] = true
			lines = append(lines, append([]byte(nil), t...))
		}
	}

	tmp := outPath + ".tmp"
	f, err := os.OpenFile(tmp, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, 0o644)
	if err != nil {
		return 0, err
	}
	w := bufio.NewWriter(f)
	for _, line := range lines {
		w.Write(line)
		w.WriteByte('\n')
	}
	if err := w.Flush(); err != nil {
		f.Close()
		os.Remove(tmp)
		return 0, err
	}
	if err := f.Sync(); err != nil {
		f.Close()
		os.Remove(tmp)
		return 0, err
	}
	if err := f.Close(); err != nil {
		os.Remove(tmp)
		return 0, err
	}

	// Prove the merge replays cleanly before publishing it under outPath.
	st, err := Open(tmp)
	if err != nil {
		os.Remove(tmp)
		return 0, fmt.Errorf("merge: result did not replay (would be corrupt): %w", err)
	}
	_ = st.Close()
	os.Remove(tmp + ".checkpoint") // the verify open wrote one for the temp name
	if err := os.Rename(tmp, outPath); err != nil {
		os.Remove(tmp)
		return 0, err
	}
	return len(lines), nil
}
