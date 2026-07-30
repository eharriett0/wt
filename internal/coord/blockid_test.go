package coord

import (
	"path/filepath"
	"sort"
	"sync"
	"testing"
	"time"
)

func TestNextBlock(t *testing.T) {
	if got := NextBlock(nil, "resume.md", 0); got != 1 {
		t.Errorf("empty → %d, want 1", got)
	}
	if got := NextBlock(nil, "resume.md", 55); got != 56 {
		t.Errorf("fileMax=55 seed → %d, want 56", got)
	}
	recs := []Record{
		{Kind: KindBlockReserve, File: "resume.md", Block: 3},
		{Kind: KindBlockReserve, File: "resume.md", Block: 7},
		{Kind: KindBlockReserve, File: "other.md", Block: 99}, // different file — ignored
		{Kind: KindAnnounce, File: "resume.md"},               // wrong kind — ignored
	}
	if got := NextBlock(recs, "resume.md", 0); got != 8 {
		t.Errorf("max reservation 7 → %d, want 8", got)
	}
	if got := NextBlock(recs, "resume.md", 20); got != 21 {
		t.Errorf("fileMax=20 beats reservation 7 → %d, want 21", got)
	}
	if got := NextBlock(recs, "other.md", 0); got != 100 {
		t.Errorf("per-file isolation: other.md → %d, want 100", got)
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
