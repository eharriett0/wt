//go:build unix

package coord

import (
	"bytes"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"
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
// other lock error is still returned and nothing is written, ENOLCK included
// (a lock service failing on a mount that locks: the owner's call, #235
// review).
func TestAppend_writesUnlockedWhereTheFilesystemCantLock(t *testing.T) {
	for _, errno := range []syscall.Errno{syscall.ENOTSUP, syscall.EOPNOTSUPP, syscall.ENOSYS} {
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
			if n := strings.Count(notes.String(), "writing without the lock"); n != 1 || strings.Count(notes.String(), path) != 1 {
				t.Errorf("warning printed %d times over two appends, want once:\n%s", n, notes)
			}
		})
	}
	for _, errno := range []syscall.Errno{syscall.ENOLCK, syscall.EIO, syscall.EBADF, syscall.EINTR} {
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
		// ENOTSUP is what Append writes unlocked on; these must not (#235
		// round-4 review: stubbed with ENOLCK alone, which nothing falls back
		// on any more, the test no longer told).
		for _, errno := range []syscall.Errno{syscall.ENOTSUP, syscall.ENOLCK} {
			t.Run(name+" no flock "+errno.Error(), func(t *testing.T) {
				path := filepath.Join(t.TempDir(), "log.jsonl")
				shortWriteLock(t, time.Second, 0)
				stubLock(t, errno)
				if err := write(path); !errors.Is(err, errno) {
					t.Fatalf("err = %v, want %v returned", err, errno)
				}
			})
		}
	}
}

// The documented bounds (#235 review: no test asserted them): the notice after
// a second, the give-up at 30s, and the notices on stderr, where a caller
// capturing stdout (N=$(wt block-id file)) never takes them in.
func TestWriteLockDefaults(t *testing.T) {
	if writeLockWait != 30*time.Second || writeLockNotice != time.Second {
		t.Errorf("writeLockWait %s, writeLockNotice %s; the docs say 30s and 1s", writeLockWait, writeLockNotice)
	}
	if lockNotices != os.Stderr {
		t.Error("lock notices must go to stderr")
	}
	err := func() error {
		path := filepath.Join(t.TempDir(), "log.jsonl")
		shortWriteLock(t, 50*time.Millisecond, time.Hour)
		holdLog(t, path)
		return Append(path, lockRec("x"))
	}()
	if !errors.Is(err, lock.ErrTimeout) {
		t.Errorf("the deadline error = %v, want it to wrap lock.ErrTimeout", err)
	}
}

// #235 review: merge-pr's hold gate refused only on a timeout, so any other
// lock failure (one after a try found the log held, say) read the log
// unlocked, which may be a log stopped between a prune's truncate and its
// rewrite. Only a filesystem that can't lock at all reads unlocked there.
func TestLoadForGate_refusesOnLockErrorsButUnsupported(t *testing.T) {
	path := filepath.Join(t.TempDir(), "log.jsonl")
	if err := Append(path, lockRec("hold")); err != nil {
		t.Fatal(err)
	}
	orig := sharedWithin
	t.Cleanup(func() { sharedWithin = orig })
	for _, tc := range []struct {
		name   string
		err    error
		refuse bool
		want   error
	}{
		{"held throughout", lock.ErrTimeout, true, ErrLogLocked},
		{"held, then the lock service failed", fmt.Errorf("%w (no locks available)", lock.ErrSeenHeld), true, ErrLogLocked},
		{"EIO", syscall.EIO, true, ErrLogUnlockable},
		{"the filesystem can't lock", syscall.ENOTSUP, false, nil},
		{"the lock service failed (ENOLCK)", syscall.ENOLCK, true, ErrLogUnlockable},
		{"locked", nil, false, nil},
	} {
		sharedWithin = func(*os.File, time.Duration) error { return tc.err }
		recs, err := LoadForGate(path)
		if refused := err != nil; refused != tc.refuse || tc.want != nil && !errors.Is(err, tc.want) {
			t.Errorf("%s: LoadForGate err = %v, want refused=%v (%v)", tc.name, err, tc.refuse, tc.want)
		}
		if !tc.refuse && len(recs) != 1 {
			t.Errorf("%s: read %d record(s), want the hold", tc.name, len(recs))
		}
		if recs, err := Load(path); err != nil || len(recs) != 1 {
			t.Errorf("%s: Load = %d record(s), %v; a non-gate read goes on", tc.name, len(recs), err)
		}
	}
}

