//go:build unix

// Package lock is wt's shared advisory-file-lock helper: an exclusive flock used
// to serialize read-modify-write on shared files across concurrent windows —
// the coordination-log block-id reservation (#23) and the section-scoped doc
// append (#22). Real on unix; a no-op stub elsewhere so the tree stays portable.
package lock

import (
	"os"
	"syscall"
)

// Exclusive takes a blocking exclusive advisory lock on f.
func Exclusive(f *os.File) error {
	return syscall.Flock(int(f.Fd()), syscall.LOCK_EX)
}

// Release drops the advisory lock on f. Errors are ignored — closing the fd
// releases the lock regardless, and a defer'd release must not mask the real op
// error.
func Release(f *os.File) {
	_ = syscall.Flock(int(f.Fd()), syscall.LOCK_UN)
}
