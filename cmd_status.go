package main

import (
	"fmt"
	"os"
	"os/signal"
	"strings"
	"syscall"
	"text/tabwriter"
	"time"

	"github.com/DIodide/agentmutex/internal/mutex"
)

func cmdStatus(args []string) int {
	fs := newFlagSet("status", "Show the holder and queue for one key, or all keys if none is given.",
		"status [flags] [<key>]")
	dir := dirFlag(fs)
	jsonOut := fs.Bool("json", false, "machine-readable output")
	exitCode := fs.Bool("exit-code", false, "exit by state for scripting: 0 held, 3 free, 4 expired, 5 corrupt/unreadable (requires <key>)")
	if code, done := parseFlags(fs, args); done {
		return code
	}
	if fs.NArg() > 1 {
		fmt.Fprintf(os.Stderr, "agentmutex: expected at most one <key> argument\n")
		return ExitUsage
	}
	st, err := openStore(*dir)
	if err != nil {
		return fail(err)
	}
	if fs.NArg() == 0 {
		if *exitCode {
			fmt.Fprintf(os.Stderr, "agentmutex: --exit-code requires a <key>\n")
			return ExitUsage
		}
		return listLocks(st, *jsonOut)
	}
	key := fs.Arg(0)
	if err := mutex.ValidateKey(key); err != nil {
		fmt.Fprintf(os.Stderr, "agentmutex: %v\n", err)
		return ExitUsage
	}
	ks, err := st.Status(key)
	if err != nil {
		return fail(err)
	}
	if *jsonOut {
		printJSON(redactTokens(ks))
	} else {
		printKeyStatus(ks)
	}
	if *exitCode {
		switch ks.State {
		case "held":
			return ExitOK
		case "free":
			return 3
		case "expired":
			return 4
		default: // corrupt / unreadable
			return 5
		}
	}
	return ExitOK
}

func cmdList(args []string) int {
	fs := newFlagSet("list", "List every lock the store knows about.", "list [flags]")
	dir := dirFlag(fs)
	jsonOut := fs.Bool("json", false, "machine-readable output")
	if code, done := parseFlags(fs, args); done {
		return code
	}
	if fs.NArg() > 0 {
		fmt.Fprintf(os.Stderr, "agentmutex: list takes no arguments (did you mean 'status %s'?)\n", fs.Arg(0))
		return ExitUsage
	}
	st, err := openStore(*dir)
	if err != nil {
		return fail(err)
	}
	return listLocks(st, *jsonOut)
}

func listLocks(st *mutex.Store, jsonOut bool) int {
	all, err := st.List()
	if err != nil {
		return fail(err)
	}
	if jsonOut {
		if all == nil {
			all = []mutex.KeyStatus{}
		}
		for i := range all {
			redactTokens(&all[i])
		}
		printJSON(all)
		return ExitOK
	}
	if len(all) == 0 {
		fmt.Println("no locks")
		return ExitOK
	}
	now := time.Now()
	w := tabwriter.NewWriter(os.Stdout, 2, 4, 2, ' ', 0)
	// HELD-FOR (elapsed since acquire) beats EXPIRES here: for the golden
	// auto-renewing `run` lease, EXPIRES is a near-constant ~TTL and tells
	// you nothing, whereas a large HELD-FOR is exactly how you spot a stuck
	// deploy. Narrow columns first; REASON (free text) last so a long value
	// can only push its own row, and truncated cells keep typical rows under
	// ~100 columns so the table survives narrow terminals.
	fmt.Fprintln(w, "KEY\tSTATE\tHOLDER\tHELD-FOR\tWAITERS\tREASON")
	for _, ks := range all {
		holder, held, reason := "-", "-", "-"
		if ks.Holder != nil {
			holder = truncate(ks.Holder.Agent, 26)
			if ks.State == "expired" {
				// How long it was actually held before the TTL ran out;
				// STATE already says it's expired.
				held = humanDur(ks.Holder.ExpiresAt.Sub(ks.Holder.AcquiredAt))
			} else {
				held = humanDur(now.Sub(ks.Holder.AcquiredAt))
			}
			if ks.Holder.Reason != "" {
				reason = truncate(ks.Holder.Reason, 36)
			}
		}
		fresh := 0
		for _, wt := range ks.Waiters {
			if wt.Fresh {
				fresh++
			}
		}
		fmt.Fprintf(w, "%s\t%s\t%s\t%s\t%d\t%s\n", truncate(ks.Key, 28), ks.State, holder, held, fresh, reason)
	}
	w.Flush()
	return ExitOK
}

