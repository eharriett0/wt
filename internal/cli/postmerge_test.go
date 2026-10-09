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

// postMergeGh is a fake gh for cmdMergePR: open PR 99999 from branch feat-x,
// whose body "Fixes #7" is its own closing reference, in a repo with GitHub's
// default squash message. Its state lives in the file "state"; `pr merge` sets
// MERGED (and closes #7) only when the file "merges" exists — so gh can exit 0
// without merging, as it does for --help, --auto, --disable-auto, a merge queue
// and -R (#185) — and exits 1 when the file "fails" exists, after the merge or
// instead of it, as `-d` does when it cannot delete a branch a worktree has
// checked out (#196). Its head commit (headRefOid) is the file "head", empty
// when gh does not know it (#187).
const postMergeGh = `#!/bin/sh
d=$(dirname "$0")
case "$1 $2" in
"pr view")
	case "$*" in
	*" state "*) cat "$d/state" ;;
	*isDraft*) echo false ;;
	*messageHeadline*) echo "Fix the widget" ;;
	*headRefName*) echo feat-x ;;
	*headRefOid*) cat "$d/head" ;;
	*title*) echo "Fix the widget" ;;
	*" body "*) echo "Fixes #7" ;;
	*" url "*) echo "https://github.com/o/r/pull/99999" ;;
	esac ;;
"pr diff") echo a.txt ;;
"api graphql")
	case "$*" in
	*squashMergeCommitTitle*) echo '{"title":"COMMIT_OR_PR_TITLE","message":"COMMIT_MESSAGES"}' ;;
	*parents*) echo '{"message":"Fix the widget\n\nplain body","parents":1}' ;;
	*) echo 7 ;;
	esac ;;
"issue view")
	case "$*" in
	*state*) cat "$d/issue7" 2>/dev/null || echo OPEN ;;
	*) echo "an issue" ;;
	esac ;;
"pr merge")
	[ -e "$d/merges" ] && echo MERGED > "$d/state" && echo CLOSED > "$d/issue7"
	[ -e "$d/fails" ] && echo "failed to delete local branch feat-x" >&2 && exit 1 ;;
esac
exit 0
`

