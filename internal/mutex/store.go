// Package mutex implements the on-disk lease store for agentmutex.
//
// Layout under the state root (default ~/.agentmutex):
//
//	locks/<encoded-key>/guard        per-key mutation guard (flock target)
//	locks/<encoded-key>/holder.json  the current lease, if any
//	locks/<encoded-key>/queue/       FIFO waiter entries
//
// Every mutation (acquire, release, renew, prune) runs while holding the
// per-key guard, so read-modify-write sequences are serialized across
// processes without any daemon. holder.json is replaced via temp-file +
// rename (mode 0600, under a 0700 tree), so guard-free readers (status,
// list) always see a complete document and the lease token is not
// world-readable.
//
// Waiter entries are named "<zero-padded-unixnano>-<id>.json", so
// lexicographic order is arrival order. The id is a public queue identity,
// distinct from the private lease token. A waiter proves it is alive by
// touching its entry's mtime on every poll; entries whose mtime is older
// than WaiterStaleAfter belong to dead waiters and are skipped.
package mutex

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"
)

// Defaults. Deploy-sized critical sections run minutes, and waiting agents
// are inference-bound (they think for seconds to minutes between actions),
// so generous TTLs and second-scale polling are free.
const (
	DefaultTTL              = 15 * time.Minute
	DefaultPollInterval     = 1 * time.Second
	DefaultWaiterStaleAfter = 30 * time.Second
	// DefaultWaiterPIDGrace bounds how long a *live* same-host waiter may go
	// without a heartbeat before it is treated as dead anyway. It is generous
	// because a deploy that saturates the box can starve a co-located queued
	// agent of CPU for a while; PID-liveness keeps its FIFO slot until then.
	DefaultWaiterPIDGrace = 5 * time.Minute

	holderFile = "holder.json"
	guardFile  = "guard"
	queueDir   = "queue"
)

// ErrNotHeld is returned when releasing or renewing a key that has no lease.
var ErrNotHeld = errors.New("no lease held for this key")

// NotHolderError is returned when the presented token does not match the
// current lease. It carries the actual holder for diagnostics.
type NotHolderError struct{ Holder *Holder }

func (e *NotHolderError) Error() string {
	return fmt.Sprintf("token does not match current lease (held by %q since %s)",
		e.Holder.Agent, e.Holder.AcquiredAt.Format(time.RFC3339))
}

// CorruptError is returned when a state file exists but cannot be parsed.
// The store never steals a lease it cannot read; use force-release to clear.
type CorruptError struct {
	Path string
	Err  error
}

func (e *CorruptError) Error() string {
	return fmt.Sprintf("corrupt state file %s: %v (use 'agentmutex force-release' to clear)", e.Path, e.Err)
}

// Holder is a lease on a semantic key. Token is omitempty so display paths
// can redact it (the CLI blanks tokens in status/list output).
type Holder struct {
	Key        string     `json:"key"`
	Token      string     `json:"token,omitempty"`
	Agent      string     `json:"agent"`
	PID        int        `json:"pid"`
	Host       string     `json:"host"`
	Reason     string     `json:"reason,omitempty"`
	AcquiredAt time.Time  `json:"acquired_at"`
	ExpiresAt  time.Time  `json:"expires_at"`
	RenewedAt  *time.Time `json:"renewed_at,omitempty"`
}

// ExpiredAt reports whether the lease is expired as of now.
func (h *Holder) ExpiredAt(now time.Time) bool { return !now.Before(h.ExpiresAt) }

// Waiter is a queued acquire attempt.
//
// ID is a public queue identity, deliberately distinct from the private
// lease token: a waiter's entry (name and body) is visible to any local
// process, so putting the lease token here would hand out the credential
// that release/renew require. FIFO ordering keys on ID, never on the token.
type Waiter struct {
	ID         string    `json:"id"`
	Agent      string    `json:"agent"`
	PID        int       `json:"pid"`
	Host       string    `json:"host"`
	Reason     string    `json:"reason,omitempty"`
	EnqueuedAt time.Time `json:"enqueued_at"`

	// Fresh and LastSeen are computed from the entry's mtime on read; the
	// on-disk "fresh" field is written true and ignored when reading.
	Fresh    bool      `json:"fresh"`
	LastSeen time.Time `json:"last_seen,omitzero"`
}

