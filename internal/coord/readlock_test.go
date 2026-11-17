package coord

import (
	"errors"
	"os"
	"path/filepath"
	"runtime"
	"sync"
	"testing"
	"time"

	"github.com/eharriett0/wt/internal/lock"
)

// #234: a read that lands while prune-coord has truncated the log and not yet
// rewritten it must wait for the rewrite. Unlocked, it read an empty log, and
// merge-pr's hold gate let a merge through past a hold that never left the
// file.
func TestLoad_waitsForAPruneRewrite(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("the advisory lock is a no-op off unix")
	}
	path := filepath.Join(t.TempDir(), "log.jsonl")
	old := time.Now().Add(-48 * time.Hour).UTC().Format(time.RFC3339)
	for _, r := range []Record{
		{ID: "a1", Kind: KindAnnounce, Window: "w", Session: "s", Message: "done", TS: old},
		{ID: "c1", Kind: KindAllClear, Window: "w", Session: "s", AckOf: "a1", TS: old},
		{ID: "keep", Kind: KindAnnounce, Window: "w", Session: "s", Message: "still open", Hold: []string{"merge-main"}, TS: old},
	} {
		if err := Append(path, r); err != nil {
			t.Fatal(err)
		}
	}

	truncated, release := make(chan struct{}), make(chan struct{})
	lockedLogTruncated = func() { close(truncated); <-release }
	var once sync.Once
	unpause := func() { once.Do(func() { close(release) }) }
	t.Cleanup(func() { unpause(); lockedLogTruncated = nil })

	pruned := make(chan error, 1)
	go func() {
		_, err := PruneLog(path, time.Now(), 24*time.Hour)
		pruned <- err
	}()
	select {
	case <-truncated:
	case err := <-pruned:
		t.Fatalf("the prune returned (%v) without truncating the log", err)
	case <-time.After(20 * time.Second):
		t.Fatal("the prune never reached its truncate (withLockedLog waiting on itself?)")
	}

	type read struct {
		recs []Record
		err  error
	}
	loaded := make(chan read, 1)
	go func() {
		recs, err := Load(path)
		loaded <- read{recs, err}
	}()
	select {
	case r := <-loaded:
		t.Fatalf("Load returned %d record(s) (%v) while the prune held the log truncated", len(r.recs), r.err)
	case <-time.After(300 * time.Millisecond):
	}
	unpause()
	if err := <-pruned; err != nil {
		t.Fatal(err)
	}
	r := <-loaded
	if r.err != nil {
		t.Fatal(r.err)
	}
	var hold bool
	for _, rec := range r.recs {
		hold = hold || rec.ID == "keep"
	}
	if !hold {
		t.Errorf("Load after the prune: %d record(s), want the hold that never left the log", len(r.recs))
	}
}

// A holder that never lets go (a prune stopped with Ctrl-Z) must not block a
// reader forever (#234 review): Load reads unlocked after loadLockWait, as
// before #234, and LoadForGate refuses, since what it would read may be a log
// stopped between its truncate and its rewrite.
func TestLoad_givesUpOnAStuckHolder(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("the advisory lock is a no-op off unix")
	}
	path := filepath.Join(t.TempDir(), "log.jsonl")
	if err := Append(path, Record{ID: "keep", Kind: KindAnnounce, Window: "w", Session: "s", Hold: []string{"merge-main"},
		TS: time.Now().UTC().Format(time.RFC3339)}); err != nil {
		t.Fatal(err)
	}
	orig := loadLockWait
	loadLockWait = 300 * time.Millisecond
	t.Cleanup(func() { loadLockWait = orig })
	holder, err := os.OpenFile(path, os.O_RDWR, 0)
	if err != nil {
		t.Fatal(err)
	}
	defer holder.Close()
	if err := lock.Exclusive(holder); err != nil {
		t.Fatal(err)
	}
	defer lock.Release(holder)

	start := time.Now()
	recs, err := Load(path)
	if took := time.Since(start); err != nil || len(recs) != 1 || took > 5*time.Second {
		t.Errorf("Load under a stuck holder: %d record(s), %v, in %s; want the log read unlocked after the wait", len(recs), err, took)
	}
	if _, err := LoadForGate(path); !errors.Is(err, ErrLogLocked) {
		t.Errorf("LoadForGate under a stuck holder: %v, want ErrLogLocked", err)
	}
	lock.Release(holder)
	if recs, err := LoadForGate(path); err != nil || len(recs) != 1 {
		t.Errorf("LoadForGate once the holder lets go: %d record(s), %v", len(recs), err)
	}
}

// The two callers that read while they hold the exclusive lock through
// another descriptor must not wait on themselves (#234): a prune and a
// reservation each finish.
func TestLockHolders_readWithoutSelfDeadlock(t *testing.T) {
	path := filepath.Join(t.TempDir(), "log.jsonl")
	if err := Append(path, Record{ID: "x", Kind: KindAnnounce, Window: "w", Session: "s", TS: time.Now().UTC().Format(time.RFC3339)}); err != nil {
		t.Fatal(err)
	}
	done := make(chan error, 2)
	go func() {
		_, err := PruneLog(path, time.Now(), 24*time.Hour)
		done <- err
	}()
	go func() {
		_, err := ReserveBlock(path, Record{ID: "r", Window: "w", Session: "s", TS: time.Now().UTC().Format(time.RFC3339)}, "doc.md", nil)
		done <- err
	}()
	for range 2 {
		select {
		case err := <-done:
			if err != nil {
				t.Fatal(err)
			}
		case <-time.After(loadLockWait / 2):
			// Well inside Load's wait: a holder reading through another
			// descriptor would sit out the whole wait on its own lock.
			t.Fatal("a lock holder waited on its own lock")
		}
	}
}
