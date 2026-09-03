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

// A branch that doesn't exist (locally or on a remote) must error clearly, so
// claim.Adopt can turn it into a "not found" instead of silently creating one.
func TestWorktreeAdopt_MissingBranchErrors(t *testing.T) {
	dir := gitRepo(t)
	t.Chdir(dir)
	if err := WorktreeAdopt(filepath.Join(t.TempDir(), "nope"), "does-not-exist"); err == nil {
		t.Error("WorktreeAdopt on a non-existent branch should error, not fork one")
	}
}