// KeyStatus is a read-only snapshot of one key.
type KeyStatus struct {
	Key string `json:"key"`
	// State is "held", "expired", "free", "corrupt" (holder.json is
	// unparseable) or "unreadable" (an I/O error prevented reading it).
	State   string   `json:"state"`
	Holder  *Holder  `json:"holder,omitempty"`
	Waiters []Waiter `json:"waiters"`
	// Error carries the underlying message when State is "unreadable".
	Error string `json:"error,omitempty"`
}

// AcquireOpts parameterizes TryAcquire.
type AcquireOpts struct {
	TTL    time.Duration
	Agent  string
	PID    int
	Host   string
	Reason string
	// WaiterID is this caller's queue identity (see Waiter.ID). When set,
	// FIFO fairness lets us take the lease only if we are the fresh queue
	// head, and our queue entry is removed on success.
	WaiterID string
	// Enqueued indicates we have a queue entry that should be removed if
	// the acquire succeeds.
	Enqueued bool
	// Reclaim skips the FIFO queue-head check: use it to re-establish a
	// lease you were actively holding (e.g. `run` re-taking a key that was
	// force-released or pruned out from under an in-flight command). It
	// still refuses a key currently held by a different live holder.
	Reclaim bool
}

// AcquireResult reports one TryAcquire attempt.
type AcquireResult struct {
	Acquired bool
	// Holder is our new lease when Acquired, otherwise the current
	// unexpired holder blocking us (nil if the key is free).
	Holder *Holder
	// Blocker is the fresh queue head ahead of us, when the key is free
	// (or expired) but FIFO order says it is not our turn.
	Blocker *Waiter
}

// Store is an on-disk lease store. Safe for concurrent use by any number of
// processes on the same machine. (Not across hosts: the guard is flock(2)
// and waiter liveness compares this host's clock against entry mtimes.)
type Store struct {
	Root             string
	WaiterStaleAfter time.Duration
	WaiterPIDGrace   time.Duration

	now  func() time.Time
	host string
}

// Open opens (creating if needed) the store at root. An empty root falls
// back to $AGENTMUTEX_DIR, then ~/.agentmutex.
func Open(root string) (*Store, error) {
	if root == "" {
		root = os.Getenv("AGENTMUTEX_DIR")
	}
	if root == "" {
		home, err := os.UserHomeDir()
		if err != nil {
			return nil, fmt.Errorf("cannot determine home directory: %w", err)
		}
		root = filepath.Join(home, ".agentmutex")
	}
	// 0700 throughout: the lock inventory (decodable key names) and the
	// tokens in holder documents are per-user and should not be readable by
	// other local users.
	if err := os.MkdirAll(filepath.Join(root, "locks"), 0o700); err != nil {
		return nil, fmt.Errorf("cannot create state directory: %w", err)
	}
	host, _ := os.Hostname()
	return &Store{
		Root:             root,
		WaiterStaleAfter: DefaultWaiterStaleAfter,
		WaiterPIDGrace:   DefaultWaiterPIDGrace,
		now:              time.Now,
		host:             host,
	}, nil
}

// waiterFresh reports whether a waiter is still a live contender. On this
// host, liveness is authoritative via the process id: a starved-but-alive
// agent keeps its FIFO slot (up to WaiterPIDGrace, bounding PID-reuse), and a
// crashed agent is dropped immediately rather than lingering for the whole
// staleness window. Waiters from another host fall back to the mtime
// heartbeat, since we can't probe a remote PID.
func (s *Store) waiterFresh(host string, pid int, mtime, now time.Time) bool {
	grace := s.WaiterPIDGrace
	if grace <= 0 {
		grace = DefaultWaiterPIDGrace
	}
	if host != "" && s.host != "" && host == s.host && pid > 0 {
		return processAlive(pid) && now.Sub(mtime) <= grace
	}
	return now.Sub(mtime) <= s.WaiterStaleAfter
}

func (s *Store) locksDir() string         { return filepath.Join(s.Root, "locks") }
func (s *Store) keyDir(key string) string { return filepath.Join(s.locksDir(), EncodeKey(key)) }

func (s *Store) ensureKeyDir(key string) (string, error) {
	dir := s.keyDir(key)
	// 0700: lease state (including tokens in holder.json) is per-user; keep
	// other UIDs out. Same-UID processes are a trust boundary either way.
	if err := os.MkdirAll(filepath.Join(dir, queueDir), 0o700); err != nil {
		return "", fmt.Errorf("cannot create lock directory: %w", err)
	}
	return dir, nil
}

