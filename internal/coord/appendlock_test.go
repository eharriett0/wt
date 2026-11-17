package coord

import (
	"path/filepath"
	"runtime"
	"sync"
	"testing"
	"time"
)

// #229: an Append that lands while prune-coord holds the log between its read
// and its rewrite must wait for it, not be truncated away or overwritten. On
// the unlocked Append the record appended during the pause was lost.
func TestAppend_waitsForAPruneRewrite(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("the advisory lock is a no-op off unix")
	}
	path := filepath.Join(t.TempDir(), "log.jsonl")
	now := time.Now()
	old := now.Add(-48 * time.Hour).UTC().Format(time.RFC3339)
	// A completed handshake (announce + all-clear) gives the prune something to drop.
	for _, r := range []Record{
		{ID: "a1", Kind: KindAnnounce, Window: "w", Session: "s", Message: "done", TS: old},
		{ID: "c1", Kind: KindAllClear, Window: "w", Session: "s", AckOf: "a1", TS: old},
		{ID: "keep", Kind: KindAnnounce, Window: "w", Session: "s", Message: "still open", Hold: []string{"merge-main"}, TS: old},
	} {
		if err := Append(path, r); err != nil {
			t.Fatal(err)
		}
	}

	paused, release := make(chan struct{}), make(chan struct{})
	lockedLogPause = func() { close(paused); <-release }
	var releaseOnce sync.Once
	unpause := func() { releaseOnce.Do(func() { close(release) }) }
	// A Fatalf before the release must not leak the paused prune holding the lock.
	t.Cleanup(func() { unpause(); lockedLogPause = nil })

	pruned := make(chan error, 1)
	go func() {
		_, err := PruneLog(path, now, 24*time.Hour)
		pruned <- err
	}()
	select {
	case <-paused:
	case err := <-pruned:
		t.Fatalf("the prune returned (%v) without reaching the pause between its read and its rewrite", err)
	}

	hold := Record{ID: "h2", Kind: KindAnnounce, Window: "w2", Session: "t", Message: "rolling", Hold: []string{"merge-main"},
		TS: now.UTC().Format(time.RFC3339)}
	appended := make(chan error, 1)
	go func() { appended <- Append(path, hold) }()
	select {
	case err := <-appended:
		t.Fatalf("Append returned (%v) while the prune held the log between its read and its rewrite", err)
	case <-time.After(300 * time.Millisecond):
	}
	unpause()
	if err := <-pruned; err != nil {
		t.Fatal(err)
	}
	if err := <-appended; err != nil {
		t.Fatal(err)
	}
	recs, err := Load(path)
	if err != nil {
		t.Fatal(err)
	}
	got := map[string]bool{}
	for _, r := range recs {
		got[r.ID] = true
	}
	if !got["h2"] || !got["keep"] || got["a1"] || got["c1"] {
		t.Errorf("after the prune and the append: %v; want keep and h2, without the completed handshake", got)
	}
}

// ReserveBlock appends while it holds the lock: it must not wait on itself,
// and concurrent Appends and reservations all land (#229).
func TestReserveBlock_noSelfDeadlockAndAppendsLand(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("the advisory lock is a no-op off unix")
	}
	path := filepath.Join(t.TempDir(), "ledger.jsonl")
	var wg sync.WaitGroup
	errs := make(chan error, 40)
	for i := 0; i < 20; i++ {
		wg.Add(2)
		go func() {
			defer wg.Done()
			_, err := ReserveBlock(path, Record{ID: NewID(time.Now()), Window: "w", Session: "s"}, "f.md", nil)
			errs <- err
		}()
		go func(i int) {
			defer wg.Done()
			errs <- Append(path, Record{ID: NewID(time.Now()), Kind: KindAck, Window: "w", Session: "s", AckOf: "x"})
		}(i)
	}
	done := make(chan struct{})
	go func() { wg.Wait(); close(done) }()
	select {
	case <-done:
	case <-time.After(20 * time.Second):
		t.Fatal("ReserveBlock/Append deadlocked")
	}
	close(errs)
	for err := range errs {
		if err != nil {
			t.Fatal(err)
		}
	}
	recs, err := Load(path)
	if err != nil {
		t.Fatal(err)
	}
	blocks := map[int]bool{}
	n := 0
	for _, r := range recs {
		if r.Kind == KindBlockReserve {
			n++
			if blocks[r.Block] {
				t.Errorf("block %d handed out twice", r.Block)
			}
			blocks[r.Block] = true
		}
	}
	if len(recs) != 40 || n != 20 {
		t.Errorf("%d records (%d reservations), want 40 (20)", len(recs), n)
	}
}
