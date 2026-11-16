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

// quotaGh is gh during #217's incident: the account's REST quota is used up.
// `gh auth status` validates over REST, so GitHub refuses it for the quota
// (the JSON form carries that 403; the human form calls the token invalid),
// and so are every REST read and the reset probe (`gh api --include /`, its
// headers saying the quota lifts in 23 minutes). GraphQL answers: the PR, its
// checks ("rollup"), its file count (6) and its file list ("files"), and the
// state, MERGED once `pr merge` ran. Its own host, quota.example.com, keeps
// ghx's process-wide auth memo (#172) away from every other test's.
const quotaGh = `#!/bin/sh
d=$(dirname "$0")
H=quota.example.com
printf '%s\n' "$*" >> "$d/calls"
refusal='API rate limit exceeded for user ID 1. If you reach out to GitHub Support for help, please include the request ID CF90:1.'
case "$*" in
"auth status --json hosts --active"*)
	printf '{"hosts":{"%s":[{"state":"error","error":"HTTP 403: %s (https://%s/api/v3/)","active":true,"host":"%s","login":"octo","tokenSource":"keyring","gitProtocol":"https"}]}}\n' $H "$refusal" $H $H; exit 0 ;;
"auth status"*) echo "X Failed to log in to $H account octo (keyring) - The token in keyring is invalid." >&2; exit 1 ;;
"api --include /"*) printf 'HTTP/2.0 403 Forbidden\r\nX-Ratelimit-Remaining: 0\r\nX-Ratelimit-Reset: %s\r\n\r\n{}\n' $(( $(date +%s) + 1380 )); exit 1 ;;
"pr diff"*) echo "could not find pull request diff: HTTP 403: $refusal" >&2; exit 1 ;;
"api --hostname $H repos/"*) printf '{"message":"%s","status":"403"}' "$refusal"; echo "gh: $refusal (HTTP 403)" >&2; exit 1 ;;
*"--json number,url"*) echo "$3 https://$H/o/r/pull/$3"; exit 0 ;;
*statusCheckRollup*) cat "$d/rollup"; exit 0 ;;
*"files(first:100"*) cat "$d/files"; exit 0 ;;
esac
case "$1 $2" in
"pr view")
	case "$*" in
	*changedFiles*) echo 6 ;;
	*" state "*) if [ -e "$d/merged" ]; then echo MERGED; else echo OPEN; fi ;;
	*isDraft*) echo false ;;
	*messageHeadline*) echo "Fix the widget" ;;
	*headRefName*) echo feat-x ;;
	*title*) echo "Fix the widget" ;;
	*" body "*) echo "Refs #3" ;;
	*" url "*) echo "https://$H/o/r/pull/99999" ;;
	esac ;;
"api graphql")
	case "$*" in
	*squashMergeCommitTitle*) echo '{"title":"COMMIT_OR_PR_TITLE","message":"COMMIT_MESSAGES"}' ;;
	*parents*) echo '{"message":"Fix the widget","parents":1}' ;;
	esac ;;
"issue view") echo OPEN ;;
"pr merge") printf '%s\n' "$@" > "$d/argv"; : > "$d/merged" ;;
esac
exit 0
`

