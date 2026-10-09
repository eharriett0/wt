package gitx

import (
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
)

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
	want := []LineRange{span(3, 3)}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("ChangedRanges(stale branch-a) = %v, want %v — the base's inserted block (base lines 16-20) must NOT be attributed to A", got, want)
	}
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
// overlap in the exact shared-manifest scenario). #184 review: and because the
// block was inserted right above the edited line, with no unchanged line between,
// git merges the two as ONE conflict region, so the block is part of A's range:
// a window editing a block line conflicts with A in git merge-tree too.
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
	// the edited content ("line16") sits at base-frame line 21 (after the block),
	// and the block (base 16-20) touches it, so the conflict region is 16-21.
	if want := []LineRange{span(16, 21)}; !reflect.DeepEqual(got, want) {
		t.Errorf("ChangedRanges = %v, want %v (A's edit at base line 21 plus the block git merges into its conflict)", got, want)
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

func TestParseHunks(t *testing.T) {
	diff := strings.Join([]string{
		"diff --git a/f b/f",
		"--- a/f",
		"+++ b/f",
		"@@ -3 +3 @@ ctx", // modify; both counts omitted → 1
		"-old",
		"+new",
		"@@ -15,0 +16,5 @@",       // pure insertion after old line 15
		"@@ -100,40 +99,0 @@ ctx", // pure deletion, gap after new line 99
		"@@ -0,0 +1,3 @@",         // insertion at the very top
		"@@ -1,3 +0,0 @@",         // deletion at the very top
		"@@ -50,2 +51,4 @@",       // unequal replace
		"+@@ -7 +7 @@ a CONTENT line, not a header",
	}, "\n")
	want := []diffHunk{{3, 1, 3, 1}, {15, 0, 16, 5}, {100, 40, 99, 0}, {0, 0, 1, 3}, {1, 3, 0, 0}, {50, 2, 51, 4}}
	if got := parseHunks(diff); !reflect.DeepEqual(got, want) {
		t.Errorf("parseHunks = %v, want %v", got, want)
	}
	if got := parseHunks(""); got != nil {
		t.Errorf("parseHunks(\"\") = %v, want nil", got)
	}
}

// mapHunksToBase is the #184 core: a behind branch's OWN hunks, in merge-base
// line numbers, moved into base line numbers through base's hunks since the
// merge base (old side = merge-base frame, new side = base frame). The own side
// is hunks, not ranges, because git's conflict rule tells an insertion from a
// two-line change: an insertion only meets changes touching its gap. So is the
// result (#199): a change, or a Gap where the branch's edit is an insertion, or
// where base deleted every line the branch changed.
func TestMapHunksToBase(t *testing.T) {
	mod := func(start, n int) diffHunk { return diffHunk{oldStart: start, oldCount: n} } // branch changed n lines from start
	ins := func(after int) diffHunk { return diffHunk{oldStart: after} }                 // branch inserted after line `after`
	var (
		ins5x3  = diffHunk{5, 0, 6, 3} // base inserted 3 lines after line 5 (base 6-8)
		insTop2 = diffHunk{0, 0, 1, 2} // base inserted 2 lines above line 1
		mod3    = diffHunk{3, 1, 3, 1} // base rewrote line 3 in place
		grow3   = diffHunk{3, 1, 3, 3} // base replaced line 3 with 3 lines (base 3-5)
		shrink  = diffHunk{3, 3, 3, 1} // base replaced lines 3-5 with 1 line (base 3)
		del2to4 = diffHunk{2, 3, 1, 0} // base deleted lines 2-4 (gap after base 1)
		delTop2 = diffHunk{1, 2, 0, 0} // base deleted lines 1-2 (gap above base 1)
		multi   = []diffHunk{{2, 0, 3, 4}, {5, 2, 9, 1}, {8, 1, 11, 1}, {12, 0, 16, 2}}
	)
	cases := []struct {
		name  string
		hunks []diffHunk
		own   []diffHunk
		want  []LineRange
	}{
		{"base never touched the file: identity", nil, []diffHunk{mod(7, 1), mod(10, 3)}, []LineRange{span(7, 7), span(10, 12)}},
		{"base insert above shifts the line down", []diffHunk{ins5x3}, []diffHunk{mod(10, 1)}, []LineRange{span(13, 13)}},
		{"base insert below leaves the line", []diffHunk{{20, 0, 21, 3}}, []diffHunk{mod(10, 1)}, []LineRange{span(10, 10)}},
		{"base insert at the top shifts line 2", []diffHunk{insTop2}, []diffHunk{mod(2, 1)}, []LineRange{span(4, 4)}},
		{"base same-length modify above: no shift", []diffHunk{mod3}, []diffHunk{mod(10, 1)}, []LineRange{span(10, 10)}},
		{"base growing modify above shifts by +2", []diffHunk{grow3}, []diffHunk{mod(10, 1)}, []LineRange{span(12, 12)}},
		{"base shrinking modify above shifts by -2", []diffHunk{shrink}, []diffHunk{mod(10, 1)}, []LineRange{span(8, 8)}},
		{"base delete above shifts the line up", []diffHunk{del2to4}, []diffHunk{mod(10, 1)}, []LineRange{span(7, 7)}},
		{"base delete at the top shifts the line up", []diffHunk{delTop2}, []diffHunk{mod(10, 1)}, []LineRange{span(8, 8)}},
		{"line after the last hunk takes every shift", []diffHunk{grow3, {10, 0, 13, 1}}, []diffHunk{mod(20, 1)}, []LineRange{span(23, 23)}},
		// A line BOTH changed must keep overlapping base's version of it: that is
		// a real 3-way conflict and must never be mapped away.
		{"both modified the line: lands on base's rewrite", []diffHunk{mod3}, []diffHunk{mod(3, 1)}, []LineRange{span(3, 3)}},
		{"both modified, base grew it: whole replacement", []diffHunk{grow3}, []diffHunk{mod(3, 1)}, []LineRange{span(3, 5)}},
		{"both modified, base collapsed the block: its 1 line", []diffHunk{shrink}, []diffHunk{mod(4, 1)}, []LineRange{span(3, 3)}},
		// Base deleted every line the branch changed: no line is left to claim, so
		// the claim is base's deletion point (a window touching either neighbour
		// merges with that deletion), not both neighbours as lines.
		{"branch edited a line base deleted: the deletion point", []diffHunk{del2to4}, []diffHunk{mod(3, 1)}, []LineRange{gapAt(1)}},
		{"branch edited a line base deleted at the top", []diffHunk{delTop2}, []diffHunk{mod(2, 1)}, []LineRange{gapAt(0)}},
		// A branch range spans its ends, so base content inserted INSIDE a branch
		// hunk stays covered.
		{"branch range straddling a base insert covers it", []diffHunk{ins5x3}, []diffHunk{mod(4, 4)}, []LineRange{span(4, 10)}},
		{"branch range across a base-modified line", []diffHunk{grow3}, []diffHunk{mod(2, 3)}, []LineRange{span(2, 6)}},
		// The branch's surviving lines 5-6 land on base 2-3; base's deletion point
		// (after base 1) is at their edge, so a window editing base line 1 still
		// conflicts with the claim, and base line 1 itself isn't claimed (#199).
		{"branch range starting in a base deletion", []diffHunk{del2to4}, []diffHunk{mod(3, 4)}, []LineRange{span(2, 3)}},
		{"branch range ending in a base deletion", []diffHunk{{7, 3, 6, 0}}, []diffHunk{mod(5, 3)}, []LineRange{span(5, 6)}},
		// Base changes that TOUCH a branch hunk with no unchanged line between are
		// the same conflict region in a git merge (#184 review): the branch claims
		// base's replacement there too.
		{"the chain: branch l11, base rewrites l12-13", []diffHunk{{12, 2, 12, 2}}, []diffHunk{mod(11, 1)}, []LineRange{span(11, 13)}},
		{"base rewrites the lines just above", []diffHunk{{9, 2, 9, 2}}, []diffHunk{mod(11, 1)}, []LineRange{span(9, 11)}},
		// Base's deletion point (after base 11) is at the line's edge: a window
		// editing base line 12 touches the claim, one editing line 13 doesn't.
		{"base deletes the lines just below", []diffHunk{{12, 2, 11, 0}}, []diffHunk{mod(11, 1)}, []LineRange{span(11, 11)}},
		{"one unchanged line between: no claim", []diffHunk{{13, 1, 13, 1}}, []diffHunk{mod(11, 1)}, []LineRange{span(11, 11)}},
		{"base insert right AFTER the line: claims the block", []diffHunk{{10, 0, 11, 3}}, []diffHunk{mod(10, 1)}, []LineRange{span(10, 13)}},
		{"base insert right BEFORE the line: claims the block", []diffHunk{{9, 0, 10, 3}}, []diffHunk{mod(10, 1)}, []LineRange{span(10, 13)}},
		{"base insert above line 1, branch edits line 1", []diffHunk{insTop2}, []diffHunk{mod(1, 1)}, []LineRange{span(1, 3)}},
		{"several base hunks accumulate", multi,
			[]diffHunk{mod(1, 1), mod(4, 1), mod(7, 1), mod(8, 1), mod(10, 1), mod(13, 1)},
			[]LineRange{span(1, 1), span(8, 9), span(9, 11), span(11, 11), span(13, 13), span(16, 18)}},
		// A branch INSERTION no base hunk meets stays an insertion (a Gap) at its
		// gap in base numbering. One that meets a base hunk sits inside or at the
		// edge of base's text there and claims exactly that text: a window touching
		// it merges with base's hunk, which conflicts with the insertion (#199: the
		// gap's neighbours as lines would claim one line too many each side).
		{"branch insert at a base insert point claims base's block", []diffHunk{ins5x3}, []diffHunk{ins(5)}, []LineRange{span(6, 8)}},
		{"branch insert at the top vs a base insert at the top", []diffHunk{insTop2}, []diffHunk{ins(0)}, []LineRange{span(1, 2)}},
		{"branch insert at the top vs base rewriting line 1", []diffHunk{{1, 1, 1, 1}}, []diffHunk{ins(0)}, []LineRange{span(1, 1)}},
		{"branch insert just above a base rewrite", []diffHunk{{8, 1, 8, 2}}, []diffHunk{ins(7)}, []LineRange{span(8, 9)}},
		{"branch insert just below a base deletion: its point", []diffHunk{{5, 2, 4, 0}}, []diffHunk{ins(6)}, []LineRange{gapAt(4)}},
		{"branch insert below a base insert shifts", []diffHunk{insTop2}, []diffHunk{ins(7)}, []LineRange{gapAt(9)}},
		{"branch insert one line clear of a base rewrite: no claim", []diffHunk{{8, 1, 8, 1}}, []diffHunk{ins(6)}, []LineRange{gapAt(6)}},
		{"branch insert at the top, base untouched", nil, []diffHunk{ins(0)}, []LineRange{gapAt(0)}},
		{"a two-line branch change there touches it: claims", []diffHunk{{8, 1, 8, 1}}, []diffHunk{mod(6, 2)}, []LineRange{span(6, 8)}},
	}
	for _, c := range cases {
		if got := mapHunksToBase(c.own, c.hunks); !reflect.DeepEqual(got, c.want) {
			t.Errorf("%s: mapHunksToBase(%v, %v) = %v, want %v", c.name, c.own, c.hunks, got, c.want)
		}
	}
	if got := mapHunksToBase(nil, []diffHunk{mod3}); len(got) != 0 {
		t.Errorf("no own hunks → %v, want none", got)
	}
}

// hunksConflict is git's 3-way rule (xdl_merge): two changes to the same old file
// stay apart only when an unchanged line separates them.
func TestHunksConflict(t *testing.T) {
	cases := []struct {
		name string
		a, b diffHunk
		want bool
	}{
		{"same line", diffHunk{5, 1, 0, 0}, diffHunk{5, 1, 0, 0}, true},
		{"overlapping spans", diffHunk{5, 3, 0, 0}, diffHunk{7, 3, 0, 0}, true},
		{"adjacent: b right below a", diffHunk{5, 2, 0, 0}, diffHunk{7, 1, 0, 0}, true},
		{"adjacent: b right above a", diffHunk{5, 2, 0, 0}, diffHunk{4, 1, 0, 0}, true},
		{"one line between below", diffHunk{5, 2, 0, 0}, diffHunk{8, 1, 0, 0}, false},
		{"one line between above", diffHunk{5, 2, 0, 0}, diffHunk{3, 1, 0, 0}, false},
		{"insert after the change's last line", diffHunk{7, 0, 0, 0}, diffHunk{5, 3, 0, 0}, true},
		{"insert before the change's first line", diffHunk{4, 0, 0, 0}, diffHunk{5, 3, 0, 0}, true},
		{"insert inside the change", diffHunk{6, 0, 0, 0}, diffHunk{5, 3, 0, 0}, true},
		{"insert one line clear below", diffHunk{8, 0, 0, 0}, diffHunk{5, 3, 0, 0}, false},
		{"insert one line clear above", diffHunk{3, 0, 0, 0}, diffHunk{5, 3, 0, 0}, false},
		{"change vs insert, argument order swapped", diffHunk{5, 3, 0, 0}, diffHunk{7, 0, 0, 0}, true},
		{"two inserts at one gap", diffHunk{5, 0, 0, 0}, diffHunk{5, 0, 0, 0}, true},
		{"two inserts at neighbouring gaps", diffHunk{5, 0, 0, 0}, diffHunk{6, 0, 0, 0}, false},
		{"insert at the top vs a change of line 1", diffHunk{0, 0, 0, 0}, diffHunk{1, 1, 0, 0}, true},
	}
	for _, c := range cases {
		if got := hunksConflict(c.a, c.b); got != c.want {
			t.Errorf("%s: hunksConflict(%v, %v) = %v, want %v", c.name, c.a, c.b, got, c.want)
		}
	}
}

func anyOverlap(a, b []LineRange) bool {
	for _, ra := range a {
		for _, rb := range b {
			if ra.Overlaps(rb) {
				return true
			}
		}
	}
	return false
}

// staleScenario builds the #184 shape: branch-a forks from a 20-line data.txt and
// applies editA; main then applies editMain; branch-b starts from that main and
// applies editB (nil: no branch-b). Returns the worktrees of a (behind main) and
// b (current with main; "" when editB is nil).
func staleScenario(t *testing.T, editA, editMain, editB func(string) string) (wtA, wtB string) {
	t.Helper()
	dir := gitRepo(t)
	base := twentyLineFile(t, dir)
	runGit(t, dir, "add", "data.txt")
	runGit(t, dir, "commit", "-qm", "base")

	runGit(t, dir, "switch", "-qc", "branch-a")
	writeFile(t, dir, "data.txt", editA(base))
	runGit(t, dir, "commit", "-qam", "A's edit")
	runGit(t, dir, "switch", "-q", "main")
	moved := editMain(base)
	writeFile(t, dir, "data.txt", moved)
	runGit(t, dir, "commit", "-qam", "main changes data.txt after A forked")

	if editB != nil {
		wtB = filepath.Join(t.TempDir(), "wt-b")
		runGit(t, dir, "worktree", "add", "-q", "-b", "branch-b", wtB, "main")
		writeFile(t, wtB, "data.txt", editB(moved))
		runGit(t, wtB, "commit", "-qam", "B's edit")
	}

	wtA = filepath.Join(t.TempDir(), "wt-a")
	runGit(t, dir, "worktree", "add", "-q", wtA, "branch-a")
	return wtA, wtB
}

func replaceLine(old, new string) func(string) string {
	return func(s string) string { return strings.Replace(s, old+"\n", new+"\n", 1) }
}

func chain(fs ...func(string) string) func(string) string {
	return func(s string) string {
		for _, f := range fs {
			s = f(s)
		}
		return s
	}
}

// #184 (the #142 sibling): a branch BEHIND base still holds the OLD text of lines
// base MODIFIED after it forked, so the base-frame diff attributed those lines to
// it and any window editing one "overlapped" a branch that never touched it: a
// false HIGH that blocked the pre-push hook while git merge-tree was clean.
// Measured from the merge base, A owns exactly its one edit.
func TestChangedRanges_StaleBranchExcludesBaseModification(t *testing.T) {
	mainEdit := chain(replaceLine("line10", "line10 changed-on-main"), replaceLine("line15", "line15 changed-on-main"))
	wtA, wtB := staleScenario(t,
		replaceLine("line03", "line03 EDITED_BY_A"),
		mainEdit,
		replaceLine("line10 changed-on-main", "line10 EDITED_BY_B"))

	a := ChangedRanges(wtA, "main", "data.txt")
	if want := []LineRange{span(3, 3)}; !reflect.DeepEqual(a, want) {
		t.Errorf("ChangedRanges(stale branch-a) = %v, want %v: lines main modified after A forked (10, 15) must NOT be attributed to A", a, want)
	}
	b := ChangedRanges(wtB, "main", "data.txt")
	if want := []LineRange{span(10, 10)}; !reflect.DeepEqual(b, want) {
		t.Fatalf("precondition: ChangedRanges(branch-b) = %v, want %v", b, want)
	}
	if anyOverlap(a, b) {
		t.Errorf("A %v overlaps B %v on a line only main and B edited (the #184 phantom)", a, b)
	}
}

// #184 control: when the stale branch ALSO edited a line base modified, that is
// a real 3-way conflict and it must keep overlapping a window editing that line.
func TestChangedRanges_StaleBranchSameLineStillOverlaps(t *testing.T) {
	wtA, wtB := staleScenario(t,
		chain(replaceLine("line03", "line03 EDITED_BY_A"), replaceLine("line10", "line10 EDITED_BY_A")),
		chain(replaceLine("line10", "line10 changed-on-main"), replaceLine("line15", "line15 changed-on-main")),
		replaceLine("line10 changed-on-main", "line10 EDITED_BY_B"))

	a := ChangedRanges(wtA, "main", "data.txt")
	if want := []LineRange{span(3, 3), span(10, 10)}; !reflect.DeepEqual(a, want) {
		t.Errorf("ChangedRanges(stale branch-a) = %v, want %v", a, want)
	}
	if b := ChangedRanges(wtB, "main", "data.txt"); !anyOverlap(a, b) {
		t.Errorf("A %v does NOT overlap B %v, but both edited line 10: a real conflict was hidden", a, b)
	}
}

// #184: a stale branch's edits are reported in BASE line numbers, not its own. Base
// deleted lines 2-4 after A forked, so A's edit of "line12" sits at base line 9,
// exactly where a current window editing "line12" sees it. Left in merge-base
// numbers (12) the two would grade disjoint and hide a real conflict.
func TestChangedRanges_StaleBranchMapsAcrossBaseDeletion(t *testing.T) {
	wtA, wtB := staleScenario(t,
		replaceLine("line12", "line12 EDITED_BY_A"),
		func(s string) string { return strings.Replace(s, "line02\nline03\nline04\n", "", 1) },
		replaceLine("line12", "line12 EDITED_BY_B"))

	a := ChangedRanges(wtA, "main", "data.txt")
	if want := []LineRange{span(9, 9)}; !reflect.DeepEqual(a, want) {
		t.Errorf("ChangedRanges(stale branch-a) = %v, want %v (A's edit, in base line numbers; no phantom for base's deletion)", a, want)
	}
	if b := ChangedRanges(wtB, "main", "data.txt"); !anyOverlap(a, b) {
		t.Errorf("A %v does NOT overlap B %v, but both edited \"line12\"", a, b)
	}
}

// #184: measuring from the merge base still counts a stale branch's UNCOMMITTED
// work (staged and unstaged), as the base diff always did, mapped like the rest.
func TestChangedRanges_StaleBranchCountsUncommittedEdits(t *testing.T) {
	// main inserts 2 lines at the top (shifts everything by 2) and rewrites line 10.
	mainEdit := chain(func(s string) string { return "top1\ntop2\n" + s }, replaceLine("line10", "line10 changed-on-main"))
	wtA, _ := staleScenario(t, replaceLine("line03", "line03 EDITED_BY_A"), mainEdit, nil)

	cur, err := os.ReadFile(filepath.Join(wtA, "data.txt"))
	if err != nil {
		t.Fatal(err)
	}
	writeFile(t, wtA, "data.txt", replaceLine("line07", "line07 STAGED")(string(cur)))
	runGit(t, wtA, "add", "data.txt")
	cur, _ = os.ReadFile(filepath.Join(wtA, "data.txt"))
	writeFile(t, wtA, "data.txt", replaceLine("line17", "line17 UNSTAGED")(string(cur)))

	got := ChangedRanges(wtA, "main", "data.txt")
	if want := []LineRange{span(5, 5), span(9, 9), span(19, 19)}; !reflect.DeepEqual(got, want) {
		t.Errorf("ChangedRanges = %v, want %v (committed line 3, staged line 7, unstaged line 17, each +2 in base; main's line 10 is not A's)", got, want)
	}
}

// #184, new frame: section attribution (#22) must also see only the branch's OWN
// edits, at their CURRENT-file line numbers. Base-frame diffing attributed the old
// text a stale branch holds (and the gap where base inserted) to the branch, so a
// structured-doc section only base had touched graded "same section" HIGH.
func TestChangedRangesNew_StaleBranchOwnLinesOnly(t *testing.T) {
	mainEdit := chain(replaceLine("line10", "line10 changed-on-main"), func(s string) string {
		return strings.Replace(s, "line15\n", "line15\nins1\nins2\n", 1)
	})
	wtA, _ := staleScenario(t, replaceLine("line03", "line03 EDITED_BY_A"), mainEdit, nil)

	if got, want := ChangedRangesNew(wtA, "main", "data.txt"), []LineRange{span(3, 3)}; !reflect.DeepEqual(got, want) {
		t.Errorf("ChangedRangesNew(stale branch-a) = %v, want %v: main's rewrite of line 10 and its insert after 15 are not A's", got, want)
	}
}

// #184 keeps today's grade for a file absent at the merge base (base and the
// branch each added their own, the #142-review add/add): the plain base diff,
// which compares the two versions line by line. Measured from the merge base,
// the branch's whole file would instead read as one insertion spanning all of
// base's.
func TestChangedRanges_AddAddKeepsBaseDiff(t *testing.T) {
	dir := gitRepo(t)
	writeFile(t, dir, "seed.txt", "seed\n")
	runGit(t, dir, "add", "seed.txt")
	runGit(t, dir, "commit", "-qm", "base (no shared.txt)")

	runGit(t, dir, "switch", "-qc", "branch-a")
	writeFile(t, dir, "shared.txt", "a1\nsame2\na3\nsame4\n")
	runGit(t, dir, "add", "shared.txt")
	runGit(t, dir, "commit", "-qm", "A adds shared.txt")
	runGit(t, dir, "switch", "-q", "main")
	writeFile(t, dir, "shared.txt", "x1\nsame2\nx3\nsame4\n")
	runGit(t, dir, "add", "shared.txt")
	runGit(t, dir, "commit", "-qm", "main adds its own shared.txt")

	wt := filepath.Join(t.TempDir(), "wt-a")
	runGit(t, dir, "worktree", "add", "-q", wt, "branch-a")
	if got, want := ChangedRanges(wt, "main", "shared.txt"), []LineRange{span(1, 1), span(3, 3)}; !reflect.DeepEqual(got, want) {
		t.Errorf("ChangedRanges(add/add) = %v, want %v (the lines where the two added versions differ)", got, want)
	}
}

// The chain shape (#184 review): A edits l11, base then rewrites l12-l13, B
// (current) edits base's l13. A's own edit is two lines from B's, but A's edit and
// base's rewrite touch, so git merges them as one conflict region and merge-tree
// of A and B conflicts. A's range must reach base's rewrite.
func TestChangedRanges_StaleBranchTouchingBaseHunkClaimsIt(t *testing.T) {
	wtA, wtB := staleScenario(t,
		replaceLine("line11", "line11 EDITED_BY_A"),
		chain(replaceLine("line12", "line12 changed-on-main"), replaceLine("line13", "line13 changed-on-main")),
		replaceLine("line13 changed-on-main", "line13 EDITED_BY_B"))

	a := ChangedRanges(wtA, "main", "data.txt")
	if want := []LineRange{span(11, 13)}; !reflect.DeepEqual(a, want) {
		t.Errorf("ChangedRanges(stale branch-a) = %v, want %v (A's line 11 plus base's touching rewrite of 12-13)", a, want)
	}
	if b := ChangedRanges(wtB, "main", "data.txt"); !anyOverlap(a, b) {
		t.Errorf("A %v does NOT overlap B %v, but git merge-tree of the two conflicts", a, b)
	}

	// Control: with one unchanged line between A's edit and base's rewrite there
	// is no conflict region, and A owns only its line (the #184 false HIGH stays fixed).
	wtA2, _ := staleScenario(t,
		replaceLine("line10", "line10 EDITED_BY_A"),
		chain(replaceLine("line12", "line12 changed-on-main"), replaceLine("line13", "line13 changed-on-main")),
		nil)
	if got, want := ChangedRanges(wtA2, "main", "data.txt"), []LineRange{span(10, 10)}; !reflect.DeepEqual(got, want) {
		t.Errorf("ChangedRanges(one line clear) = %v, want %v", got, want)
	}
}

// Every window is measured against origin/<base> when it resolves, local <base>
// only as the fallback: one frame for all windows (#29/#108). Here local main
// lags origin/main by a commit that inserts a line at the top, so the two refs
// number A's edit differently.
func TestChangedRanges_PrefersOriginBase(t *testing.T) {
	dir := gitRepo(t)
	base := twentyLineFile(t, dir)
	runGit(t, dir, "add", "data.txt")
	runGit(t, dir, "commit", "-qm", "base")
	runGit(t, dir, "branch", "branch-a")
	writeFile(t, dir, "data.txt", "top\n"+base)
	runGit(t, dir, "commit", "-qam", "origin/main gains a top line")
	runGit(t, dir, "update-ref", "refs/remotes/origin/main", "HEAD")
	runGit(t, dir, "reset", "-q", "--hard", "HEAD~1") // local main lags origin/main

	wt := filepath.Join(t.TempDir(), "wt-a")
	runGit(t, dir, "worktree", "add", "-q", wt, "branch-a")
	writeFile(t, wt, "data.txt", replaceLine("line05", "line05 EDITED")(base))
	if got, want := ChangedRanges(wt, "main", "data.txt"), []LineRange{span(6, 6)}; !reflect.DeepEqual(got, want) {
		t.Errorf("ChangedRanges = %v, want %v: line 5 in origin/main's numbering (local main would say 5)", got, want)
	}
}

// failGit makes every git call whose args satisfy match fail, for the rest of the
// test: the gitOutput seam, for failures a scratch repo can't produce on demand.
func failGit(t *testing.T, match func(args []string) bool) {
	t.Helper()
	orig := gitOutput
	t.Cleanup(func() { gitOutput = orig })
	gitOutput = func(dir string, args ...string) ([]byte, error) {
		if match(args) {
			return nil, errors.New("injected git failure")
		}
		return orig(dir, args...)
	}
}

// A git failure must never read as "no edits" (#184 review). ChangedRanges: when
// the merge-base diffs fail it falls back to the plain base diff, which
// over-reports (base's edits read as A's) but hides nothing; when git can't diff
// the file at all it says so (ok=false) and ChangedRanges is nil, which the
// graders read as indeterminate (HIGH). ChangedRangesNew then reports the whole
// file, so a structured doc grades every section edited.
func TestChangedRanges_GitFailureNeverReadsAsNoEdits(t *testing.T) {
	wtA, _ := staleScenario(t,
		replaceLine("line03", "line03 EDITED_BY_A"),
		chain(replaceLine("line10", "line10 changed-on-main"), replaceLine("line15", "line15 changed-on-main")),
		nil)
	mb := strings.TrimSpace(gitOut(t, wtA, "merge-base", "main", "HEAD"))

	failGit(t, func(args []string) bool { return len(args) > 2 && args[0] == "diff" && args[2] == mb })
	got, ok := ChangedRangesChecked(wtA, "main", "data.txt")
	if want := []LineRange{span(3, 3), span(10, 10), span(15, 15)}; !ok || !reflect.DeepEqual(got, want) {
		t.Errorf("merge-base diff failing: ChangedRangesChecked = %v, %v, want %v, true (the plain base diff's over-report)", got, ok, want)
	}
	if got := ChangedRangesNew(wtA, "main", "data.txt"); !reflect.DeepEqual(got, []LineRange{span(3, 3), span(10, 10), span(15, 15)}) {
		t.Errorf("merge-base diff failing: ChangedRangesNew = %v, want the base diff's new side", got)
	}

	failGit(t, func(args []string) bool { return len(args) > 0 && args[0] == "diff" })
	if got, ok := ChangedRangesChecked(wtA, "main", "data.txt"); ok || got != nil {
		t.Errorf("every diff failing: ChangedRangesChecked = %v, %v, want nil, false", got, ok)
	}
	if got := ChangedRanges(wtA, "main", "data.txt"); got != nil {
		t.Errorf("every diff failing: ChangedRanges = %v, want nil (indeterminate)", got)
	}
	if got := ChangedRangesNew(wtA, "main", "data.txt"); !reflect.DeepEqual(got, wholeFile) {
		t.Errorf("every diff failing: ChangedRangesNew = %v, want the whole file", got)
	}
}

// LinesToBase is the pre-edit hooks' frame mapping: a pending edit located in the
// on-disk file, moved into base line numbers through the worktree's own diff
// against base, landing where ChangedRanges will report it once made.
func TestLinesToBase(t *testing.T) {
	dir := gitRepo(t)
	base := twentyLineFile(t, dir)
	writeFile(t, dir, "other.txt", "x\n")
	runGit(t, dir, "add", "-A")
	runGit(t, dir, "commit", "-qm", "base")
	runGit(t, dir, "branch", "behind")

	at := func(dir, file string, spans ...LineRange) []LineRange {
		t.Helper()
		got, ok := LinesToBase(dir, "main", file, spans)
		if !ok {
			t.Fatalf("LinesToBase(%s) not ok", file)
		}
		return got
	}
	if got, want := at(dir, "data.txt", span(5, 5)), []LineRange{span(5, 5)}; !reflect.DeepEqual(got, want) {
		t.Errorf("up to date, untouched: %v, want identity %v", got, want)
	}
	// #199: a pending insertion keeps its shape, so it meets only edits next to
	// its gap, as ChangedRanges will report it once made.
	if got, want := at(dir, "data.txt", gapAt(5)), []LineRange{gapAt(5)}; !reflect.DeepEqual(got, want) {
		t.Errorf("up to date, an insertion: %v, want the same gap %v", got, want)
	}
	// An uncommitted 2-line insert at the top: on-disk line 7 is base line 5.
	writeFile(t, dir, "data.txt", "u1\nu2\n"+base)
	if got, want := at(dir, "data.txt", span(7, 7)), []LineRange{span(5, 5)}; !reflect.DeepEqual(got, want) {
		t.Errorf("own insert above: %v, want %v", got, want)
	}
	writeFile(t, dir, "data.txt", base)

	// Base then gains a top line and rewrites line 10; `behind` has neither.
	writeFile(t, dir, "data.txt", "top\n"+replaceLine("line10", "line10 changed-on-main")(base))
	runGit(t, dir, "commit", "-qam", "main moves data.txt after `behind` forked")
	wt := filepath.Join(t.TempDir(), "wt-behind")
	runGit(t, dir, "worktree", "add", "-q", wt, "behind")

	if got, want := at(wt, "data.txt", span(5, 5)), []LineRange{span(6, 6)}; !reflect.DeepEqual(got, want) {
		t.Errorf("behind, base inserted above: %v, want %v", got, want)
	}
	if got, want := at(wt, "data.txt", gapAt(5)), []LineRange{gapAt(6)}; !reflect.DeepEqual(got, want) {
		t.Errorf("behind, an insertion below base's insert: %v, want %v", got, want)
	}
	if got, want := at(wt, "data.txt", gapAt(10)), []LineRange{span(11, 11)}; !reflect.DeepEqual(got, want) {
		t.Errorf("behind, an insertion right after a line base rewrote: %v, want base's rewrite %v", got, want)
	}
	if got, want := at(wt, "data.txt", span(10, 10)), []LineRange{span(11, 11)}; !reflect.DeepEqual(got, want) {
		t.Errorf("behind, on a line base rewrote: %v, want base's rewrite %v", got, want)
	}
	if got, want := at(wt, "data.txt", span(11, 11)), []LineRange{span(11, 12)}; !reflect.DeepEqual(got, want) {
		t.Errorf("behind, touching a line base rewrote: %v, want %v (claims the rewrite, as ChangedRanges will)", got, want)
	}
	if got, want := at(wt, "other.txt", span(1, 1)), []LineRange{span(1, 1)}; !reflect.DeepEqual(got, want) {
		t.Errorf("behind, but base never touched other.txt: %v, want identity %v", got, want)
	}
	// The pending edit, once made, is reported by ChangedRanges exactly there.
	writeFile(t, wt, "data.txt", replaceLine("line11", "line11 EDITED")(base))
	if got, want := ChangedRanges(wt, "main", "data.txt"), []LineRange{span(11, 12)}; !reflect.DeepEqual(got, want) {
		t.Errorf("after the edit, ChangedRanges = %v, want %v (what LinesToBase predicted)", got, want)
	}

	// Can't be measured → ok=false, never the on-disk numbers: a git error, a
	// binary file, a base-less repo with uncommitted changes.
	writeFile(t, dir, "bin.dat", "\x00\x01\x02 base\n")
	runGit(t, dir, "add", "bin.dat")
	runGit(t, dir, "commit", "-qm", "binary")
	writeFile(t, dir, "bin.dat", "\x00\x01\x02 changed\n")
	if _, ok := LinesToBase(dir, "main", "bin.dat", []LineRange{span(1, 1)}); ok {
		t.Error("binary file that differs from base: want ok=false")
	}
	if _, ok := LinesToBase(dir, "no-such-base", "data.txt", []LineRange{span(1, 1)}); !ok {
		t.Error("base-less, nothing uncommitted in data.txt: want the base-less fallback's identity")
	}
	if _, ok := LinesToBase(dir, "no-such-base", "bin.dat", []LineRange{span(1, 1)}); ok {
		t.Error("base-less with an uncommitted change: want ok=false")
	}
	failGit(t, func(args []string) bool { return len(args) > 0 && args[0] == "diff" })
	if _, ok := LinesToBase(wt, "main", "other.txt", []LineRange{span(1, 1)}); ok {
		t.Error("git diff failing: want ok=false, never the on-disk numbers")
	}
}

// hunksConflict must BE git's rule, not an approximation of it: every pair of
// single changes (insert, 1-line, 2-line, at every position) of a small file,
// against `git merge-file`.
func TestHunksConflict_MatchesGitMergeFile(t *testing.T) {
	const n = 6
	dir := t.TempDir()
	base := make([]string, n)
	for i := range base {
		base[i] = fmt.Sprintf("b%d", i+1)
	}
	apply := func(h diffHunk, tag string) string {
		out := append([]string(nil), base[:h.oldStart-boolInt(h.oldCount > 0)]...)
		if h.oldCount == 0 {
			out = append(append(out, tag+"-ins"), base[h.oldStart:]...)
		} else {
			for k := 0; k < h.oldCount; k++ {
				out = append(out, fmt.Sprintf("%s-%d", tag, k))
			}
			out = append(out, base[h.oldStart-1+h.oldCount:]...)
		}
		return strings.Join(out, "\n") + "\n"
	}
	var shapes []diffHunk
	for s := 0; s <= n; s++ {
		for c := 0; c <= 2; c++ {
			if (c > 0 && s < 1) || s+c-1 > n {
				continue
			}
			shapes = append(shapes, diffHunk{oldStart: s, oldCount: c})
		}
	}
	writeFile(t, dir, "o", strings.Join(base, "\n")+"\n")
	for _, a := range shapes {
		for _, b := range shapes {
			writeFile(t, dir, "a", apply(a, "A"))
			writeFile(t, dir, "b", apply(b, "B"))
			cmd := exec.Command("git", "merge-file", "-p", "a", "o", "b")
			cmd.Dir = dir
			err := cmd.Run()
			var exit *exec.ExitError
			if err != nil && !errors.As(err, &exit) {
				t.Fatalf("git merge-file: %v", err)
			}
			if git, rule := err != nil, hunksConflict(a, b); git != rule {
				t.Errorf("hunksConflict(%v, %v) = %v, git merge-file conflicts = %v", a, b, rule, git)
			}
		}
	}
}

func boolInt(b bool) int {
	if b {
		return 1
	}
	return 0
}

// An edit the branch made that base ALSO made, identically (a cherry-pick, or a
// squash that landed part of the branch), stays the branch's. It isn't a phantom:
// if a window that edits base's copy of that line lands first, merging the branch
// conflicts (its merge base still has the old line). #122 already covers a branch
// whose WHOLE change landed; this branch still has unlanded work (line 15).
func TestChangedRanges_StaleBranchKeepsEditBaseAlsoMade(t *testing.T) {
	wtA, wtB := staleScenario(t,
		chain(replaceLine("line05", "line05 SAME"), replaceLine("line15", "line15 EDITED_BY_A")),
		replaceLine("line05", "line05 SAME"),
		replaceLine("line05 SAME", "line05 EDITED_BY_B"))
	a := ChangedRanges(wtA, "main", "data.txt")
	if want := []LineRange{span(5, 5), span(15, 15)}; !reflect.DeepEqual(a, want) {
		t.Errorf("ChangedRanges(stale branch-a) = %v, want %v", a, want)
	}
	if b := ChangedRanges(wtB, "main", "data.txt"); !anyOverlap(a, b) {
		t.Errorf("A %v does NOT overlap B %v: B landing first makes A's merge conflict on line 5", a, b)
	}
}
