package cli

import (
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"

	"github.com/eharriett0/wt/internal/ghx"
)

// forwardedGh answers merge-pr's reads ONLY for other/repo's PR 99999: by its
// URL, or resolved with -R other/repo. This clone's PR 99999 (o/r) is MERGED,
// with a lane here; any read that reaches it, or anything else, is logged as
// UNHANDLED and fails. other/repo's PR is OPEN, body "Fixes #3".
const forwardedGh = `#!/bin/sh
d=$(dirname "$0")
printf '%s\n' "$*" >> "$d/calls"
U=https://github.com/other/repo/pull/99999
case "$*" in
"auth status"*) exit 0 ;;
*"--json number,url,baseRefName"*"-R other/repo"*) echo "99999 $U main"; exit 0 ;;
*"expr=refs/heads/main:.wt.conf"*) echo '{"oid":"` + fakeHead + `","conf":null}'; exit 0 ;;
*"--json number,url"*"-R other/repo"*) echo "99999 $U"; exit 0 ;;
*"--json isDraft"*"-R other/repo"*) echo false; exit 0 ;;
*statusCheckRollup*) cat "$d/rollup"; exit 0 ;;
"api --hostname github.com repos/other/repo/rules/branches/main"*) echo '[]'; exit 0 ;;
"api --hostname github.com repos/other/repo/branches/main"*) echo '{"protected":false}'; exit 0 ;;
"pr merge $U "*) printf '%s\n' "$@" > "$d/argv"; touch "$d/merged"; exit 0 ;;
"pr view $U --json state "*) [ -e "$d/merged" ] && echo MERGED || echo OPEN; exit 0 ;;
"pr view $U --json headRefName "*) echo feat-other; exit 0 ;;
"pr view $U --json headRefOid "*) echo ` + fakeHead + `; exit 0 ;;
"pr view $U --json body "*) echo "Fixes #3"; exit 0 ;;
"pr view $U --json title "*) echo "Other repo fix"; exit 0 ;;
"pr view $U --json url "*) echo "$U"; exit 0 ;;
"pr view $U --json commits "*) echo "Other repo fix"; exit 0 ;;
"pr diff $U "*) echo a.txt; exit 0 ;;
"issue view https://github.com/other/repo/issues/3 --json state "*) echo OPEN; exit 0 ;;
"issue view https://github.com/other/repo/issues/3 --json title "*) echo "other repo issue"; exit 0 ;;
"api graphql -F owner=other -F name=repo "*squashMergeCommitTitle*) echo '{"title":"PR_TITLE","message":"PR_BODY"}'; exit 0 ;;
"api graphql"*closingIssuesReferences*"url=$U"*) echo 3; exit 0 ;;
"api graphql"*parents*"url=$U"*) echo '{"message":"Other repo fix","parents":1}'; exit 0 ;;
esac
printf 'UNHANDLED %s\n' "$*" >> "$d/calls"
exit 1
`