// truncate shortens s to n runes with an ellipsis, for table cells.
func truncate(s string, n int) string {
	r := []rune(s)
	if len(r) <= n {
		return s
	}
	return string(r[:n-1]) + "…"
}

func printKeyStatus(ks *mutex.KeyStatus) {
	now := time.Now()
	fmt.Printf("key:      %s\n", ks.Key)
	fmt.Printf("state:    %s\n", ks.State)
	if ks.State == "corrupt" || ks.State == "unreadable" {
		if ks.Error != "" {
			fmt.Printf("error:    %s\n", ks.Error)
		}
		fmt.Printf("fix:      a human should inspect it, then 'agentmutex force-release --yes %s'\n", ks.Key)
	}
	if ks.Holder != nil {
		h := ks.Holder
		// Agent already defaults to user@shorthost; don't repeat the host
		// when it is the tail of the agent name (full or short form).
		if strings.HasSuffix(h.Agent, "@"+h.Host) || strings.HasSuffix(h.Agent, "@"+shortHost(h.Host)) {
			fmt.Printf("holder:   %s (pid %d)\n", h.Agent, h.PID)
		} else {
			fmt.Printf("holder:   %s (pid %d on %s)\n", h.Agent, h.PID, h.Host)
		}
		if h.Reason != "" {
			fmt.Printf("reason:   %s\n", h.Reason)
		}
		fmt.Printf("held for: %s (acquired %s)\n", humanDur(now.Sub(h.AcquiredAt)), h.AcquiredAt.Format(time.RFC3339))
		// Renew recency separates a live auto-renewing deploy from one coasting
		// to its TTL (crashed/manual): a recent renew means the holder is alive.
		if h.RenewedAt != nil {
			fmt.Printf("renewed:  %s ago (holder is auto-renewing — likely a live deploy)\n", humanDur(now.Sub(*h.RenewedAt)))
		} else {
			fmt.Printf("renewed:  never (holder not auto-renewing; expires at its TTL)\n")
		}
		if ks.State == "expired" {
			fmt.Printf("expired:  %s ago — a waiter may take over\n", humanDur(now.Sub(h.ExpiresAt)))
		} else {
			fmt.Printf("expires:  in %s (worst-case takeover if the holder crashed now)\n", humanDur(h.ExpiresAt.Sub(now)))
		}
	}
	fresh := make([]mutex.Waiter, 0, len(ks.Waiters))
	stale := 0
	for _, wt := range ks.Waiters {
		if wt.Fresh {
			fresh = append(fresh, wt)
		} else {
			stale++
		}
	}
	fmt.Printf("waiters:  %d\n", len(fresh))
	for i, wt := range fresh {
		line := fmt.Sprintf("  %d. %s — waiting %s", i+1, wt.Agent, humanDur(now.Sub(wt.EnqueuedAt)))
		if wt.Reason != "" {
			line += fmt.Sprintf(" (%s)", truncate(wt.Reason, 50))
		}
		fmt.Println(line)
	}
	if stale > 0 {
		fmt.Printf("          (+%d stale entries; 'agentmutex prune' cleans them)\n", stale)
	}
}

