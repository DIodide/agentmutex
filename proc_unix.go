//go:build unix

package main

import (
	"os"
	"os/exec"
	"syscall"
)

// setChildProcGroup puts the child in its own process group so agentmutex can
// signal the WHOLE command tree (deploy scripts are `sh -c`, make, etc.), not
// just the direct child. Without this, backgrounded grandchildren survive
// termination and keep mutating the resource after the lease is gone.
func setChildProcGroup(cmd *exec.Cmd) {
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
}

// signalTree sends sig to the child's entire process group, falling back to
// the direct child if the group id can't be resolved.
func signalTree(cmd *exec.Cmd, sig os.Signal) {
	if cmd.Process == nil {
		return
	}
	ssig, ok := sig.(syscall.Signal)
	if !ok {
		cmd.Process.Signal(sig)
		return
	}
	if pgid, err := syscall.Getpgid(cmd.Process.Pid); err == nil {
		if syscall.Kill(-pgid, ssig) == nil {
			return
		}
	}
	cmd.Process.Signal(sig)
}

// killTree SIGKILLs the child's whole process group.
func killTree(cmd *exec.Cmd) {
	if cmd.Process == nil {
		return
	}
	if pgid, err := syscall.Getpgid(cmd.Process.Pid); err == nil {
		if syscall.Kill(-pgid, syscall.SIGKILL) == nil {
			return
		}
	}
	cmd.Process.Kill()
}

// fencesProcessTree reports whether killing the command reliably reaches its
// grandchildren on this platform (true on Unix via process groups).
const fencesProcessTree = true
