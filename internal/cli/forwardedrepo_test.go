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
	"github.com/eharriett0/wt/internal/merge"
)

// forwardedGh answers merge-pr's reads ONLY for other/repo's PR 99999: by its
// URL, or resolved with -R other/repo. This clone's PR 99999 (o/r) is MERGED,
// with a lane here; any read that reaches it, or anything else, is logged as
// UNHANDLED and fails. other/repo's PR is OPEN (MERGED once "$d/merged"
// exists), body "Fixes #3", head branch feat-other in the repository named by
// "$d/head" (other/repo when absent). With "$d/mergefail", gh pr merge merges
// and then exits 1, as `-- -d` does (#196).
const forwardedGh = `#!/bin/sh
d=$(dirname "$0")
printf '%s\n' "$*" >> "$d/calls"
U=https://github.com/other/repo/pull/99999
H=other/repo
[ -e "$d/head" ] && H=$(cat "$d/head")
case "$*" in
"auth status"*) exit 0 ;;
*"--json number,url,baseRefName"*"-R other/repo"*|"pr view $U --json number,url,baseRefName"*) echo "99999 $U main"; exit 0 ;;
*"expr=refs/heads/main:.wt.conf"*) echo '{"oid":"` + fakeHead + `","conf":null}'; exit 0 ;;
*"--json number,url,headRepository,headRepositoryOwner"*"-R other/repo"*|"pr view $U --json number,url,headRepository,headRepositoryOwner"*) printf '99999 %s\n%s\n' "$U" "$H"; exit 0 ;;
*"--json number,url "*"-R other/repo"*|"pr view $U --json number,url "*) echo "99999 $U"; exit 0 ;;
*"--json isDraft"*"-R other/repo"*|"pr view $U --json isDraft"*) echo false; exit 0 ;;
*statusCheckRollup*) cat "$d/rollup"; exit 0 ;;
"api --hostname github.com repos/other/repo/rules/branches/main"*) echo '[]'; exit 0 ;;
"api --hostname github.com repos/other/repo/branches/main"*) echo '{"protected":false}'; exit 0 ;;
"pr merge $U "*) printf '%s\n' "$@" > "$d/argv"; touch "$d/merged"; [ -e "$d/mergefail" ] && { echo "failed to delete local branch" >&2; exit 1; }; exit 0 ;;
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
"api graphql -F owner=other -F name=repo "*squashMergeCommitTitle*"--hostname github.com") echo '{"title":"PR_TITLE","message":"PR_BODY"}'; exit 0 ;;
"api graphql"*closingIssuesReferences*"url=$U"*"--hostname github.com") echo 3; exit 0 ;;
"api graphql"*parents*"url=$U"*"--hostname github.com") echo '{"message":"Other repo fix","parents":1}'; exit 0 ;;
esac
printf 'UNHANDLED %s\n' "$*" >> "$d/calls"
exit 1
`

const forwardedURL = "https://github.com/other/repo/pull/99999"

// forwardedSetup puts forwardedGh first on PATH and makes a clone of o/r with
// a lane on laneBranch, cwd in the main checkout. It returns the fake's
// directory and the lane's path.
func forwardedSetup(t *testing.T, laneBranch string) (ghDir, lane string) {
	t.Helper()
	if runtime.GOOS == "windows" {
		t.Skip("the fake gh is a shell script")
	}
	gitBin, err := exec.LookPath("git")
	if err != nil {
		t.Skip("no git")
	}
	hermeticGitT(t)
	ghDir = t.TempDir()
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
	lane = filepath.Join(parent, "repo-worktrees", laneBranch)
	for _, args := range [][]string{
		{"init", "-q", "-b", "main", repo},
		{"-C", repo, "remote", "add", "origin", "https://github.com/o/r.git"},
		{"-C", repo, "-c", "user.name=t", "-c", "user.email=t@t", "commit", "-q", "--allow-empty", "-m", "base"},
		{"-C", repo, "worktree", "add", "-q", "-b", laneBranch, lane},
	} {
		if out, err := exec.Command(gitBin, args...).CombinedOutput(); err != nil {
			t.Fatalf("git %v: %v\n%s", args, err, out)
		}
	}
	t.Chdir(repo)
	return ghDir, lane
}