// #228: `wt merge-pr 99999 -- -R other/repo` merges other/repo's PR 99999,
// so every read merge-pr makes is of THAT PR: the precheck, the guard, the
// close check (its body, title, commits, issues and squash settings), the
// merge, the post-merge reads. Before, only the checks gate, the base config
// and the deploy gate passed -R; the rest read this clone's PR 99999, which
// here is MERGED, so merge-pr skipped the merge as "already merged" and went
// to auto-clean this clone's lane. Auto-clean never touches this clone for a
// PR in another repository.
func TestCmdMergePR_forwardedRepoReadsOnlyThatPR(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("the fake gh is a shell script")
	}
	gitBin, err := exec.LookPath("git")
	if err != nil {
		t.Skip("no git")
	}
	hermeticGitT(t)
	ghDir := t.TempDir()
	if err := os.WriteFile(filepath.Join(ghDir, "gh"), []byte(forwardedGh), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(ghDir, "rollup"), []byte(rollupIn("other/repo", runNode("test", "COMPLETED", "SUCCESS"))), 0o644); err != nil {
		t.Fatal(err)
	}
	sep := string(os.PathListSeparator)
	t.Setenv("PATH", ghDir+sep+filepath.Dir(gitBin)+sep+"/usr/bin"+sep+"/bin")
	if got, err := exec.LookPath("gh"); err != nil || got != filepath.Join(ghDir, "gh") {
		t.Fatalf("gh resolves to %q (%v), not the fake", got, err)
	}
	t.Setenv("HOME", t.TempDir())
	t.Setenv("WT_SKIP_COLLISION", "1")
	postMergeSleep = func(time.Duration) {}
	t.Cleanup(func() { postMergeSleep = time.Sleep })

	parent, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	repo := filepath.Join(parent, "repo")
	lane := filepath.Join(parent, "repo-worktrees", "feat-x")
	for _, args := range [][]string{
		{"init", "-q", "-b", "main", repo},
		{"-C", repo, "remote", "add", "origin", "https://github.com/o/r.git"},
		{"-C", repo, "-c", "user.name=t", "-c", "user.email=t@t", "commit", "-q", "--allow-empty", "-m", "base"},
		{"-C", repo, "worktree", "add", "-q", "-b", "feat-x", lane},
	} {
		if out, err := exec.Command(gitBin, args...).CombinedOutput(); err != nil {
			t.Fatalf("git %v: %v\n%s", args, err, out)
		}
	}
	t.Chdir(repo)

	var code int
	out, errOut := captureT(t, func() {
		code = cmdMergePR([]string{"99999", "--merge-foreign", "--", "-R", "other/repo"})
	})
	all := out + errOut
	calls, _ := os.ReadFile(filepath.Join(ghDir, "calls"))
	if code != 0 {
		t.Fatalf("cmdMergePR = %d\n%s\ngh calls:\n%s", code, all, calls)
	}
	for _, ln := range strings.Split(strings.TrimSpace(string(calls)), "\n") {
		if strings.HasPrefix(ln, "UNHANDLED ") {
			t.Errorf("a read that is not of other/repo's PR (this clone's PR, or another repository's): %s", strings.TrimPrefix(ln, "UNHANDLED "))
		}
	}
	argv, err := os.ReadFile(filepath.Join(ghDir, "argv"))
	if err != nil || !strings.HasPrefix(string(argv), "pr\nmerge\nhttps://github.com/other/repo/pull/99999\n") {
		t.Errorf("gh pr merge argv = %q (%v), want other/repo's PR merged by its URL\n%s", argv, err, all)
	}
	if !strings.Contains(all, "#3") || strings.Contains(all, "already merged") {
		t.Errorf("the close check must judge other/repo's PR (Fixes #3), and its state, not this clone's:\n%s", all)
	}
	if _, err := os.Stat(lane); err != nil || strings.Contains(all, "removed worktree") {
		t.Errorf("auto-clean touched this clone's lane for a PR in another repository: %v\n%s", err, all)
	}
	if !strings.Contains(all, "not this clone's origin") {
		t.Errorf("want auto-clean to say the PR is in another repository:\n%s", all)
	}
}

// A forwarded -R that can't be resolved refuses, rather than read this
// clone's PR of the same number (#228).
func TestCmdMergePR_forwardedRepoUnresolvableRefuses(t *testing.T) {
	orig := resolvePRIn
	t.Cleanup(func() { resolvePRIn = orig })
	resolvePRIn = func(pr, repo string) (ghx.PRAt, error) { return ghx.PRAt{}, os.ErrNotExist }
	repo := t.TempDir()
	gitInitForTest(t, repo)
	t.Chdir(repo)
	var code int
	_, errOut := captureT(t, func() { code = cmdMergePR([]string{"99999", "--", "-R", "other/repo"}) })
	if code != 1 || !strings.Contains(errOut, "rather than read this clone's PR #99999") {
		t.Errorf("cmdMergePR = %d, want 1 with the refusal:\n%s", code, errOut)
	}
}