// cmdWait blocks until the key has no active lease. Observational: it does
// NOT reserve the key — to mutate the resource, use acquire or run.
func cmdWait(args []string) int {
	fs := newFlagSet("wait", "Block until a key is free (no unexpired lease). Does not acquire — to mutate the resource, use acquire or run.",
		"wait [flags] <key>")
	dir := dirFlag(fs)
	timeout := fs.Duration("timeout", 0, "give up after this long (exit 11); 0 = wait forever")
	poll := fs.Duration("poll", mutex.DefaultPollInterval, "poll interval (10ms–10s)")
	quiet := fs.Bool("quiet", false, "suppress progress messages on stderr")
	jsonOut := fs.Bool("json", false, "on success, print the final free/expired snapshot as JSON")
	if code, done := parseFlags(fs, args); done {
		return code
	}
	key, err := oneKeyArg(fs)
	if err != nil {
		fmt.Fprintf(os.Stderr, "agentmutex: %v\n", err)
		fs.Usage()
		return ExitUsage
	}
	if err := validatePoll(*poll); err != nil {
		fmt.Fprintf(os.Stderr, "agentmutex: %v\n", err)
		return ExitUsage
	}
	if err := validateTimeout(*timeout); err != nil {
		fmt.Fprintf(os.Stderr, "agentmutex: %v\n", err)
		return ExitUsage
	}
	st, err := openStore(*dir)
	if err != nil {
		return fail(err)
	}

	sigc := make(chan os.Signal, 1)
	signal.Notify(sigc, os.Interrupt, syscall.SIGTERM, syscall.SIGHUP)
	defer signal.Stop(sigc)
	start := time.Now()
	var deadline time.Time
	if *timeout > 0 {
		deadline = start.Add(*timeout)
	}
	nextProgress := start
	for {
		ks, err := st.Status(key)
		if err != nil {
			return fail(err)
		}
		if ks.State == "free" || ks.State == "expired" {
			if *jsonOut {
				printJSON(redactTokens(ks))
			} else if !*quiet {
				fmt.Fprintf(os.Stderr, "agentmutex: %s is free (waited %s)\n", key, humanDur(time.Since(start)))
			}
			return ExitOK
		}
		if ks.State == "corrupt" || ks.State == "unreadable" || ks.Holder == nil {
			return fail(fmt.Errorf("state for %s is %s; a human should inspect it and run 'agentmutex force-release --yes %s'", key, ks.State, key))
		}
		if !*quiet && !time.Now().Before(nextProgress) {
			h := ks.Holder
			fmt.Fprintf(os.Stderr, "agentmutex: %s held by %s, expires in %s (waited %s)\n",
				key, h.Agent, humanDur(time.Until(h.ExpiresAt)), humanDur(time.Since(start)))
			nextProgress = time.Now().Add(15 * time.Second)
		}
		if !deadline.IsZero() && !time.Now().Before(deadline) {
			fmt.Fprintf(os.Stderr, "agentmutex: timed out after %s waiting for %s\n", humanDur(*timeout), key)
			return ExitTimeout
		}
		select {
		case s := <-sigc:
			fmt.Fprintf(os.Stderr, "agentmutex: interrupted\n")
			return signalExit(s)
		case <-time.After(*poll):
		}
	}
}

func cmdPrune(args []string) int {
	fs := newFlagSet("prune", "Remove expired leases and stale queue entries. Safe: only provably dead state is touched.",
		"prune [flags]")
	dir := dirFlag(fs)
	jsonOut := fs.Bool("json", false, "machine-readable output")
	if code, done := parseFlags(fs, args); done {
		return code
	}
	if fs.NArg() > 0 {
		fmt.Fprintf(os.Stderr, "agentmutex: prune takes no arguments\n")
		return ExitUsage
	}
	st, err := openStore(*dir)
	if err != nil {
		return fail(err)
	}
	res, err := st.Prune()
	if err != nil {
		return fail(err)
	}
	if *jsonOut {
		printJSON(res)
		return ExitOK
	}
	fmt.Printf("pruned %s, %s\n",
		plural(len(res.ExpiredLeases), "expired lease", "expired leases"),
		plural(res.StaleWaiters, "stale waiter entry", "stale waiter entries"))
	for _, k := range res.ExpiredLeases {
		fmt.Printf("  expired lease: %s\n", k)
	}
	for _, e := range res.Errors {
		fmt.Fprintf(os.Stderr, "  warning: %s\n", e)
	}
	return ExitOK
}

// plural formats a count with the grammatically correct noun.
func plural(n int, one, many string) string {
	if n == 1 {
		return fmt.Sprintf("%d %s", n, one)
	}
	return fmt.Sprintf("%d %s", n, many)
}