// withGuard runs fn while holding the per-key mutation guard, creating the
// key directory if needed (used by acquire/enqueue/prune).
func (s *Store) withGuard(key string, fn func(dir string) error) error {
	dir, err := s.ensureKeyDir(key)
	if err != nil {
		return err
	}
	release, err := lockGuard(filepath.Join(dir, guardFile))
	if err != nil {
		return err
	}
	defer release()
	return fn(dir)
}

// withExistingGuard is like withGuard but does NOT create the key directory:
// if it does not exist, notThere is returned instead. Release and Renew use
// this so a mistaken `release nonexistent:key` cannot litter the store with
// empty phantom lock directories that List would then show forever.
func (s *Store) withExistingGuard(key string, notThere error, fn func(dir string) error) error {
	dir := s.keyDir(key)
	if _, err := os.Stat(dir); errors.Is(err, os.ErrNotExist) {
		return notThere
	}
	release, err := lockGuard(filepath.Join(dir, guardFile))
	if err != nil {
		return err
	}
	defer release()
	return fn(dir)
}

func readHolder(dir string) (*Holder, error) {
	path := filepath.Join(dir, holderFile)
	data, err := os.ReadFile(path)
	if errors.Is(err, os.ErrNotExist) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	var h Holder
	if err := json.Unmarshal(data, &h); err != nil {
		return nil, &CorruptError{Path: path, Err: err}
	}
	return &h, nil
}

func writeHolder(dir string, h *Holder) error {
	data, err := json.MarshalIndent(h, "", "  ")
	if err != nil {
		return err
	}
	// os.CreateTemp makes an O_EXCL file with a random name at mode 0600:
	// no predictable path to pre-plant as a symlink, and the token-bearing
	// document is never world-readable. The rename is atomic, so guard-free
	// readers see either the old or the new document, never a torn one.
	f, err := os.CreateTemp(dir, ".holder-*.tmp")
	if err != nil {
		return err
	}
	tmp := f.Name()
	if _, err := f.Write(append(data, '\n')); err != nil {
		f.Close()
		os.Remove(tmp)
		return err
	}
	if err := f.Close(); err != nil {
		os.Remove(tmp)
		return err
	}
	if err := os.Rename(tmp, filepath.Join(dir, holderFile)); err != nil {
		os.Remove(tmp)
		return err
	}
	return nil
}

// TryAcquire makes one attempt to take the lease. It never blocks (beyond
// the millisecond-scale guard). Pessimistic waiting is the caller's poll
// loop around this.
func (s *Store) TryAcquire(key, token string, o AcquireOpts) (*AcquireResult, error) {
	if err := ValidateKey(key); err != nil {
		return nil, err
	}
	if o.TTL <= 0 {
		o.TTL = DefaultTTL
	}
	res := &AcquireResult{}
	err := s.withGuard(key, func(dir string) error {
		now := s.now()
		h, err := readHolder(dir)
		if err != nil {
			return err
		}
		if h != nil && !h.ExpiredAt(now) {
			res.Holder = h
			return nil // held by someone (possibly us — re-entry is not supported)
		}
		// Key is free (or the lease expired and can be displaced).
		// FIFO fairness: only the fresh queue head may take it. Identity is
		// the public WaiterID, never the private lease token. Reclaim bypasses
		// this — an active holder re-establishing protection jumps the queue.
		if !o.Reclaim {
			head, err := s.queueHead(dir, now)
			if err != nil {
				return err
			}
			if head != nil && head.ID != o.WaiterID {
				res.Blocker = head
				return nil
			}
		}
		nh := &Holder{
			Key:        key,
			Token:      token,
			Agent:      o.Agent,
			PID:        o.PID,
			Host:       o.Host,
			Reason:     o.Reason,
			AcquiredAt: now,
			ExpiresAt:  now.Add(o.TTL),
		}
		if err := writeHolder(dir, nh); err != nil {
			return err
		}
		if o.Enqueued {
			s.removeQueueEntry(dir, o.WaiterID)
		}
		res.Acquired = true
		res.Holder = nh
		return nil
	})
	if err != nil {
		return nil, err
	}
	return res, nil
}

