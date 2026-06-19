package store

import (
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
)

func TestSingleWriterLock(t *testing.T) {
	p := filepath.Join(t.TempDir(), "c.log")

	// First writer with Lock acquires it.
	a, err := OpenOptions(p, Options{Lock: true})
	if err != nil {
		t.Fatalf("first open: %v", err)
	}
	if _, err := os.Stat(p + ".lock"); err != nil {
		t.Fatalf("lock file should exist while open: %v", err)
	}

	// Second writer with Lock is refused.
	if _, err := OpenOptions(p, Options{Lock: true}); err == nil {
		t.Fatal("a second writer should be refused while the first holds the lock")
	}

	// After Close the lock is released and the marker removed.
	if err := a.Close(); err != nil {
		t.Fatalf("close: %v", err)
	}
	if _, err := os.Stat(p + ".lock"); !os.IsNotExist(err) {
		t.Fatal("lock file should be removed after Close")
	}

	// Reopening with Lock now succeeds.
	b, err := OpenOptions(p, Options{Lock: true})
	if err != nil {
		t.Fatalf("reopen after close: %v", err)
	}
	if err := b.Close(); err != nil {
		t.Fatal(err)
	}

	// Unlocked opens never create a marker and never conflict (default).
	c, err := OpenOptions(p, Options{})
	if err != nil {
		t.Fatalf("unlocked open: %v", err)
	}
	if _, err := os.Stat(p + ".lock"); !os.IsNotExist(err) {
		t.Fatal("unlocked open must not create a lock file")
	}
	if err := c.Close(); err != nil {
		t.Fatal(err)
	}
}

// A stale lock (this host, a dead PID) must be reclaimed automatically; a
// live-PID lock or a lock written by another host must be respected.
func TestAcquireLockStaleReclaim(t *testing.T) {
	host, _ := os.Hostname()
	dp := filepath.Join(t.TempDir(), "x.log")
	lp := dp + ".lock"
	write := func(v map[string]any) {
		b, _ := json.Marshal(v)
		if err := os.WriteFile(lp, b, 0o644); err != nil {
			t.Fatal(err)
		}
	}

	// Stale: this host, an almost-certainly-dead PID → reclaimed.
	write(map[string]any{"pid": 2000000000, "host": host, "since": "x"})
	got, err := acquireLock(dp)
	if err != nil {
		t.Fatalf("stale lock should be reclaimed, got error: %v", err)
	}
	releaseLock(got)

	// Live: our own running PID → must be refused (never steal a live lock).
	write(map[string]any{"pid": os.Getpid(), "host": host})
	if _, err := acquireLock(dp); err == nil {
		t.Fatal("a live-PID lock must NOT be reclaimed")
	}
	_ = os.Remove(lp)

	// Another host → respected (we can't check a remote machine's PID).
	write(map[string]any{"pid": 2000000000, "host": host + "-other-machine"})
	if _, err := acquireLock(dp); err == nil {
		t.Fatal("a lock from another host must be respected")
	}
}