// forwardedCalls fails t for every read the fake didn't answer (this clone's
// PR, or another repository's) and returns every gh call made.
func forwardedCalls(t *testing.T, ghDir string) string {
	t.Helper()
	calls, _ := os.ReadFile(filepath.Join(ghDir, "calls"))
	for _, ln := range strings.Split(strings.TrimSpace(string(calls)), "\n") {
		if strings.HasPrefix(ln, "UNHANDLED ") {
			t.Errorf("a read that is not of other/repo's PR (this clone's PR, or another repository's): %s", strings.TrimPrefix(ln, "UNHANDLED "))
		}
	}
	return string(calls)
}

// #228: `wt merge-pr 99999 -- -R other/repo` merges other/repo's PR 99999,
// so every read merge-pr makes is of THAT PR: the precheck, the guard, the
// close check (its body, title, commits, issues and squash settings), the
// merge, the post-merge reads. Before, only the checks gate, the base config
// and the deploy gate passed -R; the rest read this clone's PR 99999, which
// here is MERGED, so merge-pr skipped the merge as "already merged" and went
// to auto-clean this clone's lane. Auto-clean never touches this clone for a
// PR whose repository and branch are both elsewhere. #228 review: the same
// PR given by its URL (the form the guard's own hints print under -R) reads
// the same way; its issues and squash settings were read in the cwd's
// repository.
func TestCmdMergePR_forwardedRepoReadsOnlyThatPR(t *testing.T) {
	for name, args := range map[string][]string{
		"-R":  {"99999", "--merge-foreign", "--", "-R", "other/repo"},
		"URL": {forwardedURL, "--merge-foreign"},
	} {
		t.Run(name, func(t *testing.T) {
			ghDir, lane := forwardedSetup(t, "feat-x")
			var code int
			out, errOut := captureT(t, func() { code = cmdMergePR(args) })
			all := out + errOut
			calls := forwardedCalls(t, ghDir)
			if code != 0 {
				t.Fatalf("cmdMergePR = %d\n%s\ngh calls:\n%s", code, all, calls)
			}
			argv, err := os.ReadFile(filepath.Join(ghDir, "argv"))
			if err != nil || !strings.HasPrefix(string(argv), "pr\nmerge\n"+forwardedURL+"\n") {
				t.Errorf("gh pr merge argv = %q (%v), want other/repo's PR merged by its URL\n%s", argv, err, all)
			}
			if !strings.Contains(all, "#3") || strings.Contains(all, "already merged") {
				t.Errorf("the close check must judge other/repo's PR (Fixes #3), and its state, not this clone's:\n%s", all)
			}
			if !strings.Contains(calls, "issue view https://github.com/other/repo/issues/3") {
				t.Errorf("issue #3 must be read in other/repo, by its URL:\n%s", calls)
			}
			if _, err := os.Stat(lane); err != nil || strings.Contains(all, "removed worktree") {
				t.Errorf("auto-clean touched this clone's lane for a PR in another repository: %v\n%s", err, all)
			}
			if !strings.Contains(all, "neither its repository (other/repo) nor its branch's (other/repo) is this clone's origin") {
				t.Errorf("want auto-clean to say the PR is in another repository:\n%s", all)
			}
			if strings.Contains(all, "PR #http") {
				t.Errorf("a message names the PR as #<url>:\n%s", all)
			}
		})
	}
}

