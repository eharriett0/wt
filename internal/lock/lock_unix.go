//go:build unix

// Package lock is wt's shared advisory-file-lock helper: flock, exclusive for
// a writer and shared for a reader, on the files windows share. The
// coordination logs and block ledgers take it for every write (#229) and read
// (#234); the section-scoped doc append (#22) for its read-modify-write. Real
// on unix; a no-op stub elsewhere so the tree stays portable.
package lock

import (
	"errors"
	"os"
	"syscall"
	"time"
)

// Exclusive takes a blocking exclusive advisory lock on f.
func Exclusive(f *os.File) error {
	return syscall.Flock(int(f.Fd()), syscall.LOCK_EX)
}

// SharedWithin takes a shared advisory lock on f: many readers hold it at
// once, and it waits while any Exclusive holder (a prune's rewrite, an append)
// has the file (#234). It waits at most d (#234 review: a holder stopped with
// Ctrl-Z blocked every reader, the per-turn hooks included, for as long as it
// stayed stopped); then ErrTimeout. Any other flock error is returned as is.
func SharedWithin(f *os.File, d time.Duration) error {
	deadline := time.Now().Add(d)
	for {
		err := syscall.Flock(int(f.Fd()), syscall.LOCK_SH|syscall.LOCK_NB)
		if err == nil || !errors.Is(err, syscall.EWOULDBLOCK) {
			return err
		}
		if time.Now().After(deadline) {
			return ErrTimeout
		}
		time.Sleep(10 * time.Millisecond)
	}
}

// Release drops the advisory lock on f. Errors are ignored — closing the fd
// releases the lock regardless, and a defer'd release must not mask the real op
// error.
func Release(f *os.File) {
	_ = syscall.Flock(int(f.Fd()), syscall.LOCK_UN)
}
