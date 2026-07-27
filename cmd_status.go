package main

import (
	"context"
	"fmt"
	"os"
	"os/signal"
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
		return ExitOK
	}
	printKeyStatus(ks)
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
	fmt.Fprintln(w, "KEY\tSTATE\tHOLDER\tEXPIRES\tWAITERS")
	for _, ks := range all {
		holder, expires := "-", "-"
		if ks.Holder != nil {
			holder = ks.Holder.Agent
			if ks.State == "expired" {
				expires = fmt.Sprintf("%s ago", humanDur(now.Sub(ks.Holder.ExpiresAt)))
			} else {
				expires = fmt.Sprintf("in %s", humanDur(ks.Holder.ExpiresAt.Sub(now)))
			}
		}
		fresh := 0
		for _, wt := range ks.Waiters {
			if wt.Fresh {
				fresh++
			}
		}
		fmt.Fprintf(w, "%s\t%s\t%s\t%s\t%d\n", ks.Key, ks.State, holder, expires, fresh)
	}
	w.Flush()
	return ExitOK
}

func printKeyStatus(ks *mutex.KeyStatus) {
	now := time.Now()
	fmt.Printf("key:      %s\n", ks.Key)
	fmt.Printf("state:    %s\n", ks.State)
	if ks.Holder != nil {
		h := ks.Holder
		fmt.Printf("holder:   %s (pid %d on %s)\n", h.Agent, h.PID, h.Host)
		if h.Reason != "" {
			fmt.Printf("reason:   %s\n", h.Reason)
		}
		fmt.Printf("acquired: %s (%s ago)\n", h.AcquiredAt.Format(time.RFC3339), humanDur(now.Sub(h.AcquiredAt)))
		if ks.State == "expired" {
			fmt.Printf("expired:  %s ago\n", humanDur(now.Sub(h.ExpiresAt)))
		} else {
			fmt.Printf("expires:  in %s\n", humanDur(h.ExpiresAt.Sub(now)))
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
		fmt.Printf("  %d. %s — waiting %s\n", i+1, wt.Agent, humanDur(now.Sub(wt.EnqueuedAt)))
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
	st, err := openStore(*dir)
	if err != nil {
		return fail(err)
	}

	ctx, cancel := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer cancel()
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
			if !*quiet {
				fmt.Fprintf(os.Stderr, "agentmutex: %s is free (waited %s)\n", key, humanDur(time.Since(start)))
			}
			return ExitOK
		}
		if ks.State == "corrupt" || ks.Holder == nil {
			return fail(fmt.Errorf("state for %s is corrupt; a human should inspect it and run 'agentmutex force-release %s'", key, key))
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
		case <-ctx.Done():
			fmt.Fprintf(os.Stderr, "agentmutex: interrupted\n")
			return 130
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
	fmt.Printf("pruned %d expired lease(s), %d stale waiter entrie(s)\n", len(res.ExpiredLeases), res.StaleWaiters)
	for _, k := range res.ExpiredLeases {
		fmt.Printf("  expired lease: %s\n", k)
	}
	return ExitOK
}
