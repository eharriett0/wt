package gitx

import (
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
)

// #177: what `wt discard` reads from git.

func TestParseRemoteHeads(t *testing.T) {
	out := "aaa\trefs/heads/main\n" +
		"bbb\trefs/heads/feat/x\n" +
		"ccc\trefs/tags/v1\n" + // not a branch
		"ddd\trefs/heads/\n" + // no name
		"\n" +
		"garbage line\n" +
		"\trefs/heads/nosha\n"
	want := map[string]string{"main": "aaa", "feat/x": "bbb"}
	if got := parseRemoteHeads(out); !reflect.DeepEqual(got, want) {
		t.Errorf("parseRemoteHeads = %v, want %v", got, want)
	}
	if got := parseRemoteHeads(""); len(got) != 0 {
		t.Errorf("parseRemoteHeads(\"\") = %v, want none", got)
	}
}

func TestPickTrackingTips(t *testing.T) {
	out := "aaa refs/remotes/origin/main\n" +
		"bbb refs/remotes/origin/canary\n" +
		"ccc refs/remotes/origin/canary-2\n" + // a name that merely starts the same stays
		"ddd refs/remotes/upstream/canary\n" + // another remote's stays
		"\n"
	got := pickTrackingTips(out, "refs/remotes/origin/canary")
	if want := []string{"aaa", "ccc", "ddd"}; !reflect.DeepEqual(got, want) {
		t.Errorf("pickTrackingTips = %v, want %v", got, want)
	}
}

func TestParseCommitLines(t *testing.T) {
	got := parseCommitLines("aaa\tfirst subject\nbbb\tsecond\twith a tab\n\nccc\t\n")
	want := []Commit{{"aaa", "first subject"}, {"bbb", "second\twith a tab"}, {"ccc", ""}}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("parseCommitLines = %+v, want %+v", got, want)
	}
}

// CommitsOnlyOn lists tip's commits minus what exclude reaches, skips an
// excluded commit this clone lacks, and never reads a missing TIP as "nothing
// only on it": under --ignore-missing git skips a missing tip too and prints
// nothing, which would let `wt discard` drop commits without --drop-commits.
func TestCommitsOnlyOn(t *testing.T) {
	dir := gitRepo(t)
	t.Chdir(dir)
	base := gitOut(t, dir, "rev-parse", "HEAD")
	runGit(t, dir, "checkout", "-q", "-b", "x")
	runGit(t, dir, "commit", "--allow-empty", "-qm", "x 1")
	one := gitOut(t, dir, "rev-parse", "HEAD")
	runGit(t, dir, "commit", "--allow-empty", "-qm", "x 2")
	two := gitOut(t, dir, "rev-parse", "HEAD")
	missing := "0123456789012345678901234567890123456789"

	got, err := CommitsOnlyOn(two, []string{base, missing})
	want := []Commit{{two, "x 2"}, {one, "x 1"}}
	if err != nil || !reflect.DeepEqual(got, want) {
		t.Fatalf("CommitsOnlyOn(x, base + a missing sha) = %+v, %v; want %+v", got, err, want)
	}
	if got, err := CommitsOnlyOn(two, []string{one}); err != nil || len(got) != 1 || got[0].SHA != two {
		t.Errorf("CommitsOnlyOn(x, x~1) = %+v, %v; want only %s", got, err, two)
	}
	if got, err := CommitsOnlyOn(two, []string{two}); err != nil || len(got) != 0 {
		t.Errorf("CommitsOnlyOn(x, x) = %+v, %v; want none", got, err)
	}
	for _, tip := range []string{missing, "", "no-such-ref"} {
		if got, err := CommitsOnlyOn(tip, []string{base}); err == nil {
			t.Errorf("CommitsOnlyOn(%q) = %+v, nil; want an error, never \"no commits\"", tip, got)
		}
	}
}

// status.showUntrackedFiles=no hides untracked files from a plain porcelain
// status, and from `git worktree remove`'s own check, which then deleted them
// with the worktree (measured, git 2.39). StatusEntries lists them anyway, and
// RemoveCleanWorktree makes git's check see them too.
func TestUntrackedFilesHiddenByConfig(t *testing.T) {
	dir := gitRepo(t)
	t.Chdir(dir)
	runGit(t, dir, "config", "status.showUntrackedFiles", "no")
	wt := filepath.Join(t.TempDir(), "wt")
	runGit(t, dir, "worktree", "add", "-q", "-b", "w", wt)
	writeFile(t, wt, "notes.txt", "not committed\n")

	got, err := StatusEntries(wt)
	if err != nil || len(got) != 1 || got[0] != "?? notes.txt" {
		t.Fatalf("StatusEntries = %q, %v; want [?? notes.txt]", got, err)
	}
	if err := RemoveCleanWorktree(wt); err == nil {
		t.Fatal("RemoveCleanWorktree removed a worktree holding an untracked file")
	}
	if _, err := os.Stat(filepath.Join(wt, "notes.txt")); err != nil {
		t.Fatalf("the untracked file is gone: %v", err)
	}
	if _, err := StatusEntries(filepath.Join(t.TempDir(), "nowhere")); err == nil {
		t.Error("StatusEntries of a missing directory succeeded; an unreadable status must not read as clean")
	}

	if err := os.Remove(filepath.Join(wt, "notes.txt")); err != nil {
		t.Fatal(err)
	}
	if got, err := StatusEntries(wt); err != nil || len(got) != 0 {
		t.Fatalf("StatusEntries of a clean worktree = %q, %v", got, err)
	}
	if err := RemoveCleanWorktree(wt); err != nil {
		t.Fatalf("RemoveCleanWorktree of a clean worktree: %v", err)
	}
	if _, err := os.Stat(wt); !os.IsNotExist(err) {
		t.Errorf("the worktree is still there (stat: %v)", err)
	}
}

// DeleteBranchAt deletes a branch only at the tip it was given, and its error
// carries git's reason.
func TestDeleteBranchAt(t *testing.T) {
	dir := gitRepo(t)
	t.Chdir(dir)
	runGit(t, dir, "branch", "x")
	at := gitOut(t, dir, "rev-parse", "x")
	runGit(t, dir, "checkout", "-q", "x")
	runGit(t, dir, "commit", "--allow-empty", "-qm", "moved")
	runGit(t, dir, "checkout", "-q", "main")
	if err := DeleteBranchAt("x", at); err == nil || !strings.Contains(err.Error(), "now") {
		t.Errorf("DeleteBranchAt on a moved branch = %v, want a refusal", err)
	}
	moved := gitOut(t, dir, "rev-parse", "x")
	runGit(t, dir, "checkout", "-q", "x")
	if err := DeleteBranchAt("x", moved); err == nil || !strings.Contains(err.Error(), "checked out") {
		t.Errorf("DeleteBranchAt on a checked-out branch = %v, want git's refusal", err)
	}
	runGit(t, dir, "checkout", "-q", "main")
	if err := DeleteBranchAt("x", moved); err != nil {
		t.Fatalf("DeleteBranchAt: %v", err)
	}
	if BranchTip("x") != "" {
		t.Error("x is still there")
	}
	for _, c := range [][2]string{{"", moved}, {"main", ""}} {
		if err := DeleteBranchAt(c[0], c[1]); err == nil {
			t.Errorf("DeleteBranchAt(%q, %q) = nil, want an error", c[0], c[1])
		}
	}
}
