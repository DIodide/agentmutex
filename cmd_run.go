package main

import (
	"errors"
	"fmt"
	"os"
	"os/exec"
	"os/signal"
	"strings"
	"sync"
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

	st, err := openStore(*dir)
	if err != nil {
		return fail(err)
	}

	// Register for signals BEFORE acquiring: a SIGINT in the gap between
	// acquire returning and the child starting must not kill us with the
	// lease held (it would strand the lease until TTL expiry). Buffered
	// signals are forwarded to the child as soon as it starts.
	sigc := make(chan os.Signal, 4)
	signal.Notify(sigc, os.Interrupt, syscall.SIGTERM, syscall.SIGHUP)
	defer signal.Stop(sigc)

	holder, code := acquireBlocking(st, key, af)
	if code != ExitOK {
		return code
	}
	if !*af.quiet {
		fmt.Fprintf(os.Stderr, "agentmutex: holding %s (ttl %s, auto-renewing) — running: %s\n",
			key, humanDur(*af.ttl), shellJoin(cmdArgs))
		if *onLoss == "terminate" && !fencesProcessTree {
			fmt.Fprintf(os.Stderr, "agentmutex: note: on this platform only the direct child is terminated on lease loss; detached grandchildren are not fenced\n")
		}
	}

	// Child inherits the lease so wrapped scripts can renew/release early and
	// nested agentmutex calls can detect self-reentry instead of deadlocking.
	childEnv := append(os.Environ(),
		"AGENTMUTEX_LEASE_KEY="+key,
		"AGENTMUTEX_TOKEN="+holder.Token,
		"AGENTMUTEX_DIR="+st.Root,
	)

	// Auto-renew at ttl/3 so even two consecutive missed renewals cannot
	// lose the lease while the command runs. Definitive loss (someone else
	// holds it, or no lease exists) closes leaseLost; transient errors only
	// warn.
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
		// lastGood tracks the most recent moment the lease was provably ours.
		// If renews keep failing (I/O error, ENOSPC, holder.json damaged)
		// past the point our lease must have expired, the lease is gone even
		// though we never saw an explicit NotHolder — treat that as loss too.
		lastGood := holder.ExpiresAt
		for {
			select {
			case <-t.C:
				h, err := st.Renew(key, holder.Token, *af.ttl)
				if err == nil {
					lastGood = h.ExpiresAt
					continue
				}
				var nh *mutex.NotHolderError
				if errors.Is(err, mutex.ErrNotHeld) || errors.As(err, &nh) {
					fmt.Fprintf(os.Stderr, "agentmutex: LEASE LOST on %s: %v\n", key, err)
					lostOnce.Do(func() { close(leaseLost) })
					return
				}
				if !time.Now().Before(lastGood) {
					fmt.Fprintf(os.Stderr, "agentmutex: LEASE LOST on %s: renewals have been failing past the TTL (%v); assuming the lease expired\n", key, err)
					lostOnce.Do(func() { close(leaseLost) })
					return
				}
				fmt.Fprintf(os.Stderr, "agentmutex: warning: failed to renew lease on %s: %v\n", key, err)
			case <-stopRenew:
				return
			}
		}
	}()

	code = runChild(cmdArgs, childEnv, sigc, leaseLost, *onLoss == "terminate")

	close(stopRenew)
	<-renewDone

	lost := false
	select {
	case <-leaseLost:
		lost = true
	default:
	}
	if _, err := st.Release(key, holder.Token, false); err != nil {
		// NotHolderError means someone else holds the lease now — a genuine
		// collision. ErrNotHeld only means the lease is already gone (it
		// expired, or the wrapped command released early via the exported
		// token); since the command has already finished, that is not a
		// collision, so don't escalate it to exit 14.
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

	if lost {
		fmt.Fprintf(os.Stderr, "agentmutex: lease on %s was lost mid-run; the resource may have been mutated concurrently\n", key)
		if *onLoss == "terminate" {
			return ExitLeaseLost
		}
	}
	return code
}

// runChild runs the command with stdio passed through, forwarding signals
// from sigc to the child's whole process tree, and returns its exit code. If
// leaseLost closes and terminateOnLoss is set, the tree gets SIGTERM, then
// SIGKILL after a grace period — so backgrounded grandchildren cannot keep
// mutating the resource once the lease is gone.
func runChild(argv []string, env []string, sigc chan os.Signal, leaseLost <-chan struct{}, terminateOnLoss bool) int {
	cmd := exec.Command(argv[0], argv[1:]...)
	cmd.Stdin = os.Stdin
	cmd.Stdout = os.Stdout
	cmd.Stderr = os.Stderr
	cmd.Env = env
	setChildProcGroup(cmd)
	if err := cmd.Start(); err != nil {
		fmt.Fprintf(os.Stderr, "agentmutex: %v\n", err)
		return 127
	}
	waitDone := make(chan struct{})
	go func() {
		for {
			select {
			case s := <-sigc:
				signalTree(cmd, s)
			case <-leaseLost:
				leaseLost = nil // closed channel would spin this loop
				if terminateOnLoss {
					fmt.Fprintf(os.Stderr, "agentmutex: terminating command (lease lost); SIGKILL in %s\n", humanDur(leaseLossGrace))
					signalTree(cmd, syscall.SIGTERM)
					go func() {
						select {
						case <-time.After(leaseLossGrace):
							killTree(cmd)
						case <-waitDone:
						}
					}()
				}
			case <-waitDone:
				return
			}
		}
	}()
	err := cmd.Wait()
	close(waitDone)
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
