//go:build unix

package lock

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
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
	start := time.Now()
	err := ExclusiveWithin(f, 200*time.Millisecond, 0, func() { calls++ })
	if !errors.Is(err, ErrTimeout) {
		t.Fatalf("err = %v, want ErrTimeout", err)
	}
	if took := time.Since(start); took < 200*time.Millisecond {
		t.Errorf("gave up after %s, before the deadline", took)
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