// #228 review: a forwarded PR that is already merged skips the merge and goes
// to auto-clean from the precheck, which must get the resolved PR too: given
// the bare number it read this clone's PR 99999's head branch and cleaned the
// lane of that name. Here this clone HAS a lane on the forwarded PR's head
// branch's name, the case that would lose work.
func TestCmdMergePR_forwardedRepoAlreadyMergedLeavesTheLane(t *testing.T) {
	ghDir, lane := forwardedSetup(t, "feat-other")
	if err := os.WriteFile(filepath.Join(ghDir, "merged"), nil, 0o644); err != nil {
		t.Fatal(err)
	}
	var code int
	out, errOut := captureT(t, func() {
		code = cmdMergePR([]string{"99999", "--", "-R", "other/repo"})
	})
	all := out + errOut
	calls := forwardedCalls(t, ghDir)
	if code != 0 || !strings.Contains(all, "PR "+forwardedURL+" is already merged") {
		t.Fatalf("cmdMergePR = %d, want 0 with the already-merged skip:\n%s\ngh calls:\n%s", code, all, calls)
	}
	if strings.Contains(calls, "pr merge") {
		t.Errorf("merged an already-merged PR:\n%s", calls)
	}
	if _, err := os.Stat(lane); err != nil || strings.Contains(all, "removed worktree") {
		t.Errorf("auto-clean removed this clone's feat-other lane for other/repo's PR: %v\n%s", err, all)
	}
	if !strings.Contains(all, "nothing to clean") {
		t.Errorf("want auto-clean to say why it cleans nothing:\n%s", all)
	}
}

// #228 review: gh can merge and then exit non-zero (#196, `-- -d`); merge-pr
// then reads the state to decide, and under -R that read must be of the
// forwarded PR, not this clone's PR of the same number.
func TestCmdMergePR_forwardedRepoMergeFailureReadsThatPR(t *testing.T) {
	ghDir, lane := forwardedSetup(t, "feat-x")
	if err := os.WriteFile(filepath.Join(ghDir, "mergefail"), nil, 0o644); err != nil {
		t.Fatal(err)
	}
	var code int
	out, errOut := captureT(t, func() {
		code = cmdMergePR([]string{"99999", "--merge-foreign", "--", "-R", "other/repo"})
	})
	all := out + errOut
	calls := forwardedCalls(t, ghDir)
	if code != 0 {
		t.Fatalf("cmdMergePR = %d, want 0: the PR merged though gh exited 1\n%s\ngh calls:\n%s", code, all, calls)
	}
	if !strings.Contains(calls, "pr view "+forwardedURL+" --json state") {
		t.Errorf("the state after gh's failure must be read by the PR's URL:\n%s", calls)
	}
	if _, err := os.Stat(lane); err != nil {
		t.Errorf("lane removed: %v", err)
	}
}

