//go:build unix

package mutex

import "syscall"

// processAlive reports whether a process with the given PID exists on this
// host. signal 0 performs error-checking without delivering a signal:
// success or EPERM means the process exists; ESRCH means it does not.
func processAlive(pid int) bool {
	if pid <= 0 {
		return false
	}
	err := syscall.Kill(pid, 0)
	return err == nil || err == syscall.EPERM
}