// TestCmdMergePR_restQuotaUsedUp is #217 end to end: `wt merge-pr` while the
// account's REST quota is used up. It used to say the checks were unread
// because gh "is not available or not authenticated", call a 6-line diff
// EMPTY and point at --bypass, gate the deploy on an unlisted file list, and
// after a merge that went through, say the PR's state could not be read. Now
// the refusal names the rate limit and when it lifts, the diff and the file
// list come from GraphQL, nothing points at --bypass, and the merge is
// confirmed.
func TestCmdMergePR_restQuotaUsedUp(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("the fake gh is a shell script")
	}
	gitBin, err := exec.LookPath("git")
	if err != nil {
		t.Skip("no git")
	}
	hermeticGitT(t)
	repo := t.TempDir()
	for _, args := range [][]string{
		{"init", "-q", "-b", "main"},
		{"-c", "user.name=t", "-c", "user.email=t@t", "commit", "-q", "--allow-empty", "-m", "base"},
		{"remote", "add", "origin", "https://quota.example.com/o/r.git"}, // never fetched: gh is the fake
	} {
		if out, err := exec.Command(gitBin, append([]string{"-C", repo}, args...)...).CombinedOutput(); err != nil {
			t.Fatalf("git %v: %v\n%s", args, err, out)
		}
	}
	ghDir := t.TempDir()
	if err := os.WriteFile(filepath.Join(ghDir, "gh"), []byte(quotaGh), 0o755); err != nil {
		t.Fatal(err)
	}
	sep := string(os.PathListSeparator)
	t.Setenv("PATH", ghDir+sep+filepath.Dir(gitBin)+sep+"/usr/bin"+sep+"/bin")
	if got, err := exec.LookPath("gh"); err != nil || got != filepath.Join(ghDir, "gh") {
		t.Fatalf("gh resolves to %q (%v), not the fake", got, err)
	}
	t.Setenv("HOME", t.TempDir())
	t.Chdir(repo)
	postMergeSleep = func(time.Duration) {}
	liveChecksReads.sleep = func(time.Duration) {}
	realTTY := stdinIsTTY
	stdinIsTTY = func() bool { return false }
	t.Cleanup(func() {
		postMergeSleep, liveChecksReads.sleep = time.Sleep, time.Sleep
		stdinIsTTY = realTTY
	})
	if err := os.WriteFile(filepath.Join(ghDir, "rollup"), []byte(greenRollup+"\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	never := []string{"not available or not authenticated", "EMPTY", "wt merge-pr 99999 --bypass", "state could not be read", "wait for CI"}
	cases := []struct {
		name   string
		args   []string
		env    map[string]string
		files  string // GraphQL's file list
		code   int
		merged bool
		out    []string // in stdout or stderr
	}{
		{name: "checks: the required ones are REST, so unread, and the refusal names the quota",
			args: []string{"99999", "--keep"}, code: 1,
			out: []string{"required: could not be read", "REST API rate limit for this account is used up (it lifts at", "in 23m)",
				"re-run once gh can read the required checks, or pass --checks-ok"}},
		{name: "--checks-ok: the diff is read through GraphQL, the merge confirmed",
			args: []string{"99999", "--keep", "--checks-ok"}, code: 0, merged: true,
			out: []string{"6 changed file(s)"}},
		{name: "a deploy-path repo: GraphQL's file list decides the gate (no deploy path)",
			args: []string{"99999", "--keep", "--checks-ok"}, code: 0, merged: true,
			env:   map[string]string{"WT_MERGE_IS_DEPLOY": "1", "WT_MERGE_IS_DEPLOY_PATHS": "infra/**"},
			files: `{"total":2,"paths":["docs/a.md","README.md"]}`,
			out:   []string{"changes no deploy-path files"}},
		{name: "a deploy-path repo: GraphQL's file list decides the gate (a deploy path)",
			args: []string{"99999", "--keep", "--checks-ok"}, code: 1,
			env:   map[string]string{"WT_MERGE_IS_DEPLOY": "1", "WT_MERGE_IS_DEPLOY_PATHS": "infra/**"},
			files: `{"total":2,"paths":["docs/a.md","infra/x.yaml"]}`,
			out:   []string{"prod deploy not confirmed"}},
		{name: "a deploy-path repo: a partial GraphQL list gates, naming both failures",
			args: []string{"99999", "--keep", "--checks-ok"}, code: 1,
			env:   map[string]string{"WT_MERGE_IS_DEPLOY": "1", "WT_MERGE_IS_DEPLOY_PATHS": "infra/**"},
			files: `{"total":285,"paths":["docs/a.md"]}`,
			out:   []string{"REST API rate limit", "GraphQL listed 1 of the PR's 285 changed files", "gating to be safe", "prod deploy not confirmed"}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			for k, v := range tc.env {
				t.Setenv(k, v)
			}
			for _, name := range []string{"argv", "calls", "merged"} {
				_ = os.Remove(filepath.Join(ghDir, name))
			}
			if err := os.WriteFile(filepath.Join(ghDir, "files"), []byte(tc.files+"\n"), 0o644); err != nil {
				t.Fatal(err)
			}
			var code int
			stdout, stderr := captureT(t, func() { code = cmdMergePR(tc.args) })
			all := stdout + stderr
			if code != tc.code {
				t.Errorf("cmdMergePR(%q) = %d, want %d\nstdout: %s\nstderr: %s", tc.args, code, tc.code, stdout, stderr)
			}
			for _, w := range tc.out {
				if !strings.Contains(all, w) {
					t.Errorf("output lacks %q\nstdout: %s\nstderr: %s", w, stdout, stderr)
				}
			}
			for _, w := range never {
				if strings.Contains(all, w) {
					t.Errorf("output says %q (the #217 misreading)\nstdout: %s\nstderr: %s", w, stdout, stderr)
				}
			}
			if _, err := os.Stat(filepath.Join(ghDir, "argv")); (err == nil) != tc.merged {
				t.Errorf("gh pr merge ran = %v, want %v", err == nil, tc.merged)
			}
			calls, _ := os.ReadFile(filepath.Join(ghDir, "calls"))
			// A read refused for the quota is not retried (it lifts in minutes),
			// and the reset is probed once per host, not per read.
			if n := strings.Count(string(calls), "repos/o/r/branches/main"); n > 1 {
				t.Errorf("the branch read ran %d times, want once (no retry)", n)
			}
			if n := strings.Count(string(calls), "api --include /"); n > 1 {
				t.Errorf("the reset was probed %d times, want at most once", n)
			}
			// #217 review: the GraphQL file list goes to the PR's host, never
			// github.com (the fake answers either, so the call is what's checked).
			if strings.Contains(string(calls), "files(first:100") && !strings.Contains(string(calls), "api graphql --hostname quota.example.com --paginate") {
				t.Errorf("the GraphQL file list was not asked of the PR's host:\n%s", calls)
			}
		})
	}
}

// TestRefuseHintsForAnUnreadRequiredRead: when the one thing blocking is that
// the required checks could not be read (#217: usually the REST quota), the
// hint says to re-run once they can be, not to wait for CI; anything else
// blocking still says wait for CI.
func TestRefuseHintsForAnUnreadRequiredRead(t *testing.T) {
	unread := []string{"branch protection: GitHub's REST API rate limit for this account is used up"}
	pending := []ghx.Check{{Name: "test", State: "QUEUED"}}
	for _, c := range []struct {
		name string
		v    merge.ChecksVerdict
		want string
	}{
		{"the checks unread", merge.ChecksVerdict{ReadErr: "x"}, "re-run once gh can read them"},
		{"only the required read failed", merge.ChecksVerdict{RequiredUnread: unread}, "re-run once gh can read the required checks"},
		{"the required read failed, and a check is pending", merge.ChecksVerdict{RequiredUnread: unread, Pending: pending}, "wait for CI (gh pr checks 7 --watch)"},
		{"a check pending", merge.ChecksVerdict{Pending: pending}, "wait for CI (gh pr checks 7 --watch)"},
	} {
		if got := refuseHints("7", c.v, checksOpts{}); len(got) == 0 || !strings.HasPrefix(got[0], c.want) {
			t.Errorf("%s: refuseHints = %q, want it to start %q", c.name, got, c.want)
		}
	}
}
