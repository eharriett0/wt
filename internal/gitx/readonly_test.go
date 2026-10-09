package gitx

import (
	"os"
	"path/filepath"
	"slices"
	"testing"
	"time"
)

// readOnlyEnv adds GIT_OPTIONAL_LOCKS=0 to the env it is given, last (so it wins
// over an inherited value), without touching the caller's slice.
func TestReadOnlyEnv(t *testing.T) {
	base := []string{"PATH=/bin", "GIT_OPTIONAL_LOCKS=1"}
	got := readOnlyEnv(base)
	if want := []string{"PATH=/bin", "GIT_OPTIONAL_LOCKS=1", "GIT_OPTIONAL_LOCKS=0"}; !slices.Equal(got, want) {
		t.Errorf("readOnlyEnv = %v, want %v", got, want)
	}
	if len(base) != 2 {
		t.Errorf("caller's env modified: %v", base)
	}
}

// IsUntracked probes ANOTHER window's worktree, once per prompt from the agent
// banner (#182). It must not refresh that worktree's index: the refresh takes
// index.lock, which can fail the other window's own `git add` / `git commit`.
// A plain `git status` does rewrite it here (the control), so the probe is what
// leaves it alone.
func TestIsUntracked_LeavesTheIndexAlone(t *testing.T) {
	dir := gitRepo(t)
	file := filepath.Join(dir, "tracked.py")
	if err := os.WriteFile(file, []byte("x\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	runGit(t, dir, "add", "tracked.py")
	runGit(t, dir, "commit", "-qm", "add")
	// same content, new mtime: the index's stat cache for the file is now stale,
	// which is what makes `git status` want to rewrite the index
	later := time.Now().Add(2 * time.Hour)
	if err := os.Chtimes(file, later, later); err != nil {
		t.Fatal(err)
	}
	index := filepath.Join(dir, ".git", "index")
	mtime := func() time.Time {
		t.Helper()
		fi, err := os.Stat(index)
		if err != nil {
			t.Fatal(err)
		}
		return fi.ModTime()
	}

	before := mtime()
	if IsUntracked(dir, "tracked.py") {
		t.Fatal("a committed file must be IsUntracked=false")
	}
	if after := mtime(); !after.Equal(before) {
		t.Fatalf("IsUntracked rewrote the index (mtime %v → %v): it must run with GIT_OPTIONAL_LOCKS=0", before, after)
	}
	runGit(t, dir, "status", "--porcelain")
	if mtime().Equal(before) {
		t.Skip("this git did not refresh the index on a plain status; the check above proves nothing here")
	}
}
