package coord

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func writeLines(t *testing.T, path string, lines ...string) {
	t.Helper()
	if err := os.WriteFile(path, []byte(strings.Join(lines, "\n")+"\n"), 0o644); err != nil {
		t.Fatalf("write: %v", err)
	}
}

func ids(recs []Record) []string {
	out := make([]string, len(recs))
	for i, r := range recs {
		out[i] = r.ID
	}
	return out
}

func rec(kind, window, id, ackOf string, hold ...string) Record {
	return Record{ID: id, TS: "2026-07-18T12:00:00Z", Window: window, Repo: "eagle-valley", Kind: kind, AckOf: ackOf, Hold: hold}
}

func TestAppendLoadRoundTrip(t *testing.T) {
	path := filepath.Join(t.TempDir(), "sub", "eagle-valley.jsonl")
	in := []Record{
		{ID: "a1", TS: "2026-07-18T12:00:00Z", Window: "roll-nodes", Repo: "eagle-valley", Kind: KindAnnounce, Message: "AMI roll", Hold: []string{"merge-main"}, Issue: 1072},
		{ID: "k2", TS: "2026-07-18T12:01:00Z", Window: "dep-sweep", Repo: "eagle-valley", Kind: KindAck, AckOf: "a1", State: "read-only + PR #1073 held"},
	}
	for _, r := range in {
		if err := Append(path, r); err != nil {
			t.Fatalf("append: %v", err)
		}
	}
	got, err := Load(path)
	if err != nil {
		t.Fatalf("load: %v", err)
	}
	if len(got) != 2 || got[0].ID != "a1" || got[1].AckOf != "a1" || got[0].Issue != 1072 {
		t.Fatalf("round-trip mismatch: %+v", got)
	}
	if len(got[0].Hold) != 1 || got[0].Hold[0] != "merge-main" {
		t.Fatalf("hold not preserved: %+v", got[0].Hold)
	}
}

func TestLoadMissingFileIsEmpty(t *testing.T) {
	got, err := Load(filepath.Join(t.TempDir(), "nope.jsonl"))
	if err != nil || got != nil {
		t.Fatalf("missing file should be empty/no-error, got %v %v", got, err)
	}
}

func TestLoadSkipsMalformedLines(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "eagle-valley.jsonl")
	// One good line, one garbage line, one good line.
	writeLines(t, path,
		`{"id":"a1","kind":"announce","window":"w1"}`,
		`not json at all`,
		`{"id":"a2","kind":"announce","window":"w2"}`,
		`{"kind":"announce"}`, // no id → skipped
	)
	got, err := Load(path)
	if err != nil {
		t.Fatalf("load: %v", err)
	}
	if len(got) != 2 || got[0].ID != "a1" || got[1].ID != "a2" {
		t.Fatalf("malformed-skip mismatch: %+v", got)
	}
}

func TestInbox(t *testing.T) {
	recs := []Record{
		rec(KindAnnounce, "roll-nodes", "a1", ""),   // from other → should show
		rec(KindAnnounce, "dep-sweep", "self1", ""), // our own → hidden
		rec(KindAnnounce, "other", "a2", ""),        // from other, will be acked → hidden
		rec(KindAck, "dep-sweep", "x", "a2"),        // self acked a2
		rec(KindAnnounce, "other", "a3", ""),        // from other, will be all-cleared → hidden
		rec(KindAllClear, "other", "y", "a3"),
	}
	got := Inbox(recs, "dep-sweep")
	if len(got) != 1 || got[0].ID != "a1" {
		t.Fatalf("inbox should be [a1], got %+v", ids(got))
	}
}

func TestActiveHoldsBlocksUntilAckedOrCleared(t *testing.T) {
	base := []Record{
		{ID: "a1", Kind: KindAnnounce, Window: "roll-nodes", Hold: []string{"merge-main", "flux-reconcile"}},
	}
	// Un-acked hold covering merge-main → blocks.
	if h := ActiveHolds(base, "dep-sweep", "merge-main"); len(h) != 1 {
		t.Fatalf("expected 1 active hold, got %d", len(h))
	}
	// Op not covered → no block.
	if h := ActiveHolds(base, "dep-sweep", "kubectl-mutate:loki"); len(h) != 0 {
		t.Fatalf("uncovered op should not block, got %d", len(h))
	}
	// After self acks → cleared for self.
	acked := append(append([]Record{}, base...), Record{ID: "k", Kind: KindAck, Window: "dep-sweep", AckOf: "a1"})
	if h := ActiveHolds(acked, "dep-sweep", "merge-main"); len(h) != 0 {
		t.Fatalf("acked hold should not block self, got %d", len(h))
	}
	// After all-clear → cleared for everyone.
	clr := append(append([]Record{}, base...), Record{ID: "z", Kind: KindAllClear, Window: "roll-nodes", AckOf: "a1"})
	if h := ActiveHolds(clr, "dep-sweep", "merge-main"); len(h) != 0 {
		t.Fatalf("all-cleared hold should not block, got %d", len(h))
	}
	// A window never blocks on its OWN hold.
	if h := ActiveHolds(base, "roll-nodes", "merge-main"); len(h) != 0 {
		t.Fatalf("self hold should not block self, got %d", len(h))
	}
}