// #235 review: one block ledger another process keeps locked stopped
// PruneBlockLedgers, so every ledger after it went unpruned. Now the rest are
// pruned and the failure comes back with what they dropped.
func TestPruneBlockLedgers_prunesPastAHeldLedger(t *testing.T) {
	home := t.TempDir()
	const wait = 400 * time.Millisecond
	shortWriteLock(t, wait, 0)
	old := time.Now().Add(-72 * time.Hour).UTC().Format(time.RFC3339)
	held := filepath.Join(BlocksDir(home), "a-held.jsonl")
	held2 := filepath.Join(BlocksDir(home), "b-held.jsonl")
	free := filepath.Join(BlocksDir(home), "c-free.jsonl")
	for _, p := range []string{held, held2, free} {
		for i, id := range []string{"r1", "r2"} {
			r := Record{ID: id, Kind: KindBlockReserve, Window: "w", Session: "s", File: "/doc.md", Block: i + 1, TS: old}
			if err := Append(p, r); err != nil {
				t.Fatal(err)
			}
		}
	}
	holdLog(t, held)
	holdLog(t, held2)
	start := time.Now()
	dropped, locked, err := PruneBlockLedgers(home, time.Now(), 24*time.Hour)
	if err != nil || strings.Join(locked, ",") != held+","+held2 {
		t.Errorf("locked = %v, err = %v; want both held ledgers listed, no other error", locked, err)
	}
	// One deadline for every ledger (#235 review: each held ledger cost its
	// own 30s).
	if took := time.Since(start); took > wait+300*time.Millisecond {
		t.Errorf("took %s for two held ledgers, want one %s deadline in all", took, wait)
	}
	if dropped != 2 {
		t.Errorf("dropped %d, want the free ledger's 2", dropped)
	}
	if got := logIDs(t, free); len(got) != 0 {
		t.Errorf("the ledger after the held one kept %v", got)
	}
}

// Every ledger that fails for another reason is reported, not only the last
// (errors.Join, #235 review).
func TestPruneBlockLedgers_reportsEveryFailure(t *testing.T) {
	if os.Geteuid() == 0 {
		t.Skip("root opens a mode-000 file")
	}
	home := t.TempDir()
	var bad []string
	for _, n := range []string{"a-bad.jsonl", "b-bad.jsonl"} {
		p := filepath.Join(BlocksDir(home), n)
		if err := Append(p, lockRec("r")); err != nil {
			t.Fatal(err)
		}
		if err := os.Chmod(p, 0); err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { _ = os.Chmod(p, 0o644) })
		bad = append(bad, p)
	}
	_, locked, err := PruneBlockLedgers(home, time.Now(), 24*time.Hour)
	if len(locked) != 0 || err == nil || !strings.Contains(err.Error(), bad[0]) || !strings.Contains(err.Error(), bad[1]) {
		t.Errorf("locked %v, err = %v; want both unreadable ledgers in err", locked, err)
	}
}