// Release drops the lease for key. Without force, token must match the
// current lease. Returns the released holder (nil if force-releasing
// corrupt state).
func (s *Store) Release(key, token string, force bool) (*Holder, error) {
	if err := ValidateKey(key); err != nil {
		return nil, err
	}
	var released *Holder
	err := s.withExistingGuard(key, ErrNotHeld, func(dir string) error {
		if force {
			// The human override must always win: never block on being able
			// to *read* the document we are about to delete. RemoveAll also
			// clears a holder.json that was replaced by a directory.
			holder := filepath.Join(dir, holderFile)
			if _, statErr := os.Stat(holder); errors.Is(statErr, os.ErrNotExist) {
				return ErrNotHeld
			}
			if h, rerr := readHolder(dir); rerr == nil {
				released = h // best-effort: report who we displaced
			}
			return os.RemoveAll(holder)
		}
		h, err := readHolder(dir)
		if err != nil {
			return err
		}
		if h == nil {
			return ErrNotHeld
		}
		if h.Token != token {
			return &NotHolderError{Holder: h}
		}
		released = h
		return os.Remove(filepath.Join(dir, holderFile))
	})
	if err != nil {
		return nil, err
	}
	return released, nil
}

// Renew extends the lease for key by ttl from now. Token must match. A
// lease that expired but has not been displaced yet is revived — better to
// keep a live agent's lease than to invite a collision.
func (s *Store) Renew(key, token string, ttl time.Duration) (*Holder, error) {
	if err := ValidateKey(key); err != nil {
		return nil, err
	}
	if ttl <= 0 {
		ttl = DefaultTTL
	}
	var renewed *Holder
	err := s.withExistingGuard(key, ErrNotHeld, func(dir string) error {
		h, err := readHolder(dir)
		if err != nil {
			return err
		}
		if h == nil {
			return ErrNotHeld
		}
		if h.Token != token {
			return &NotHolderError{Holder: h}
		}
		now := s.now()
		h.ExpiresAt = now.Add(ttl)
		h.RenewedAt = &now
		if err := writeHolder(dir, h); err != nil {
			return err
		}
		renewed = h
		return nil
	})
	if err != nil {
		return nil, err
	}
	return renewed, nil
}

// Enqueue registers w as a waiter for key and returns the entry path, which
// the caller uses for Heartbeat and Dequeue. Creating a uniquely-named file
// is atomic, so no guard is needed.
func (s *Store) Enqueue(key string, w Waiter) (string, error) {
	if err := ValidateKey(key); err != nil {
		return "", err
	}
	dir, err := s.ensureKeyDir(key)
	if err != nil {
		return "", err
	}
	if w.ID == "" {
		return "", fmt.Errorf("waiter ID must be set")
	}
	if w.EnqueuedAt.IsZero() {
		w.EnqueuedAt = s.now()
	}
	name := fmt.Sprintf("%020d-%s.json", w.EnqueuedAt.UnixNano(), w.ID)
	path := filepath.Join(dir, queueDir, name)
	if err := writeWaiter(path, w); err != nil {
		return "", err
	}
	return path, nil
}

// writeWaiter writes a queue entry atomically (temp + rename) so guarded
// readers never observe a torn document and mistakenly skip a live waiter.
// The temp name lacks the ".json" suffix, so readQueue ignores it.
func writeWaiter(path string, w Waiter) error {
	w.Fresh = true
	data, err := json.MarshalIndent(w, "", "  ")
	if err != nil {
		return err
	}
	tmp := path + ".tmp"
	if err := os.WriteFile(tmp, append(data, '\n'), 0o600); err != nil {
		return err
	}
	return os.Rename(tmp, path)
}

// Heartbeat proves the waiter behind entryPath is still alive by touching
// its mtime. If the entry vanished (e.g. an aggressive prune), it is
// re-created so the waiter keeps its queue position.
func (s *Store) Heartbeat(entryPath string, w Waiter) error {
	now := s.now()
	err := os.Chtimes(entryPath, now, now)
	if errors.Is(err, os.ErrNotExist) {
		return writeWaiter(entryPath, w)
	}
	return err
}

// Dequeue removes a waiter entry. Missing entries are fine (the acquire
// that succeeded already removed it).
func (s *Store) Dequeue(entryPath string) error {
	err := os.Remove(entryPath)
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	return err
}

