package main

import (
	"bytes"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"os"
	"os/user"
	"strings"
	"syscall"
	"time"
	"unicode"

	"github.com/DIodide/agentmutex/internal/mutex"
)

// newFlagSet builds a flag set whose usage output matches the house style.
func newFlagSet(name, oneLiner, usageLine string) *flag.FlagSet {
	fs := flag.NewFlagSet(name, flag.ContinueOnError)
	fs.Usage = func() {
		fmt.Fprintf(fs.Output(), "%s\n\nUsage:\n  agentmutex %s\n\nFlags:\n", oneLiner, usageLine)
		fs.PrintDefaults()
	}
	return fs
}

// dirFlag registers the shared --dir flag.
func dirFlag(fs *flag.FlagSet) *string {
	return fs.String("dir", "", "state directory (default $AGENTMUTEX_DIR or ~/.agentmutex)")
}

// parseFlags parses args, routing -h/--help output to stdout with exit 0 and
// flag errors to stderr with exit 2. The second return is true when parsing
// already resolved the command (help shown or usage error).
func parseFlags(fs *flag.FlagSet, args []string) (int, bool) {
	var buf bytes.Buffer
	fs.SetOutput(&buf)
	err := fs.Parse(args)
	if err == nil {
		fs.SetOutput(os.Stderr) // subsequent fs.Usage() calls are error paths
		return 0, false
	}
	if errors.Is(err, flag.ErrHelp) {
		os.Stdout.Write(buf.Bytes())
		return ExitOK, true
	}
	os.Stderr.Write(buf.Bytes())
	return ExitUsage, true
}

// validatePoll bounds the wait-loop poll interval: below 10ms it busy-spins
// the guard; above 10s a waiter's heartbeats can't reliably outpace the 30s
// staleness window and it would be treated as dead.
func validatePoll(poll time.Duration) error {
	if poll < 10*time.Millisecond || poll > 10*time.Second {
		return fmt.Errorf("--poll must be between 10ms and 10s, got %s", poll)
	}
	return nil
}

// validateTTL rejects nonsensical lease durations up front instead of
// silently substituting the default.
func validateTTL(ttl time.Duration) error {
	if ttl <= 0 {
		return fmt.Errorf("--ttl must be positive, got %s", ttl)
	}
	return nil
}

// openStore opens the lease store rooted at --dir / $AGENTMUTEX_DIR / ~/.agentmutex.
func openStore(dir string) (*mutex.Store, error) {
	return mutex.Open(dir)
}

// oneKeyArg extracts and validates exactly one positional <key> argument.
// Invalid keys are usage errors (exit 2), not runtime errors.
func oneKeyArg(fs *flag.FlagSet) (string, error) {
	if fs.NArg() != 1 {
		return "", fmt.Errorf("expected exactly one <key> argument, got %d", fs.NArg())
	}
	key := fs.Arg(0)
	if err := mutex.ValidateKey(key); err != nil {
		return "", err
	}
	return key, nil
}

// redactTokens strips the holder's lease token from a status snapshot before
// display. The token authorizes release/renew; status must inform other
// agents, not hand them the holder's credential to misuse. (Waiter entries
// carry only a public ID, never the token.)
func redactTokens(ks *mutex.KeyStatus) *mutex.KeyStatus {
	if ks.Holder != nil {
		h := *ks.Holder
		h.Token = ""
		ks.Holder = &h
	}
	return ks
}

// defaultAgent names this agent: $AGENTMUTEX_AGENT, else user@host.
func defaultAgent() string {
	if a := sanitizeMeta(os.Getenv("AGENTMUTEX_AGENT")); a != "" {
		return a
	}
	name := "unknown"
	if u, err := user.Current(); err == nil && u.Username != "" {
		name = u.Username
	}
	host, err := os.Hostname()
	if err != nil || host == "" {
		host = "localhost"
	}
	return name + "@" + host
}

// errNoToken is returned by tokenOrEnv when no token is available; callers
// map it to the usage exit code (a missing token is a caller mistake).
var errNoToken = errors.New("no token: pass --token or set AGENTMUTEX_TOKEN")

