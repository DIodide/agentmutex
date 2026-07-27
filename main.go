// agentmutex — semantic mutexes for AI agents. No daemon, no server: just files.
package main

import (
	"fmt"
	"os"
	"runtime/debug"
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

Aliases: lock=acquire, unlock=release, ls=list.

Keys are structured namespaces matching the resource they protect:
  deploy:staging     service:api:database     account:12345

Flags go before positional arguments. Run 'agentmutex <command> -h' (or
'agentmutex help <command>') for command flags.

Environment:
  AGENTMUTEX_DIR     State directory (default ~/.agentmutex)
  AGENTMUTEX_TOKEN   Default token for release/renew
  AGENTMUTEX_AGENT   Default agent name
  Inside 'run', the child also sees AGENTMUTEX_LEASE_KEY and AGENTMUTEX_TOKEN
  for the held lease (enabling early release / renew from the command).

Exit codes:
  0 success   2 usage    10 lock held    11 timed out    12 not lock holder
  13 no lease exists     14 lease lost mid-run           1 other errors
  status --exit-code: 0 held, 3 free, 4 expired, 5 corrupt/unreadable
  run forwards the wrapped command's exit code; 127 command not found;
  a signal-interrupted wait exits 128+signum (130 INT, 143 TERM, 129 HUP)
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
	// `help <command>` and `<command> help` both show that command's help.
	if (cmd == "help" || cmd == "--help" || cmd == "-h") && len(rest) == 1 {
		return dispatch(rest[0], []string{"-h"})
	}
	if len(rest) == 1 && rest[0] == "help" {
		return dispatch(cmd, []string{"-h"})
	}
	return dispatch(cmd, rest)
}

func dispatch(cmd string, rest []string) int {
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
		fmt.Printf("agentmutex %s\n", buildVersion())
		return ExitOK
	case "help", "--help", "-h":
		fmt.Print(rootUsage)
		return ExitOK
	default:
		fmt.Fprintf(os.Stderr, "agentmutex: unknown command %q\n\n%s", cmd, rootUsage)
		return ExitUsage
	}
}

// buildVersion returns the ldflags-stamped version, falling back to the
// module version embedded by `go install` (which does not set ldflags).
func buildVersion() string {
	if version != "dev" {
		return version
	}
	if info, ok := debug.ReadBuildInfo(); ok {
		if v := info.Main.Version; v != "" && v != "(devel)" {
			return v
		}
	}
	return version
}
