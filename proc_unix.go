//go:build unix

package main

import (
	"os"
	"os/exec"
	"syscall"
	"unsafe"
)

// childProc bundles a running command with whether we placed it in its own
// process group, so the signal helpers know whether group-signaling is safe.
type childProc struct {
	cmd      *exec.Cmd
	ownGroup bool
}

// setChildProcGroup decides whether to put the child in its own process
// group. Putting it in one lets us signal the WHOLE command tree (deploy
// scripts are `sh -c`, make, etc.) on lease loss — but a child in a separate
// group that is NOT the terminal's foreground group receives SIGTTIN/SIGTTOU
// (default: stop) the moment it touches the controlling terminal, which would
// hang interactive commands (sudo prompts, editors, REPLs).
//
// So: only create a separate group when stdin is not a terminal (the deploy /
// CI case, where tree-fencing matters and nothing reads the tty). Interactive
// children stay in our foreground group and work exactly as before, at the
// cost of only-direct-child termination on lease loss — acceptable, since the
// concurrent-deploy collision this fences against is non-interactive.
func setChildProcGroup(cmd *exec.Cmd) bool {
	if isTerminal(os.Stdin) {
		return false
	}
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	return true
}

// signalTree sends sig to the child. When the child has its own process group
// it is delivered to the whole group; otherwise (shared group) only to the
// direct child, so we never signal agentmutex's own group.
func signalTree(c childProc, sig os.Signal) {
	if c.cmd.Process == nil {
		return
	}
	ssig, ok := sig.(syscall.Signal)
	if c.ownGroup && ok {
		if pgid, err := syscall.Getpgid(c.cmd.Process.Pid); err == nil && pgid != syscall.Getpgrp() {
			if syscall.Kill(-pgid, ssig) == nil {
				return
			}
		}
	}
	c.cmd.Process.Signal(sig)
}

// killTree SIGKILLs the child (its whole group when it has its own).
func killTree(c childProc) {
	if c.cmd.Process == nil {
		return
	}
	if c.ownGroup {
		if pgid, err := syscall.Getpgid(c.cmd.Process.Pid); err == nil && pgid != syscall.Getpgrp() {
			if syscall.Kill(-pgid, syscall.SIGKILL) == nil {
				return
			}
		}
	}
	c.cmd.Process.Kill()
}

// isTerminal reports whether f is a terminal (uses the termios ioctl, so
// /dev/null and pipes correctly report false — unlike an os.ModeCharDevice
// check).
func isTerminal(f *os.File) bool {
	var t syscall.Termios
	_, _, errno := syscall.Syscall(syscall.SYS_IOCTL, f.Fd(), ioctlReadTermios, uintptr(unsafe.Pointer(&t)))
	return errno == 0
}