// queueHead returns the oldest fresh waiter, or nil. Caller holds the guard.
func (s *Store) queueHead(dir string, now time.Time) (*Waiter, error) {
	ws, err := s.readQueue(dir, now)
	if err != nil {
		return nil, err
	}
	for i := range ws {
		if ws[i].Fresh {
			return &ws[i], nil
		}
	}
	return nil, nil
}

// readQueue returns all waiter entries in FIFO order with computed
// freshness. Unreadable or unparseable entries are skipped: a waiter that
// cannot maintain a valid entry must not block the queue.
func (s *Store) readQueue(dir string, now time.Time) ([]Waiter, error) {
	qdir := filepath.Join(dir, queueDir)
	entries, err := os.ReadDir(qdir)
	if errors.Is(err, os.ErrNotExist) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	names := make([]string, 0, len(entries))
	for _, e := range entries {
		if !e.IsDir() && strings.HasSuffix(e.Name(), ".json") {
			names = append(names, e.Name())
		}
	}
	sort.Strings(names) // zero-padded unixnano prefix => arrival order
	var ws []Waiter
	for _, name := range names {
		path := filepath.Join(qdir, name)
		fi, err := os.Stat(path)
		if err != nil {
			continue // raced with dequeue
		}
		data, err := os.ReadFile(path)
		if err != nil {
			continue
		}
		var w Waiter
		if err := json.Unmarshal(data, &w); err != nil {
			continue
		}
		w.LastSeen = fi.ModTime()
		w.Fresh = s.waiterFresh(w.Host, w.PID, fi.ModTime(), now)
		ws = append(ws, w)
	}
	return ws, nil
}

// Status returns a read-only snapshot of key. It takes no guard: holder.json
// is replaced atomically, so the read is consistent.
func (s *Store) Status(key string) (*KeyStatus, error) {
	if err := ValidateKey(key); err != nil {
		return nil, err
	}
	return s.statusFromDir(key, s.keyDir(key))
}

func (s *Store) statusFromDir(key, dir string) (*KeyStatus, error) {
	now := s.now()
	st := &KeyStatus{Key: key, State: "free", Waiters: []Waiter{}}
	h, err := readHolder(dir)
	if err != nil {
		// Both failure modes carry their diagnostic in Error so status/list
		// can show what is wrong and how to clear it.
		var corrupt *CorruptError
		if errors.As(err, &corrupt) {
			st.State = "corrupt"
		} else {
			// An I/O error (permissions, holder.json is a directory, …).
			st.State = "unreadable"
		}
		st.Error = err.Error()
	} else if h != nil {
		st.Holder = h
		if h.ExpiredAt(now) {
			st.State = "expired"
		} else {
			st.State = "held"
		}
	}
	ws, qerr := s.readQueue(dir, now)
	if qerr != nil {
		// A failed queue read must not discard an already-read holder (which
		// would mislabel a held key as unreadable with no holder). Keep what
		// we have and note the queue problem.
		if st.State == "free" {
			st.State = "unreadable"
		}
		if st.Error == "" {
			st.Error = qerr.Error()
		}
	}
	if ws != nil {
		st.Waiters = ws
	}
	return st, nil
}

// List returns snapshots of every key the store knows about, sorted by key.
// Keys whose state cannot be read are still reported (State "unreadable")
// rather than dropped, so a wedged lock never looks like it does not exist.
func (s *Store) List() ([]KeyStatus, error) {
	entries, err := os.ReadDir(s.locksDir())
	if err != nil {
		return nil, err
	}
	var out []KeyStatus
	for _, e := range entries {
		if !e.IsDir() {
			continue
		}
		key, err := DecodeKey(e.Name())
		if err != nil {
			continue // not one of ours
		}
		st, err := s.statusFromDir(key, filepath.Join(s.locksDir(), e.Name()))
		if err != nil {
			out = append(out, KeyStatus{Key: key, State: "unreadable", Waiters: []Waiter{}, Error: err.Error()})
			continue
		}
		out = append(out, *st)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Key < out[j].Key })
	return out, nil
}

// PruneResult reports what Prune removed.
type PruneResult struct {
	ExpiredLeases []string `json:"expired_leases"`
	StaleWaiters  int      `json:"stale_waiters"`
	// Errors holds per-key failures; Prune keeps going past them so one
	// wedged key cannot hide cleanup of all the others.
	Errors []string `json:"errors,omitempty"`
}

