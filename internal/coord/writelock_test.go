//go:build unix

package coord

import (
	"bytes"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"syscall"
	"testing"
	"time"

	"github.com/eharriett0/wt/internal/lock"
)

// shortWriteLock shortens the writers' wait and notice delay and captures the
// notices.
func shortWriteLock(t *testing.T, wait, notice time.Duration) *bytes.Buffer {
	t.Helper()
	ow, on, onotes := writeLockWait, writeLockNotice, lockNotices
	var buf bytes.Buffer
	writeLockWait, writeLockNotice, lockNotices = wait, notice, &buf
	t.Cleanup(func() { writeLockWait, writeLockNotice, lockNotices = ow, on, onotes })
	return &buf
}

// holdLog takes path's exclusive lock through another descriptor, as a
// stopped prune-coord would hold it. release lets go early.
func holdLog(t *testing.T, path string) (release func()) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	h, err := os.OpenFile(path, os.O_CREATE|os.O_RDWR, 0o644)
	if err != nil {
		t.Fatal(err)
	}
	if err := lock.Exclusive(h); err != nil {
		t.Fatal(err)
	}
	var once sync.Once
	release = func() { once.Do(func() { lock.Release(h); h.Close() }) }
	t.Cleanup(release)
	return release
}

func lockRec(id string) Record {
	return Record{ID: id, Kind: KindAnnounce, Window: "w", Session: "s", Message: "m",
		TS: time.Now().UTC().Format(time.RFC3339)}
}

func logIDs(t *testing.T, path string) []string {
	t.Helper()
	recs, err := Load(path)
	if err != nil {
		t.Fatal(err)
	}
	var out []string
	for _, r := range recs {
		out = append(out, r.ID)
	}
	return out
}

// #235: a holder that never lets go (a prune-coord stopped with Ctrl-Z) hung
// every announce and ack, silently and for as long as it stayed stopped. Now
// Append says it is waiting, once, and gives up at the deadline with an error
// that names the log and says to resume the holder, not kill it.
func TestAppend_givesUpOnAStuckHolder(t *testing.T) {
	path := filepath.Join(t.TempDir(), "log.jsonl")
	notes := shortWriteLock(t, 300*time.Millisecond, 50*time.Millisecond)
	release := holdLog(t, path)

	start := time.Now()
	err := Append(path, lockRec("late"))
	if err == nil {
		t.Fatal("Append wrote past a lock another process held")
	}
	if took := time.Since(start); took < 300*time.Millisecond {
		t.Errorf("gave up after %s, before the deadline", took)
	}
	for _, want := range []string{path, "kill -CONT", "Don't kill it"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("error %q lacks %q", err, want)
		}
	}
	if n := strings.Count(notes.String(), "waiting for the lock on "+path); n != 1 {
		t.Errorf("waiting notice printed %d times, want once:\n%s", n, notes)
	}
	release()
	if got := logIDs(t, path); len(got) != 0 {
		t.Errorf("records after a refused append: %v, want none", got)
	}
}

// A holder that lets go within the deadline: Append waits, says so once, and
// writes.
func TestAppend_waitsForAHolderThatLetsGo(t *testing.T) {
	path := filepath.Join(t.TempDir(), "log.jsonl")
	notes := shortWriteLock(t, 10*time.Second, 50*time.Millisecond)
	release := holdLog(t, path)

	done := make(chan error, 1)
	go func() { done <- Append(path, lockRec("waited")) }()
	select {
	case err := <-done:
		t.Fatalf("Append returned (%v) while the lock was held", err)
	case <-time.After(250 * time.Millisecond):
	}
	release()
	select {
	case err := <-done:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("Append still waiting after the holder let go")
	}
	if n := strings.Count(notes.String(), "waiting for the lock"); n != 1 {
		t.Errorf("waiting notice printed %d times, want once:\n%s", n, notes)
	}
	if got := logIDs(t, path); len(got) != 1 || got[0] != "waited" {
		t.Errorf("records = %v, want [waited]", got)
	}
}

// Another writer's append holds the lock for microseconds; that is not worth
// a line on every contended announce.
func TestAppend_noNoticeForAShortWait(t *testing.T) {
	path := filepath.Join(t.TempDir(), "log.jsonl")
	notes := shortWriteLock(t, 10*time.Second, 2*time.Second)
	release := holdLog(t, path)
	go func() { time.Sleep(50 * time.Millisecond); release() }()
	if err := Append(path, lockRec("a")); err != nil {
		t.Fatal(err)
	}
	if notes.Len() != 0 {
		t.Errorf("notice for a 50ms wait: %q", notes)
	}
}

