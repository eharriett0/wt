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
	return within(f, syscall.LOCK_SH, d, 0, nil)
}

// ExclusiveWithin is Exclusive bounded by d (#235): a holder stopped with
// Ctrl-Z made every writer (announce, ack, block-id) wait silently for as
// long as it stayed stopped. waiting, when set, runs once, when the file has
// stayed held for noticeAfter, so the caller can say what it is waiting on
// (another writer's append holds it for microseconds: no notice for that).
// Past d: ErrTimeout. Any other flock error is returned as is.
func ExclusiveWithin(f *os.File, d, noticeAfter time.Duration, waiting func()) error {
	return within(f, syscall.LOCK_EX, d, noticeAfter, waiting)
}

func within(f *os.File, how int, d, noticeAfter time.Duration, waiting func()) error {
	start := time.Now()
	for told := false; ; {
		err := syscall.Flock(int(f.Fd()), how|syscall.LOCK_NB)
		if err == nil || !errors.Is(err, syscall.EWOULDBLOCK) {
			return err
		}
		waited := time.Since(start)
		if waiting != nil && !told && waited >= noticeAfter {
			told = true
			waiting()
		}
		if waited > d {
			return ErrTimeout
		}
		time.Sleep(10 * time.Millisecond)
	}
}

// Unsupported reports whether err says the filesystem can't lock at all (NFS
// mounted nolocks, lockd unreachable, some FUSE and SMB mounts) rather than
// that the lock is held (#235).
func Unsupported(err error) bool {
	return errors.Is(err, syscall.ENOTSUP) || errors.Is(err, syscall.EOPNOTSUPP) ||
		errors.Is(err, syscall.ENOLCK) || errors.Is(err, syscall.ENOSYS)
}

// Release drops the advisory lock on f. Errors are ignored — closing the fd
// releases the lock regardless, and a defer'd release must not mask the real op
// error.
func Release(f *os.File) {
	_ = syscall.Flock(int(f.Fd()), syscall.LOCK_UN)
}
