package worktree

import (
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"slices"
	"strings"
	"testing"
	"time"
)

// headGh puts a fake gh first on PATH that answers `gh pr view N --json
// headRefOid` with head, as merge.UnshippedLane asks it, and fails anything
// else.
func headGh(t *testing.T, head string) {
	t.Helper()
	if runtime.GOOS == "windows" {
		t.Skip("the fake gh is a shell script")
	}
	dir := t.TempDir()
	script := "#!/bin/sh\ncase \"$*\" in\n*headRefOid*) echo " + head + " ;;\n*) exit 1 ;;\nesac\n"
	if err := os.WriteFile(filepath.Join(dir, "gh"), []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", dir+string(os.PathListSeparator)+os.Getenv("PATH"))
	if got, err := exec.LookPath("gh"); err != nil || got != filepath.Join(dir, "gh") {
		t.Fatalf("gh resolves to %q (%v), not the fake", got, err)
	}
}

// mergedLane makes the worktree feat-x with one commit, pushed under its own
// name, created an hour ago, whose PR #5 reads MERGED with that commit as its
// head: what `wt clean -y` reaps. Returns the worktree and the PR's head.
func (f *adoptFixture) mergedLane() (dir, head string) {
	f.t.Helper()
	dir = f.worktreeOn("feat-x", f.base)
	head = f.commitHere(dir, "pr work")
	gitW(f.t, dir, "push", "-q", "-u", "origin", "feat-x")
	old := time.Now().Add(-time.Hour)
	if err := os.Chtimes(filepath.Join(dir, ".git"), old, old); err != nil {
		f.t.Fatal(err)
	}
	headGh(f.t, head)
	orig := prLookup
	f.t.Cleanup(func() { prLookup = orig })
	prLookup = func(branch, baseRef string) (string, string, bool, bool) {
		if branch == "feat-x" {
			return "5", "MERGED", false, true
		}
		return "", "", false, false
	}
	return dir, head
}

// #230 review: a merged PR proves only the commits it had. `wt clean -y` reaped
// a merged PR's lane without asking whether the local tip shipped in it, so a
// commit made in the lane after the merge (not pushed) was deleted with the
// branch. #230 made that reachable from inside the lane too, where the
// branch delete used to fail. Now clean keeps it, as merge-pr has since #187.
func TestClean_MergedPRReapsOnlyTheTipItShipped(t *testing.T) {
	for _, tc := range []struct {
		name     string
		followUp bool // a commit made in the lane after the merge
		inside   bool // clean runs from inside the lane
	}{
		{"control: the tip is the PR's head, from the main checkout", false, false},
		{"control: the tip is the PR's head, from inside the lane", false, true},
		{"a follow-up commit, from the main checkout", true, false},
		{"a follow-up commit, from inside the lane", true, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f := newAdoptFixture(t)
			dir, head := f.mergedLane()
			tip := head
			if tc.followUp {
				tip = f.commitHere(dir, "follow-up, not pushed")
			}
			if tc.inside {
				t.Chdir(dir)
			}
			var err error
			out := captured(t, func() { err = Clean(f.c, true, false, false, []string{"feat-x"}) })
			if err != nil {
				t.Fatalf("Clean: %v\n%s", err, out)
			}
			_, serr := os.Stat(dir)
			kept := serr == nil
			if kept != tc.followUp {
				t.Fatalf("worktree kept=%v, want %v\n%s", kept, tc.followUp, out)
			}
			if got := branchAt(f, "feat-x"); tc.followUp && got != tip {
				t.Errorf("branch at %q, want it kept at the follow-up %s\n%s", got, tip, out)
			} else if !tc.followUp && got != "" {
				t.Errorf("branch kept at %s after the reap\n%s", got, out)
			}
			if tc.followUp && !strings.Contains(out, "feat-x — PR #5 merged, but feat-x has 1 commit(s) that were not in PR #5") {
				t.Errorf("clean said:\n%s\nwant it to name the commit not in the PR", out)
			}
			if !tc.followUp && tc.inside && !strings.Contains(out, "cd "+realPath(f.repo)+" ") {
				t.Errorf("clean removed the lane it ran in without the cd hint:\n%s", out)
			}
		})
	}
}

// branchAt is refs/heads/b's commit in the fixture's repository, "" when there
// is none.
func branchAt(f *adoptFixture, b string) string {
	out, err := exec.Command("git", "-C", f.repo, "rev-parse", "--verify", "--quiet", "refs/heads/"+b).Output()
	if err != nil {
		return ""
	}
	return strings.TrimSpace(string(out))
}

