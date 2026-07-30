//go:build unix

package coord

import (
	"os"
	"syscall"
)

// flockExclusive takes an exclusive advisory lock on f (blocking). Used by
// ReserveBlock to serialize concurrent block-id allocations (#23).
func flockExclusive(f *os.File) error {
	return syscall.Flock(int(f.Fd()), syscall.LOCK_EX)
}

// flockRelease drops the advisory lock on f. Errors are ignored (closing the
// fd releases the lock regardless; a defer'd release must not mask the real
// operation error).
func flockRelease(f *os.File) {
	_ = syscall.Flock(int(f.Fd()), syscall.LOCK_UN)
}