// tokenOrEnv resolves the lease token: --token flag, else $AGENTMUTEX_TOKEN.
func tokenOrEnv(token string) (string, error) {
	if token != "" {
		return token, nil
	}
	if t := os.Getenv("AGENTMUTEX_TOKEN"); t != "" {
		return t, nil
	}
	return "", errNoToken
}

// signalExit maps an interrupting signal to the conventional 128+N exit code
// (130 for SIGINT, 143 for SIGTERM), falling back to 130.
func signalExit(s os.Signal) int {
	if ssig, ok := s.(syscall.Signal); ok {
		return 128 + int(ssig)
	}
	return 130
}

// validateTimeout rejects a negative wait timeout, which would otherwise be
// silently indistinguishable from 0 ("wait forever").
func validateTimeout(timeout time.Duration) error {
	if timeout < 0 {
		return fmt.Errorf("--timeout must be >= 0 (0 means wait forever), got %s", timeout)
	}
	return nil
}

// sanitizeMeta cleans a user-supplied agent/reason string so it cannot forge
// status/list lines or drive the terminal: C0 and C1 control characters
// (newlines, ANSI/CSI escapes, NUL) and Unicode format/bidi-override
// characters (which can visually reorder text) are replaced with a space.
// Length is bounded by rune count, so truncation never splits a multibyte
// character.
func sanitizeMeta(s string) string {
	const maxRunes = 200
	var b strings.Builder
	n := 0
	for _, r := range s {
		if n >= maxRunes {
			break
		}
		switch {
		case r == '\t':
			b.WriteRune(r)
		case unicode.IsControl(r): // C0 (incl. \n, ESC) and C1 (0x80–0x9F)
			b.WriteByte(' ')
		case unicode.Is(unicode.Cf, r) || unicode.Is(unicode.Bidi_Control, r):
			b.WriteByte(' ') // zero-width / bidi-override formatting chars
		default:
			b.WriteRune(r)
		}
		n++
	}
	return strings.TrimSpace(b.String())
}

func printJSON(v any) {
	data, err := json.MarshalIndent(v, "", "  ")
	if err != nil {
		fmt.Fprintf(os.Stderr, "agentmutex: %v\n", err)
		return
	}
	fmt.Println(string(data))
}

func fail(err error) int {
	fmt.Fprintf(os.Stderr, "agentmutex: %v\n", err)
	return ExitError
}

// humanDur renders a duration compactly ("4m12s", "1h3m").
func humanDur(d time.Duration) string {
	if d < 0 {
		d = -d
	}
	d = d.Round(time.Second)
	if d >= time.Hour {
		d = d.Round(time.Minute)
	}
	return d.String()
}

// describeBlock explains why an acquire attempt did not succeed. id is our
// own waiter ID ("" if we are not queued yet).
func describeBlock(res *mutex.AcquireResult, st *mutex.Store, key, id string) string {
	if res.Holder != nil {
		now := time.Now()
		msg := fmt.Sprintf("held by %s", res.Holder.Agent)
		if res.Holder.Reason != "" {
			msg += fmt.Sprintf(" (%s)", res.Holder.Reason)
		}
		msg += fmt.Sprintf(", expires in %s", humanDur(res.Holder.ExpiresAt.Sub(now)))
		if n := waitersAhead(st, key, id); n > 0 {
			msg += fmt.Sprintf(", %d waiter(s) ahead", n)
		}
		return msg
	}
	if res.Blocker != nil {
		n := waitersAhead(st, key, id)
		if n <= 0 {
			n = 1
		}
		return fmt.Sprintf("free, but %d waiter(s) are ahead in the queue", n)
	}
	return "unavailable"
}

// waitersAhead counts fresh waiters queued before our entry (all of them if
// we are not in the queue).
func waitersAhead(st *mutex.Store, key, id string) int {
	ks, err := st.Status(key)
	if err != nil {
		return 0
	}
	n := 0
	for _, w := range ks.Waiters {
		if !w.Fresh {
			continue
		}
		if id != "" && w.ID == id {
			return n
		}
		n++
	}
	return n
}