// #230 review: the branch is deleted only at the tip the caller checked, in
// one step. `git branch -D` deleted whatever the branch pointed at by then, so
// a commit landing between the check and the delete went with it.
func TestRemove_DeletesTheBranchOnlyAtTheCheckedTip(t *testing.T) {
	f := newAdoptFixture(t)
	dir := f.worktreeOn("lane", f.base)
	checked := f.commitHere(dir, "checked")
	landed := f.commitHere(dir, "landed after the check")
	out := captured(t, func() {
		if err := Remove(f.c, dir, "lane", checked, false); err != nil {
			t.Errorf("Remove: %v", err)
		}
	})
	if got := branchAt(f, "lane"); got != landed {
		t.Errorf("branch at %q, want it kept at the commit that landed, %s\n%s", got, landed, out)
	}
	if !strings.Contains(out, "branch lane not deleted") {
		t.Errorf("Remove said:\n%s\nwant it to say the branch was not deleted", out)
	}

	// Control: at its tip, it goes.
	dir2 := f.worktreeOn("lane2", f.base)
	tip := f.commitHere(dir2, "work")
	out = captured(t, func() {
		if err := Remove(f.c, dir2, "lane2", tip, false); err != nil {
			t.Errorf("Remove: %v", err)
		}
	})
	if got := branchAt(f, "lane2"); got != "" || !strings.Contains(out, "deleted local branch lane2") {
		t.Errorf("control: branch at %q\n%s", got, out)
	}
	// No tip read: the branch stays.
	dir3 := f.worktreeOn("lane3", f.base)
	tip3 := f.commitHere(dir3, "work")
	out = captured(t, func() { _ = Remove(f.c, dir3, "lane3", "", false) })
	if got := branchAt(f, "lane3"); got != tip3 || !strings.Contains(out, "kept branch lane3") {
		t.Errorf("no tip: branch at %q, want kept at %s\n%s", got, tip3, out)
	}
}

// #230 review: git runs from the main checkout, so a relative path must be
// resolved from the cwd, where the guards read it. Resolved in the main
// checkout instead, `wt release --clean` run in a subdirectory checked
// sub/wts/feat-12 and removed wts/feat-12: another lane.
func TestRemove_RelativePathIsReadFromTheCwd(t *testing.T) {
	f := newAdoptFixture(t)
	sub := filepath.Join(f.repo, "sub")
	if err := os.MkdirAll(sub, 0o755); err != nil {
		t.Fatal(err)
	}
	mine := filepath.Join(sub, "wts", "feat-12")
	other := filepath.Join(f.repo, "wts", "feat-12")
	gitW(t, f.repo, "worktree", "add", "-q", "-b", "feat-12", mine, f.base)
	gitW(t, f.repo, "worktree", "add", "-q", "-b", "other", other, f.base)
	tip := f.tip(mine, "HEAD")
	f.c.WorktreeRoot = "wts"
	t.Chdir(sub)
	out := captured(t, func() {
		if err := Remove(f.c, filepath.Join("wts", "feat-12"), "feat-12", tip, false); err != nil {
			t.Errorf("Remove: %v", err)
		}
	})
	if _, err := os.Stat(other); err != nil {
		t.Errorf("the other lane %s was removed\n%s", other, out)
	}
	if _, err := os.Stat(mine); err == nil {
		t.Errorf("the lane named %s was kept\n%s", mine, out)
	}
}

// #230 review: under core.ignorecase a case twin of the base is the base's own
// loose ref file, and deleting it deleted main. The base guard folds case.
func TestRemove_KeepsACaseTwinOfTheBase(t *testing.T) {
	f := newAdoptFixture(t)
	gitW(t, f.repo, "config", "core.ignorecase", "true")
	dir := f.worktreeOn("lane", f.base)
	out := captured(t, func() { _ = Remove(f.c, dir, "MAIN", f.base, false) })
	if !strings.Contains(out, "kept branch MAIN — it is the base branch") {
		t.Errorf("Remove said:\n%s\nwant the base twin kept", out)
	}
	if branchAt(f, "main") == "" {
		t.Error("main is gone")
	}
}

// #230 review: the cd hint compares directories, not path text. Reached
// through a symlink, the cwd was not "under" the lane, which was removed with
// no hint.
func TestRemove_HintsTheCdThroughASymlink(t *testing.T) {
	f := newAdoptFixture(t)
	dir := f.worktreeOn("lane", f.base)
	tip := f.tip(dir, "HEAD")
	link := filepath.Join(t.TempDir(), "link")
	if err := os.Symlink(f.c.WorktreeRoot, link); err != nil {
		t.Skip("no symlinks:", err)
	}
	t.Chdir(filepath.Join(link, "lane"))
	out := captured(t, func() {
		if err := Remove(f.c, dir, "lane", tip, false); err != nil {
			t.Errorf("Remove: %v", err)
		}
	})
	if !strings.Contains(out, "(you were inside the removed worktree)") {
		t.Errorf("Remove said:\n%s\nwant the cd hint", out)
	}
}

// cwdLast keeps order and moves the lane holding the cwd last (#230 review):
// removed first, it left every later lane's git without a working directory.
func TestCwdLast(t *testing.T) {
	holds := func(p string) bool { return p == "b" }
	if got := cwdLast([]string{"a", "b", "c"}, holds); !slices.Equal(got, []string{"a", "c", "b"}) {
		t.Errorf("cwdLast = %v, want [a c b]", got)
	}
	if got := cwdLast([]string{"a", "c"}, holds); !slices.Equal(got, []string{"a", "c"}) {
		t.Errorf("cwdLast = %v, want [a c]", got)
	}
}
