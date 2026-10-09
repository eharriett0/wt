package gitx

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// #198: `wt claim` takes its placeholder commit back off a branch it did not
// create when the push fails, instead of deleting the branch. UndoCommit moves
// HEAD back exactly one EMPTY, single-parent commit (what CommitEmpty makes),
// leaves the index and the files alone, and refuses anything else (#198
// review: it used to reset a commit that swept staged work in, or a merge).
func TestUndoCommit(t *testing.T) {
	dir := gitRepo(t)
	t.Chdir(dir)
	before := gitOut(t, dir, "rev-parse", "HEAD")
	writeFile(t, dir, "staged.txt", "staged before the claim\n")
	runGit(t, dir, "add", "staged.txt")
	if err := CommitEmpty(dir, "WIP: claim #1"); err != nil {
		t.Fatalf("CommitEmpty: %v", err)
	}
	placeholder := gitOut(t, dir, "rev-parse", "HEAD")

	// HEAD moved on past the placeholder: a reset to its parent would drop the
	// newer commit too, so nothing moves.
	runGit(t, dir, "commit", "--allow-empty", "--only", "-qm", "after the claim")
	after := gitOut(t, dir, "rev-parse", "HEAD")
	if err := UndoCommit(dir, placeholder, before); err == nil {
		t.Error("UndoCommit succeeded although HEAD moved on past the commit")
	}
	if got := gitOut(t, dir, "rev-parse", "HEAD"); got != after {
		t.Fatalf("HEAD moved to %s, want it left at %s", got, after)
	}
	runGit(t, dir, "reset", "-q", "--soft", placeholder)

	for _, c := range []struct{ name, commit, parent string }{
		{"empty commit", "", before},
		{"empty parent", placeholder, ""},
		{"not its parent", placeholder, placeholder},
	} {
		if err := UndoCommit(dir, c.commit, c.parent); err == nil {
			t.Errorf("%s: UndoCommit succeeded, want a refusal", c.name)
		}
		if got := gitOut(t, dir, "rev-parse", "HEAD"); got != placeholder {
			t.Fatalf("%s: HEAD moved to %s, want it left at %s", c.name, got, placeholder)
		}
	}

	if err := UndoCommit(dir, placeholder, before); err != nil {
		t.Fatalf("UndoCommit: %v", err)
	}
	if got := gitOut(t, dir, "rev-parse", "main"); got != before {
		t.Errorf("main is at %s after the undo, want %s", got, before)
	}
	if got := gitOut(t, dir, "diff", "--cached", "--name-only"); got != "staged.txt" {
		t.Errorf("staged after the undo = %q, want staged.txt, staged all along", got)
	}
}

// A commit that is not what CommitEmpty makes is somebody's work, so UndoCommit
// refuses it (#198 review): one that changes files (it swept staged work in),
// and a merge, whose reset would drop the second parent with no MERGE_HEAD left
// to restore it. The merge here changes nothing, so the parent check alone
// refuses it.
func TestUndoCommit_RefusesWhatIsNotAPlaceholder(t *testing.T) {
	t.Run("changes files", func(t *testing.T) {
		dir := gitRepo(t)
		t.Chdir(dir)
		parent := gitOut(t, dir, "rev-parse", "HEAD")
		writeFile(t, dir, "work.txt", "work\n")
		runGit(t, dir, "add", "work.txt")
		runGit(t, dir, "commit", "--allow-empty", "-qm", "WIP: claim #1")
		swept := gitOut(t, dir, "rev-parse", "HEAD")
		if err := UndoCommit(dir, swept, parent); err == nil || !strings.Contains(err.Error(), "changes files") {
			t.Errorf("UndoCommit of a commit holding work = %v, want the changes-files refusal", err)
		}
		if got := gitOut(t, dir, "rev-parse", "HEAD"); got != swept {
			t.Errorf("HEAD moved to %s, want it left at %s", got, swept)
		}
	})
	t.Run("merge", func(t *testing.T) {
		dir := gitRepo(t)
		t.Chdir(dir)
		runGit(t, dir, "switch", "-qc", "side")
		runGit(t, dir, "commit", "--allow-empty", "-qm", "side")
		runGit(t, dir, "switch", "-q", "main")
		parent := gitOut(t, dir, "rev-parse", "HEAD")
		runGit(t, dir, "merge", "-q", "--no-ff", "-m", "WIP: claim #1", "side")
		merge := gitOut(t, dir, "rev-parse", "HEAD")
		if err := UndoCommit(dir, merge, parent); err == nil || !strings.Contains(err.Error(), "merge commit") {
			t.Errorf("UndoCommit of a merge = %v, want the merge refusal", err)
		}
		if got := gitOut(t, dir, "rev-parse", "HEAD"); got != merge {
			t.Errorf("HEAD moved to %s, want it left at %s", got, merge)
		}
	})
}

