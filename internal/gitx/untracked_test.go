package gitx

import (
	"os"
	"path/filepath"
	"testing"
)

// IsUntracked must fire ONLY for a purely-untracked file ("?? path"). A staged
// (added) new file, a modified tracked file, and a committed file all have real
// content that can be pushed, so they must read false — the #113 downgrade
// applies only to content that cannot collide until it's committed.
func TestIsUntracked(t *testing.T) {
	dir := gitRepo(t)
	write := func(name, content string) {
		t.Helper()
		if err := os.WriteFile(filepath.Join(dir, name), []byte(content), 0o644); err != nil {
			t.Fatal(err)
		}
	}

	// purely untracked new file → true (the #113 case)
	write("new.py", "line1\nline2\n")
	if !IsUntracked(dir, "new.py") {
		t.Error("a new untracked file should be IsUntracked=true")
	}

	// STAGED new file (git add) → false: it's added, will be committed & pushed,
	// so it can genuinely collide and must keep its normal grade.
	write("staged.py", "z\n")
	runGit(t, dir, "add", "staged.py")
	if IsUntracked(dir, "staged.py") {
		t.Error("a staged (added) new file must be IsUntracked=false")
	}

	// committed, unmodified → false
	write("tracked.py", "x\n")
	runGit(t, dir, "add", "tracked.py")
	runGit(t, dir, "commit", "-qm", "add tracked")
	if IsUntracked(dir, "tracked.py") {
		t.Error("a committed file must be IsUntracked=false")
	}

	// modified tracked (unstaged edit) → false: a real edit, grades normally
	write("tracked.py", "y\n")
	if IsUntracked(dir, "tracked.py") {
		t.Error("a modified tracked file must be IsUntracked=false")
	}

	// empty worktree arg → false (fail-safe)
	if IsUntracked("", "anything") {
		t.Error("empty worktree must be IsUntracked=false")
	}

	// absent path → false
	if IsUntracked(dir, "does-not-exist.py") {
		t.Error("an absent path must be IsUntracked=false")
	}
}
