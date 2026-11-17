package gitx

import (
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
)

// RefFileText reads a file at a ref as last fetched (#223): its text, a ref
// without it, and an error for a ref that does not resolve or something that
// is not a regular file, neither of which is a ref without the file.
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
	// The path is from the top, wherever the cwd is (#223 review).
	t.Chdir(filepath.Join(dir, "sub"))
	if text, found, err := RefFileText("refs/remotes/origin/main", ".wt.conf"); err != nil || !found || text != "merge_is_deploy = true\n" {
		t.Errorf("from a subdirectory: (%q, %v, %v)", text, found, err)
	}
}

// #223 review: a symlinked .wt.conf is a blob whose text is the link's target
// (`cat-file blob` hands it back, as GitHub's GraphQL does), which would parse
// as a config with no keys. It is an error, never read as the file.
func TestRefFileText_symlinkIsAnError(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("symlinks")
	}
	dir := gitRepo(t)
	if err := os.MkdirAll(filepath.Join(dir, "conf"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "conf", "wt.conf"), []byte("merge_is_deploy = true\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink("conf/wt.conf", filepath.Join(dir, ".wt.conf")); err != nil {
		t.Fatal(err)
	}
	runGit(t, dir, "add", ".")
	runGit(t, dir, "commit", "-qm", "link")
	t.Chdir(dir)
	text, found, err := RefFileText("HEAD", ".wt.conf")
	if err == nil || found || !strings.Contains(err.Error(), "symlink") {
		t.Errorf("a symlink: (%q, %v, %v), want a symlink error", text, found, err)
	}
}

// #223 review: in a blob:none partial clone, a .wt.conf the base added after
// the clone is in the fetched tree but its blob is not here. Offline, the read
// fails: that is an error, never "no .wt.conf".
func TestRefFileText_unfetchedBlobIsAnError(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip()
	}
	hermeticGit(t)
	root := t.TempDir()
	srv := filepath.Join(root, "srv")
	run := func(dir string, args ...string) {
		t.Helper()
		cmd := exec.Command("git", args...)
		cmd.Dir = dir
		if out, err := cmd.CombinedOutput(); err != nil {
			t.Fatalf("git %v: %v\n%s", args, err, out)
		}
	}
	if err := os.MkdirAll(srv, 0o755); err != nil {
		t.Fatal(err)
	}
	run(srv, "init", "-q", "-b", "main")
	run(srv, "config", "uploadpack.allowFilter", "true")
	run(srv, "config", "uploadpack.allowAnySHA1InWant", "true")
	if err := os.WriteFile(filepath.Join(srv, "a.txt"), []byte("a\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	run(srv, "add", "a.txt")
	run(srv, "-c", "user.name=t", "-c", "user.email=t@t", "commit", "-q", "-m", "one")
	run(root, "clone", "-q", "--filter=blob:none", "file://"+srv, "clone")
	clone := filepath.Join(root, "clone")
	if err := os.WriteFile(filepath.Join(srv, ".wt.conf"), []byte("merge_is_deploy = true\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	run(srv, "add", ".wt.conf")
	run(srv, "-c", "user.name=t", "-c", "user.email=t@t", "commit", "-q", "-m", "gate")
	run(clone, "fetch", "-q", "origin")
	if err := os.Rename(srv, srv+".gone"); err != nil { // offline
		t.Fatal(err)
	}
	t.Chdir(clone)
	text, found, err := RefFileText("refs/remotes/origin/main", ".wt.conf")
	if err == nil || found {
		t.Errorf("an unfetched blob, offline: (%q, %v, %v), want an error, never \"no file\"", text, found, err)
	}
}

func TestLsTreeEntry(t *testing.T) {
	out := "100644 blob aaa\t.wt.conf.bak\x00120000 blob bbb\t.wt.conf\x00"
	if mode, kind, oid, ok := lsTreeEntry(out, ".wt.conf"); !ok || mode != "120000" || kind != "blob" || oid != "bbb" {
		t.Errorf("lsTreeEntry = (%q, %q, %q, %v)", mode, kind, oid, ok)
	}
	if _, _, _, ok := lsTreeEntry(out, "x"); ok {
		t.Error("found an entry nobody listed")
	}
	if _, _, _, ok := lsTreeEntry("", ".wt.conf"); ok {
		t.Error("found an entry in empty output")
	}
}
