package gitx

import (
	"os"
	"path/filepath"
	"testing"
)

// `git worktree list`, read as git prints it (#253 review): a path ending in
// a space is kept whole, a removed worktree is prunable, and a locked one
// whose directory is gone (an unmounted volume) is not.
func TestWorktreeList_PrunableLockedVerbatim(t *testing.T) {
	dir := gitRepo(t)
	base, err := filepath.EvalSymlinks(t.TempDir()) // git records the real path
	if err != nil {
		t.Fatal(err)
	}
	gone, locked, spaced := filepath.Join(base, "gone"), filepath.Join(base, "locked"), filepath.Join(base, "spaced ")
	for br, p := range map[string]string{"gone": gone, "locked": locked, "spaced": spaced} {
		runGit(t, dir, "worktree", "add", "-q", "-b", br, p, "main")
	}
	runGit(t, dir, "worktree", "lock", locked)
	for _, p := range []string{gone, locked} {
		if err := os.RemoveAll(p); err != nil {
			t.Fatal(err)
		}
	}
	t.Chdir(dir)
	refs, err := WorktreeList()
	if err != nil {
		t.Fatal(err)
	}
	by := map[string]WorktreeRef{}
	for _, r := range refs {
		by[r.Branch] = r
	}
	if r := by["gone"]; !r.Prunable || r.Locked {
		t.Errorf("removed worktree: %+v, want prunable, not locked", r)
	}
	if r := by["locked"]; r.Prunable || !r.Locked {
		t.Errorf("locked worktree, directory gone: %+v, want locked, not prunable", r)
	}
	if r := by["spaced"]; r.Path != spaced || r.Prunable {
		t.Errorf("worktree at %q: %+v, want its path whole", spaced, r)
	}
	paths, err := WorktreePaths()
	if err != nil {
		t.Fatal(err)
	}
	found := false
	for _, p := range paths {
		found = found || p == spaced
	}
	if !found {
		t.Errorf("WorktreePaths() = %q, want %q whole", paths, spaced)
	}
}
