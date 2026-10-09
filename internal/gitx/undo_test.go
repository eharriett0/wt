package gitx

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// #198: `wt claim` takes its placeholder commit back off a branch it did not
// create when the push fails, instead of deleting the branch. UndoCommit must
// move HEAD back exactly one commit, keep what the commit swept in as staged,
// and refuse anything else.
func TestUndoCommit(t *testing.T) {
	dir := gitRepo(t)
	t.Chdir(dir)
	before := gitOut(t, dir, "rev-parse", "HEAD")
	writeFile(t, dir, "staged.txt", "staged before the claim\n")
	runGit(t, dir, "add", "staged.txt")
	runGit(t, dir, "commit", "--allow-empty", "-qm", "WIP: claim #1")
	placeholder := gitOut(t, dir, "rev-parse", "HEAD")

	// HEAD moved on past the placeholder: a reset to its parent would drop the
	// newer commit too, so nothing moves.
	runGit(t, dir, "commit", "--allow-empty", "-qm", "after the claim")
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
		t.Errorf("staged after the undo = %q, want what the commit swept in (staged.txt) staged again", got)
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