// TestCmdMergePR_keepsTheLaneUnlessMerged drives the whole merge-pr command in
// a scratch repo with a worktree for the PR's branch: when gh exits 0 but the
// PR is still OPEN the worktree and branch survive, and only a real merge
// removes them (#185). The fake gh sits first and alone on PATH (with git's
// own dir), the test stops unless "gh" resolves to it, and the repo has no
// GitHub remote — it can never reach a real PR.
func TestCmdMergePR_keepsTheLaneUnlessMerged(t *testing.T) {
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

	const verified = "issue #7 changed state on merge: OPEN → CLOSED"
	for _, tc := range []struct {
		name     string
		args     []string
		state    string // what `gh pr view --json state` prints before the merge
		merges   bool
		fails    bool // gh exits 1, after merging when merges is set (#196)
		unpushed bool // the lane gets a commit after its tip became the PR's head
		noHead   bool // gh does not know the PR's head commit
		kept     bool
		code     int
		warn     string // in stderr
	}{
		{"gh prints its help and exits 0", []string{"99999", "--", "--help"}, "OPEN", false, false, false, false, true, 0, ""},
		{"gh exits 0 and the state cannot be read", []string{"99999"}, "", false, false, false, false, true, 0, ""},
		{"gh merges", []string{"99999"}, "OPEN", true, false, false, false, false, 0, verified},
		// #187: the PR merged, but the lane has a commit that was not in it
		{"gh merges, the lane has an unpushed commit", []string{"99999"}, "OPEN", true, false, true, false, true, 0,
			"feat-x has 1 commit(s) that were not in PR #99999; kept the worktree and branch"},
		{"gh merges, its head commit is unknown", []string{"99999"}, "OPEN", true, false, false, true, true, 0,
			"feat-x may have commits that were not in PR #99999: wt could not read the PR's head commit"},
		// #196: gh fails AFTER merging (`-d` cannot delete the branch the lane
		// has checked out): the closes are still verified and the lane cleaned
		{"gh merges, then fails", []string{"99999", "--", "-d"}, "OPEN", true, true, false, false, false, 0,
			"gh pr merge exited non-zero, but PR #99999 is MERGED"},
		{"gh merges, then fails: the verify still runs", []string{"99999", "--", "-d"}, "OPEN", true, true, false, false, false, 0, verified},
		{"gh merges, then fails, the lane has an unpushed commit", []string{"99999", "--", "-d"}, "OPEN", true, true, true, false, true, 0,
			"feat-x has 1 commit(s) that were not in PR #99999; kept the worktree and branch"},
		{"gh fails without merging", []string{"99999"}, "OPEN", false, true, false, false, true, 1, ""},
		{"gh fails and the state cannot be read", []string{"99999"}, "", false, true, false, false, true, 1, ""},
	} {
		t.Run(tc.name, func(t *testing.T) {
			parent, err := filepath.EvalSymlinks(t.TempDir())
			if err != nil {
				t.Fatal(err)
			}
			repo := filepath.Join(parent, "repo")
			lane := filepath.Join(parent, "repo-worktrees", "feat-x")
			for _, args := range [][]string{
				{"init", "-q", "-b", "main", repo},
				{"-C", repo, "-c", "user.name=t", "-c", "user.email=t@t", "commit", "-q", "--allow-empty", "-m", "base"},
				{"-C", repo, "worktree", "add", "-q", "-b", "feat-x", lane},
			} {
				if out, err := exec.Command(gitBin, args...).CombinedOutput(); err != nil {
					t.Fatalf("git %v: %v\n%s", args, err, out)
				}
			}
			if err := os.WriteFile(filepath.Join(ghDir, "state"), []byte(tc.state), 0o644); err != nil {
				t.Fatal(err)
			}
			head, err := exec.Command(gitBin, "-C", lane, "rev-parse", "HEAD").Output()
			if err != nil || tc.noHead {
				head = nil
			}
			if err := os.WriteFile(filepath.Join(ghDir, "head"), head, 0o644); err != nil {
				t.Fatal(err)
			}
			if tc.unpushed {
				if out, err := exec.Command(gitBin, "-C", lane, "-c", "user.name=t", "-c", "user.email=t@t",
					"commit", "-q", "--allow-empty", "-m", "never pushed").CombinedOutput(); err != nil {
					t.Fatalf("git commit: %v\n%s", err, out)
				}
			}
			for name, on := range map[string]bool{"merges": tc.merges, "fails": tc.fails, "issue7": false} {
				_ = os.Remove(filepath.Join(ghDir, name))
				if on {
					if err := os.WriteFile(filepath.Join(ghDir, name), nil, 0o644); err != nil {
						t.Fatal(err)
					}
				}
			}
			t.Chdir(repo)

			stderr, err := os.Create(filepath.Join(t.TempDir(), "stderr"))
			if err != nil {
				t.Fatal(err)
			}
			old := os.Stderr
			os.Stderr = stderr
			code := cmdMergePR(tc.args)
			os.Stderr = old
			stderr.Close()
			if code != tc.code {
				t.Fatalf("cmdMergePR(%q) = %d, want %d", tc.args, code, tc.code)
			}
			if warned, _ := os.ReadFile(stderr.Name()); !strings.Contains(string(warned), tc.warn) {
				t.Errorf("stderr = %q, want it to say %q", warned, tc.warn)
			}
			_, statErr := os.Stat(lane)
			branchErr := exec.Command(gitBin, "-C", repo, "rev-parse", "-q", "--verify", "refs/heads/feat-x").Run()
			if gotKept := statErr == nil && branchErr == nil; gotKept != tc.kept {
				t.Errorf("worktree kept=%v (stat err %v), branch kept=%v; want both kept=%v",
					statErr == nil, statErr, branchErr == nil, tc.kept)
			}
		})
	}
}
