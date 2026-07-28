package main

import (
	"flag"
	"fmt"
	"math/rand"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/DIodide/agentmutex/internal/mutex"
)

// acquireFlags are the knobs shared by `acquire` and `run`.
type acquireFlags struct {
	ttl     *time.Duration
	noWait  *bool
	timeout *time.Duration
	poll    *time.Duration
	agent   *string
	reason  *string
	quiet   *bool
}

func registerAcquireFlags(fs *flag.FlagSet) acquireFlags {
	return acquireFlags{
		ttl:     fs.Duration("ttl", mutex.DefaultTTL, "lease time-to-live; cover your worst case or renew"),
		noWait:  fs.Bool("no-wait", false, "fail immediately (exit 10) instead of waiting"),
		timeout: fs.Duration("timeout", 0, "give up waiting after this long (exit 11); 0 = wait forever"),
		poll:    fs.Duration("poll", mutex.DefaultPollInterval, "poll interval while waiting (10ms–10s)"),
		agent:   fs.String("agent", "", "agent name recorded on the lease (default $AGENTMUTEX_AGENT or user@host)"),
		reason:  fs.String("reason", "", "human-readable reason recorded on the lease"),
		quiet:   fs.Bool("quiet", false, "suppress progress messages on stderr"),
	}
}

func (af acquireFlags) validate() error {
	if err := validateTTL(*af.ttl); err != nil {
		return err
	}
	if err := validatePoll(*af.poll); err != nil {
		return err
	}
	return validateTimeout(*af.timeout)
}

func cmdAcquire(args []string) int {
	fs := newFlagSet("acquire", "Acquire a lease on a key. Waits (pessimistically) by default and prints the lease token on stdout.",
		"acquire [flags] <key>")
	dir := dirFlag(fs)
	af := registerAcquireFlags(fs)
	jsonOut := fs.Bool("json", false, "print the lease as JSON instead of just the token")
	tokenFile := fs.String("token-file", "", "write the token to this file (mode 0600) instead of stdout — keeps it out of CI logs")
	if code, done := parseFlags(fs, args); done {
		return code
	}
	key, err := oneKeyArg(fs)
	if err != nil {
		fmt.Fprintf(os.Stderr, "agentmutex: %v\n", err)
		fs.Usage()
		return ExitUsage
	}
	if err := af.validate(); err != nil {
		fmt.Fprintf(os.Stderr, "agentmutex: %v\n", err)
		return ExitUsage
	}
	st, err := openStore(*dir)
	if err != nil {
		return fail(err)
	}

	start := time.Now()
	holder, code := acquireBlocking(st, key, af)
	if code != ExitOK {
		return code
	}
	if *tokenFile != "" {
		if werr := os.WriteFile(*tokenFile, []byte(holder.Token+"\n"), 0o600); werr != nil {
			return fail(fmt.Errorf("writing --token-file: %w", werr))
		}
	}
	switch {
	case *jsonOut:
		red := *holder
		if *tokenFile != "" {
			red.Token = "" // don't also echo it to stdout
		}
		printJSON(struct {
			*mutex.Holder
			WaitedSeconds float64 `json:"waited_seconds"`
		}{&red, time.Since(start).Seconds()})
	case *tokenFile != "":
		fmt.Fprintf(os.Stderr, "agentmutex: acquired %s; token written to %s\n", key, *tokenFile)
	default:
		fmt.Println(holder.Token)
	}
	return ExitOK
}

