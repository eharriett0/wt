package worktree

import (
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"
)

// #208: with status.showUntrackedFiles=no, `wt clean -y` read a shipped
// worktree holding only untracked work as clean and removed it, untracked files
// and all, and `git worktree remove`'s own check, blinded the same way, let it.
// These run Clean itself against a scratch origin, with a fake gh first on PATH
// that fails every call (no PR is ever found, and nothing reaches GitHub).

// failingGh puts a gh that fails every call first on PATH.
func failingGh(t *testing.T) {
	t.Helper()
	if runtime.GOOS == "windows" {
		t.Skip("the fake gh is a shell script")
	}
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "gh"), []byte("#!/bin/sh\nexit 1\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", dir+string(os.PathListSeparator)+os.Getenv("PATH"))
	if got, err := exec.LookPath("gh"); err != nil || got != filepath.Join(dir, "gh") {
		t.Fatalf("gh resolves to %q (%v), not the fake", got, err)
	}
}

// captured runs fn with stdout and stderr going to a file, and returns both.
func captured(t *testing.T, fn func()) string {
	t.Helper()
	f, err := os.Create(filepath.Join(t.TempDir(), "out"))
	if err != nil {
		t.Fatal(err)
	}
	stdout, stderr := os.Stdout, os.Stderr
	os.Stdout, os.Stderr = f, f
	defer func() { os.Stdout, os.Stderr = stdout, stderr; f.Close() }()
	fn()
	b, _ := os.ReadFile(f.Name())
	return string(b)
}

// shippedWorktree makes a worktree on branch "shipped" whose one commit (a
// .gitignore for *.log) is pushed under its own name and is on origin's main
// too, created an hour ago: everything `wt clean -y` needs to reap it.
func (f *adoptFixture) shippedWorktree() string {
	f.t.Helper()
	dir := f.worktreeOn("shipped", f.base)
	if err := os.WriteFile(filepath.Join(dir, ".gitignore"), []byte("*.log\n"), 0o644); err != nil {
		f.t.Fatal(err)
	}
	gitW(f.t, dir, "add", ".gitignore")
	f.commitHere(dir, "shipped work")
	gitW(f.t, dir, "push", "-q", "-u", "origin", "shipped")
	gitW(f.t, dir, "push", "-q", "origin", "shipped:main")
	old := time.Now().Add(-time.Hour)
	if err := os.Chtimes(filepath.Join(dir, ".git"), old, old); err != nil {
		f.t.Fatal(err)
	}
	return dir
}

func TestClean_KeepsUntrackedFilesTheConfigHides(t *testing.T) {
	for _, tc := range []struct {
		name, file string // file: written into the worktree before the clean
		hide       bool   // status.showUntrackedFiles=no
		reaped     bool
		says       string
	}{
		{"control: nothing uncommitted, reaped", "", true, true, "removed worktree shipped"},
		{"an untracked file the config hides: kept", "notes.txt", true, false, "shipped — has uncommitted changes (1 file(s)), leave alone"},
		{"an untracked file, default config: kept", "notes.txt", false, false, "shipped — has uncommitted changes (1 file(s)), leave alone"},
		// Ignored files never counted: the worktree is reaped and the file goes with it.
		{"an ignored file the config also hides: reaped with it", "build.log", true, true, "removed worktree shipped"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f := newAdoptFixture(t)
			failingGh(t)
			if tc.hide {
				gitW(t, f.repo, "config", "status.showUntrackedFiles", "no")
			}
			dir := f.shippedWorktree()
			file := filepath.Join(dir, tc.file)
			if tc.file != "" {
				if err := os.WriteFile(file, []byte("not committed\n"), 0o644); err != nil {
					t.Fatal(err)
				}
			}
			var err error
			out := captured(t, func() { err = Clean(f.c, true, false, false, []string{"shipped"}) })
			if err != nil {
				t.Fatalf("Clean: %v\n%s", err, out)
			}
			if !strings.Contains(out, tc.says) {
				t.Errorf("clean said:\n%s\nwant it to say %q", out, tc.says)
			}
			if _, serr := os.Stat(dir); (serr == nil) == tc.reaped {
				t.Errorf("worktree kept=%v, want reaped=%v\n%s", serr == nil, tc.reaped, out)
			}
			if f.has("refs/heads/shipped") == tc.reaped {
				t.Errorf("branch kept=%v, want reaped=%v", !tc.reaped, tc.reaped)
			}
			if tc.file != "" && !tc.reaped {
				if _, serr := os.Stat(file); serr != nil {
					t.Errorf("the untracked file is gone: %v", serr)
				}
			}
		})
	}
}

// Remove (merge-pr's auto-clean and claim's rollback go through it) refuses an
// untracked file the config hides, in its own check, before git is asked.
func TestRemove_RefusesUntrackedFilesTheConfigHides(t *testing.T) {
	f := newAdoptFixture(t)
	gitW(t, f.repo, "config", "status.showUntrackedFiles", "no")
	dir := f.worktreeOn("lane", f.base)
	file := filepath.Join(dir, "notes.txt")
	if err := os.WriteFile(file, []byte("not committed\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	err := Remove(f.c, dir, "lane", false)
	if err == nil || !strings.Contains(err.Error(), "has uncommitted changes") {
		t.Fatalf("Remove = %v, want its own refusal of uncommitted changes", err)
	}
	if _, serr := os.Stat(file); serr != nil || !f.has("refs/heads/lane") {
		t.Errorf("file kept: %v, branch kept: %v", serr == nil, f.has("refs/heads/lane"))
	}
}
