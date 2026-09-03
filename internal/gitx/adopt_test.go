package gitx

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

// gitOut runs git in dir and returns its output (fatal on error).
func gitOut(t *testing.T, dir string, args ...string) string {
	t.Helper()
	cmd := exec.Command("git", args...)
	cmd.Dir = dir
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("git %v: %v\n%s", args, err, out)
	}
	return strings.TrimSpace(string(out))
}

// #134: WorktreeAdopt must land the worktree on the EXISTING branch — carrying
// that branch's commits — not fork a fresh branch off base the way WorktreeAdd
// would. That distinction is the whole point of `wt adopt`: picking up an
// in-flight PR branch, not creating a parallel one.
func TestWorktreeAdopt_LandsOnExistingBranchNotAFork(t *testing.T) {
	dir := gitRepo(t)
	t.Chdir(dir) // WorktreeAdopt runs git in cwd

	// A branch with a commit that main does NOT have.
	runGit(t, dir, "switch", "-qc", "feat-x")
	writeFile(t, dir, "onbranch.txt", "branch-only\n")
	runGit(t, dir, "add", "onbranch.txt")
	runGit(t, dir, "commit", "-qm", "work on feat-x")
	branchSHA := gitOut(t, dir, "rev-parse", "HEAD")
	runGit(t, dir, "switch", "-q", "main")

	wt := filepath.Join(t.TempDir(), "wt-feat-x")
	if err := WorktreeAdopt(wt, "feat-x"); err != nil {
		t.Fatalf("WorktreeAdopt: %v", err)
	}

	if got := gitOut(t, wt, "rev-parse", "--abbrev-ref", "HEAD"); got != "feat-x" {
		t.Errorf("adopted worktree HEAD = %q, want feat-x (a fork would be a new name)", got)
	}
	if got := gitOut(t, wt, "rev-parse", "HEAD"); got != branchSHA {
		t.Errorf("adopted worktree HEAD sha = %q, want the branch's %q (a fork would be main's commit)", got, branchSHA)
	}
	if _, err := os.Stat(filepath.Join(wt, "onbranch.txt")); err != nil {
		t.Errorf("adopted worktree missing the branch's content: %v", err)
	}
}

// A branch that doesn't exist (locally or on a remote) must error clearly — and
// the error must carry git's own stderr so the caller surfaces the real reason
// instead of a bare "exit status 128".
func TestWorktreeAdopt_MissingBranchErrors(t *testing.T) {
	dir := gitRepo(t)
	t.Chdir(dir)
	err := WorktreeAdopt(filepath.Join(t.TempDir(), "nope"), "does-not-exist")
	if err == nil {
		t.Fatal("WorktreeAdopt on a non-existent branch should error, not fork one")
	}
	if !strings.Contains(err.Error(), "does-not-exist") && !strings.Contains(err.Error(), "invalid reference") {
		t.Errorf("error should carry git's reason, got %q", err.Error())
	}
}

// #134: the whole reason Adopt fetches first is so a branch that lives ONLY on
// origin materializes as a local tracking branch — WorktreeAdopt's DWIM. Lock
// that path (the local-branch + missing cases are covered above).
func TestWorktreeAdopt_MaterializesRemoteOnlyBranch(t *testing.T) {
	remote := t.TempDir()
	runGit(t, remote, "init", "-q", "--bare", "-b", "main")

	work := gitRepo(t)
	runGit(t, work, "remote", "add", "origin", remote)
	runGit(t, work, "push", "-q", "origin", "main")
	runGit(t, work, "switch", "-qc", "feat-remote")
	writeFile(t, work, "r.txt", "remote-only\n")
	runGit(t, work, "add", "r.txt")
	runGit(t, work, "commit", "-qm", "remote work")
	runGit(t, work, "push", "-q", "-u", "origin", "feat-remote")
	remoteSHA := gitOut(t, work, "rev-parse", "HEAD")
	runGit(t, work, "switch", "-q", "main")
	runGit(t, work, "branch", "-qD", "feat-remote") // now feat-remote lives ONLY on origin

	t.Chdir(work)
	runGit(t, work, "fetch", "-q", "origin", "feat-remote") // what worktree.Adopt does first
	wt := filepath.Join(t.TempDir(), "wt-remote")
	if err := WorktreeAdopt(wt, "feat-remote"); err != nil {
		t.Fatalf("WorktreeAdopt (remote-only DWIM): %v", err)
	}
	if got := gitOut(t, wt, "rev-parse", "--abbrev-ref", "HEAD"); got != "feat-remote" {
		t.Errorf("adopted worktree HEAD = %q, want feat-remote", got)
	}
	if got := gitOut(t, wt, "rev-parse", "HEAD"); got != remoteSHA {
		t.Errorf("adopted remote-only branch HEAD sha = %q, want origin's %q", got, remoteSHA)
	}
}