// #228 review: origin is a fork and -R names upstream. The PR's branch is in
// origin, so its lane can be here: auto-clean goes on to read the PR's head
// branch (by URL), instead of saying no lane here is the PR's.
func TestCmdMergePR_forwardedRepoFromAForkOriginCleansOn(t *testing.T) {
	ghDir, _ := forwardedSetup(t, "feat-x")
	if err := os.WriteFile(filepath.Join(ghDir, "head"), []byte("o/r\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	var code int
	out, errOut := captureT(t, func() {
		code = cmdMergePR([]string{"99999", "--merge-foreign", "--", "-R", "other/repo"})
	})
	all := out + errOut
	calls := forwardedCalls(t, ghDir)
	if code != 0 {
		t.Fatalf("cmdMergePR = %d\n%s\ngh calls:\n%s", code, all, calls)
	}
	if strings.Contains(all, "nothing to clean") || !strings.Contains(calls, "pr view "+forwardedURL+" --json headRefName") {
		t.Errorf("auto-clean must go on for a PR whose branch is in origin:\n%s\ngh calls:\n%s", all, calls)
	}
}

// laneMayBeHere: the PR is origin's, or its branch is; never on an unknown
// side alone.
func TestLaneMayBeHere(t *testing.T) {
	pr := func(o, n, ho, hn string) ghx.PRAt {
		return ghx.PRAt{Owner: o, Name: n, HeadOwner: ho, HeadName: hn}
	}
	for _, tc := range []struct {
		name        string
		at          ghx.PRAt
		owner, repo string
		want        bool
	}{
		{"origin's own PR", pr("o", "r", "o", "r"), "o", "r", true},
		{"origin's PR from a contributor's fork", pr("o", "r", "c", "r"), "o", "r", true},
		{"origin is the fork, -R upstream", pr("up", "r", "o", "r"), "o", "r", true},
		{"case differs", pr("O", "R", "x", "y"), "o", "r", true},
		{"another repository's own PR", pr("other", "repo", "other", "repo"), "o", "r", false},
		{"another repository, deleted fork", pr("other", "repo", "", ""), "o", "r", false},
		{"unknown origin", pr("o", "r", "o", "r"), "", "", false},
		{"same owner, other name", pr("o", "x", "o", "y"), "o", "r", false},
	} {
		if got := laneMayBeHere(tc.at, tc.owner, tc.repo); got != tc.want {
			t.Errorf("%s: laneMayBeHere = %v, want %v", tc.name, got, tc.want)
		}
	}
}

// The hints' gh commands name the forwarded repository's PR, and never with a
// "#" a shell reads as a comment (#228 review: `gh pr ready #5 -R o/r` ran a
// bare `gh pr ready`, marking the current branch's PR ready).
func TestPRCommandHints(t *testing.T) {
	for _, tc := range []struct{ got, want string }{
		{readyHint("5", "other/repo"), "gh pr ready 5 -R other/repo"},
		{readyHint("#5", "other/repo"), "gh pr ready 5 -R other/repo"},
		{readyHint("5", ""), "gh pr ready 5"},
		{readyHint(forwardedURL, ""), "gh pr ready " + forwardedURL},
		{prCommand("checks", "#5", "other/repo", "--watch"), "gh pr checks 5 --watch -R other/repo"},
	} {
		if tc.got != tc.want {
			t.Errorf("%q, want %q", tc.got, tc.want)
		}
	}
	hints := strings.Join(refuseHints("#5", merge.ChecksVerdict{}, checksOpts{repo: "other/repo"}), " | ")
	if !strings.Contains(hints, "gh pr checks 5 --watch -R other/repo") {
		t.Errorf("the checks gate's wait hint must name the forwarded PR: %s", hints)
	}
}

// A forwarded -R that can't be resolved refuses, rather than read this
// clone's PR of the same number (#228). So does a URL (#228 review).
func TestCmdMergePR_forwardedRepoUnresolvableRefuses(t *testing.T) {
	orig := resolvePRIn
	t.Cleanup(func() { resolvePRIn = orig })
	resolvePRIn = func(pr, repo string) (ghx.PRAt, error) { return ghx.PRAt{}, os.ErrNotExist }
	repo := t.TempDir()
	gitInitForTest(t, repo)
	t.Chdir(repo)
	for _, tc := range []struct {
		args []string
		want string
	}{
		{[]string{"99999", "--", "-R", "other/repo"}, "rather than read this clone's PR #99999"},
		{[]string{"#99999", "--", "-R", "other/repo"}, "rather than read this clone's PR #99999"},
		{[]string{forwardedURL}, "refusing to merge a PR whose repository wt could not read"},
		{[]string{forwardedURL, "--", "-R", "other/repo"}, "rather than read this clone's PR " + forwardedURL + " in its place"},
	} {
		var code int
		_, errOut := captureT(t, func() { code = cmdMergePR(tc.args) })
		if code != 1 || !strings.Contains(errOut, tc.want) {
			t.Errorf("cmdMergePR(%q) = %d, want 1 with %q:\n%s", tc.args, code, tc.want, errOut)
		}
	}
}
