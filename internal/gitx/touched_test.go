package gitx

import (
	"os"
	"os/exec"
	"path/filepath"
	"testing"
)

func runGit(t *testing.T, dir string, args ...string) {
	t.Helper()
	cmd := exec.Command("git", args...)
	cmd.Dir = dir
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("git %v: %v\n%s", args, err, out)
	}
}

func gitRepo(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	runGit(t, dir, "init", "-q", "-b", "main")
	runGit(t, dir, "config", "user.email", "t@t.test")
	runGit(t, dir, "config", "user.name", "t")
	runGit(t, dir, "commit", "--allow-empty", "-qm", "init")
	return dir
}

func touchedContains(ss []string, want string) bool {
	for _, s := range ss {
		if s == want {
			return true
		}
	}
	return false
}

// #27: a fully-untracked new directory must list its files individually, not
// collapse to "dir/" — otherwise a collision under it never matches.
func TestTouchedFiles_UntrackedNewDirListsFiles(t *testing.T) {
	dir := gitRepo(t)
	if err := os.MkdirAll(filepath.Join(dir, "newpkg"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "newpkg", "foo.go"), []byte("package newpkg\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	got := TouchedFiles(dir, "main")
	if !touchedContains(got, "newpkg/foo.go") {
		t.Errorf("expected full path newpkg/foo.go in touched set, got %v", got)
	}
	if touchedContains(got, "newpkg/") {
		t.Errorf("dir-collapsed newpkg/ should NOT appear with -uall, got %v", got)
	}
}

// #28: a staged rename must record BOTH the old and new paths so an edit to the
// pre-rename name in another window still overlaps.
func TestTouchedFiles_RenameRecordsBothPaths(t *testing.T) {
	dir := gitRepo(t)
	if err := os.WriteFile(filepath.Join(dir, "old.go"), []byte("package x\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	runGit(t, dir, "add", "old.go")
	runGit(t, dir, "commit", "-qm", "add old")
	runGit(t, dir, "mv", "old.go", "new.go") // staged rename

	got := TouchedFiles(dir, "main")
	if !touchedContains(got, "old.go") {
		t.Errorf("expected OLD path old.go in touched set, got %v", got)
	}
	if !touchedContains(got, "new.go") {
		t.Errorf("expected NEW path new.go in touched set, got %v", got)
	}
}
