package main

import (
	"errors"
	"fmt"
	"os"
	"os/exec"
	"os/signal"
	"strings"
	"sync"
	"sync/atomic"
	"syscall"
	"time"

	"github.com/DIodide/agentmutex/internal/mutex"
)

// minRunTTL keeps the auto-renew cadence (ttl/3, floored at 1s) meaningfully
// below the TTL; smaller TTLs would expire before the first renewal.
const minRunTTL = 5 * time.Second

// leaseLossGrace is how long a child gets after SIGTERM (on lease loss)
// before SIGKILL.
const leaseLossGrace = 10 * time.Second

// cmdRun is the golden path for agents: acquire the lease, run the command
// with stdio passed through, auto-renew while it runs, and always release —
// even if the command fails or we get signalled.
//
// If the lease is definitively lost mid-run (host suspended past the TTL, or
// a human force-released), mutual exclusion is broken: by default the child
// is terminated and run exits 14 so callers get a machine-detectable signal.
func cmdRun(args []string) int {
	fs := newFlagSet("run", "Acquire a lease, run a command under it (auto-renewing), then release.\nForwards the command's exit code; agentmutex's own codes are 10/11 (never acquired) and 14 (lease lost mid-run).",
		"run [flags] <key> -- <command> [args...]")
	dir := dirFlag(fs)
	af := registerAcquireFlags(fs)
	onLoss := fs.String("on-lease-loss", "terminate", "if the lease is lost mid-run: 'terminate' the command (exit 14) or 'continue' it (warn only)")
	maxHold := fs.Duration("max-hold", 0, "abort the command (exit 14) if it holds the lease longer than this — catches a wedged/hung deploy; 0 = no cap")
	exportToken := fs.Bool("export-token", false, "export AGENTMUTEX_TOKEN to the child so it can renew/release the lease (off by default — the token is the release credential and env is readable by same-user processes)")
	if code, done := parseFlags(fs, args); done {
		return code
	}
	rest := fs.Args()
	if len(rest) < 2 {
		fmt.Fprintf(os.Stderr, "agentmutex: expected <key> -- <command...>\n")
		fs.Usage()
		return ExitUsage
	}
	key := rest[0]
	cmdArgs := rest[1:]
	if cmdArgs[0] == "--" {
		cmdArgs = cmdArgs[1:]
	} else if cmdArgs[0] != "" && cmdArgs[0][0] == '-' {
		// `run key --ttl 5m -- cmd` would otherwise execute "--ttl" as the
		// command with default flags — a silent footgun.
		fmt.Fprintf(os.Stderr, "agentmutex: flags must come before the key (got %q after it); usage: run [flags] <key> -- <command...>\n", cmdArgs[0])
		return ExitUsage
	}
	if len(cmdArgs) == 0 {
		fmt.Fprintf(os.Stderr, "agentmutex: no command given after --\n")
		return ExitUsage
	}
	if err := mutex.ValidateKey(key); err != nil {
		fmt.Fprintf(os.Stderr, "agentmutex: %v\n", err)
		return ExitUsage
	}
	if err := af.validate(); err != nil {
		fmt.Fprintf(os.Stderr, "agentmutex: %v\n", err)
		return ExitUsage
	}
	if *af.ttl < minRunTTL {
		fmt.Fprintf(os.Stderr, "agentmutex: run requires --ttl >= %s (auto-renew fires at ttl/3; a smaller lease would expire mid-command)\n", minRunTTL)
		return ExitUsage
	}
	if *onLoss != "terminate" && *onLoss != "continue" {
		fmt.Fprintf(os.Stderr, "agentmutex: --on-lease-loss must be 'terminate' or 'continue', got %q\n", *onLoss)
		return ExitUsage
	}
	if *maxHold < 0 {
		fmt.Fprintf(os.Stderr, "agentmutex: --max-hold must be >= 0, got %s\n", *maxHold)
		return ExitUsage
	}

	st, err := openStore(*dir)
	if err != nil {
		return fail(err)
	}

	// Register for signals BEFORE acquiring: a SIGINT in the gap between
	// acquire returning and the child starting must not kill us with the
	// lease held (it would strand the lease until TTL expiry). Buffered
	// signals are forwarded to the child as soon as it starts.
	sigc := make(chan os.Signal, 4)
	signal.Notify(sigc, os.Interrupt, syscall.SIGTERM, syscall.SIGHUP, syscall.SIGQUIT)
	defer signal.Stop(sigc)

	holder, code := acquireBlocking(st, key, af)
	if code != ExitOK {
		return code
	}
	if !*af.quiet {
		fmt.Fprintf(os.Stderr, "agentmutex: holding %s (ttl %s, auto-renewing) — running: %s\n",
			key, humanDur(*af.ttl), shellJoin(cmdArgs))
	}

	// The child sees the key and store dir (for nested self-reentry detection
	// and monitoring), but NOT the lease token by default — the token is the
	// release credential and a build step that dumps its environment (or any
	// same-user process reading /proc/<pid>/environ) could then release the
	// lease. Pass --export-token to opt in.
	childEnv := append(os.Environ(),
		"AGENTMUTEX_LEASE_KEY="+key,
		"AGENTMUTEX_DIR="+st.Root,
	)
	if *exportToken {
		childEnv = append(childEnv, "AGENTMUTEX_TOKEN="+holder.Token)
	}

	// lease holds the current token; the watchdog may rotate it when it
	// reclaims a lease that was cleared out from under us.
	lease := &leaseHolder{token: holder.Token}
	reclaimOpts := mutex.AcquireOpts{
		TTL: *af.ttl, Agent: holder.Agent, PID: holder.PID,
		Host: holder.Host, Reason: holder.Reason, Reclaim: true,
	}

	// Auto-renew at ttl/3 so even two consecutive missed renewals cannot lose
	// the lease while the command runs. If the lease is cleared out from
	// under us (force-release, prune, a transient failure past the TTL), we
	// RE-ACQUIRE it to re-establish exclusivity rather than run unprotected;
	// only a different live holder (a genuine collision) closes leaseLost.
	leaseLost := make(chan struct{})
	var lostOnce sync.Once
	stopRenew := make(chan struct{})
	renewDone := make(chan struct{})
	go func() {
		defer close(renewDone)
		interval := *af.ttl / 3
		if interval < time.Second {
			interval = time.Second
		}
		t := time.NewTicker(interval)
		defer t.Stop()
		lastGood := holder.ExpiresAt
		warnedReclaim := false
		loseTo := func(err error) {
			fmt.Fprintf(os.Stderr, "agentmutex: LEASE LOST on %s: %v\n", key, err)
			lostOnce.Do(func() { close(leaseLost) })
		}
		for {
			select {
			case <-t.C:
				h, err := st.Renew(key, lease.get(), *af.ttl)
				if err == nil {
					lastGood = h.ExpiresAt
					continue
				}
				var nh *mutex.NotHolderError
				if errors.As(err, &nh) {
					loseTo(err) // someone else holds it now
					return
				}
				// The lease is gone (ErrNotHeld) or a transient error hit.
				// Try to re-establish exclusivity by reclaiming the key.
				newTok := mutex.NewToken()
				res, aerr := st.TryAcquire(key, newTok, reclaimOpts)
				if aerr == nil && res.Acquired {
					lease.set(newTok)
					lastGood = res.Holder.ExpiresAt
					if !warnedReclaim {
						fmt.Fprintf(os.Stderr, "agentmutex: re-acquired %s after it was cleared mid-run (protection restored)\n", key)
						warnedReclaim = true
					}
					continue
				}
				if res != nil && res.Holder != nil {
					loseTo(fmt.Errorf("%s is now held by %s", key, res.Holder.Agent))
					return
				}
				// Couldn't reclaim and nobody else holds it (transient error).
				// If we are past the last provably-good expiry, exclusivity
				// can no longer be guaranteed — treat as loss.
				if !time.Now().Before(lastGood) {
					loseTo(fmt.Errorf("renewals failing past the TTL (%v)", err))
					return
				}
				fmt.Fprintf(os.Stderr, "agentmutex: warning: failed to renew lease on %s: %v\n", key, err)
			case <-stopRenew:
				return
			}
		}
	}()

	code, maxHoldHit := runChild(cmdArgs, childEnv, sigc, leaseLost, *onLoss == "terminate", *af.quiet, *maxHold)

	close(stopRenew)
	<-renewDone

	lost := false
	select {
	case <-leaseLost:
		lost = true
	default:
	}
	if _, err := st.Release(key, lease.get(), false); err != nil {
		// NotHolderError means someone else holds the lease now — a genuine
		// collision. ErrNotHeld only means the lease is already gone; since
		// the command has already finished, that is not a collision.
		var nh *mutex.NotHolderError
		if errors.As(err, &nh) {
			lost = true
		}
		if !errors.Is(err, mutex.ErrNotHeld) || !*af.quiet {
			fmt.Fprintf(os.Stderr, "agentmutex: warning: failed to release %s: %v\n", key, err)
		}
	} else if !*af.quiet {
		fmt.Fprintf(os.Stderr, "agentmutex: released %s\n", key)
	}

	if maxHoldHit {
		fmt.Fprintf(os.Stderr, "agentmutex: aborted %s: the command held the lease longer than --max-hold %s (deploy appears wedged)\n", key, humanDur(*maxHold))
		return ExitLeaseLost
	}
	if lost {
		fmt.Fprintf(os.Stderr, "agentmutex: lease on %s was lost mid-run; the resource may have been mutated concurrently\n", key)
		if *onLoss == "terminate" {
			return ExitLeaseLost
		}
	}
	return code
}

