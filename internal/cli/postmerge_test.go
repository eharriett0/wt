package cli

import (
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"testing"
	"time"
)

// postMergeGh is a fake gh for cmdMergePR: open PR 99999 from branch feat-x,
// whose body "Fixes #7" is its own closing reference. Its state lives in the
// file "state"; `pr merge` exits 0 either way, and sets MERGED only when the
// file "merges" exists — so gh can exit 0 without merging, as it does for
// --help, --auto, --disable-auto, a merge queue and -R (#185).
const postMergeGh = `#!/bin/sh
d=$(dirname "$0")
case "$1 $2" in
"pr view")
	case "$*" in
	*" state "*) cat "$d/state" ;;
	*isDraft*) echo false ;;
	*messageHeadline*) echo "Fix the widget" ;;
	*join*) printf 'Fix the widget\nplain body\n' ;;
	*headRefName*) echo feat-x ;;
	*title*) echo "Fix the widget" ;;
	*" body "*) echo "Fixes #7" ;;
	*" url "*) echo "https://github.com/o/r/pull/99999" ;;
	esac ;;
"pr diff") echo a.txt ;;
"api graphql") echo 7 ;;
"issue view")
	case "$*" in
	*state*) echo OPEN ;;
	*) echo "an issue" ;;
	esac ;;
"pr merge") [ -e "$d/merges" ] && echo MERGED > "$d/state" ;;
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
		name   string
		args   []string
		state  string // what `gh pr view --json state` prints before the merge
		merges bool
		kept   bool
	}{
		{"gh prints its help and exits 0", []string{"99999", "--", "--help"}, "OPEN", false, true},
		{"gh exits 0 and the state cannot be read", []string{"99999"}, "", false, true},
		{"gh merges", []string{"99999"}, "OPEN", true, false},
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
			_ = os.Remove(filepath.Join(ghDir, "merges"))
			if tc.merges {
				if err := os.WriteFile(filepath.Join(ghDir, "merges"), nil, 0o644); err != nil {
					t.Fatal(err)
				}
			}
			t.Chdir(repo)

			if code := cmdMergePR(tc.args); code != 0 {
				t.Fatalf("cmdMergePR(%q) = %d, want 0", tc.args, code)
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
