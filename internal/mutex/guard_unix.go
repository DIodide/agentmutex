//go:build unix

package mutex

import (
	"fmt"
	"os"
	"syscall"
)

// lockGuard takes an exclusive flock(2) on path, creating it if needed.
// It returns a release func. The kernel drops the lock automatically if the
// process dies, so a crashed CLI invocation can never wedge the store.
//
// Guard critical sections are single read-modify-write operations lasting
// milliseconds, so blocking (rather than polling) is fine here.
func lockGuard(path string) (func(), error) {
	f, err := os.OpenFile(path, os.O_CREATE|os.O_RDWR, 0o644)
	if err != nil {
		return nil, fmt.Errorf("open guard: %w", err)
	}
	if err := syscall.Flock(int(f.Fd()), syscall.LOCK_EX); err != nil {
		f.Close()
		return nil, fmt.Errorf("flock guard: %w", err)
	}
	return func() {
		// Closing the fd releases the flock; the explicit unlock is a
		// courtesy so the release is prompt even if the fd lingers.
		syscall.Flock(int(f.Fd()), syscall.LOCK_UN)
		f.Close()
	}, nil
}
