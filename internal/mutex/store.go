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
// rename, so guard-free readers (status, list) always see a complete
// document.
//
// Waiter entries are named "<zero-padded-unixnano>-<token>.json", so
// lexicographic order is arrival order. A waiter proves it is alive by
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
type Waiter struct {
	Token      string    `json:"token,omitempty"`
	Agent      string    `json:"agent"`
	PID        int       `json:"pid"`
	Host       string    `json:"host"`
	Reason     string    `json:"reason,omitempty"`
	EnqueuedAt time.Time `json:"enqueued_at"`

	// Computed on read, not stored.
	Fresh    bool      `json:"fresh"`
	LastSeen time.Time `json:"last_seen,omitzero"`
}

// KeyStatus is a read-only snapshot of one key.
type KeyStatus struct {
	Key     string   `json:"key"`
	State   string   `json:"state"` // "held", "expired" or "free"
	Holder  *Holder  `json:"holder,omitempty"`
	Waiters []Waiter `json:"waiters"`
}

// AcquireOpts parameterizes TryAcquire.
type AcquireOpts struct {
	TTL    time.Duration
	Agent  string
	PID    int
	Host   string
	Reason string
	// Enqueued indicates the token has a queue entry that should be
	// removed if the acquire succeeds.
	Enqueued bool
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

	now func() time.Time
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
	if err := os.MkdirAll(filepath.Join(root, "locks"), 0o755); err != nil {
		return nil, fmt.Errorf("cannot create state directory: %w", err)
	}
	return &Store{
		Root:             root,
		WaiterStaleAfter: DefaultWaiterStaleAfter,
		now:              time.Now,
	}, nil
}

func (s *Store) locksDir() string         { return filepath.Join(s.Root, "locks") }
func (s *Store) keyDir(key string) string { return filepath.Join(s.locksDir(), EncodeKey(key)) }

func (s *Store) ensureKeyDir(key string) (string, error) {
	dir := s.keyDir(key)
	if err := os.MkdirAll(filepath.Join(dir, queueDir), 0o755); err != nil {
		return "", fmt.Errorf("cannot create lock directory: %w", err)
	}
	return dir, nil
}

// withGuard runs fn while holding the per-key mutation guard.
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
	tmp := filepath.Join(dir, fmt.Sprintf(".holder-%d.tmp", os.Getpid()))
	if err := os.WriteFile(tmp, append(data, '\n'), 0o644); err != nil {
		return err
	}
	// Atomic replace: guard-free readers see either the old or the new
	// document, never a torn one.
	return os.Rename(tmp, filepath.Join(dir, holderFile))
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
		// FIFO fairness: only the fresh queue head may take it.
		head, err := s.queueHead(dir, now)
		if err != nil {
			return err
		}
		if head != nil && head.Token != token {
			res.Blocker = head
			return nil
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
			s.removeQueueEntry(dir, token)
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
	err := s.withGuard(key, func(dir string) error {
		h, err := readHolder(dir)
		var corrupt *CorruptError
		if errors.As(err, &corrupt) && force {
			return os.Remove(filepath.Join(dir, holderFile))
		}
		if err != nil {
			return err
		}
		if h == nil {
			return ErrNotHeld
		}
		if !force && h.Token != token {
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
	err := s.withGuard(key, func(dir string) error {
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
	if w.EnqueuedAt.IsZero() {
		w.EnqueuedAt = s.now()
	}
	name := fmt.Sprintf("%020d-%s.json", w.EnqueuedAt.UnixNano(), w.Token)
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
	if err := os.WriteFile(tmp, append(data, '\n'), 0o644); err != nil {
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
		w.Fresh = now.Sub(fi.ModTime()) <= s.WaiterStaleAfter
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
		var corrupt *CorruptError
		if !errors.As(err, &corrupt) {
			return nil, err
		}
		st.State = "corrupt"
	} else if h != nil {
		st.Holder = h
		if h.ExpiredAt(now) {
			st.State = "expired"
		} else {
			st.State = "held"
		}
	}
	ws, err := s.readQueue(dir, now)
	if err != nil {
		return nil, err
	}
	if ws != nil {
		st.Waiters = ws
	}
	return st, nil
}

// List returns snapshots of every key the store knows about, sorted by key.
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
}

// Prune removes expired leases and stale waiter entries. It never removes
// lock directories: an open guard file descriptor in another process must
// stay valid, and empty directories are free.
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
		err = s.withGuard(key, func(dir string) error {
			now := s.now()
			h, herr := readHolder(dir)
			if herr == nil && h != nil && h.ExpiredAt(now) {
				if os.Remove(filepath.Join(dir, holderFile)) == nil {
					res.ExpiredLeases = append(res.ExpiredLeases, key)
				}
			}
			qdir := filepath.Join(dir, queueDir)
			qents, qerr := os.ReadDir(qdir)
			if qerr != nil {
				return nil
			}
			for _, q := range qents {
				fi, ferr := q.Info()
				if ferr != nil {
					continue
				}
				if now.Sub(fi.ModTime()) <= s.WaiterStaleAfter {
					continue
				}
				// Re-stat immediately before removal: a live waiter may
				// have heartbeated between ReadDir and here (Heartbeat
				// deliberately takes no guard).
				path := filepath.Join(qdir, q.Name())
				if fi2, serr := os.Stat(path); serr == nil && now.Sub(fi2.ModTime()) > s.WaiterStaleAfter {
					if os.Remove(path) == nil {
						res.StaleWaiters++
					}
				}
			}
			return nil
		})
		if err != nil {
			return nil, err
		}
	}
	return res, nil
}

// removeQueueEntry deletes any queue entry for token. Caller holds the guard.
func (s *Store) removeQueueEntry(dir, token string) {
	qdir := filepath.Join(dir, queueDir)
	entries, err := os.ReadDir(qdir)
	if err != nil {
		return
	}
	suffix := "-" + token + ".json"
	for _, e := range entries {
		if strings.HasSuffix(e.Name(), suffix) {
			os.Remove(filepath.Join(qdir, e.Name()))
		}
	}
}
