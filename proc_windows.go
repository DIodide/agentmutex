//go:build windows

package main

import (
	"os"
	"os/exec"
)

// Windows has no process groups the way Unix does, and no SIGTERM. We make a
// best effort: terminate the direct child. Detached grandchildren are NOT
// fenced — surfaced to the user via fencesProcessTree so run can warn.
//
// A future improvement is a Windows Job Object (CREATE_SUSPENDED + assign +
// resume, with JOB_OBJECT_LIMIT_KILL_ON_JOB_CLOSE) to kill the whole tree.

func setChildProcGroup(cmd *exec.Cmd) {
	// No-op: see the package comment above.
}

func signalTree(cmd *exec.Cmd, sig os.Signal) {
	if cmd.Process == nil {
		return
	}
	// Only os.Kill is reliably supported on Windows; any termination intent
	// becomes a hard kill of the direct child.
	cmd.Process.Kill()
}

func killTree(cmd *exec.Cmd) {
	if cmd.Process != nil {
		cmd.Process.Kill()
	}
}

const fencesProcessTree = false