// acquireBlocking implements the pessimistic wait: enqueue as a waiter, then
// poll TryAcquire until we win, time out, or get interrupted. Because agents
// are inference-bound, a ~1s poll interval adds negligible latency while
// keeping everything inline — no daemon, no notifications.
func acquireBlocking(st *mutex.Store, key string, af acquireFlags) (*mutex.Holder, int) {
	// The lease token is the private credential release/renew require; the
	// waiter ID is a separate public queue identity. Keeping them distinct
	// means our on-disk queue entry never exposes the future lease token.
	token := mutex.NewToken()
	waiterID := mutex.NewToken()
	opts := mutex.AcquireOpts{
		TTL:    *af.ttl,
		Agent:  sanitizeMeta(*af.agent),
		PID:    os.Getpid(),
		Host:   hostname(),
		Reason: sanitizeMeta(*af.reason),
	}
	if opts.Agent == "" {
		opts.Agent = defaultAgent()
	}

	// Fast path / no-wait: one attempt, no queue entry.
	res, err := st.TryAcquire(key, token, opts)
	if err != nil {
		return nil, fail(err)
	}
	if res.Acquired {
		return res.Holder, ExitOK
	}
	// Self-reentry: if we are nested inside an `agentmutex run` that already
	// holds this exact key (it exported AGENTMUTEX_LEASE_KEY), waiting for it
	// is a guaranteed self-deadlock — leases are not reentrant. Fail fast
	// with a clear message instead of blocking forever. (If the ancestor had
	// already released, the fast path above would have succeeded.)
	if os.Getenv("AGENTMUTEX_LEASE_KEY") == key {
		fmt.Fprintf(os.Stderr, "agentmutex: refusing to acquire %s: it is already held by an ancestor 'agentmutex run' (self-deadlock — leases are not reentrant; use a different key or release the parent lease first)\n", key)
		return nil, ExitHeld
	}
	if *af.noWait {
		fmt.Fprintf(os.Stderr, "agentmutex: %s is %s\n", key, describeBlock(res, st, key, ""))
		return nil, ExitHeld
	}

	// Slow path: join the FIFO queue and poll.
	opts.WaiterID = waiterID
	w := mutex.Waiter{
		ID:         waiterID,
		Agent:      opts.Agent,
		PID:        opts.PID,
		Host:       opts.Host,
		Reason:     opts.Reason,
		EnqueuedAt: time.Now(),
	}
	entry, err := st.Enqueue(key, w)
	if err != nil {
		return nil, fail(err)
	}
	opts.Enqueued = true

	sigc := make(chan os.Signal, 1)
	signal.Notify(sigc, os.Interrupt, syscall.SIGTERM, syscall.SIGHUP)
	defer signal.Stop(sigc)

	start := time.Now()
	var deadline time.Time
	if *af.timeout > 0 {
		deadline = start.Add(*af.timeout)
	}
	nextProgress := start // announce immediately on first block
	for {
		res, err := st.TryAcquire(key, token, opts)
		switch {
		case err != nil:
			// A transient store error (a peer wedged in the guard, an I/O
			// blip). The key was validated up front, so retrying is correct —
			// crucially, we keep our queue entry rather than dropping to the
			// back of the line. --timeout / interrupt still bound the wait.
			if !*af.quiet && !time.Now().Before(nextProgress) {
				fmt.Fprintf(os.Stderr, "agentmutex: retrying %s (keeping queue position): %v\n", key, err)
				nextProgress = time.Now().Add(15 * time.Second)
			}
		case res.Acquired:
			if !*af.quiet {
				fmt.Fprintf(os.Stderr, "agentmutex: acquired %s after %s\n", key, humanDur(time.Since(start)))
			}
			return res.Holder, ExitOK
		default:
			if !*af.quiet && !time.Now().Before(nextProgress) {
				fmt.Fprintf(os.Stderr, "agentmutex: waiting for %s — %s (waited %s)\n",
					key, describeBlock(res, st, key, waiterID), humanDur(time.Since(start)))
				nextProgress = time.Now().Add(15 * time.Second)
			}
		}
		if err := st.Heartbeat(entry, w); err != nil && !*af.quiet {
			fmt.Fprintf(os.Stderr, "agentmutex: warning: queue heartbeat failed: %v\n", err)
		}
		if !deadline.IsZero() && !time.Now().Before(deadline) {
			st.Dequeue(entry)
			fmt.Fprintf(os.Stderr, "agentmutex: timed out after %s waiting for %s (%s)\n",
				humanDur(*af.timeout), key, describeBlock(res, st, key, waiterID))
			return nil, ExitTimeout
		}
		// Jittered sleep so simultaneous waiters do not poll in lockstep.
		sleep := *af.poll + time.Duration(rand.Int63n(int64(*af.poll)/4+1))
		if !deadline.IsZero() {
			if until := time.Until(deadline); until < sleep {
				sleep = until
			}
		}
		select {
		case s := <-sigc:
			st.Dequeue(entry)
			fmt.Fprintf(os.Stderr, "agentmutex: interrupted while waiting for %s\n", key)
			return nil, signalExit(s)
		case <-time.After(sleep):
		}
	}
}

func hostname() string {
	h, err := os.Hostname()
	if err != nil {
		return "localhost"
	}
	return h
}
