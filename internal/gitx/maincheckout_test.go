package gitx

import (
	"path/filepath"
	"testing"
)

// MainCheckout is the main working tree from the main checkout and from a
// linked worktree, and in a bare repository the repository itself, from the
// repository and from a linked worktree of it, where git can still remove a
// worktree and delete a branch (#230, #230 review).
func TestMainCheckout(t *testing.T) {
	repo := gitRepo(t)
	real, err := filepath.EvalSymlinks(repo)
	if err != nil {
		t.Fatal(err)
	}
	lane := filepath.Join(t.TempDir(), "lane")
	runGit(t, repo, "worktree", "add", "-q", "-b", "feat-x", lane)
	for _, dir := range []string{repo, lane} {
		t.Chdir(dir)
		got, _ := filepath.EvalSymlinks(MainCheckout())
		if got != real {
			t.Errorf("from %s: MainCheckout = %q, want %q", dir, got, real)
		}
	}
	bare := filepath.Join(t.TempDir(), "bare.git")
	runGit(t, repo, "clone", "-q", "--bare", repo, bare)
	realBare, err := filepath.EvalSymlinks(bare)
	if err != nil {
		t.Fatal(err)
	}
	bareLane := filepath.Join(t.TempDir(), "bare-lane")
	runGit(t, bare, "worktree", "add", "-q", "-b", "feat-y", bareLane)
	for _, dir := range []string{bare, bareLane} {
		t.Chdir(dir)
		got, _ := filepath.EvalSymlinks(MainCheckout())
		if got != realBare {
			t.Errorf("bare, from %s: MainCheckout = %q, want the repository %q", dir, got, realBare)
		}
	}
	// A submodule's git dir is neither a .git nor bare: "", git runs from the cwd.
	sub := filepath.Join(t.TempDir(), "sub")
	runGit(t, repo, "init", "-q", "-b", "main", sub)
	runGit(t, sub, "commit", "-q", "--allow-empty", "-m", "sub")
	runGit(t, repo, "-c", "protocol.file.allow=always", "submodule", "add", "-q", sub, "sub")
	t.Chdir(filepath.Join(repo, "sub"))
	if got := MainCheckout(); got != "" {
		t.Errorf("in a submodule: %q, want \"\"", got)
	}
}
