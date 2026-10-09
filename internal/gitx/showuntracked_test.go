package gitx

import (
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
)

// #208: with status.showUntrackedFiles=no a plain porcelain status hides
// untracked files, and so does `git worktree remove`'s own check: a worktree
// holding only untracked work read as clean, and a non-force remove deleted it.
// IsClean (through StatusEntries) and WorktreeRemove (through a scoped -c) see
// them whatever the config says. Ignored files keep today's behaviour: never
// dirty, and removed with the worktree.

// hiddenUntrackedWorktree is a repo set to status.showUntrackedFiles=no, and a
// worktree of it holding one untracked file, which it returns.
func hiddenUntrackedWorktree(t *testing.T) (wt, file string) {
	t.Helper()
	dir := gitRepo(t)
	t.Chdir(dir)
	runGit(t, dir, "config", "status.showUntrackedFiles", "no")
	wt = filepath.Join(t.TempDir(), "wt")
	runGit(t, dir, "worktree", "add", "-q", "-b", "w", wt)
	writeFile(t, wt, "notes.txt", "not committed\n")
	if out := gitOut(t, wt, "status", "--porcelain"); out != "" {
		t.Fatalf("fixture: a plain status lists %q; the config should hide the file", out)
	}
	return wt, filepath.Join(wt, "notes.txt")
}

func TestIsClean_SeesUntrackedFilesTheConfigHides(t *testing.T) {
	wt, file := hiddenUntrackedWorktree(t)
	if IsClean(wt) {
		t.Fatal("IsClean read a worktree holding an untracked file as clean")
	}
	if err := os.Remove(file); err != nil {
		t.Fatal(err)
	}
	if !IsClean(wt) {
		t.Error("IsClean of a clean worktree = false")
	}
	if IsClean(filepath.Join(t.TempDir(), "nowhere")) {
		t.Error("IsClean of a missing directory = true; an unreadable status must not read as clean")
	}
}

func TestWorktreeRemove_RefusesUntrackedFilesTheConfigHides(t *testing.T) {
	wt, file := hiddenUntrackedWorktree(t)
	err := WorktreeRemove(wt, false)
	if err == nil {
		t.Fatal("a non-force WorktreeRemove removed a worktree holding an untracked file")
	}
	if !strings.Contains(err.Error(), "untracked") {
		t.Errorf("error %q does not carry git's reason", err)
	}
	if _, serr := os.Stat(file); serr != nil {
		t.Fatalf("the untracked file is gone: %v", serr)
	}
	if err := WorktreeRemove(wt, true); err != nil { // force still discards, as asked
		t.Fatalf("WorktreeRemove --force: %v", err)
	}
	if _, serr := os.Stat(wt); !os.IsNotExist(serr) {
		t.Errorf("a forced remove left the worktree (stat: %v)", serr)
	}
}

// Ignored files are what they were: not dirty (IsClean, StatusEntries), and
// deleted with the worktree by a non-force remove. With the config hiding
// untracked files and without.
func TestIgnoredFilesKeepTodaysBehaviour(t *testing.T) {
	for _, hide := range []bool{false, true} {
		dir := gitRepo(t)
		t.Chdir(dir)
		writeFile(t, dir, ".gitignore", "*.log\n")
		runGit(t, dir, "add", ".gitignore")
		runGit(t, dir, "commit", "-qm", "ignore logs")
		if hide {
			runGit(t, dir, "config", "status.showUntrackedFiles", "no")
		}
		wt := filepath.Join(t.TempDir(), "wt")
		runGit(t, dir, "worktree", "add", "-q", "-b", "w", wt)
		writeFile(t, wt, "build.log", "ignored output\n")
		if entries, err := StatusEntries(wt); err != nil || len(entries) != 0 || !IsClean(wt) {
			t.Fatalf("hide=%v: StatusEntries = %q, %v, IsClean = %v; an ignored file must not count", hide, entries, err, IsClean(wt))
		}
		if err := WorktreeRemove(wt, false); err != nil {
			t.Fatalf("hide=%v: WorktreeRemove refused a worktree holding only an ignored file: %v", hide, err)
		}
		if _, err := os.Stat(filepath.Join(wt, "build.log")); !os.IsNotExist(err) {
			t.Errorf("hide=%v: the ignored file survived the remove (stat: %v); git has always deleted it", hide, err)
		}
	}
}

func TestWithUntrackedShown(t *testing.T) {
	got := withUntrackedShown("worktree", "remove")
	if want := []string{"-c", "status.showUntrackedFiles=normal", "worktree", "remove"}; !reflect.DeepEqual(got, want) {
		t.Errorf("withUntrackedShown = %q, want %q", got, want)
	}
}
