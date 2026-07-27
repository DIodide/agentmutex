package main

// Black-box integration tests: they build the real binary and exercise it
// with genuinely concurrent processes, the same way a fleet of agents would.

import (
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"
)

var binPath string

// TestMain doubles as the "agent workload" helper (portable stand-ins for
// shell commands): AGENTMUTEX_TEST_MODE=increment performs a deliberately
// racy read-sleep-write on a counter file — only mutual exclusion makes it
// safe — and AGENTMUTEX_TEST_MODE=sleep blocks for AGENTMUTEX_TEST_SECONDS.
func TestMain(m *testing.M) {
	switch os.Getenv("AGENTMUTEX_TEST_MODE") {
	case "increment":
		file := os.Getenv("AGENTMUTEX_TEST_FILE")
		data, _ := os.ReadFile(file)
		n, _ := strconv.Atoi(strings.TrimSpace(string(data)))
		time.Sleep(25 * time.Millisecond) // widen the race window
		if err := os.WriteFile(file, []byte(fmt.Sprintf("%d\n", n+1)), 0o644); err != nil {
			fmt.Fprintln(os.Stderr, err)
			os.Exit(1)
		}
		os.Exit(0)
	case "sleep":
		secs, _ := strconv.Atoi(os.Getenv("AGENTMUTEX_TEST_SECONDS"))
		time.Sleep(time.Duration(secs) * time.Second)
		os.Exit(0)
	}

	dir, err := os.MkdirTemp("", "agentmutex-bin")
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
	binPath = filepath.Join(dir, "agentmutex")
	if runtime.GOOS == "windows" {
		binPath += ".exe"
	}
	if out, err := exec.Command("go", "build", "-o", binPath, ".").CombinedOutput(); err != nil {
		fmt.Fprintf(os.Stderr, "build failed: %v\n%s", err, out)
		os.Exit(1)
	}
	code := m.Run()
	os.RemoveAll(dir)
	os.Exit(code)
}

func mutexCmd(t *testing.T, stateDir string, args ...string) *exec.Cmd {
	t.Helper()
	cmd := exec.Command(binPath, args...)
	cmd.Env = append(os.Environ(), "AGENTMUTEX_DIR="+stateDir)
	return cmd
}

func exitCode(err error) int {
	if err == nil {
		return 0
	}
	if ee, ok := err.(*exec.ExitError); ok {
		return ee.ExitCode()
	}
	return -1
}