// Prune removes expired leases, stale waiter entries and orphaned temp
// files. It never removes lock directories: an open guard file descriptor in
// another process must stay valid, and empty directories are free.
func (s *Store) Prune() (*PruneResult, error) {
	entries, err := os.ReadDir(s.locksDir())
	if err != nil {
		return nil, err
	}
	res := &PruneResult{ExpiredLeases: []string{}}
	for _, e := range entries {
		if !e.IsDir() {
			continue
		}
		key, err := DecodeKey(e.Name())
		if err != nil {
			continue
		}
		// Guard the actual on-disk directory by its own name; re-encoding
		// the decoded key could point at a *different* directory for any
		// non-canonical name.
		dir := filepath.Join(s.locksDir(), e.Name())
		if perr := s.pruneKey(dir, key, res); perr != nil {
			res.Errors = append(res.Errors, fmt.Sprintf("%s: %v", key, perr))
		}
	}
	return res, nil
}

func (s *Store) pruneKey(dir, key string, res *PruneResult) error {
	release, err := lockGuard(filepath.Join(dir, guardFile))
	if err != nil {
		return err
	}
	defer release()

	now := s.now()
	if h, herr := readHolder(dir); herr == nil && h != nil && h.ExpiredAt(now) {
		if os.Remove(filepath.Join(dir, holderFile)) == nil {
			res.ExpiredLeases = append(res.ExpiredLeases, key)
		}
	}
	// Sweep orphaned holder temp files (.holder-*.tmp) left by an interrupted
	// writeHolder. writeHolder only runs under this same guard, so any that
	// still exist here belong to a crashed prior op and are safe to remove.
	if dents, derr := os.ReadDir(dir); derr == nil {
		for _, d := range dents {
			n := d.Name()
			if !d.IsDir() && strings.HasPrefix(n, ".holder-") && strings.HasSuffix(n, ".tmp") {
				os.Remove(filepath.Join(dir, n))
			}
		}
	}
	qdir := filepath.Join(dir, queueDir)
	qents, qerr := os.ReadDir(qdir)
	if qerr != nil {
		return nil
	}
	for _, q := range qents {
		name := q.Name()
		path := filepath.Join(qdir, name)
		fi, ferr := q.Info()
		if ferr != nil {
			continue
		}
		// Sweep leftover temp files from an interrupted writeWaiter — but
		// only once they are older than the staleness window. writeWaiter is
		// guard-free (Enqueue/Heartbeat), so a *fresh* .tmp may be an
		// in-flight write we must not delete out from under its rename.
		if strings.HasSuffix(name, ".tmp") {
			if now.Sub(fi.ModTime()) > s.WaiterStaleAfter {
				os.Remove(path)
			}
			continue
		}
		if q.IsDir() || !strings.HasSuffix(name, ".json") {
			continue // only real waiter entries count
		}
		// A waiter is prunable only if it is not a live contender — i.e. its
		// same-host process is gone (or it is a remote waiter past the mtime
		// window). A CPU-starved-but-alive local agent is kept.
		host, pid := waiterIdentity(path)
		if s.waiterFresh(host, pid, fi.ModTime(), now) {
			continue
		}
		// Re-stat immediately before removal: a live waiter may have
		// heartbeated between ReadDir and here (Heartbeat takes no guard).
		if fi2, serr := os.Stat(path); serr == nil && !s.waiterFresh(host, pid, fi2.ModTime(), now) {
			if os.Remove(path) == nil {
				res.StaleWaiters++
			}
		}
	}
	return nil
}

// waiterIdentity reads just the host/pid from a queue entry (best effort).
func waiterIdentity(path string) (host string, pid int) {
	data, err := os.ReadFile(path)
	if err != nil {
		return "", 0
	}
	var w Waiter
	if json.Unmarshal(data, &w) != nil {
		return "", 0
	}
	return w.Host, w.PID
}

// removeQueueEntry deletes any queue entry for the given waiter ID. Caller
// holds the guard.
func (s *Store) removeQueueEntry(dir, id string) {
	qdir := filepath.Join(dir, queueDir)
	entries, err := os.ReadDir(qdir)
	if err != nil {
		return
	}
	suffix := "-" + id + ".json"
	for _, e := range entries {
		if strings.HasSuffix(e.Name(), suffix) {
			os.Remove(filepath.Join(qdir, e.Name()))
		}
	}
}
