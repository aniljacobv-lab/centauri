//go:build !windows

package store

import "syscall"

// processAlive reports whether a PID is running. On Unix, signal 0 performs no
// delivery but still does the existence/permission check: nil means alive,
// EPERM means it exists but is owned by another user (still alive), and ESRCH
// means no such process.
func processAlive(pid int) bool {
	err := syscall.Kill(pid, 0)
	return err == nil || err == syscall.EPERM
}
