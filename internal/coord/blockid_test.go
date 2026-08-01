package coord

import (
	"path/filepath"
	"sort"
	"sync"
	"testing"
	"time"
)

func TestNextBlock(t *testing.T) {
	now := time.Now()
	if got := NextBlock(nil, "resume.md", 0, now, 30*time.Minute); got != 1 {
		t.Errorf("empty → %d, want 1", got)
	}
	if got := NextBlock(nil, "resume.md", 55, now, 30*time.Minute); got != 56 {
		t.Errorf("fileMax=55 seed → %d, want 56", got)
	}
	// TS-less records have Age 0 → always fresh, so they still count.
	recs := []Record{
		{Kind: KindBlockReserve, File: "resume.md", Block: 3},
		{Kind: KindBlockReserve, File: "resume.md", Block: 7},
		{Kind: KindBlockReserve, File: "other.md", Block: 99}, // different file — ignored
		{Kind: KindAnnounce, File: "resume.md"},               // wrong kind — ignored
	}
	if got := NextBlock(recs, "resume.md", 0, now, 30*time.Minute); got != 8 {
		t.Errorf("max reservation 7 → %d, want 8", got)
	}
	if got := NextBlock(recs, "resume.md", 20, now, 30*time.Minute); got != 21 {
		t.Errorf("fileMax=20 beats reservation 7 → %d, want 21", got)
	}
	if got := NextBlock(recs, "other.md", 0, now, 30*time.Minute); got != 100 {
		t.Errorf("per-file isolation: other.md → %d, want 100", got)
	}
}

// A stale, never-written reservation frees its id (#35): NextBlock skips it, so
// a crashed window that reserved N but never prepended doesn't burn N.
func TestNextBlock_FreesStaleUnwritten(t *testing.T) {
	now := time.Date(2026, 7, 30, 12, 0, 0, 0, time.UTC)
	ago := func(min int) string { return now.Add(-time.Duration(min) * time.Minute).Format(time.RFC3339) }
	recs := []Record{
		{Kind: KindBlockReserve, File: "r.md", Block: 5, TS: ago(90)}, // stale, no write → freed
	}
	if got := NextBlock(recs, "r.md", 0, now, 30*time.Minute); got != 1 {
		t.Errorf("stale unwritten reservation should be freed → %d, want 1", got)
	}
	// A written reservation of the same age is still honored via the file's
	// fileMax (the block is on disk); the marker itself doesn't inflate beyond it.
	recs = append(recs,
		Record{Kind: KindBlockReserve, File: "r.md", Block: 8, TS: ago(90)},
		Record{Kind: KindBlockWritten, File: "r.md", Block: 8, TS: ago(89)},
	)
	if got := NextBlock(recs, "r.md", 8, now, 30*time.Minute); got != 9 {
		t.Errorf("written block 8 (in fileMax) → %d, want 9", got)
	}
	// ttl<=0 disables aging: even the stale reservation counts (back-compat).
	if got := NextBlock(recs, "r.md", 0, now, 0); got != 9 {
		t.Errorf("ttl=0 counts all reservations → %d, want 9 (max 8)", got)
	}
}

// A written reservation drops out of the imminent-prepend banner and `wt holds`.
func TestBlockWritten_ExcludesFromViews(t *testing.T) {
	now := time.Date(2026, 7, 30, 12, 0, 0, 0, time.UTC)
	fresh := now.Add(-time.Minute).Format(time.RFC3339)
	recs := []Record{
		{Kind: KindBlockReserve, Window: "other", File: "r.md", Block: 3, TS: fresh},
		{Kind: KindBlockReserve, Window: "self", File: "r.md", Block: 4, TS: fresh},
	}
	if got := RecentBlockReservations(recs, "self", now, 30*time.Minute); len(got) != 1 {
		t.Fatalf("before write: banner shows %d, want 1", len(got))
	}
	if got := OwnBlockReservations(recs, "self"); len(got) != 1 {
		t.Fatalf("before write: holds shows %d, want 1", len(got))
	}
	// Mark both written.
	recs = append(recs,
		Record{Kind: KindBlockWritten, Window: "other", File: "r.md", Block: 3, TS: fresh},
		Record{Kind: KindBlockWritten, Window: "self", File: "r.md", Block: 4, TS: fresh},
	)
	if got := RecentBlockReservations(recs, "self", now, 30*time.Minute); len(got) != 0 {
		t.Errorf("after write: banner should be empty, got %d", len(got))
	}
	if got := OwnBlockReservations(recs, "self"); len(got) != 0 {
		t.Errorf("after write: holds should be empty, got %d", len(got))
	}
	// prune-coord GCs the written reservation + its marker.
	kept, dropped := PruneRecords(recs, now, 24*time.Hour)
	if dropped != 4 {
		t.Errorf("prune dropped %d, want 4 (2 reservations + 2 markers)", dropped)
	}
	for _, r := range kept {
		if r.Kind == KindBlockReserve || r.Kind == KindBlockWritten {
			t.Errorf("prune left a block record: %+v", r)
		}
	}
}

