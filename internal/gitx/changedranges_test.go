package gitx

import (
	"path/filepath"
	"reflect"
	"strings"
	"testing"
)

func TestParseInsertedRangesNew(t *testing.T) {
	diff := strings.Join([]string{
		"@@ -15,0 +16,5 @@ ctx",   // pure insertion → new range 16-20
		"@@ -3 +3 @@ ctx",         // modify (old count 1) → excluded
		"@@ -100,40 +99,0 @@ ctx", // deletion (new count 0) → excluded
		"@@ -0,0 +1,3 @@ ctx",     // insertion at file start → new range 1-3
		"@@ -50,2 +51,4 @@ ctx",   // replace (old count 2) → NOT a pure insertion → excluded
	}, "\n")
	got := parseInsertedRangesNew(diff)
	want := []LineRange{{16, 20}, {1, 3}}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("parseInsertedRangesNew = %v, want %v", got, want)
	}
}

func TestSubtractRanges(t *testing.T) {
	// #142: a fully-contained phantom range (== a base insertion) is removed; a
	// disjoint real edit is kept untouched.
	r := []LineRange{{3, 3}, {16, 20}, {30, 30}}
	if got, want := subtractRanges(r, []LineRange{{16, 20}}), []LineRange{{3, 3}, {30, 30}}; !reflect.DeepEqual(got, want) {
		t.Errorf("subtractRanges (contained) = %v, want %v", got, want)
	}
	// #142 REVIEW: a span that STRADDLES a g span must be SPLIT, not dropped —
	// git -U0 folds a real edit adjacent to a base block into one hunk, so
	// dropping the whole span would silently lose the real edit.
	if got, want := subtractRanges([]LineRange{{5, 10}}, []LineRange{{8, 12}}), []LineRange{{5, 7}}; !reflect.DeepEqual(got, want) {
		t.Errorf("subtractRanges (overlap tail) = %v, want %v", got, want)
	}
	if got, want := subtractRanges([]LineRange{{16, 21}}, []LineRange{{16, 20}}), []LineRange{{21, 21}}; !reflect.DeepEqual(got, want) {
		t.Errorf("subtractRanges (edit right after block) = %v, want %v", got, want)
	}
	if got, want := subtractRanges([]LineRange{{15, 21}}, []LineRange{{16, 20}}), []LineRange{{15, 15}, {21, 21}}; !reflect.DeepEqual(got, want) {
		t.Errorf("subtractRanges (straddle both ends) = %v, want %v", got, want)
	}
	// multiple g spans carve one r span into several
	if got, want := subtractRanges([]LineRange{{1, 30}}, []LineRange{{5, 10}, {20, 25}}), []LineRange{{1, 4}, {11, 19}, {26, 30}}; !reflect.DeepEqual(got, want) {
		t.Errorf("subtractRanges (multi-g) = %v, want %v", got, want)
	}
	// empty g → unchanged (up-to-date branch)
	if got, want := subtractRanges(r, nil), r; !reflect.DeepEqual(got, want) {
		t.Errorf("subtractRanges(_, nil) = %v, want %v", got, want)
	}
}

// #142: a branch that is BEHIND the base must NOT have the base's newly-added
// block attributed to it as changed lines. ChangedRanges (base frame) must
// return only the branch's real edit, never the phantom deletion of base-gained
// content — otherwise an unrelated window editing that block falsely collides.
func TestChangedRanges_StaleBranchExcludesBaseInsertion(t *testing.T) {
	dir := gitRepo(t)
	// 20-line base file.
	var b strings.Builder
	for i := 1; i <= 20; i++ {
		b.WriteString("line")
		b.WriteByte(byte('0' + i/10))
		b.WriteByte(byte('0' + i%10))
		b.WriteByte('\n')
	}
	writeFile(t, dir, "data.txt", b.String())
	runGit(t, dir, "add", "data.txt")
	runGit(t, dir, "commit", "-qm", "base")

	// branch-a edits ONLY line 3, then main advances with a NEW 5-line block.
	runGit(t, dir, "switch", "-qc", "branch-a")
	edited := strings.Replace(b.String(), "line03\n", "line03 EDITED\n", 1)
	writeFile(t, dir, "data.txt", edited)
	runGit(t, dir, "commit", "-qam", "A edits line 3")
	runGit(t, dir, "switch", "-q", "main")
	lines := strings.SplitAfter(b.String(), "\n")
	// insert a 5-line block after line 15 (base line 15 → new lines 16-20)
	block := "new01\nnew02\nnew03\nnew04\nnew05\n"
	withBlock := strings.Join(lines[:15], "") + block + strings.Join(lines[15:], "")
	writeFile(t, dir, "data.txt", withBlock)
	runGit(t, dir, "commit", "-qam", "main adds a 5-line block after A's base")

	// branch-a in its own worktree (ChangedRanges reads a worktree dir).
	wt := filepath.Join(t.TempDir(), "wt-a")
	runGit(t, dir, "worktree", "add", "-q", wt, "branch-a")

	got := ChangedRanges(wt, "main", "data.txt")
	want := []LineRange{{3, 3}}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("ChangedRanges(stale branch-a) = %v, want %v — the base's inserted block (base lines 16-20) must NOT be attributed to A", got, want)
	}
}