// TestMutualExclusion is the core guarantee: N concurrent `run` invocations
// performing a racy read-modify-write must never lose an update.
func TestMutualExclusion(t *testing.T) {
	state := t.TempDir()
	counter := filepath.Join(t.TempDir(), "counter")
	if err := os.WriteFile(counter, []byte("0\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	const workers, iters = 8, 5
	var wg sync.WaitGroup
	errs := make(chan error, workers*iters)
	for w := 0; w < workers; w++ {
		wg.Add(1)
		go func(w int) {
			defer wg.Done()
			for i := 0; i < iters; i++ {
				cmd := mutexCmd(t, state,
					"run", "--poll", "25ms", "--quiet", "--agent", fmt.Sprintf("worker-%d", w),
					"test:counter", "--", os.Args[0])
				cmd.Env = append(cmd.Env,
					"AGENTMUTEX_TEST_MODE=increment",
					"AGENTMUTEX_TEST_FILE="+counter)
				if out, err := cmd.CombinedOutput(); err != nil {
					errs <- fmt.Errorf("worker %d iter %d: %v\n%s", w, i, err, out)
					return
				}
			}
		}(w)
	}
	wg.Wait()
	close(errs)
	for err := range errs {
		t.Error(err)
	}

	data, err := os.ReadFile(counter)
	if err != nil {
		t.Fatal(err)
	}
	got, _ := strconv.Atoi(strings.TrimSpace(string(data)))
	if want := workers * iters; got != want {
		t.Fatalf("lost updates under concurrency: counter = %d, want %d", got, want)
	}
}

func TestNoWaitExitCode(t *testing.T) {
	state := t.TempDir()
	out, err := mutexCmd(t, state, "acquire", "--quiet", "k").Output()
	if err != nil {
		t.Fatalf("acquire: %v", err)
	}
	token := strings.TrimSpace(string(out))

	if err := mutexCmd(t, state, "acquire", "--no-wait", "k").Run(); exitCode(err) != 10 {
		t.Fatalf("no-wait on held lock: exit %d, want 10", exitCode(err))
	}
	if err := mutexCmd(t, state, "release", "--token", "wrong", "k").Run(); exitCode(err) != 12 {
		t.Fatalf("wrong-token release: exit %d, want 12", exitCode(err))
	}
	if err := mutexCmd(t, state, "release", "--token", token, "k").Run(); exitCode(err) != 0 {
		t.Fatalf("release: exit %d, want 0", exitCode(err))
	}
	if err := mutexCmd(t, state, "release", "--token", token, "k").Run(); exitCode(err) != 13 {
		t.Fatalf("double release: exit %d, want 13", exitCode(err))
	}
}

func TestWaitTimesOut(t *testing.T) {
	state := t.TempDir()
	if err := mutexCmd(t, state, "acquire", "--quiet", "k").Run(); err != nil {
		t.Fatal(err)
	}
	start := time.Now()
	err := mutexCmd(t, state, "acquire", "--quiet", "--timeout", "700ms", "--poll", "50ms", "k").Run()
	if exitCode(err) != 11 {
		t.Fatalf("timeout acquire: exit %d, want 11", exitCode(err))
	}
	if elapsed := time.Since(start); elapsed < 600*time.Millisecond {
		t.Fatalf("gave up too early: %v", elapsed)
	}
}

// TestExpiryTakeover proves a crashed holder (lease never released or
// renewed) only blocks others until the TTL runs out.
func TestExpiryTakeover(t *testing.T) {
	state := t.TempDir()
	if err := mutexCmd(t, state, "acquire", "--quiet", "--ttl", "1s", "k").Run(); err != nil {
		t.Fatal(err)
	}
	start := time.Now()
	out, err := mutexCmd(t, state, "acquire", "--quiet", "--poll", "100ms", "--timeout", "10s", "k").Output()
	if err != nil {
		t.Fatalf("takeover acquire failed: %v", err)
	}
	if elapsed := time.Since(start); elapsed < 800*time.Millisecond {
		t.Fatalf("acquired before expiry: %v", elapsed)
	}
	if tok := strings.TrimSpace(string(out)); len(tok) != 32 {
		t.Fatalf("bad token %q", tok)
	}
}

// TestFIFOOrder: with two agents queued behind a holder, the one that
// arrived first must win the lock first.
func TestFIFOOrder(t *testing.T) {
	state := t.TempDir()
	out, err := mutexCmd(t, state, "acquire", "--quiet", "k").Output()
	if err != nil {
		t.Fatal(err)
	}
	holderTok := strings.TrimSpace(string(out))

	type result struct {
		name string
		at   time.Time
		tok  string
		err  error
	}
	results := make(chan result, 2)
	launch := func(name string) {
		cmd := mutexCmd(t, state, "acquire", "--quiet", "--poll", "50ms", "--timeout", "15s", "--agent", name, "k")
		out, err := cmd.Output()
		results <- result{name, time.Now(), strings.TrimSpace(string(out)), err}
	}
	go launch("w1")
	time.Sleep(500 * time.Millisecond) // let w1 enqueue first
	go launch("w2")
	time.Sleep(500 * time.Millisecond)

	if err := mutexCmd(t, state, "release", "--token", holderTok, "k").Run(); err != nil {
		t.Fatal(err)
	}

	first := <-results
	if first.err != nil {
		t.Fatalf("%s failed: %v", first.name, first.err)
	}
	if first.name != "w1" {
		t.Fatalf("FIFO violated: %s acquired first", first.name)
	}
	// Release w1's lease so w2 can finish.
	if err := mutexCmd(t, state, "release", "--token", first.tok, "k").Run(); err != nil {
		t.Fatal(err)
	}
	second := <-results
	if second.err != nil {
		t.Fatalf("%s failed: %v", second.name, second.err)
	}
	if second.name != "w2" {
		t.Fatalf("expected w2 second, got %s", second.name)
	}
}

func TestForceRelease(t *testing.T) {
	state := t.TempDir()
	if err := mutexCmd(t, state, "acquire", "--quiet", "k").Run(); err != nil {
		t.Fatal(err)
	}
	// Dry run does not release.
	if err := mutexCmd(t, state, "force-release", "k").Run(); err != nil {
		t.Fatalf("dry run: %v", err)
	}
	if err := mutexCmd(t, state, "acquire", "--no-wait", "k").Run(); exitCode(err) != 10 {
		t.Fatal("dry run released the lock")
	}
	// --yes releases.
	if err := mutexCmd(t, state, "force-release", "--yes", "k").Run(); err != nil {
		t.Fatal(err)
	}
	if err := mutexCmd(t, state, "acquire", "--no-wait", "--quiet", "k").Run(); err != nil {
		t.Fatalf("lock not free after force-release: %v", err)
	}
}

func TestRunReleasesOnCommandFailure(t *testing.T) {
	state := t.TempDir()
	// The increment helper fails when its target file is unwritable.
	cmd := mutexCmd(t, state, "run", "--quiet", "k", "--", os.Args[0])
	cmd.Env = append(cmd.Env,
		"AGENTMUTEX_TEST_MODE=increment",
		"AGENTMUTEX_TEST_FILE="+filepath.Join(t.TempDir(), "missing", "nested", "counter"))
	err := cmd.Run()
	if exitCode(err) != 1 {
		t.Fatalf("expected child failure to propagate as exit 1, got %d", exitCode(err))
	}
	// Lock must be free afterwards.
	if err := mutexCmd(t, state, "acquire", "--no-wait", "--quiet", "k").Run(); err != nil {
		t.Fatalf("lock leaked after failed run: %v", err)
	}
}

func TestHelpGoesToStdoutExitZero(t *testing.T) {
	for _, sub := range []string{"acquire", "release", "run", "status", "wait", "prune"} {
		cmd := mutexCmd(t, t.TempDir(), sub, "-h")
		out, err := cmd.Output() // stdout only
		if exitCode(err) != 0 {
			t.Errorf("%s -h: exit %d, want 0", sub, exitCode(err))
		}
		if !strings.Contains(string(out), "Usage:") {
			t.Errorf("%s -h: help not on stdout:\n%s", sub, out)
		}
	}
}

func TestUsageValidation(t *testing.T) {
	state := t.TempDir()
	cases := [][]string{
		{"acquire", "--poll", "-2s", "k"},             // negative poll would panic pre-fix
		{"acquire", "--poll", "0s", "k"},              // zero poll would busy-spin
		{"acquire", "--poll", "60s", "k"},             // poll beyond staleness breaks FIFO
		{"acquire", "--ttl", "0s", "k"},               // silently coerced to 15m pre-fix
		{"acquire", "--timeout", "-5s", "k"},          // negative timeout != wait-forever
		{"acquire", "bad key"},                        // invalid key is a usage error
		{"renew", "--ttl", "0s", "--token", "x", "k"}, // renew ttl validated too
		{"release", "k"},                              // missing token is exit 2, not 1
		{"run", "--ttl", "1s", "k", "--", "true"},     // ttl below auto-renew floor
		{"run", "k", "--ttl", "5m", "--", "true"},     // flags after key footgun
		{"list", "some-key"},                          // list takes no args
		{"prune", "some-key"},                         // prune takes no args
		{"status", "--exit-code"},                     // --exit-code needs a key
	}
	for _, c := range cases {
		if err := mutexCmd(t, state, c...).Run(); exitCode(err) != 2 {
			t.Errorf("%v: exit %d, want 2 (usage)", c, exitCode(err))
		}
	}
}

func TestWaitOnCorruptStateErrsInsteadOfPanic(t *testing.T) {
	state := t.TempDir()
	out, err := mutexCmd(t, state, "acquire", "--quiet", "k").Output()
	if err != nil {
		t.Fatal(err)
	}
	_ = out
	// Corrupt the holder document directly.
	holder := filepath.Join(state, "locks", "k", "holder.json")
	if err := os.WriteFile(holder, []byte("{not json"), 0o644); err != nil {
		t.Fatal(err)
	}
	res := mutexCmd(t, state, "wait", "--timeout", "3s", "--poll", "100ms", "k")
	combined, err := res.CombinedOutput()
	if code := exitCode(err); code != 1 {
		t.Fatalf("wait on corrupt state: exit %d, want 1; output:\n%s", code, combined)
	}
	if strings.Contains(string(combined), "panic") {
		t.Fatalf("wait panicked:\n%s", combined)
	}
}

func TestLeaseLossTerminatesRun(t *testing.T) {
	state := t.TempDir()
	// run with the minimum TTL so renew cadence is fast (5s/3 ≈ 1.7s). The
	// child is our portable sleep helper (Windows runners have no `sleep`).
	cmd := mutexCmd(t, state, "run", "--quiet", "--ttl", "5s", "k", "--", os.Args[0])
	cmd.Env = append(cmd.Env, "AGENTMUTEX_TEST_MODE=sleep", "AGENTMUTEX_TEST_SECONDS=30")
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	done := make(chan error, 1)
	go func() { done <- cmd.Wait() }()

	time.Sleep(1 * time.Second) // let it acquire and start the child
	if err := mutexCmd(t, state, "force-release", "--yes", "k").Run(); err != nil {
		t.Fatalf("force-release: %v", err)
	}

	select {
	case err := <-done:
		if code := exitCode(err); code != 14 {
			t.Fatalf("run after lease loss: exit %d, want 14", code)
		}
	case <-time.After(20 * time.Second):
		cmd.Process.Kill()
		t.Fatal("run did not terminate after losing its lease")
	}
}

func TestStatusJSONRedactsToken(t *testing.T) {
	state := t.TempDir()
	out, err := mutexCmd(t, state, "acquire", "--quiet", "k").Output()
	if err != nil {
		t.Fatal(err)
	}
	token := strings.TrimSpace(string(out))
	for _, args := range [][]string{{"status", "--json", "k"}, {"list", "--json"}} {
		got, err := mutexCmd(t, state, args...).Output()
		if err != nil {
			t.Fatal(err)
		}
		if strings.Contains(string(got), token) {
			t.Errorf("%v leaks the lease token:\n%s", args, got)
		}
	}
	// The token still works, of course.
	if err := mutexCmd(t, state, "release", "--token", token, "k").Run(); err != nil {
		t.Fatal(err)
	}
}

func TestStatusJSON(t *testing.T) {
	state := t.TempDir()
	if err := mutexCmd(t, state, "acquire", "--quiet", "--reason", "deploying v2", "deploy:staging").Run(); err != nil {
		t.Fatal(err)
	}
	out, err := mutexCmd(t, state, "status", "--json", "deploy:staging").Output()
	if err != nil {
		t.Fatal(err)
	}
	s := string(out)
	for _, want := range []string{`"state": "held"`, `"deploy:staging"`, `"deploying v2"`} {
		if !strings.Contains(s, want) {
			t.Errorf("status --json missing %s:\n%s", want, s)
		}
	}
}