func TestRecentBlockReservations(t *testing.T) {
	now := time.Date(2026, 7, 30, 12, 0, 0, 0, time.UTC)
	mk := func(win string, block, ageMin int) Record {
		return Record{
			Kind: KindBlockReserve, Window: win, File: "resume.md", Block: block,
			TS: now.Add(-time.Duration(ageMin) * time.Minute).Format(time.RFC3339),
		}
	}
	recs := []Record{
		mk("winA", 10, 5),  // other window, recent → included
		mk("self", 11, 2),  // self → excluded
		mk("winB", 12, 90), // too old → excluded
		mk("winA", 13, 1),  // other, recent → included (newest)
		{Kind: KindAnnounce, Window: "winA", TS: now.Format(time.RFC3339)}, // wrong kind
	}
	got := RecentBlockReservations(recs, "self", now, 30*time.Minute)
	if len(got) != 2 {
		t.Fatalf("got %d reservations, want 2: %+v", len(got), got)
	}
	if got[0].Block != 13 || got[1].Block != 10 {
		t.Errorf("order = [%d,%d], want newest-first [13,10]", got[0].Block, got[1].Block)
	}
}

func TestReserveBlock_Sequential(t *testing.T) {
	path := filepath.Join(t.TempDir(), "coord.jsonl")
	zero := func() (int, error) { return 0, nil }
	for i := 1; i <= 3; i++ {
		r := Record{ID: NewID(time.Now()), Window: "w", Repo: "r"}
		out, err := ReserveBlock(path, r, "resume.md", zero)
		if err != nil {
			t.Fatalf("reserve #%d: %v", i, err)
		}
		if out.Block != i {
			t.Errorf("reserve #%d → block %d, want %d", i, out.Block, i)
		}
		if out.Kind != KindBlockReserve || out.File != "resume.md" {
			t.Errorf("record shape wrong: %+v", out)
		}
	}
	// fileMax seed applies on the next allocation.
	r := Record{ID: NewID(time.Now()), Window: "w", Repo: "r"}
	out, _ := ReserveBlock(path, r, "resume.md", func() (int, error) { return 40, nil })
	if out.Block != 41 {
		t.Errorf("fileMax=40 → block %d, want 41", out.Block)
	}
}

// TestReserveBlock_ConcurrentNoDuplicates is the load-bearing test: the whole
// point of #23 is that concurrent allocators never grab the same id. flock must
// serialize the read-modify-write so G goroutines get exactly {1..G}.
func TestReserveBlock_ConcurrentNoDuplicates(t *testing.T) {
	path := filepath.Join(t.TempDir(), "coord.jsonl")
	const G = 20
	var wg sync.WaitGroup
	got := make([]int, G)
	zero := func() (int, error) { return 0, nil }
	for i := 0; i < G; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			r := Record{ID: NewID(time.Now()), Window: "w", Repo: "r"}
			out, err := ReserveBlock(path, r, "resume.md", zero)
			if err != nil {
				t.Errorf("goroutine %d: %v", i, err)
				return
			}
			got[i] = out.Block
		}(i)
	}
	wg.Wait()
	sort.Ints(got)
	for i := 0; i < G; i++ {
		if got[i] != i+1 {
			t.Fatalf("block ids not a contiguous unique 1..%d (duplicate/gap): %v", G, got)
		}
	}
}