func covers(rs []LineRange, line int) bool {
	for _, r := range rs {
		if r.Start <= line && line <= r.End {
			return true
		}
	}
	return false
}

// twentyLineFile writes line01..line20 and returns the content.
func twentyLineFile(t *testing.T, dir string) string {
	t.Helper()
	var b strings.Builder
	for i := 1; i <= 20; i++ {
		b.WriteString("line")
		b.WriteByte(byte('0' + i/10))
		b.WriteByte(byte('0' + i%10))
		b.WriteByte('\n')
	}
	writeFile(t, dir, "data.txt", b.String())
	return b.String()
}

// #142 REVIEW (the defect the adversarial pass caught): a branch edit DIRECTLY
// ADJACENT to the base's inserted block must survive. git -U0 folds the phantom
// block deletion and the adjacent edit into ONE hunk; the fix must SPLIT it and
// keep the real edit, not drop the whole span (which would silently miss a real
// overlap in the exact shared-manifest scenario).
func TestChangedRanges_AdjacentEditKept(t *testing.T) {
	dir := gitRepo(t)
	base := twentyLineFile(t, dir)
	runGit(t, dir, "add", "data.txt")
	runGit(t, dir, "commit", "-qm", "base")

	// branch-a edits line 16 — the line immediately after where main will insert.
	runGit(t, dir, "switch", "-qc", "branch-a")
	writeFile(t, dir, "data.txt", strings.Replace(base, "line16\n", "line16 EDITED\n", 1))
	runGit(t, dir, "commit", "-qam", "A edits line 16 (adjacent to the future block)")
	runGit(t, dir, "switch", "-q", "main")

	lines := strings.SplitAfter(base, "\n")
	withBlock := strings.Join(lines[:15], "") + "new01\nnew02\nnew03\nnew04\nnew05\n" + strings.Join(lines[15:], "")
	writeFile(t, dir, "data.txt", withBlock)
	runGit(t, dir, "commit", "-qam", "main inserts a 5-line block after line 15")

	wt := filepath.Join(t.TempDir(), "wt-a")
	runGit(t, dir, "worktree", "add", "-q", wt, "branch-a")

	got := ChangedRanges(wt, "main", "data.txt")
	if len(got) == 0 {
		t.Fatalf("ChangedRanges = [] — A's real edit adjacent to the base block was DROPPED (the #142-review false negative)")
	}
	// the edited content ("line16") sits at base-frame line 21 (after the block);
	// the phantom block lines (16-20) must NOT be attributed to A.
	if !covers(got, 21) {
		t.Errorf("ChangedRanges = %v, want a range covering line 21 (A's real edit)", got)
	}
	if covers(got, 18) {
		t.Errorf("ChangedRanges = %v, still attributes a base-block line (18) to A", got)
	}
}

// #142 REVIEW (secondary): base adds a NEW file and the branch independently
// adds its OWN version of the same file (both absent at the merge-base) — a real
// add/add conflict. baseInsertedRanges must NOT treat the whole file as
// phantom-the-branch-lacks (the merge-base has no such file), so the branch's
// changes are kept and the conflict stays detectable.
func TestChangedRanges_AddAddNotSuppressed(t *testing.T) {
	dir := gitRepo(t)
	writeFile(t, dir, "seed.txt", "seed\n")
	runGit(t, dir, "add", "seed.txt")
	runGit(t, dir, "commit", "-qm", "base (no shared.txt)")

	runGit(t, dir, "switch", "-qc", "branch-a")
	writeFile(t, dir, "shared.txt", "a1\na2\na3\n")
	runGit(t, dir, "add", "shared.txt")
	runGit(t, dir, "commit", "-qm", "A adds shared.txt")
	runGit(t, dir, "switch", "-q", "main")
	writeFile(t, dir, "shared.txt", "x1\nx2\nx3\n")
	runGit(t, dir, "add", "shared.txt")
	runGit(t, dir, "commit", "-qm", "main adds a DIFFERENT shared.txt")

	wt := filepath.Join(t.TempDir(), "wt-a2")
	runGit(t, dir, "worktree", "add", "-q", wt, "branch-a")

	if got := ChangedRanges(wt, "main", "shared.txt"); len(got) == 0 {
		t.Errorf("ChangedRanges = [] — an add/add conflict was suppressed (base-inserted != content-the-branch-lacks when the file is new since the merge-base)")
	}
}
