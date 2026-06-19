//go:build windows

package store

import "os"

// processAlive reports whether a PID is running. On Windows, os.FindProcess
// opens the process (OpenProcess) and returns an error if it doesn't exist, so
// a successful open means the process is alive. (If a dead PID was recycled by
// an unrelated process we'll see it as alive and simply NOT reclaim the lock —
// the safe direction.)
func processAlive(pid int) bool {
	p, err := os.FindProcess(pid)
	if err != nil {
		return false
	}
	_ = p.Release()
	return true
}
