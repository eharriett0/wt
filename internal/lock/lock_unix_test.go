//go:build unix

package lock

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
	"time"
)

func heldFile(t *testing.T) (path string, holder *os.File) {
	t.Helper()
	path = filepath.Join(t.TempDir(), "log.jsonl")
	holder, err := os.OpenFile(path, os.O_CREATE|os.O_RDWR, 0o644)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { holder.Close() })
	if err := Exclusive(holder); err != nil {
		t.Fatal(err)
	}
	return path, holder
}

func open(t *testing.T, path string) *os.File {
	t.Helper()
	f, err := os.OpenFile(path, os.O_RDWR, 0)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { f.Close() })
	return f
}

// #235: a writer waits at most d for a lock held through another descriptor,
// says so once, and gets ErrTimeout.
func TestExclusiveWithin_givesUpOnAHeldLock(t *testing.T) {
	path, _ := heldFile(t)
	f := open(t, path)
	calls := 0
	const d = 500 * time.Millisecond
	start := time.Now()
	err := ExclusiveWithin(f, d, 0, func() { calls++ })
	if !errors.Is(err, ErrTimeout) {
		t.Fatalf("err = %v, want ErrTimeout", err)
	}
	if took := time.Since(start); took < d || took > d+400*time.Millisecond {
		t.Errorf("gave up after %s, want at the %s deadline", took, d)
	}
	if calls != 1 {
		t.Errorf("waiting ran %d times, want once", calls)
	}
}

// The notice waits for noticeAfter: another writer's append holds the lock for
// microseconds, and saying "waiting" for that is noise.
func TestExclusiveWithin_noticeOnlyAfterItsDelay(t *testing.T) {
	path, holder := heldFile(t)
	f := open(t, path)
	calls := 0
	released := make(chan struct{})
	go func() { time.Sleep(50 * time.Millisecond); Release(holder); close(released) }()
	err := ExclusiveWithin(f, 5*time.Second, time.Second, func() { calls++ })
	<-released // the holder's cleanup closes it: not while the goroutine uses it
	if err != nil {
		t.Fatal(err)
	}
	if calls != 0 {
		t.Errorf("waiting ran %d times for a 50ms hold with a 1s notice delay, want none", calls)
	}
}

func TestExclusiveWithin_takesAFreeLockAndPassesOtherErrors(t *testing.T) {
	path := filepath.Join(t.TempDir(), "log.jsonl")
	f, err := os.OpenFile(path, os.O_CREATE|os.O_RDWR, 0o644)
	if err != nil {
		t.Fatal(err)
	}
	called := false
	if err := ExclusiveWithin(f, time.Second, 0, func() { called = true }); err != nil {
		t.Fatalf("free lock: %v", err)
	}
	if called {
		t.Error("waiting ran for a free lock")
	}
	f.Close()
	start := time.Now()
	err = ExclusiveWithin(f, 5*time.Second, 0, nil)
	if err == nil || errors.Is(err, ErrTimeout) || Unsupported(err) {
		t.Fatalf("closed file: err = %v, want flock's own error", err)
	}
	if time.Since(start) > time.Second {
		t.Error("a non-EWOULDBLOCK error was retried until the deadline")
	}
}

// #235 review: a "can't lock here" error after a try found the file HELD
// means this machine's lock service failed (NFS lockd unreachable), not that
// the filesystem can't lock: someone holds it. It must not read as
// Unsupported, or Append writes unlocked into a prune's rewrite. The same
// error on the first try is Unsupported, as before.
func TestExclusiveWithin_unsupportedAfterHeldFailsClosed(t *testing.T) {
	path, _ := heldFile(t)
	f := open(t, path)
	orig := flock
	t.Cleanup(func() { flock = orig })
	tries := 0
	flock = func(fd int, how int) error {
		tries++
		if tries == 1 {
			return syscall.EWOULDBLOCK
		}
		return syscall.ENOLCK
	}
	err := ExclusiveWithin(f, time.Second, 0, nil)
	if !errors.Is(err, ErrSeenHeld) || Unsupported(err) || errors.Is(err, ErrTimeout) || !strings.Contains(err.Error(), "no locks available") {
		t.Fatalf("held, then ENOLCK: err = %v (Unsupported %v), want a plain error", err, Unsupported(err))
	}
	tries = 1 // the next try is the first to fail: ENOLCK straight away
	if err := ExclusiveWithin(f, time.Second, 0, nil); !Unsupported(err) {
		t.Errorf("ENOLCK on the first try: err = %v, want Unsupported", err)
	}
	tries = 0
	if err := SharedWithin(f, time.Second); err == nil || Unsupported(err) {
		t.Errorf("readers too: held, then ENOLCK: err = %v, want a plain error", err)
	}
}

// #235 review: a signal can interrupt an NFS lock call (EINTR); that is no
// answer about the lock, so it is tried again, and never read as a holder.
func TestExclusiveWithin_retriesEINTR(t *testing.T) {
	path := filepath.Join(t.TempDir(), "log.jsonl")
	f, err := os.OpenFile(path, os.O_CREATE|os.O_RDWR, 0o644)
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	orig := flock
	t.Cleanup(func() { flock = orig })
	tries := 0
	flock = func(fd int, how int) error {
		if tries++; tries < 3 {
			return syscall.EINTR
		}
		return orig(fd, how)
	}
	called := false
	if err := ExclusiveWithin(f, time.Second, 0, func() { called = true }); err != nil || tries != 3 {
		t.Errorf("err = %v after %d tries, want the lock on the third", err, tries)
	}
	if called {
		t.Error("an interrupted try read as a holder (the waiting notice ran)")
	}
	flock = func(int, int) error { return syscall.EINTR }
	if err := ExclusiveWithin(f, 50*time.Millisecond, 0, nil); !errors.Is(err, syscall.EINTR) {
		t.Errorf("only signals until the deadline: err = %v, want EINTR, not ErrTimeout", err)
	}
}

func TestUnsupported(t *testing.T) {
	for _, tc := range []struct {
		err  error
		want bool
	}{
		{syscall.ENOTSUP, true},
		{syscall.EOPNOTSUPP, true},
		{syscall.ENOLCK, true},
		{syscall.ENOSYS, true},
		{fmt.Errorf("flock: %w", syscall.ENOLCK), true},
		{&os.PathError{Op: "flock", Path: "x", Err: syscall.ENOTSUP}, true},
		{syscall.EWOULDBLOCK, false},
		{syscall.EBADF, false},
		{syscall.EIO, false},
		{ErrTimeout, false},
		{nil, false},
	} {
		if got := Unsupported(tc.err); got != tc.want {
			t.Errorf("Unsupported(%v) = %v, want %v", tc.err, got, tc.want)
		}
	}
}