// A ledger held for a moment (a block-id's normal write) within the shared
// deadline is waited for and pruned, not listed as locked (#235 review: with
// the deadline spent up front, every ledger got no wait at all, and no test
// told).
func TestPruneBlockLedgers_waitsWithinTheSharedDeadline(t *testing.T) {
	home := t.TempDir()
	shortWriteLock(t, 2*time.Second, time.Hour)
	old := time.Now().Add(-72 * time.Hour).UTC().Format(time.RFC3339)
	ledger := filepath.Join(BlocksDir(home), "a.jsonl")
	if err := Append(ledger, Record{ID: "r1", Kind: KindBlockReserve, Window: "w", Session: "s", File: "/doc.md", Block: 1, TS: old}); err != nil {
		t.Fatal(err)
	}
	release := holdLog(t, ledger)
	go func() { time.Sleep(200 * time.Millisecond); release() }()
	dropped, locked, err := PruneBlockLedgers(home, time.Now(), 24*time.Hour)
	if dropped != 1 || len(locked) != 0 || err != nil {
		t.Errorf("dropped %d, locked %v, err %v; want the briefly held ledger pruned", dropped, locked, err)
	}
}

// The advisory shared-checkout read (#197 review): a ledger untouched for
// longer than the window is skipped, and a ledger another process holds
// locked is read at once, not after loadLockWait (every `wt status --json`
// stalled 5s per stuck ledger).
func TestRecentBlockLedgers(t *testing.T) {
	home := t.TempDir()
	now := time.Now()
	fresh := filepath.Join(BlocksDir(home), "fresh.jsonl")
	stale := filepath.Join(BlocksDir(home), "stale.jsonl")
	locked := filepath.Join(BlocksDir(home), "locked.jsonl")
	for _, p := range []string{fresh, stale, locked} {
		if err := Append(p, lockRec(filepath.Base(p))); err != nil {
			t.Fatal(err)
		}
	}
	old := now.Add(-25 * time.Hour)
	if err := os.Chtimes(stale, old, old); err != nil {
		t.Fatal(err)
	}
	holdLog(t, locked)
	start := time.Now()
	recs := RecentBlockLedgers(home, now, 24*time.Hour)
	if took := time.Since(start); took > time.Second {
		t.Errorf("took %s with one ledger locked, want no wait", took)
	}
	var ids []string
	for _, r := range recs {
		ids = append(ids, r.ID)
	}
	sort.Strings(ids)
	if strings.Join(ids, ",") != "fresh.jsonl,locked.jsonl" {
		t.Errorf("read %v, want the fresh and the locked ledger, not the stale one", ids)
	}
	if RecentBlockLedgers("", now, 24*time.Hour) != nil {
		t.Error("no home: want nothing")
	}
}

// AppendIfFree never waits (#197 item 2: the per-turn hook marks presence
// with it): a held lock writes nothing and returns at once; a filesystem that
// can't lock gets the unlocked write, silently; any other lock error writes
// nothing and is returned.
func TestAppendIfFree(t *testing.T) {
	path := filepath.Join(t.TempDir(), "log.jsonl")
	notes := shortWriteLock(t, 30*time.Second, 0)
	if ok, err := AppendIfFree(path, lockRec("free")); !ok || err != nil {
		t.Fatalf("free lock: %v, %v", ok, err)
	}
	release := holdLog(t, path)
	start := time.Now()
	if ok, err := AppendIfFree(path, lockRec("held")); ok || err != nil {
		t.Errorf("held lock: %v, %v; want not written, no error", ok, err)
	}
	if took := time.Since(start); took > time.Second {
		t.Errorf("took %s on a held lock, want no wait", took)
	}
	release()
	stubLock(t, syscall.ENOTSUP)
	if ok, err := AppendIfFree(path, lockRec("unsupported")); !ok || err != nil {
		t.Errorf("can't lock: %v, %v; want written", ok, err)
	}
	stubLock(t, syscall.EIO)
	if ok, err := AppendIfFree(path, lockRec("eio")); ok || !errors.Is(err, syscall.EIO) {
		t.Errorf("EIO: %v, %v; want not written, EIO", ok, err)
	}
	if got := strings.Join(logIDs(t, path), ","); got != "free,unsupported" {
		t.Errorf("records = %s, want free,unsupported", got)
	}
	if notes.Len() != 0 {
		t.Errorf("AppendIfFree printed: %q", notes)
	}
}