// leaseHolder is a token that the auto-renew watchdog may rotate (on reclaim)
// while the main goroutine still needs to release with the current value.
type leaseHolder struct {
	mu    sync.Mutex
	token string
}

func (l *leaseHolder) get() string  { l.mu.Lock(); defer l.mu.Unlock(); return l.token }
func (l *leaseHolder) set(t string) { l.mu.Lock(); l.token = t; l.mu.Unlock() }

// runChild runs the command with stdio passed through, forwarding signals
// from sigc to the child's process tree, and returns its exit code plus
// whether the max-hold cap fired. The tree gets SIGTERM then SIGKILL after a
// grace period when: leaseLost closes (and terminateOnLoss), an external
// SIGINT/SIGTERM arrives (so a signal-trapping deploy can't wedge the lease),
// or maxHold elapses.
func runChild(argv []string, env []string, sigc chan os.Signal, leaseLost <-chan struct{}, terminateOnLoss, quiet bool, maxHold time.Duration) (int, bool) {
	cmd := exec.Command(argv[0], argv[1:]...)
	cmd.Stdin = os.Stdin
	cmd.Stdout = os.Stdout
	cmd.Stderr = os.Stderr
	cmd.Env = env
	ownGroup := setChildProcGroup(cmd)
	if err := cmd.Start(); err != nil {
		fmt.Fprintf(os.Stderr, "agentmutex: %v\n", err)
		return 127, false
	}
	child := childProc{cmd: cmd, ownGroup: ownGroup}
	if terminateOnLoss && !ownGroup && !quiet {
		fmt.Fprintf(os.Stderr, "agentmutex: note: this command is not in its own process group (interactive stdin or unsupported platform); on lease loss only the direct child is terminated, not detached grandchildren\n")
	}
	waitDone := make(chan struct{})
	var maxHoldHit atomic.Bool

	// killAfterGrace SIGKILLs the tree once grace elapses unless the child
	// has already exited.
	killAfterGrace := func() {
		go func() {
			select {
			case <-time.After(leaseLossGrace):
				killTree(child)
			case <-waitDone:
			}
		}()
	}

	var maxHoldC <-chan time.Time
	if maxHold > 0 {
		maxHoldC = time.After(maxHold)
	}
	go func() {
		escalating := false
		for {
			select {
			case s := <-sigc:
				signalTree(child, s)
				// A deploy that traps and ignores SIGTERM must not keep run
				// alive holding the lease: escalate to SIGKILL after a grace.
				if !escalating && (s == os.Interrupt || s == syscall.SIGTERM) {
					escalating = true
					killAfterGrace()
				}
			case <-leaseLost:
				leaseLost = nil // closed channel would spin this loop
				if terminateOnLoss && !escalating {
					escalating = true
					fmt.Fprintf(os.Stderr, "agentmutex: terminating command (lease lost); SIGKILL in %s\n", humanDur(leaseLossGrace))
					signalTree(child, syscall.SIGTERM)
					killAfterGrace()
				}
			case <-maxHoldC:
				maxHoldC = nil
				maxHoldHit.Store(true)
				if !escalating {
					escalating = true
					fmt.Fprintf(os.Stderr, "agentmutex: max-hold %s exceeded; terminating command, SIGKILL in %s\n", humanDur(maxHold), humanDur(leaseLossGrace))
					signalTree(child, syscall.SIGTERM)
					killAfterGrace()
				}
			case <-waitDone:
				return
			}
		}
	}()
	err := cmd.Wait()
	close(waitDone)
	return childExitCode(err), maxHoldHit.Load()
}

func childExitCode(err error) int {
	if err == nil {
		return 0
	}
	var ee *exec.ExitError
	if errors.As(err, &ee) {
		if code := ee.ExitCode(); code >= 0 {
			return code
		}
		if ws, ok := ee.Sys().(syscall.WaitStatus); ok && ws.Signaled() {
			return 128 + int(ws.Signal())
		}
		return ExitError
	}
	fmt.Fprintf(os.Stderr, "agentmutex: %v\n", err)
	return ExitError
}

// shellJoin renders argv for the human-readable "running:" banner, quoting
// any argument that contains whitespace or shell-significant characters so
// the displayed command is unambiguous (and copy-pasteable).
func shellJoin(argv []string) string {
	var b strings.Builder
	for i, a := range argv {
		if i > 0 {
			b.WriteByte(' ')
		}
		if a == "" || strings.ContainsAny(a, " \t\n\"'\\$`*?()|&;<>") {
			b.WriteByte('\'')
			b.WriteString(strings.ReplaceAll(a, "'", `'\''`))
			b.WriteByte('\'')
		} else {
			b.WriteString(a)
		}
	}
	return b.String()
}
