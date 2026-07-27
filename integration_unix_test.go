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
	// Real collision: break the lock and let a competitor take it, so the
	// original run's renew sees a different holder (NotHolderError) and
	// terminates — the path that must fence the whole process tree.
	if err := mutexCmd(t, state, "force-release", "--yes", "pg:key").Run(); err != nil {
		t.Fatalf("force-release: %v", err)
	}
	if err := mutexCmd(t, state, "acquire", "--quiet", "--no-wait", "--agent", "thief", "pg:key").Run(); err != nil {
		t.Fatalf("competitor acquire: %v", err)
	}

	select {
	case code := <-done:
		if code != 14 {
			t.Fatalf("run exit after lease takeover: %d, want 14", code)
		}
	case <-time.After(20 * time.Second):
		cmd.Process.Kill()
		t.Fatal("run did not exit after losing its lease to a competitor")
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

// TestRunChildEarlyReleaseNotLost: a child releasing the lease early via its
// exported token must NOT be treated as a lost lease (no exit 14, not killed).
func TestRunChildEarlyReleaseNotLost(t *testing.T) {
	state := t.TempDir()
	done := filepath.Join(t.TempDir(), "finished")
	// Child releases immediately, then keeps working (non-critical tail) and
	// writes the marker. run must let it finish and exit 0.
	script := fmt.Sprintf(`"%s" release "$AGENTMUTEX_LEASE_KEY"; sleep 3; : > %q`, binPath, done)
	cmd := mutexCmd(t, state, "run", "--quiet", "--ttl", "5s", "k", "--", "sh", "-c", script)
	code := exitCode(cmd.Run())
	if code != 0 {
		t.Fatalf("early-release run exit: %d, want 0", code)
	}
	if _, err := os.Stat(done); err != nil {
		t.Fatalf("child was killed after its own early release: %v", err)
	}
}

// TestOnLeaseLossContinue: with --on-lease-loss continue, a takeover must not
// kill the child; run returns the child's own exit code.
func TestOnLeaseLossContinue(t *testing.T) {
	state := t.TempDir()
	cmd := mutexCmd(t, state, "run", "--quiet", "--on-lease-loss", "continue", "--ttl", "5s", "k",
		"--", "sh", "-c", "sleep 4; exit 7")
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	done := make(chan int, 1)
	go func() { done <- exitCode(cmd.Wait()) }()
	time.Sleep(1 * time.Second)
	mutexCmd(t, state, "force-release", "--yes", "k").Run()
	mutexCmd(t, state, "acquire", "--quiet", "--no-wait", "--agent", "thief", "k").Run()
	select {
	case code := <-done:
		if code != 7 {
			t.Fatalf("continue mode: want child exit 7, got %d", code)
		}
	case <-time.After(15 * time.Second):
		cmd.Process.Kill()
		t.Fatal("continue-mode run did not finish the child")
	}
}

// TestSelfReentryDetected: acquiring the same key from inside a run must fail
// fast (exit 10, self-deadlock) rather than block forever.
func TestSelfReentryDetected(t *testing.T) {
	state := t.TempDir()
	out := filepath.Join(t.TempDir(), "inner")
	// A *blocking* acquire with a long timeout: only self-reentry detection
	// makes it return quickly. Without it, it would block the full 30s.
	script := fmt.Sprintf(`"%s" acquire --timeout 30s --quiet k; echo $? > %q`, binPath, out)
	cmd := mutexCmd(t, state, "run", "--quiet", "--ttl", "40s", "k", "--", "sh", "-c", script)
	// The whole thing must return quickly (no infinite wait).
	donec := make(chan error, 1)
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	go func() { donec <- cmd.Wait() }()
	select {
	case <-donec:
	case <-time.After(10 * time.Second):
		cmd.Process.Kill()
		t.Fatal("self-reentry hung instead of failing fast")
	}
	data, err := os.ReadFile(out)
	if err != nil {
		t.Fatal(err)
	}
	if strings.TrimSpace(string(data)) != "10" {
		t.Fatalf("inner acquire exit: %q, want 10 (self-deadlock)", strings.TrimSpace(string(data)))
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
