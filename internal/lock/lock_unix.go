//go:build unix

// Package lock is wt's shared advisory-file-lock helper: flock, exclusive for
// a writer and shared for a reader, on the files windows share. The
// coordination logs and block ledgers take it for every write (#229) and read
// (#234); the section-scoped doc append (#22) for its read-modify-write. Real
// on unix; a no-op stub elsewhere so the tree stays portable.
package lock

import (
	"errors"
	"fmt"
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
	return within(f, syscall.LOCK_SH, d, 0, nil)
}

// ExclusiveWithin is Exclusive bounded by d (#235): a holder stopped with
// Ctrl-Z made every writer (announce, ack, block-id) wait silently for as
// long as it stayed stopped. waiting, when set, runs once, when the file has
// stayed held for noticeAfter, so the caller can say what it is waiting on
// (another writer's append holds it for microseconds: no notice for that).
// Past d: ErrTimeout. EINTR is retried. Any other flock error is returned as
// is, except after a try found the file held: then ErrSeenHeld, which is not
// Unsupported.
func ExclusiveWithin(f *os.File, d, noticeAfter time.Duration, waiting func()) error {
	return within(f, syscall.LOCK_EX, d, noticeAfter, waiting)
}

func within(f *os.File, how int, d, noticeAfter time.Duration, waiting func()) error {
	start := time.Now()
	for told, held := false, false; ; {
		err := flock(int(f.Fd()), how|syscall.LOCK_NB)
		switch {
		case err == nil:
			return nil
		case errors.Is(err, syscall.EWOULDBLOCK):
			held = true
		case errors.Is(err, syscall.EINTR):
			// A signal during an NFS lock call (#235 review): try again.
		case held:
			// #235 review: a try failed after one found the file HELD, so
			// locks do work here and someone holds this one. Never
			// Unsupported (%v, not %w): a caller that writes unlocked on
			// Unsupported would write into a prune's rewrite.
			return fmt.Errorf("%w (%v)", ErrSeenHeld, err)
		default:
			return err
		}
		waited := time.Since(start)
		if waiting != nil && !told && held && waited >= noticeAfter {
			told = true
			waiting()
		}
		if waited > d {
			if !held {
				return err // only signals, never a holder: not "stayed locked"
			}
			return ErrTimeout
		}
		time.Sleep(10 * time.Millisecond)
	}
}

// flock is syscall.Flock; a var so a test can make a try fail after another
// found the file held (#235 review).
var flock = syscall.Flock

// Unsupported reports whether err says the filesystem can't lock at all (NFS
// mounted nolocks on macOS, some FUSE and SMB mounts: ENOTSUP, EOPNOTSUPP,
// ENOSYS) rather than that the lock is held (#235). ⚠ Not ENOLCK (#235
// review, the owner's call): "no locks available" is a lock service failing
// (NFS lockd unreachable) on a mount that DOES lock, which can change while
// a prune holding the log is stopped; written unlocked then, a retry lost a
// hold to the prune's rewrite. ENOLCK fails, until the service is back.
func Unsupported(err error) bool {
	return errors.Is(err, syscall.ENOTSUP) || errors.Is(err, syscall.EOPNOTSUPP) ||
		errors.Is(err, syscall.ENOSYS)
}

// Release drops the advisory lock on f. Errors are ignored — closing the fd
// releases the lock regardless, and a defer'd release must not mask the real op
// error.
func Release(f *os.File) {
	_ = syscall.Flock(int(f.Fd()), syscall.LOCK_UN)
}
