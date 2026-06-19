package store

import (
	"encoding/json"
	"fmt"
	"os"
	"time"
)

// Single-writer lock. When you keep your data in a synced folder (OneDrive,
// Google Drive, Dropbox) or on a shared device, two machines opening it for
// writing at once would corrupt the log. acquireLock creates an exclusive
// marker file next to the data file so the second writer is refused instead.
//
// Pure stdlib and cross-platform: an O_EXCL create is atomic on every OS.
//
// Stale-lock recovery: a crash or forced kill (taskkill /F, no graceful close)
// leaves the marker behind. acquireLock reclaims it AUTOMATICALLY, but only when
// it can prove it's safe — the marker was written by THIS host and the recorded
// PID is no longer running. A marker from another host (e.g. a synced folder a
// second machine still holds) is never stolen; that still returns a clear error.
func acquireLock(dataPath string) (string, error) {
	lockPath := dataPath + ".lock"
	f, err := os.OpenFile(lockPath, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0o644)
	if err != nil && os.IsExist(err) && reclaimStaleLock(lockPath) {
		// Previous holder is gone (same host, dead PID) — try once more.
		f, err = os.OpenFile(lockPath, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0o644)
	}
	if err != nil {
		if os.IsExist(err) {
			owner := "unknown"
			if b, e := os.ReadFile(lockPath); e == nil && len(b) > 0 {
				owner = string(b)
			}
			return "", fmt.Errorf("store is locked by another writer (%s); "+
				"close that instance, delete %q if it is stale, or open read-only", owner, lockPath)
		}
		return "", err
	}
	host, _ := os.Hostname()
	info, _ := json.Marshal(map[string]any{
		"pid": os.Getpid(), "host": host,
		"since": time.Now().UTC().Format(time.RFC3339),
	})
	_, _ = f.Write(info)
	_ = f.Close()
	return lockPath, nil
}

// reclaimStaleLock removes a lock marker IFF it was written by this host and its
// recorded PID is no longer running, returning true if it did. It is deliberately
// conservative: an unreadable/unparseable marker, a PID that's still alive, a
// missing/zero PID, or a marker from a different host are all left untouched, so
// a genuinely-held lock (including a remote one on a synced folder) is respected.
func reclaimStaleLock(lockPath string) bool {
	b, err := os.ReadFile(lockPath)
	if err != nil {
		return false
	}
	var rec struct {
		PID  int    `json:"pid"`
		Host string `json:"host"`
	}
	if json.Unmarshal(b, &rec) != nil {
		return false
	}
	host, _ := os.Hostname()
	if rec.Host == "" || rec.Host != host {
		return false // another machine — we cannot check its PID; respect it
	}
	if rec.PID <= 0 || rec.PID == os.Getpid() {
		return false // can't verify, or somehow our own
	}
	if processAlive(rec.PID) {
		return false // the writer is genuinely still running
	}
	return os.Remove(lockPath) == nil
}

// releaseLock removes the marker file (best effort; a no-op if unset).
func releaseLock(lockPath string) {
	if lockPath != "" {
		_ = os.Remove(lockPath)
	}
}
