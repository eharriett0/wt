package config

import (
	"os"
	"os/exec"
	"path/filepath"
	"testing"
)

func TestAnchorPath(t *testing.T) {
	for _, tc := range []struct{ p, want string }{
		{"", ""},
		{"/abs/wts", "/abs/wts"},
		{"wts", "/main/wts"},
		{"../repo-worktrees", "/repo-worktrees"},
	} {
		if got := anchorPath(tc.p, "/main"); got != tc.want {
			t.Errorf("anchorPath(%q) = %q, want %q", tc.p, got, tc.want)
		}
	}
}

// #238: a relative worktree_root (from .wt.conf or WT_WORKTREE_ROOT) is read
// from the main checkout, so it names one directory from every cwd: the main
// checkout, a linked worktree and a subdirectory.
func TestLoad_RelativeWorktreeRootIsTheMainCheckouts(t *testing.T) {
	t.Setenv("GIT_CONFIG_GLOBAL", os.DevNull)
	t.Setenv("GIT_CONFIG_NOSYSTEM", "1")
	git := func(dir string, args ...string) {
		t.Helper()
		cmd := exec.Command("git", args...)
		cmd.Dir = dir
		cmd.Env = append(os.Environ(), "GIT_AUTHOR_NAME=t", "GIT_AUTHOR_EMAIL=t@t", "GIT_COMMITTER_NAME=t", "GIT_COMMITTER_EMAIL=t@t")
		if out, err := cmd.CombinedOutput(); err != nil {
			t.Fatalf("git %v: %v\n%s", args, err, out)
		}
	}
	main, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	git(main, "init", "-q", "-b", "main")
	if err := os.WriteFile(filepath.Join(main, ".wt.conf"), []byte("worktree_root = wts\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Join(main, "sub"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(main, "sub", "f"), []byte("x\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	git(main, "add", "-A")
	git(main, "commit", "-qm", "base")
	lane := filepath.Join(main, "wts", "lane")
	git(main, "worktree", "add", "-q", "-b", "lane", lane)

	// A relative active_work is read from the main checkout too (#238 review).
	t.Setenv("WT_ACTIVE_WORK", "aw.md")
	t.Chdir(filepath.Join(lane, "sub"))
	if c, err := Load(); err != nil || c.ActiveWork != filepath.Join(main, "aw.md") {
		t.Errorf("WT_ACTIVE_WORK=aw.md from a lane's subdirectory: %q (%v), want %q", c.ActiveWork, err, filepath.Join(main, "aw.md"))
	}
	t.Setenv("WT_ACTIVE_WORK", "")

	want := filepath.Join(main, "wts")
	for _, env := range []string{"", "wts"} {
		t.Setenv("WT_WORKTREE_ROOT", env)
		for _, cwd := range []string{main, filepath.Join(main, "sub"), lane, filepath.Join(lane, "sub")} {
			t.Chdir(cwd)
			c, err := Load()
			if err != nil {
				t.Fatal(err)
			}
			if got, _ := filepath.EvalSymlinks(c.WorktreeRoot); got != want {
				t.Errorf("WT_WORKTREE_ROOT=%q, from %s: WorktreeRoot = %q, want %q", env, cwd, c.WorktreeRoot, want)
			}
			if got, _ := filepath.EvalSymlinks(c.MainRoot); got != main {
				t.Errorf("from %s: MainRoot = %q, want %q", cwd, c.MainRoot, main)
			}
		}
	}

	// A bare layout (#238 review): every worktree read MainRoot as itself, so
	// a relative root meant a directory per lane. It is the repository now,
	// as `git worktree list` gives it, the same from every lane.
	base, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	bare := filepath.Join(base, "proj.git")
	git(base, "clone", "-q", "--bare", main, bare)
	w1, w2 := filepath.Join(base, "w1"), filepath.Join(base, "w2")
	git(bare, "worktree", "add", "-q", "-b", "w1", w1)
	git(bare, "worktree", "add", "-q", "-b", "w2", w2)
	for _, cwd := range []string{w1, w2} {
		t.Chdir(cwd)
		if c, err := Load(); err != nil || c.MainRoot != bare {
			t.Errorf("bare layout, from %s: MainRoot = %q (%v), want the repository %q", cwd, c.MainRoot, err, bare)
		}
	}
	// ⚠ Under --separate-git-dir git itself lists the git dir's parent as the
	// main worktree, from the work tree too (measured, git 2.39), so that is
	// MainRoot: not the work tree, but the same from every worktree.
	work := filepath.Join(base, "work")
	if err := os.MkdirAll(filepath.Join(base, "elsewhere"), 0o755); err != nil {
		t.Fatal(err)
	}
	git(base, "init", "-q", "-b", "main", "--separate-git-dir="+filepath.Join(base, "elsewhere", ".git"), work)
	t.Chdir(work)
	if c, err := Load(); err != nil || c.MainRoot != filepath.Join(base, "elsewhere") {
		t.Errorf("--separate-git-dir: MainRoot = %q (%v), want what git lists, %q", c.MainRoot, err, filepath.Join(base, "elsewhere"))
	}
}
