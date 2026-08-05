package main

import (
	"fmt"
	"os"
	"text/tabwriter"
	"time"

	"github.com/DIodide/agentmutex/internal/history"
	"github.com/DIodide/agentmutex/internal/mutex"
)

// cmdHistory shows the lock changelog: who acquired, renewed, released,
// force-released, or lost each lease, newest first.
func cmdHistory(args []string) int {
	fs := newFlagSet("history", "Show the lock changelog (who held what, when, and how each lease ended), newest first.",
		"history [flags] [<key>]")
	dir := dirFlag(fs)
	limit := fs.Int("limit", 50, "maximum events to show")
	since := fs.Duration("since", 0, "only events newer than this (e.g. 24h); 0 = no limit")
	all := fs.Bool("all", false, "include renew heartbeats (hidden by default)")
	jsonOut := fs.Bool("json", false, "machine-readable output")
	if code, done := parseFlags(fs, args); done {
		return code
	}
	if fs.NArg() > 1 {
		fmt.Fprintf(os.Stderr, "agentmutex: expected at most one <key> argument\n")
		return ExitUsage
	}
	key := ""
	if fs.NArg() == 1 {
		key = fs.Arg(0)
		if err := mutex.ValidateKey(key); err != nil {
			fmt.Fprintf(os.Stderr, "agentmutex: %v\n", err)
			return ExitUsage
		}
	}
	if *limit <= 0 {
		fmt.Fprintf(os.Stderr, "agentmutex: --limit must be positive\n")
		return ExitUsage
	}
	if *since < 0 {
		fmt.Fprintf(os.Stderr, "agentmutex: --since must be >= 0\n")
		return ExitUsage
	}

	st, err := openStore(*dir)
	if err != nil {
		return fail(err)
	}
	log, err := st.History()
	if err != nil {
		return fail(fmt.Errorf("opening history database: %w", err))
	}
	defer log.Close()

	opts := history.QueryOpts{Key: key, IncludeRenews: *all, Limit: *limit}
	if *since > 0 {
		opts.Since = time.Now().Add(-*since)
	}
	events, err := log.Query(opts)
	if err != nil {
		return fail(fmt.Errorf("querying history: %w", err))
	}

	if *jsonOut {
		if events == nil {
			events = []history.Event{}
		}
		printJSON(events)
		return ExitOK
	}
	if len(events) == 0 {
		fmt.Println("no history recorded yet")
		return ExitOK
	}
	w := tabwriter.NewWriter(os.Stdout, 2, 4, 2, ' ', 0)
	if key == "" {
		fmt.Fprintln(w, "TIME\tEVENT\tKEY\tAGENT\tINFO")
	} else {
		fmt.Fprintln(w, "TIME\tEVENT\tAGENT\tINFO")
	}
	for _, e := range events {
		t := e.TS.Local().Format("2006-01-02 15:04:05")
		if key == "" {
			fmt.Fprintf(w, "%s\t%s\t%s\t%s\t%s\n", t, e.Event, e.Key, e.Agent, eventInfo(e))
		} else {
			fmt.Fprintf(w, "%s\t%s\t%s\t%s\n", t, e.Event, e.Agent, eventInfo(e))
		}
	}
	w.Flush()
	return ExitOK
}

// eventInfo composes the human INFO cell: hold/ttl durations, then reason,
// then any detail.
func eventInfo(e history.Event) string {
	out := ""
	switch e.Event {
	case history.EventAcquired, history.EventReclaimed, history.EventRenewed:
		if e.TTLSeconds > 0 {
			out = "ttl " + humanDur(time.Duration(e.TTLSeconds*float64(time.Second)))
		}
	case history.EventReleased, history.EventForceReleased, history.EventExpired:
		if e.HeldSeconds > 0 {
			out = "held " + humanDur(time.Duration(e.HeldSeconds*float64(time.Second)))
		}
	}
	if e.Reason != "" {
		if out != "" {
			out += " — "
		}
		out += truncate(e.Reason, 48)
	}
	if e.Detail != "" {
		if out != "" {
			out += " "
		}
		out += "(" + truncate(e.Detail, 60) + ")"
	}
	if out == "" {
		out = "-"
	}
	return out
}
