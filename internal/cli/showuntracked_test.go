package cli

import (
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"
)

// #208: merge-pr's auto-clean removes the merged lane only when it is clean.
// With status.showUntrackedFiles=no a lane holding only untracked work read as
// clean, and git's own check let `git worktree remove` delete the files. The
// whole command runs here, with postMergeGh first and alone on PATH (with git's
// own dir); the repo has no GitHub remote.
func TestCmdMergePR_autoCleanKeepsUntrackedFilesTheConfigHides(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("the fake gh is a shell script")
	}
	gitBin, err := exec.LookPath("git")
	if err != nil {
		t.Skip("no git")
	}
	hermeticGitT(t)
	ghDir := t.TempDir()
	if err := os.WriteFile(filepath.Join(ghDir, "gh"), []byte(postMergeGh), 0o755); err != nil {
		t.Fatal(err)
	}
	sep := string(os.PathListSeparator)
	t.Setenv("PATH", ghDir+sep+filepath.Dir(gitBin)+sep+"/usr/bin"+sep+"/bin")
	if got, err := exec.LookPath("gh"); err != nil || got != filepath.Join(ghDir, "gh") {
		t.Fatalf("gh resolves to %q (%v), not the fake", got, err)
	}
	t.Setenv("HOME", t.TempDir())
	postMergeSleep = func(time.Duration) {}
	t.Cleanup(func() { postMergeSleep = time.Sleep })

	for _, tc := range []struct {
		name, file string // file: written into the lane before the merge
		mech       string // #210: the file is the tracked conf.txt, its edit hidden from git status this way
		kept       bool
	}{
		{"control: nothing uncommitted, the merged lane is removed", "", "", false},
		{"an untracked file the config hides: the lane is kept, file and all", "notes.txt", "", true},
		{"an assume-unchanged edit: the lane is kept, edit and all (#210)", "conf.txt", "assume-unchanged", true},
		{"a skip-worktree edit: the lane is kept, edit and all (#210)", "conf.txt", "skip-worktree", true},
		{"an edit core.ignoreStat hides: the lane is kept, edit and all (#210)", "conf.txt", "core.ignoreStat", true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			parent, err := filepath.EvalSymlinks(t.TempDir())
			if err != nil {
				t.Fatal(err)
			}
			repo := filepath.Join(parent, "repo")
			lane := filepath.Join(parent, "repo-worktrees", "feat-x")
			if out, err := exec.Command(gitBin, "init", "-q", "-b", "main", repo).CombinedOutput(); err != nil {
				t.Fatalf("git init: %v\n%s", err, out)
			}
			if err := os.WriteFile(filepath.Join(repo, "conf.txt"), []byte("v1\n"), 0o644); err != nil {
				t.Fatal(err)
			}
			setup := [][]string{
				{"-C", repo, "add", "conf.txt"},
				{"-C", repo, "-c", "user.name=t", "-c", "user.email=t@t", "commit", "-q", "-m", "base"},
			}
			if tc.mech == "core.ignoreStat" { // the lane's checkout flags every file
				setup = append(setup, []string{"-C", repo, "config", "core.ignoreStat", "true"})
			}
			setup = append(setup, []string{"-C", repo, "worktree", "add", "-q", "-b", "feat-x", lane},
				[]string{"-C", repo, "config", "status.showUntrackedFiles", "no"})
			if tc.mech == "assume-unchanged" || tc.mech == "skip-worktree" {
				setup = append(setup, []string{"-C", lane, "update-index", "--" + tc.mech, "conf.txt"})
			}
			for _, args := range setup {
				if out, err := exec.Command(gitBin, args...).CombinedOutput(); err != nil {
					t.Fatalf("git %v: %v\n%s", args, err, out)
				}
			}
			head, err := exec.Command(gitBin, "-C", lane, "rev-parse", "HEAD").Output()
			if err != nil {
				t.Fatal(err)
			}
			for name, content := range map[string][]byte{"state": []byte("OPEN"), "head": head, "merges": nil} {
				if err := os.WriteFile(filepath.Join(ghDir, name), content, 0o644); err != nil {
					t.Fatal(err)
				}
			}
			for _, name := range []string{"fails", "issue7"} {
				_ = os.Remove(filepath.Join(ghDir, name))
			}
			file := filepath.Join(lane, tc.file)
			if tc.file != "" {
				if err := os.WriteFile(file, []byte("not committed\n"), 0o644); err != nil {
					t.Fatal(err)
				}
				if st, _ := exec.Command(gitBin, "-C", lane, "status", "--porcelain").Output(); len(st) != 0 {
					t.Fatalf("fixture: git status shows %q; it should hide the work", st)
				}
			}
			t.Chdir(repo)

			stderr, err := os.Create(filepath.Join(t.TempDir(), "stderr"))
			if err != nil {
				t.Fatal(err)
			}
			old := os.Stderr
			os.Stderr = stderr
			code := cmdMergePR([]string{"99999"})
			os.Stderr = old
			stderr.Close()
			warned, _ := os.ReadFile(stderr.Name())
			if code != 0 {
				t.Fatalf("cmdMergePR = %d, want 0 (the merge happened)\n%s", code, warned)
			}
			_, statErr := os.Stat(lane)
			branchErr := exec.Command(gitBin, "-C", repo, "rev-parse", "-q", "--verify", "refs/heads/feat-x").Run()
			if gotKept := statErr == nil && branchErr == nil; gotKept != tc.kept {
				t.Fatalf("lane kept=%v, branch kept=%v; want kept=%v\n%s", statErr == nil, branchErr == nil, tc.kept, warned)
			}
			if !tc.kept {
				return
			}
			if b, err := os.ReadFile(file); err != nil || string(b) != "not committed\n" {
				t.Errorf("the uncommitted work in %s is gone (%v)", tc.file, err)
			}
			// wt's own check refuses, before git is asked to remove anything.
			if w := string(warned); !strings.Contains(w, "worktree for feat-x not auto-removed: has uncommitted changes") {
				t.Errorf("stderr = %q, want auto-clean's own refusal of uncommitted changes", w)
			}
		})
	}
}
