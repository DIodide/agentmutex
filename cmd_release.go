package main

import (
	"errors"
	"fmt"
	"os"
	"time"

	"github.com/DIodide/agentmutex/internal/mutex"
)

func cmdRelease(args []string) int {
	fs := newFlagSet("release", "Release a lease you hold. Requires the token printed by acquire.",
		"release [flags] <key>")
	dir := dirFlag(fs)
	token := fs.String("token", "", "lease token (default $AGENTMUTEX_TOKEN)")
	if code, done := parseFlags(fs, args); done {
		return code
	}
	key, err := oneKeyArg(fs)
	if err != nil {
		fmt.Fprintf(os.Stderr, "agentmutex: %v\n", err)
		fs.Usage()
		return ExitUsage
	}
	tok, err := tokenOrEnv(*token)
	if err != nil {
		return fail(err)
	}
	st, err := openStore(*dir)
	if err != nil {
		return fail(err)
	}
	if _, err := st.Release(key, tok, false); err != nil {
		return releaseErrCode(err)
	}
	fmt.Fprintf(os.Stderr, "agentmutex: released %s\n", key)
	return ExitOK
}

func cmdRenew(args []string) int {
	fs := newFlagSet("renew", "Extend a lease you hold by --ttl from now.",
		"renew [flags] <key>")
	dir := dirFlag(fs)
	token := fs.String("token", "", "lease token (default $AGENTMUTEX_TOKEN)")
	ttl := fs.Duration("ttl", mutex.DefaultTTL, "new time-to-live, measured from now")
	jsonOut := fs.Bool("json", false, "print the renewed lease as JSON")
	if code, done := parseFlags(fs, args); done {
		return code
	}
	key, err := oneKeyArg(fs)
	if err != nil {
		fmt.Fprintf(os.Stderr, "agentmutex: %v\n", err)
		fs.Usage()
		return ExitUsage
	}
	tok, err := tokenOrEnv(*token)
	if err != nil {
		return fail(err)
	}
	st, err := openStore(*dir)
	if err != nil {
		return fail(err)
	}
	h, err := st.Renew(key, tok, *ttl)
	if err != nil {
		return releaseErrCode(err)
	}
	if *jsonOut {
		printJSON(h)
	} else {
		fmt.Fprintf(os.Stderr, "agentmutex: renewed %s until %s (in %s)\n",
			key, h.ExpiresAt.Format(time.RFC3339), humanDur(time.Until(h.ExpiresAt)))
	}
	return ExitOK
}

func cmdForceRelease(args []string) int {
	fs := newFlagSet("force-release", "Forcibly clear a lock regardless of token. Human override for crashed or wedged agents — dry-run unless --yes.",
		"force-release [flags] <key>")
	dir := dirFlag(fs)
	yes := fs.Bool("yes", false, "actually release (default is a dry run)")
	if code, done := parseFlags(fs, args); done {
		return code
	}
	key, err := oneKeyArg(fs)
	if err != nil {
		fmt.Fprintf(os.Stderr, "agentmutex: %v\n", err)
		fs.Usage()
		return ExitUsage
	}
	st, err := openStore(*dir)
	if err != nil {
		return fail(err)
	}
	if !*yes {
		ks, err := st.Status(key)
		if err != nil {
			return fail(err)
		}
		if ks.State == "free" {
			fmt.Fprintf(os.Stderr, "agentmutex: %s is already free; nothing to do\n", key)
			return ExitNotHeld
		}
		fmt.Fprintf(os.Stderr, "agentmutex: would force-release %s (state %s", key, ks.State)
		if ks.Holder != nil {
			fmt.Fprintf(os.Stderr, ", held by %s, expires %s", ks.Holder.Agent, ks.Holder.ExpiresAt.Format(time.RFC3339))
		}
		fmt.Fprintf(os.Stderr, ").\nDry run — pass --yes to execute.\n")
		return ExitOK
	}
	h, err := st.Release(key, "", true)
	if err != nil {
		return releaseErrCode(err)
	}
	if h != nil {
		fmt.Fprintf(os.Stderr, "agentmutex: force-released %s (was held by %s)\n", key, h.Agent)
	} else {
		fmt.Fprintf(os.Stderr, "agentmutex: force-released %s\n", key)
	}
	return ExitOK
}

// releaseErrCode maps store errors to the documented exit codes.
func releaseErrCode(err error) int {
	var nh *mutex.NotHolderError
	switch {
	case errors.Is(err, mutex.ErrNotHeld):
		fmt.Fprintf(os.Stderr, "agentmutex: %v\n", err)
		return ExitNotHeld
	case errors.As(err, &nh):
		fmt.Fprintf(os.Stderr, "agentmutex: %v\n", err)
		return ExitNotHolder
	default:
		return fail(err)
	}
}
