//go:build linux

package main

import "syscall"

const ioctlReadTermios = syscall.TCGETS

// applyDeathSignal asks the kernel to SIGKILL the child if agentmutex dies
// (e.g. an OOM-killed CI runner), so an orphaned deploy can't keep mutating
// staging after the lease lapses. Linux-only; a partial mitigation (fires for
// the direct child, and typically cascades to a waiting shell's children).
func applyDeathSignal(a *syscall.SysProcAttr) {
	a.Pdeathsig = syscall.SIGKILL
}
