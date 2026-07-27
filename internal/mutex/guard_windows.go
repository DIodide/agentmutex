//go:build windows

package mutex

import (
	"errors"
	"fmt"
	"os"
	"time"
)

// Windows has no flock(2), so the guard is an O_EXCL marker file. Guard
// critical sections last milliseconds; a marker older than guardStaleAfter
// can only belong to a crashed (or pathologically stalled) process.
//
// Correctness notes:
//   - Stale reclaim is RENAME-based: exactly one contender wins the rename,
//     so a raced reclaim can never delete a live marker that was created
//     after the staleness check (a plain Remove could).
//   - Release verifies marker identity before removing, so a holder whose
//     marker was reclaimed away cannot delete the reclaimer's live marker.
//   - The generous 60s staleness means a crash inside a guard section stalls
//     that key's mutations for up to a minute — acceptable, since agents are
//     inference-bound — in exchange for making a live-but-stalled mutator
//     losing its guard essentially impossible.
const (
	guardStaleAfter  = 60 * time.Second
	guardAcquireMax  = 90 * time.Second
	guardPollBackoff = 15 * time.Millisecond
)

func lockGuard(path string) (func(), error) {
	excl := path + ".excl"
	self := fmt.Sprintf("%d-%s\n", os.Getpid(), NewToken())
	deadline := time.Now().Add(guardAcquireMax)
	for {
		f, err := os.OpenFile(excl, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0o644)
		if err == nil {
			_, werr := f.WriteString(self)
			cerr := f.Close()
			if werr != nil || cerr != nil {
				os.Remove(excl)
				return nil, fmt.Errorf("write guard: %w", errors.Join(werr, cerr))
			}
			release := func() {
				// Remove only while the marker is still ours: after a
				// stale reclaim, removing by path would delete the
				// reclaimer's live marker.
				if cur, rerr := os.ReadFile(excl); rerr == nil && string(cur) == self {
					os.Remove(excl)
				}
			}
			return release, nil
		}
		// IsPermission covers Windows' transient ACCESS_DENIED while a
		// just-removed marker is delete-pending; treat it as contention.
		if !os.IsExist(err) && !os.IsPermission(err) {
			return nil, fmt.Errorf("open guard: %w", err)
		}
		if fi, serr := os.Stat(excl); serr == nil && time.Since(fi.ModTime()) > guardStaleAfter {
			// Atomic reclaim: exactly one contender wins this rename; the
			// losers see ENOENT and retry the O_EXCL create as usual.
			graveyard := fmt.Sprintf("%s.stale-%s", excl, NewToken())
			if os.Rename(excl, graveyard) == nil {
				os.Remove(graveyard)
			}
			continue
		}
		if time.Now().After(deadline) {
			return nil, fmt.Errorf("timed out waiting for store guard %s", excl)
		}
		time.Sleep(guardPollBackoff)
	}
}
