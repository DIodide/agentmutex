// agentmutex — semantic mutexes for AI agents. No daemon, no server: just files.
package main

import (
	"fmt"
	"os"
)

// version is stamped at build time via -ldflags "-X main.version=...".
var version = "dev"

// Exit codes. Stable — agents branch on these.
const (
	ExitOK        = 0
	ExitError     = 1
	ExitUsage     = 2
	ExitHeld      = 10 // lock is held (or queued) and we did not wait
	ExitTimeout   = 11 // gave up waiting
	ExitNotHolder = 12 // token does not match current lease
	ExitNotHeld   = 13 // no lease exists for the key
	ExitLeaseLost = 14 // run: lease lost while the command was executing
)

const rootUsage = `agentmutex — semantic mutexes for AI agents. No daemon, no server: just files.

Usage:
  agentmutex <command> [flags] <arguments>

Commands:
  acquire <key>          Acquire a lease on a key (waits by default; prints token)
  release <key>          Release a lease you hold
  renew <key>            Extend a lease you hold
  run <key> -- <cmd...>  Acquire, run a command with auto-renew, then release
  status [<key>]         Show holder and waiters for a key (or all keys)
  list                   List all known locks
  wait <key>             Block until a key is free (does not acquire)
  force-release <key>    Forcibly clear a lock (human override; requires --yes)
  prune                  Remove expired leases and stale queue entries
  version                Print version

Keys are structured namespaces matching the resource they protect:
  deploy:staging     service:api:database     account:12345

Flags go before positional arguments. Run 'agentmutex <command> -h' for
command flags.

Environment:
  AGENTMUTEX_DIR     State directory (default ~/.agentmutex)
  AGENTMUTEX_TOKEN   Default token for release/renew
  AGENTMUTEX_AGENT   Default agent name

Exit codes:
  0 success   2 usage    10 lock held    11 timed out    12 not lock holder
  13 no lease exists     14 lease lost mid-run           1 other errors
  (run forwards the wrapped command's exit code; 127 command not found)
`

func main() {
	os.Exit(run(os.Args[1:]))
}

func run(args []string) int {
	if len(args) == 0 {
		fmt.Fprint(os.Stderr, rootUsage)
		return ExitUsage
	}
	cmd, rest := args[0], args[1:]
	switch cmd {
	case "acquire", "lock":
		return cmdAcquire(rest)
	case "release", "unlock":
		return cmdRelease(rest)
	case "renew":
		return cmdRenew(rest)
	case "run":
		return cmdRun(rest)
	case "status":
		return cmdStatus(rest)
	case "list", "ls":
		return cmdList(rest)
	case "wait":
		return cmdWait(rest)
	case "force-release":
		return cmdForceRelease(rest)
	case "prune":
		return cmdPrune(rest)
	case "version", "--version", "-v":
		fmt.Printf("agentmutex %s\n", version)
		return ExitOK
	case "help", "--help", "-h":
		fmt.Print(rootUsage)
		return ExitOK
	default:
		fmt.Fprintf(os.Stderr, "agentmutex: unknown command %q\n\n%s", cmd, rootUsage)
		return ExitUsage
	}
}
