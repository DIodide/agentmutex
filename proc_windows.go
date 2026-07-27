//go:build windows

package main

import (
	"os"
	"os/exec"
)

// Windows has no Unix-style process groups or SIGTERM. We make a best effort:
// terminate the direct child. Detached grandchildren are NOT fenced —
// surfaced via fencesProcessTree so run can warn.
//
// A future improvement is a Windows Job Object (assign the child, set
// JOB_OBJECT_LIMIT_KILL_ON_JOB_CLOSE) to kill the whole tree.

type childProc struct {
	cmd      *exec.Cmd
	ownGroup bool
}

func setChildProcGroup(cmd *exec.Cmd) bool {
	return false // see package comment
}

func signalTree(c childProc, sig os.Signal) {
	if c.cmd.Process == nil {
		return
	}
	// Only os.Kill is reliably supported on Windows; any termination intent
	// becomes a hard kill of the direct child.
	c.cmd.Process.Kill()
}

func killTree(c childProc) {
	if c.cmd.Process != nil {
		c.cmd.Process.Kill()
	}
}

const fencesProcessTree = false
