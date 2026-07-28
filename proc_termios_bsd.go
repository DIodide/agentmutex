//go:build unix && !linux

package main

import "syscall"

// Darwin and the BSDs use TIOCGETA for the termios read ioctl.
const ioctlReadTermios = syscall.TIOCGETA

// applyDeathSignal is a no-op: Darwin/BSD SysProcAttr has no parent-death
// signal. (Orphan fencing there relies on the lease TTL.)
func applyDeathSignal(a *syscall.SysProcAttr) {}
