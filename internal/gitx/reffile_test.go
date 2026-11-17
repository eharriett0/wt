package gitx

import (
	"os"
	"path/filepath"
	"testing"
)

// RefFileText reads a file at a ref as last fetched (#223): its text, a ref
// without it, and an error for a ref that does not resolve or something that
// is not a file, neither of which is a ref without the file.
func TestRefFileText(t *testing.T) {
	dir := gitRepo(t)
	if err := os.WriteFile(filepath.Join(dir, ".wt.conf"), []byte("merge_is_deploy = true\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Join(dir, "sub"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "sub", "x"), []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}
	runGit(t, dir, "add", ".")
	runGit(t, dir, "commit", "-qm", "conf")
	runGit(t, dir, "update-ref", "refs/remotes/origin/main", "HEAD")
	runGit(t, dir, "update-ref", "refs/remotes/origin/bare", "HEAD~1")
	t.Chdir(dir)

	if text, found, err := RefFileText("refs/remotes/origin/main", ".wt.conf"); err != nil || !found || text != "merge_is_deploy = true\n" {
		t.Errorf("the file: (%q, %v, %v)", text, found, err)
	}
	if text, found, err := RefFileText("refs/remotes/origin/bare", ".wt.conf"); err != nil || found || text != "" {
		t.Errorf("a commit without it: (%q, %v, %v), want not found, no error", text, found, err)
	}
	if _, found, err := RefFileText("refs/remotes/origin/nope", ".wt.conf"); err == nil || found {
		t.Errorf("a ref that doesn't resolve: found=%v err=%v, want an error", found, err)
	}
	if _, found, err := RefFileText("refs/remotes/origin/main", "sub"); err == nil || found {
		t.Errorf("a directory: found=%v err=%v, want an error", found, err)
	}
}
