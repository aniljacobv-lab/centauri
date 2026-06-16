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
// A stale lock left by a crash is never silently stolen — the error tells
// you exactly which file to delete, or you can open with Options.Lock=false.
func acquireLock(dataPath string) (string, error) {
	lockPath := dataPath + ".lock"
	f, err := os.OpenFile(lockPath, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0o644)
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

// releaseLock removes the marker file (best effort; a no-op if unset).
func releaseLock(lockPath string) {
	if lockPath != "" {
		_ = os.Remove(lockPath)
	}
}
