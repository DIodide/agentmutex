//go:build unix

package mutex

import (
	"fmt"
	"os"
	"syscall"
	"time"
)

// guardAcquireMax bounds how long lockGuard will wait for the per-key mutation
// guard. Guard critical sections are single read-modify-write sequences
// lasting milliseconds, so reaching this bound means a peer is wedged (a bug,
// or stuck I/O such as a hung network filesystem). Failing loudly beats
// hanging every command forever with an uncancellable blocking flock.
const (
	guardAcquireMax  = 30 * time.Second
	guardPollBackoff = 5 * time.Millisecond
)

// lockGuard takes an exclusive flock(2) on path, creating it if needed. It
// polls with LOCK_NB rather than blocking in the kernel, so the wait is
// bounded (guardAcquireMax) instead of hanging indefinitely on a wedged
// holder. The kernel drops the lock automatically if the process dies, so a
// crashed CLI invocation can never wedge the store.
func lockGuard(path string) (func(), error) {
	f, err := os.OpenFile(path, os.O_CREATE|os.O_RDWR, 0o600)
	if err != nil {
		return nil, fmt.Errorf("open guard: %w", err)
	}
	deadline := time.Now().Add(guardAcquireMax)
	for {
		err := syscall.Flock(int(f.Fd()), syscall.LOCK_EX|syscall.LOCK_NB)
		if err == nil {
			return func() {
				// Closing the fd releases the flock; the explicit unlock is a
				// courtesy so the release is prompt even if the fd lingers.
				syscall.Flock(int(f.Fd()), syscall.LOCK_UN)
				f.Close()
			}, nil
		}
		if err != syscall.EWOULDBLOCK {
			f.Close()
			return nil, fmt.Errorf("flock guard: %w", err)
		}
		if time.Now().After(deadline) {
			f.Close()
			return nil, fmt.Errorf("timed out after %s waiting for store guard %s (a peer process may be wedged)", guardAcquireMax, path)
		}
		time.Sleep(guardPollBackoff)
	}
}
