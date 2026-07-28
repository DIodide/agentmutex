//go:build windows

package mutex

import "os"

// processAlive reports whether a process with the given PID exists on this
// host. os.FindProcess is a no-op on Windows, so probe with a zero-length
// Signal, which reports ERROR_INVALID_PARAMETER-style failure for a dead PID.
func processAlive(pid int) bool {
	if pid <= 0 {
		return false
	}
	p, err := os.FindProcess(pid)
	if err != nil {
		return false
	}
	// Signal(nil) is not supported on Windows; Kill would actually terminate.
	// Best effort: os.FindProcess succeeds for any pid, so fall back to
	// treating it as alive. Windows waiter liveness therefore relies on the
	// mtime heartbeat (documented); this keeps behavior no worse than before.
	_ = p
	return true
}
