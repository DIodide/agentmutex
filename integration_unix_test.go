//go:build unix

package main

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// TestRunTerminatesProcessTreeOnLeaseLoss proves the exit-14 machinery
// actually fences the command's whole process tree: a backgrounded
// grandchild must be killed on lease loss, not left mutating the resource
// after a new holder could take over.
func TestRunTerminatesProcessTreeOnLeaseLoss(t *testing.T) {
	state := t.TempDir()
	mark := filepath.Join(t.TempDir(), "grandchild-ran")
	// The child sh backgrounds a grandchild that writes the marker after 4s,
	// then waits. If the process group is killed on lease loss, the marker
	// never appears.
	script := fmt.Sprintf(`( sleep 4; : > %q ) & wait`, mark)
	cmd := mutexCmd(t, state, "run", "--quiet", "--ttl", "5s", "pg:key", "--", "sh", "-c", script)
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	done := make(chan int, 1)
	go func() { done <- exitCode(cmd.Wait()) }()

	time.Sleep(1500 * time.Millisecond) // let child + grandchild start
	if err := mutexCmd(t, state, "force-release", "--yes", "pg:key").Run(); err != nil {
		t.Fatalf("force-release: %v", err)
	}

	select {
	case code := <-done:
		if code != 14 {
			t.Fatalf("run exit after lease loss: %d, want 14", code)
		}
	case <-time.After(20 * time.Second):
		cmd.Process.Kill()
		t.Fatal("run did not exit after losing its lease")
	}
	// Give the grandchild's 4s timer time to have fired had it survived.
	time.Sleep(4 * time.Second)
	if _, err := os.Stat(mark); err == nil {
		t.Fatal("grandchild survived lease-loss termination and mutated the resource")
	}
}

// TestRunExportsLeaseToChild verifies the wrapped command can see its lease.
func TestRunExportsLeaseToChild(t *testing.T) {
	state := t.TempDir()
	out := filepath.Join(t.TempDir(), "env")
	script := fmt.Sprintf(`printf '%%s\n%%s\n' "$AGENTMUTEX_LEASE_KEY" "$AGENTMUTEX_TOKEN" > %q`, out)
	if err := mutexCmd(t, state, "run", "--quiet", "svc:api", "--", "sh", "-c", script).Run(); err != nil {
		t.Fatalf("run: %v", err)
	}
	data, err := os.ReadFile(out)
	if err != nil {
		t.Fatal(err)
	}
	lines := strings.Split(strings.TrimSpace(string(data)), "\n")
	if len(lines) != 2 || lines[0] != "svc:api" || len(lines[1]) != 32 {
		t.Fatalf("child did not see its lease: %q", data)
	}
}

// TestStatusExitCodeMode checks the scripting exit-code contract.
func TestStatusExitCodeMode(t *testing.T) {
	state := t.TempDir()
	if got := exitCode(mutexCmd(t, state, "status", "--exit-code", "sc:key").Run()); got != 3 {
		t.Fatalf("free key: want exit 3, got %d", got)
	}
	if err := mutexCmd(t, state, "acquire", "--quiet", "sc:key").Run(); err != nil {
		t.Fatal(err)
	}
	if got := exitCode(mutexCmd(t, state, "status", "--exit-code", "sc:key").Run()); got != 0 {
		t.Fatalf("held key: want exit 0, got %d", got)
	}
}