func TestPendingHolds(t *testing.T) {
	recs := []Record{
		rec(KindAnnounce, "roll-nodes", "a1", "", "merge-main"), // other + hold → pending
		rec(KindAnnounce, "docs", "a2", ""),                     // other, no hold → not pending
		rec(KindAnnounce, "self", "s1", "", "merge-main"),       // self → excluded
		rec(KindAnnounce, "other", "a3", "", "flux-reconcile"),  // will be acked → excluded
		rec(KindAck, "self", "k", "a3"),
	}
	got := PendingHolds(recs, "self")
	if len(got) != 1 || got[0].ID != "a1" {
		t.Fatalf("PendingHolds should be [a1], got %+v", ids(got))
	}
}

func TestHoldCovers(t *testing.T) {
	cases := []struct {
		hold []string
		op   string
		want bool
	}{
		{[]string{"merge-main"}, "merge-main", true},
		{[]string{"merge-main"}, "flux-reconcile", false},
		{[]string{"kubectl-mutate"}, "kubectl-mutate:harbor", true}, // family scope
		{[]string{"kubectl-mutate:harbor"}, "kubectl-mutate:loki", false},
		{[]string{"*"}, "anything", true},
		{nil, "merge-main", false},
	}
	for _, c := range cases {
		if got := HoldCovers(c.hold, c.op); got != c.want {
			t.Errorf("HoldCovers(%v, %q) = %v, want %v", c.hold, c.op, got, c.want)
		}
	}
}

func TestNewIDMonotonic(t *testing.T) {
	a := NewID(time.Unix(0, 1))
	b := NewID(time.Unix(0, 2))
	if a == b || len(a) == 0 {
		t.Fatalf("ids should differ and be non-empty: %q %q", a, b)
	}
}

func TestLogPathSlug(t *testing.T) {
	p := LogPath("/home/u", "timoniersystems/eagle-valley")
	if filepath.Base(p) != "timoniersystems-eagle-valley.jsonl" {
		t.Fatalf("unexpected slug path: %s", p)
	}
}

func TestWindowID(t *testing.T) {
	cases := []struct {
		name, env, toplevel, branch, want string
	}{
		{"env wins over everything", "roll-nodes", "/x/me-worktrees/feat-1", "feat-1", "roll-nodes"},
		{"env is trimmed", "  dep-sweep  ", "/x/me", "main", "dep-sweep"},
		{"no env -> FULL worktree path (not basename — over-relax fix)", "", "/Users/e/engineering/me-worktrees/feat-1591-carry", "feat-1591-carry", "/Users/e/engineering/me-worktrees/feat-1591-carry"},
		{"no env -> full main-checkout path", "", "/Users/e/engineering/me", "some-branch", "/Users/e/engineering/me"},
		// THE #18 FIX: same worktree dir, branch flipped (shared-checkout
		// contamination) -> identity is UNCHANGED, so the own-hold exemption fires.
		{"STABLE across branch flip: same toplevel diff branch", "", "/x/me-worktrees/feat-1", "OTHER-AFTER-CONTAMINATION", "/x/me-worktrees/feat-1"},
		{"trailing slash canonicalized", "", "/x/me-worktrees/feat-1/", "b", "/x/me-worktrees/feat-1"},
		{"no env, no toplevel -> branch fallback", "", "", "feat-9", "feat-9"},
		{"blank toplevel ignored -> branch", "", "   ", "feat-9", "feat-9"},
		{"nothing -> detached", "", "", "", "detached"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := WindowID(tc.env, tc.toplevel, tc.branch); got != tc.want {
				t.Errorf("WindowID(%q,%q,%q) = %q, want %q", tc.env, tc.toplevel, tc.branch, got, tc.want)
			}
		})
	}
}

