package gitx

import (
	"path/filepath"
	"testing"
)

// MainCheckout is the main working tree from the main checkout and from a
// linked worktree, and "" in a bare repository (#230).
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
	runGit(t, repo, "init", "-q", "--bare", bare)
	t.Chdir(bare)
	if got := MainCheckout(); got != "" {
		t.Errorf("in a bare repository: %q, want \"\"", got)
	}
}
