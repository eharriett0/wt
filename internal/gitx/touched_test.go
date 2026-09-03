package gitx

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

func runGit(t *testing.T, dir string, args ...string) {
	t.Helper()
	cmd := exec.Command("git", args...)
	cmd.Dir = dir
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("git %v: %v\n%s", args, err, out)
	}
}

func gitRepo(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	runGit(t, dir, "init", "-q", "-b", "main")
	runGit(t, dir, "config", "user.email", "t@t.test")
	runGit(t, dir, "config", "user.name", "t")
	runGit(t, dir, "commit", "--allow-empty", "-qm", "init")
	return dir
}

func touchedContains(ss []string, want string) bool {
	for _, s := range ss {
		if s == want {
			return true
		}
	}
	return false
}

// #27: a fully-untracked new directory must list its files individually, not
// collapse to "dir/" — otherwise a collision under it never matches.
func TestTouchedFiles_UntrackedNewDirListsFiles(t *testing.T) {
	dir := gitRepo(t)
	if err := os.MkdirAll(filepath.Join(dir, "newpkg"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "newpkg", "foo.go"), []byte("package newpkg\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	got := TouchedFiles(dir, "main")
	if !touchedContains(got, "newpkg/foo.go") {
		t.Errorf("expected full path newpkg/foo.go in touched set, got %v", got)
	}
	if touchedContains(got, "newpkg/") {
		t.Errorf("dir-collapsed newpkg/ should NOT appear with -uall, got %v", got)
	}
}

// #28: a staged rename must record BOTH the old and new paths so an edit to the
// pre-rename name in another window still overlaps.
func TestTouchedFiles_RenameRecordsBothPaths(t *testing.T) {
	dir := gitRepo(t)
	if err := os.WriteFile(filepath.Join(dir, "old.go"), []byte("package x\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	runGit(t, dir, "add", "old.go")
	runGit(t, dir, "commit", "-qm", "add old")
	runGit(t, dir, "mv", "old.go", "new.go") // staged rename

	got := TouchedFiles(dir, "main")
	if !touchedContains(got, "old.go") {
		t.Errorf("expected OLD path old.go in touched set, got %v", got)
	}
	if !touchedContains(got, "new.go") {
		t.Errorf("expected NEW path new.go in touched set, got %v", got)
	}
}

// writeFile is a tiny helper for the range-scope tests below.
func writeFile(t *testing.T, dir, rel, content string) {
	t.Helper()
	if err := os.WriteFile(filepath.Join(dir, rel), []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
}

// The pre-push outgoing scope must be THREE-dot (diff since the merge-base), so a
// branch BEHIND base — you branched, base moved on, you did NOT rebase — is scoped
// to only what the branch itself added, never the files base advanced by. Two-dot
// (base..head) diffs the two trees directly and reports base's divergent files as
// outgoing (reversals), which is exactly the pre-push guard's false-HIGH block on
// files the pusher never touched.
func TestRangeChangedPaths_BehindBaseScopesToBranchContributionOnly(t *testing.T) {
	dir := gitRepo(t) // main @ empty init
	writeFile(t, dir, "base.txt", "base\n")
	runGit(t, dir, "add", "-A")
	runGit(t, dir, "commit", "-qm", "base") // <- the eventual merge-base

	runGit(t, dir, "checkout", "-q", "-b", "feature")
	writeFile(t, dir, "feature.txt", "feat\n")
	runGit(t, dir, "add", "-A")
	runGit(t, dir, "commit", "-qm", "feature")

	// main advances with an UNRELATED file; feature does NOT rebase → it is behind.
	runGit(t, dir, "checkout", "-q", "main")
	writeFile(t, dir, "main-advanced.txt", "adv\n")
	runGit(t, dir, "add", "-A")
	runGit(t, dir, "commit", "-qm", "advance")

	paths, err := RangeChangedPaths(dir, "main", "feature")
	if err != nil {
		t.Fatalf("RangeChangedPaths: %v", err)
	}
	if !touchedContains(paths, "feature.txt") {
		t.Errorf("branch's own file missing from outgoing scope: %v", paths)
	}
	if touchedContains(paths, "main-advanced.txt") {
		t.Errorf("two-dot regression: base's advanced file must NOT be attributed to the push: %v", paths)
	}
	if len(paths) != 1 {
		t.Errorf("outgoing scope should be exactly [feature.txt], got %v", paths)
	}
}

// The common case (branch AHEAD of base, base is the merge-base) is unchanged by
// three-dot: it still reports exactly the branch's commits.
func TestRangeChangedPaths_AheadOfBaseUnchanged(t *testing.T) {
	dir := gitRepo(t)
	writeFile(t, dir, "base.txt", "base\n")
	runGit(t, dir, "add", "-A")
	runGit(t, dir, "commit", "-qm", "base")

	runGit(t, dir, "checkout", "-q", "-b", "feature")
	writeFile(t, dir, "a.go", "package a\n")
	writeFile(t, dir, "b.go", "package b\n")
	runGit(t, dir, "add", "-A")
	runGit(t, dir, "commit", "-qm", "feature work")

	paths, err := RangeChangedPaths(dir, "main", "feature")
	if err != nil {
		t.Fatalf("RangeChangedPaths: %v", err)
	}
	if !touchedContains(paths, "a.go") || !touchedContains(paths, "b.go") {
		t.Errorf("ahead-of-base scope should list the branch's files, got %v", paths)
	}
	if len(paths) != 2 {
		t.Errorf("expected exactly [a.go b.go], got %v", paths)
	}
}

func rangeCovers(rs []LineRange, line int) bool {
	for _, r := range rs {
		if r.Start <= line && line <= r.End {
			return true
		}
	}
	return false
}

// ChangedRangesNew reports a change at its CURRENT-file line (new frame); the
// same change appears at a DIFFERENT (base-file) line via ChangedRanges. A late
// edit whose line shifted because of an earlier insert must not be confused
// across frames — the #123 section-grade bug used base-frame ranges against
// current-frame section spans.
func TestChangedRangesNew_NewFrameDiffersFromBaseFrame(t *testing.T) {
	dir := gitRepo(t)
	writeFile(t, dir, "doc.md", "line1\nline2\nline3\nTARGET\n")
	runGit(t, dir, "add", "-A")
	runGit(t, dir, "commit", "-qm", "base")
	// Insert 2 lines early (shifts TARGET from base line 4 → current line 6) and
	// change TARGET.
	writeFile(t, dir, "doc.md", "line1\nINS-A\nINS-B\nline2\nline3\nTARGET-EDIT\n")

	newF := ChangedRangesNew(dir, "main", "doc.md")
	baseF := ChangedRanges(dir, "main", "doc.md")

	if !rangeCovers(newF, 6) {
		t.Errorf("ChangedRangesNew should cover current line 6 (TARGET's new position): %v", newF)
	}
	if rangeCovers(newF, 4) {
		t.Errorf("ChangedRangesNew should NOT report base line 4 (unchanged 'line2' in the current file): %v", newF)
	}
	if !rangeCovers(baseF, 4) {
		t.Errorf("ChangedRanges should cover base line 4 (TARGET's base position): %v", baseF)
	}
}

// FileChangeSubsumed: a branch whose change to a file already landed on base (via
// a different branch's squash) contributes nothing new → subsumed; a branch that
// changes a line base didn't → not subsumed; an unresolvable base → known=false
// (fail-safe: stays a collision). #122.
func TestFileChangeSubsumed(t *testing.T) {
	dir := gitRepo(t)
	writeFile(t, dir, "f.txt", "a\nb\nc\nd\ne\n")
	runGit(t, dir, "add", "-A")
	runGit(t, dir, "commit", "-qm", "C0")
	c0, _ := Run("-C", dir, "rev-parse", "HEAD")

	// orphan forks at C0 and changes line 3 (its real work).
	runGit(t, dir, "checkout", "-q", "-b", "orphan")
	writeFile(t, dir, "f.txt", "a\nb\nC-ORPHAN\nd\ne\n")
	runGit(t, dir, "add", "-A")
	runGit(t, dir, "commit", "-qm", "orphan")

	// main gains orphan's line-3 change (landed elsewhere) AND changes line 5.
	runGit(t, dir, "checkout", "-q", "main")
	writeFile(t, dir, "f.txt", "a\nb\nC-ORPHAN\nd\nE-LANDED\n")
	runGit(t, dir, "add", "-A")
	runGit(t, dir, "commit", "-qm", "landed")

	// Evaluate from the orphan worktree.
	runGit(t, dir, "checkout", "-q", "orphan")
	if subsumed, known := FileChangeSubsumed(dir, "main", "f.txt"); !known || !subsumed {
		t.Errorf("orphan's landed change should be subsumed: subsumed=%v known=%v", subsumed, known)
	}

	// A branch that changes line 2 (base did NOT) is genuinely contested.
	runGit(t, dir, "checkout", "-q", "-b", "real", strings.TrimSpace(c0))
	writeFile(t, dir, "f.txt", "a\nB-REAL\nc\nd\ne\n")
	runGit(t, dir, "add", "-A")
	runGit(t, dir, "commit", "-qm", "real")
	if subsumed, known := FileChangeSubsumed(dir, "main", "f.txt"); !known || subsumed {
		t.Errorf("real branch adds a line-2 change → not subsumed: subsumed=%v known=%v", subsumed, known)
	}

	// Fail-safe: an unresolvable base ref → known=false (stays a collision).
	if subsumed, known := FileChangeSubsumed(dir, "no-such-base", "f.txt"); known || subsumed {
		t.Errorf("unresolvable base must be known=false: subsumed=%v known=%v", subsumed, known)
	}
	// Base lacks the file (branch adds a brand-new one) → known=false.
	writeFile(t, dir, "new-only.txt", "x\n")
	runGit(t, dir, "add", "-A")
	runGit(t, dir, "commit", "-qm", "add new")
	if subsumed, known := FileChangeSubsumed(dir, "main", "new-only.txt"); known || subsumed {
		t.Errorf("base lacking the file must be known=false: subsumed=%v known=%v", subsumed, known)
	}
}