// #198 review: claim's placeholder takes nothing that is staged in a worktree
// it was handed back. A plain `commit --allow-empty` committed it under the
// "WIP: claim" subject and pushed it, and `release --clean` then deleted the
// branch as placeholder-only. Mid-merge it refuses instead of concluding the
// merge, and says why.
func TestCommitEmpty_TakesNothingStaged(t *testing.T) {
	dir := gitRepo(t)
	t.Chdir(dir)
	parent := gitOut(t, dir, "rev-parse", "HEAD")
	writeFile(t, dir, "work.txt", "work in progress\n")
	runGit(t, dir, "add", "work.txt")
	if err := CommitEmpty(dir, "WIP: claim #1"); err != nil {
		t.Fatalf("CommitEmpty: %v", err)
	}
	if got := gitOut(t, dir, "rev-parse", "HEAD^"); got != parent {
		t.Errorf("the placeholder's parent is %s, want %s", got, parent)
	}
	if got, want := gitOut(t, dir, "rev-parse", "HEAD^{tree}"), gitOut(t, dir, "rev-parse", parent+"^{tree}"); got != want {
		t.Errorf("the placeholder changes files (tree %s, its parent's %s): it took the staged work", got, want)
	}
	if got := gitOut(t, dir, "diff", "--cached", "--name-only"); got != "work.txt" {
		t.Errorf("staged after the placeholder = %q, want work.txt still staged", got)
	}
}

func TestCommitEmpty_RefusesMidMerge(t *testing.T) {
	dir := gitRepo(t)
	t.Chdir(dir)
	runGit(t, dir, "switch", "-qc", "side")
	writeFile(t, dir, "side.txt", "side\n")
	runGit(t, dir, "add", "side.txt")
	runGit(t, dir, "commit", "-qm", "side")
	runGit(t, dir, "switch", "-q", "main")
	head := gitOut(t, dir, "rev-parse", "HEAD")
	runGit(t, dir, "merge", "-q", "--no-ff", "--no-commit", "side")
	err := CommitEmpty(dir, "WIP: claim #1")
	if err == nil {
		t.Fatal("CommitEmpty concluded a merge in progress")
	}
	if !strings.Contains(err.Error(), "merge") {
		t.Errorf("error %q should carry git's reason", err)
	}
	if got := gitOut(t, dir, "rev-parse", "HEAD"); got != head {
		t.Errorf("HEAD moved to %s, want it left at %s", got, head)
	}
	if _, serr := os.Stat(filepath.Join(dir, ".git", "MERGE_HEAD")); serr != nil {
		t.Errorf("the merge in progress is gone (MERGE_HEAD: %v)", serr)
	}
}

// #198: Create's new-branch path must never re-attach an existing branch: the
// re-attach is decided (and checked against origin/<branch>) by PlanNew, so a
// branch that appeared in between must make worktree-add fail, not be taken.
func TestWorktreeAddNewBranch_NeverReattaches(t *testing.T) {
	dir := gitRepo(t)
	t.Chdir(dir)
	base := gitOut(t, dir, "rev-parse", "HEAD")
	runGit(t, dir, "commit", "--allow-empty", "-qm", "on main")
	runGit(t, dir, "branch", "feat", base)

	wt := filepath.Join(t.TempDir(), "wt-feat")
	err := WorktreeAddNewBranch(wt, "feat", "main")
	if err == nil {
		t.Fatal("WorktreeAddNewBranch re-attached an existing branch")
	}
	if !strings.Contains(err.Error(), "already exists") {
		t.Errorf("error %q should carry git's own reason", err)
	}
	if got := gitOut(t, dir, "rev-parse", "feat"); got != base {
		t.Errorf("feat moved to %s, want it left at %s", got, base)
	}
	if _, serr := os.Stat(wt); !os.IsNotExist(serr) {
		t.Errorf("no worktree may be left at %s (stat: %v)", wt, serr)
	}

	fresh := filepath.Join(t.TempDir(), "wt-new")
	if err := WorktreeAddNewBranch(fresh, "new", "main"); err != nil {
		t.Fatalf("WorktreeAddNewBranch: %v", err)
	}
	if got := gitOut(t, fresh, "rev-parse", "--abbrev-ref", "HEAD"); got != "new" {
		t.Errorf("new worktree is on %q, want new", got)
	}
}