// #235: on a filesystem that can't lock at all, Append refused, and announce,
// ack, all-clear and block-id --written/--abandon exited 1 where they had
// worked before #229. It writes unlocked there, warning once per process; any
// other lock error is still returned and nothing is written.
func TestAppend_writesUnlockedWhereTheFilesystemCantLock(t *testing.T) {
	for _, errno := range []syscall.Errno{syscall.ENOTSUP, syscall.EOPNOTSUPP, syscall.ENOLCK, syscall.ENOSYS} {
		t.Run(errno.Error(), func(t *testing.T) {
			path := filepath.Join(t.TempDir(), "log.jsonl")
			notes := shortWriteLock(t, time.Second, 0)
			stubLock(t, errno)
			for _, id := range []string{"a", "b"} {
				if err := Append(path, lockRec(id)); err != nil {
					t.Fatalf("Append(%s) = %v, want the unlocked write", id, err)
				}
			}
			if got := logIDs(t, path); strings.Join(got, ",") != "a,b" {
				t.Errorf("records = %v, want [a b]", got)
			}
			if n := strings.Count(notes.String(), "can't be locked"); n != 1 {
				t.Errorf("warning printed %d times over two appends, want once:\n%s", n, notes)
			}
		})
	}
	for _, errno := range []syscall.Errno{syscall.EIO, syscall.EBADF, syscall.EINTR} {
		t.Run("other "+errno.Error(), func(t *testing.T) {
			path := filepath.Join(t.TempDir(), "log.jsonl")
			shortWriteLock(t, time.Second, 0)
			stubLock(t, errno)
			if err := Append(path, lockRec("a")); !errors.Is(err, errno) {
				t.Fatalf("Append = %v, want %v returned", err, errno)
			}
			if got := logIDs(t, path); len(got) != 0 {
				t.Errorf("records = %v after a refused lock, want none", got)
			}
		})
	}
}

func stubLock(t *testing.T, err error) {
	t.Helper()
	orig := exclusiveWithin
	exclusiveWithin = func(*os.File, time.Duration, time.Duration, func()) error { return err }
	noLockOnce = sync.Once{}
	t.Cleanup(func() { exclusiveWithin = orig; noLockOnce = sync.Once{} })
}

// #235: ReserveBlock and prune-coord's rewrite take the same bounded lock. A
// filesystem that can't lock still fails them: a block id needs the lock
// across its read and write, and a rewrite without it is #229.
func TestLockedWriters_sameDeadlineAndNoUnlockedFallback(t *testing.T) {
	writers := map[string]func(path string) error{
		"ReserveBlock": func(path string) error {
			_, err := ReserveBlock(path, lockRec("r"), "doc.md", nil)
			return err
		},
		"PruneLog": func(path string) error {
			_, err := PruneLog(path, time.Now(), 24*time.Hour)
			return err
		},
		"PruneLegacy": func(path string) error {
			_, _, err := PruneLegacy(path, time.Now(), 24*time.Hour, true)
			return err
		},
	}
	for name, write := range writers {
		t.Run(name+" held", func(t *testing.T) {
			path := filepath.Join(t.TempDir(), "log.jsonl")
			notes := shortWriteLock(t, 200*time.Millisecond, 0)
			holdLog(t, path)
			err := write(path)
			if err == nil || !strings.Contains(err.Error(), path) || !strings.Contains(err.Error(), "kill -CONT") {
				t.Fatalf("err = %v, want the deadline error naming %s", err, path)
			}
			if n := strings.Count(notes.String(), "waiting for the lock on "+path); n != 1 {
				t.Errorf("waiting notice printed %d times, want once:\n%s", n, notes)
			}
		})
		t.Run(name+" no flock", func(t *testing.T) {
			path := filepath.Join(t.TempDir(), "log.jsonl")
			shortWriteLock(t, time.Second, 0)
			stubLock(t, syscall.ENOLCK)
			if err := write(path); !errors.Is(err, syscall.ENOLCK) {
				t.Fatalf("err = %v, want ENOLCK returned", err)
			}
		})
	}
}