// TestSelfBlockFixedByStableIdentity is the end-to-end #15/#18 acceptance: a
// window announces a merge-main hold, then its branch flips before it merges.
// With a STABLE identity the coordinator is NOT blocked on its own hold, while a
// genuinely different window still IS. (Guards against over-relaxing the block.)
func TestSelfBlockFixedByStableIdentity(t *testing.T) {
	top := "/x/me-worktrees/roll"
	selfAtAnnounce := WindowID("", top, "roll")
	selfAtMerge := WindowID("", top, "some-other-branch-after-flip") // branch changed, dir didn't
	if selfAtAnnounce != selfAtMerge {
		t.Fatalf("identity drifted across branch flip: %q != %q", selfAtAnnounce, selfAtMerge)
	}
	recs := []Record{
		{ID: "a1", Kind: KindAnnounce, Window: selfAtAnnounce, Hold: []string{"merge-main"}},
	}
	if h := ActiveHolds(recs, selfAtMerge, "merge-main"); len(h) != 0 {
		t.Fatalf("coordinator self-blocked on its OWN hold: got %d", len(h))
	}
	other := WindowID("", "/x/me-worktrees/dep-sweep", "dep-sweep")
	if h := ActiveHolds(recs, other, "merge-main"); len(h) != 1 {
		t.Fatalf("a genuinely different window must still be blocked: got %d", len(h))
	}
	// WT_WINDOW override also yields a stable, self-exempting identity across dirs.
	pinned := WindowID("roll-coordinator", "/anywhere/else", "any-branch")
	recs2 := []Record{{ID: "a2", Kind: KindAnnounce, Window: WindowID("roll-coordinator", top, "roll"), Hold: []string{"merge-main"}}}
	if h := ActiveHolds(recs2, pinned, "merge-main"); len(h) != 0 {
		t.Fatalf("WT_WINDOW-pinned coordinator self-blocked across dirs: got %d", len(h))
	}
}

// TestDistinctTreesSameBasenameDoNotCollapse pins the over-relax fix from the
// 2026-07-23 adversarial pass: two DIFFERENT working trees that share a dir leaf
// name (e.g. two `git clone`s both named "me" on different branches, which share
// one coordination log) must NOT collapse to a single identity — otherwise one
// silently bypasses the other's merge-main hold. Using the FULL path (not its
// basename) keeps them distinct.
func TestDistinctTreesSameBasenameDoNotCollapse(t *testing.T) {
	a := WindowID("", "/Users/e/engineering/me", "feat-x")
	b := WindowID("", "/Users/e/review/me", "feat-y")
	if a == b {
		t.Fatalf("distinct trees collapsed to one identity %q — over-relax regression", a)
	}
	// clone B must still be blocked by clone A's merge-main hold.
	recs := []Record{{ID: "h", Kind: KindAnnounce, Window: a, Hold: []string{"merge-main"}}}
	if h := ActiveHolds(recs, b, "merge-main"); len(h) != 1 {
		t.Fatalf("clone B must be blocked by clone A's hold, got %d", len(h))
	}
	// main checkout vs a worktree that happens to share the leaf name: distinct.
	main := WindowID("", "/Users/e/engineering/me", "main")
	wt := WindowID("", "/Users/e/engineering/me-worktrees/me", "me")
	if main == wt {
		t.Fatalf("main checkout and worktree collapsed to %q", main)
	}
}

func TestActiveHoldsAt(t *testing.T) {
	now := time.Date(2026, 7, 31, 12, 0, 0, 0, time.UTC)
	mk := func(id, win string, ageH int) Record {
		return Record{ID: id, Kind: KindAnnounce, Window: win, Hold: []string{"merge-main"},
			TS: now.Add(-time.Duration(ageH) * time.Hour).Format(time.RFC3339)}
	}
	recs := []Record{mk("a", "winA", 1), mk("b", "winB", 48)} // a fresh, b 2 days old
	fresh, stale := ActiveHoldsAt(recs, "self", "merge-main", now, 24*time.Hour)
	if len(fresh) != 1 || fresh[0].ID != "a" {
		t.Errorf("fresh = %+v, want [a]", fresh)
	}
	if len(stale) != 1 || stale[0].ID != "b" {
		t.Errorf("stale = %+v, want [b]", stale)
	}
	// maxAge 0 disables expiry — everything fresh (pre-#32 behavior)
	f0, s0 := ActiveHoldsAt(recs, "self", "merge-main", now, 0)
	if len(f0) != 2 || len(s0) != 0 {
		t.Errorf("maxAge=0: fresh=%d stale=%d, want 2/0", len(f0), len(s0))
	}
}

func TestOwnOpenAnnouncements(t *testing.T) {
	recs := []Record{
		{ID: "1", Kind: KindAnnounce, Window: "me"},
		{ID: "2", Kind: KindAnnounce, Window: "other"},            // not mine
		{ID: "3", Kind: KindAnnounce, Window: "me"},               // mine, will be cleared
		{ID: "x", Kind: KindAllClear, Window: "me", AckOf: "3"},   // clears 3
		{ID: "4", Kind: KindBlockReserve, Window: "me", Block: 5}, // not an announcement
	}
	own := OwnOpenAnnouncements(recs, "me")
	if len(own) != 1 || own[0].ID != "1" {
		t.Errorf("OwnOpenAnnouncements = %+v, want just id 1 (2=other, 3=cleared)", own)
	}
	res := OwnBlockReservations(recs, "me")
	if len(res) != 1 || res[0].Block != 5 {
		t.Errorf("OwnBlockReservations = %+v, want [block 5]", res)
	}
}
