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

func TestSubtractOverlapping(t *testing.T) {
	// #142: a phantom range that coincides with a base insertion is dropped; the
	// branch's real edits (which never intersect a base insertion) are kept.
	r := []LineRange{{3, 3}, {16, 20}, {30, 30}}
	g := []LineRange{{16, 20}}
	if got, want := subtractOverlapping(r, g), []LineRange{{3, 3}, {30, 30}}; !reflect.DeepEqual(got, want) {
		t.Errorf("subtractOverlapping = %v, want %v", got, want)
	}
	// any intersection drops the span
	if got := subtractOverlapping([]LineRange{{5, 10}}, []LineRange{{8, 12}}); len(got) != 0 {
		t.Errorf("overlapping span not dropped: %v", got)
	}
	// empty g → unchanged (up-to-date branch)
	if got, want := subtractOverlapping(r, nil), r; !reflect.DeepEqual(got, want) {
		t.Errorf("subtractOverlapping(_, nil) = %v, want %v", got, want)
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
