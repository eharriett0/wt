package cli

import (
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"runtime"
	"strings"
	"testing"
	"time"

	"github.com/eharriett0/wt/internal/ghx"
	"github.com/eharriett0/wt/internal/merge"
)

// fakeHead is the head commit greenChecksGh reports.
const fakeHead = "0123456789abcdef0123456789abcdef01234567"

// greenChecksGh is the part of a fake gh that answers the checks gate's reads
// (#179): one passing check run on fakeHead, an unprotected base and no
// rulesets. Put it right after the script's `d=$(dirname "$0")`.
const greenChecksGh = `case "$*" in
*statusCheckRollup*) echo '{"data":{"resource":{"headRefOid":"` + fakeHead + `","baseRefName":"main","commits":{"nodes":[{"commit":{"oid":"` + fakeHead + `","statusCheckRollup":{"contexts":{"pageInfo":{"hasNextPage":false,"endCursor":"MQ"},"nodes":[{"__typename":"CheckRun","databaseId":1,"name":"test","status":"COMPLETED","conclusion":"SUCCESS","checkSuite":{"workflowRun":{"event":"pull_request","workflow":{"name":"CI"}}}}]}}}}]}}}}'; exit 0 ;;
"api repos/{owner}/{repo}/branches/"*) echo '{"name":"main","protected":false,"protection":{"enabled":false,"required_status_checks":{"enforcement_level":"off","contexts":[],"checks":[]}}}'; exit 0 ;;
"api repos/{owner}/{repo}/rules/branches/"*) echo '[]'; exit 0 ;;
esac
`

// captureT runs f with os.Stdout and os.Stderr sent to files, and returns what
// each received.
func captureT(t *testing.T, f func()) (stdout, stderr string) {
	t.Helper()
	dir := t.TempDir()
	out, err := os.Create(filepath.Join(dir, "stdout"))
	if err != nil {
		t.Fatal(err)
	}
	errf, err := os.Create(filepath.Join(dir, "stderr"))
	if err != nil {
		t.Fatal(err)
	}
	oldOut, oldErr := os.Stdout, os.Stderr
	os.Stdout, os.Stderr = out, errf
	func() {
		defer func() { os.Stdout, os.Stderr = oldOut, oldErr }()
		f()
	}()
	out.Close()
	errf.Close()
	o, _ := os.ReadFile(out.Name())
	e, _ := os.ReadFile(errf.Name())
	return string(o), string(e)
}

func TestChecksAction(t *testing.T) {
	cases := []struct {
		status                merge.ChecksStatus
		dryRun, checksOK, tty bool
		want                  gateAction
	}{
		{merge.ChecksGreen, false, false, false, gateProceed},
		{merge.ChecksGreen, true, true, true, gateProceed},
		{merge.ChecksNone, false, false, false, gateNote},
		{merge.ChecksNone, true, false, true, gateNote},
		{merge.ChecksBlocked, false, false, false, gateRefuse},
		{merge.ChecksBlocked, false, false, true, gateAsk},
		{merge.ChecksBlocked, false, true, false, gateOverride},
		{merge.ChecksBlocked, false, true, true, gateOverride},
		{merge.ChecksBlocked, true, false, true, gatePreview}, // a dry run never prompts (#170)
		{merge.ChecksBlocked, true, true, false, gatePreview},
		{merge.ChecksStatus("something else"), false, false, false, gateRefuse},
	}
	for _, c := range cases {
		if got := checksAction(c.status, c.dryRun, c.checksOK, c.tty); got != c.want {
			t.Errorf("checksAction(%s, dryRun=%v, checksOK=%v, tty=%v) = %v, want %v", c.status, c.dryRun, c.checksOK, c.tty, got, c.want)
		}
	}
}

// gateFake stands in for the checks gate's gh reads, counting them.
type gateFake struct {
	read      ghx.PRChecksRead
	err       error // every checks read fails with it
	failFirst bool  // only the first checks read fails
	prot      []string
	protErr   error
	rules     []string
	rulesErr  error
	calls     map[string]int
	bases     []string
	slept     int
}

func (f *gateFake) reads() checksReads {
	f.calls = map[string]int{}
	return checksReads{
		checks: func(string) (ghx.PRChecksRead, error) {
			f.calls["checks"]++
			if f.err != nil || (f.failFirst && f.calls["checks"] == 1) {
				return ghx.PRChecksRead{}, errors.New("gh: Something went wrong (HTTP 502)")
			}
			return f.read, nil
		},
		protection: func(base string) ([]string, error) {
			f.calls["protection"]++
			f.bases = append(f.bases, base)
			return f.prot, f.protErr
		},
		rulesets: func(base string) ([]string, error) {
			f.calls["rulesets"]++
			f.bases = append(f.bases, base)
			return f.rules, f.rulesErr
		},
		sleep: func(time.Duration) { f.slept++ },
	}
}

func headRead(checks ...ghx.Check) ghx.PRChecksRead {
	return ghx.PRChecksRead{Head: fakeHead, Base: "trunk", Checks: checks}
}

func ck(name, state string) ghx.Check { return ghx.Check{Name: name, State: state} }

// TestChecksGate drives the gate with its reads faked: what it prints, whether
// it lets the merge go on, the commit it pins it to, and that it never reads
// stdin unless it is asking at a terminal.
func TestChecksGate(t *testing.T) {
	boom := errors.New("Server Error (HTTP 502)")
	builds := []string{"build (macos-latest)", "build (ubuntu-latest)", "build (windows-latest)"}
	cases := []struct {
		name      string
		fake      gateFake
		opts      checksOpts
		tty       bool
		answer    string // what a terminal types
		pin       string
		ok        bool
		stdout    []string
		stderr    []string
		notStderr []string
	}{
		{name: "green", fake: gateFake{read: headRead(ck("test", "SUCCESS"))},
			pin: fakeHead, ok: true, stdout: []string{"merge-pr: PR #7 checks=green on 0123456789ab: 1 passed; no required checks"}},
		{name: "green, the required checks reported", fake: gateFake{read: headRead(ck("test", "SUCCESS"), ck("lint", "SKIPPED")),
			prot: []string{"test"}, rules: []string{"lint"}},
			pin: fakeHead, ok: true, stdout: []string{"checks=green on 0123456789ab: 1 passed, 1 skipped; required: 2, all reported"}},
		{name: "no checks, nothing requires one: merges, and says so", fake: gateFake{read: headRead()},
			pin: fakeHead, ok: true, stdout: []string{"checks=none on 0123456789ab: no checks reported; no required checks"},
			stderr: []string{"no check ran on PR #7 (no checks reported): nothing in CI verified this merge", "set merge_min_checks"}},
		{name: "pending: refused", fake: gateFake{read: headRead(ck("test", "SUCCESS"), ck("lint", "QUEUED"), ck("e2e", "IN_PROGRESS"))},
			stdout: []string{"checks=blocked", "wait for CI (gh pr checks 7 --watch) and re-run, or pass --checks-ok"},
			stderr: []string{"pending: lint [QUEUED], e2e [IN_PROGRESS]", "refusing to merge PR #7 — 2 checks pending."}},
		{name: "failed under --admin: refused, and says GitHub would not stop it", fake: gateFake{read: headRead(ck("build", "FAILURE"))},
			opts:   checksOpts{admin: true},
			stdout: []string{"--admin bypasses GitHub's required checks too"},
			stderr: []string{"failed: build [FAILURE]", "refusing to merge PR #7 — 1 check failed."}},
		{name: "cancelled is not green", fake: gateFake{read: headRead(ck("build", "CANCELLED"))},
			stderr: []string{"failed: build [CANCELLED]"}},
		{name: "cli/cli#14629: required builds never reported", fake: gateFake{read: headRead(ck("label-external / label_issues", "SUCCESS"),
			ck("ready-for-review", "SKIPPED")), prot: builds},
			stdout: []string{"required: 3, 3 never reported"},
			stderr: []string{"required, but never reported on 0123456789ab: build (macos-latest), build (ubuntu-latest), build (windows-latest) (CI may not have started)",
				"refusing to merge PR #7 — 3 required checks never reported on this head."}},
		{name: "unreadable: retried once, then refused with the bypass", fake: gateFake{err: boom},
			stdout: []string{"merge-pr: PR #7 checks=unread: gh: Something went wrong (HTTP 502)", "re-run once gh can read them, or pass --checks-ok"},
			stderr: []string{"refusing to merge PR #7 — its checks could not be read."}},
		{name: "a failed read that the retry reads", fake: gateFake{failFirst: true, read: headRead(ck("test", "SUCCESS"))},
			pin: fakeHead, ok: true, stdout: []string{"checks=green"}},
		{name: "--checks-ok past pending: merges, loudly", fake: gateFake{read: headRead(ck("lint", "QUEUED"))},
			opts: checksOpts{checksOK: true}, pin: fakeHead, ok: true,
			stderr: []string{"--checks-ok set — merging PR #7 anyway: 1 check pending."}},
		{name: "--checks-ok with --admin", fake: gateFake{read: headRead(ck("lint", "FAILURE"))},
			opts: checksOpts{checksOK: true, admin: true}, pin: fakeHead, ok: true,
			stderr: []string{"--checks-ok set — merging PR #7 anyway: 1 check failed. With --admin, GitHub will not stop it either."}},
		{name: "--checks-ok when unreadable: merges unpinned", fake: gateFake{err: boom},
			opts: checksOpts{checksOK: true}, pin: "", ok: true,
			stderr: []string{"--checks-ok set — merging PR #7 anyway: its checks could not be read."}},
		{name: "--dry-run previews a refusal and never asks", fake: gateFake{read: headRead(ck("lint", "QUEUED"))},
			opts: checksOpts{dryRun: true}, tty: true, answer: "merge\n", pin: fakeHead, ok: true,
			stdout:    []string{"checks=blocked", "wait for CI (gh pr checks 7 --watch) and re-run, or pass --checks-ok"},
			stderr:    []string{"--dry-run: a real merge would REFUSE here (or ask at a terminal): 1 check pending."},
			notStderr: []string{"Type"}},
		{name: "--dry-run with --checks-ok", fake: gateFake{read: headRead(ck("lint", "QUEUED"))},
			opts: checksOpts{dryRun: true, checksOK: true}, pin: fakeHead, ok: true,
			stderr: []string{"--dry-run: 1 check pending; --checks-ok is set, so a real merge would merge anyway."}},
		{name: "a terminal that types merge", fake: gateFake{read: headRead(ck("lint", "QUEUED"))},
			tty: true, answer: "merge\n", pin: fakeHead, ok: true, stderr: []string{"PR #7: 1 check pending. Type merge to merge anyway"}},
		{name: "a terminal that types anything else", fake: gateFake{read: headRead(ck("lint", "QUEUED"))},
			tty: true, answer: "y\n", stderr: []string{"aborted — PR #7 not merged."}},
		{name: "a terminal that closes stdin", fake: gateFake{read: headRead(ck("lint", "QUEUED"))},
			tty: true, answer: "", stderr: []string{"aborted"}},
		{name: "--bypass does not cover it", fake: gateFake{read: headRead(ck("lint", "QUEUED"))},
			opts: checksOpts{bypass: true}, stdout: []string{"--bypass does not cover the checks gate; --checks-ok does"}},
		{name: "branch protection unreadable: retried, then refused", fake: gateFake{read: headRead(ck("test", "SUCCESS")), protErr: boom},
			stdout: []string{"required: could not be read"},
			stderr: []string{"could not read the required checks from branch protection: Server Error (HTTP 502)",
				"refusing to merge PR #7 — the required checks could not be read."}},
		{name: "no rulesets for the repo is an answer", fake: gateFake{read: headRead(ck("test", "SUCCESS")), rulesErr: ghx.ErrRulesetsUnavailable},
			pin: fakeHead, ok: true, stdout: []string{"checks=green"}},
		{name: "below merge_min_checks", fake: gateFake{read: headRead(ck("test", "SUCCESS"), ck("lint", "SKIPPED"))},
			opts:   checksOpts{minChecks: 3},
			stderr: []string{"only 1 check(s) ran, fewer than merge_min_checks = 3 (CI may not have started)"}},
		{name: "merge_min_checks that is not a count", fake: gateFake{read: headRead(ck("test", "SUCCESS"))},
			opts:   checksOpts{minChecksBad: "five"},
			stderr: []string{`merge_min_checks = "five" is not a count`}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			f := tc.fake
			var pin string
			var ok bool
			stdin := strings.NewReader(tc.answer)
			stdout, stderr := captureT(t, func() {
				pin, ok = checksGate("7", tc.opts, stdin, tc.tty, f.reads())
			})
			if pin != tc.pin || ok != tc.ok {
				t.Errorf("checksGate = (%q, %v), want (%q, %v)", pin, ok, tc.pin, tc.ok)
			}
			for _, w := range tc.stdout {
				if !strings.Contains(stdout, w) {
					t.Errorf("stdout = %q\nwant it to say %q", stdout, w)
				}
			}
			for _, w := range tc.stderr {
				if !strings.Contains(stderr, w) {
					t.Errorf("stderr = %q\nwant it to say %q", stderr, w)
				}
			}
			for _, w := range tc.notStderr {
				if strings.Contains(stderr, w) {
					t.Errorf("stderr = %q\nwant it NOT to say %q", stderr, w)
				}
			}
			if !tc.tty || tc.opts.dryRun {
				if stdin.Len() != len(tc.answer) {
					t.Errorf("the gate read stdin when it was not asking")
				}
			}
		})
	}
}

// TestChecksGateReads pins the reads: one retry per failed read (never for
// rulesets a server does not have), and the required checks are read from the
// PR's base branch, not the repo's default.
func TestChecksGateReads(t *testing.T) {
	boom := errors.New("Server Error (HTTP 502)")
	cases := []struct {
		name  string
		fake  gateFake
		want  map[string]int
		slept int // once per retry
	}{
		{"all read", gateFake{read: headRead()}, map[string]int{"checks": 1, "protection": 1, "rulesets": 1}, 0},
		{"the checks never read: nothing else is", gateFake{err: boom}, map[string]int{"checks": 2}, 1},
		{"each failed read retried once", gateFake{read: headRead(), protErr: boom, rulesErr: boom},
			map[string]int{"checks": 1, "protection": 2, "rulesets": 2}, 2},
		{"no rulesets: not retried", gateFake{read: headRead(), rulesErr: ghx.ErrRulesetsUnavailable},
			map[string]int{"checks": 1, "protection": 1, "rulesets": 1}, 0},
	}
	for _, tc := range cases {
		f := tc.fake
		captureT(t, func() { checksGate("7", checksOpts{}, strings.NewReader(""), false, f.reads()) })
		if !reflect.DeepEqual(f.calls, tc.want) || f.slept != tc.slept {
			t.Errorf("%s: reads = %v, slept %d; want %v, slept %d", tc.name, f.calls, f.slept, tc.want, tc.slept)
		}
		for _, b := range f.bases {
			if b != "trunk" {
				t.Errorf("%s: required checks read from %q, want the PR's base trunk", tc.name, b)
			}
		}
	}
}

// checksGh is a fake gh for merge-pr's checks gate (#179), end to end: open PR
// 99999 that closes nothing, whose checks answer is the file "rollup" (gh fails
// when "rollup.fail" exists), whose base branch answer is "branch" and whose
// rulesets answer is "rules" (gh exits 1 after printing it when "rules.fail"
// exists). Every `pr merge` argv is recorded, one per line, in "argv".
const checksGh = `#!/bin/sh
d=$(dirname "$0")
case "$*" in
*statusCheckRollup*) [ -e "$d/rollup.fail" ] && { echo "gh: Something went wrong (HTTP 502)" >&2; exit 1; }; cat "$d/rollup"; exit 0 ;;
"api repos/{owner}/{repo}/branches/"*) cat "$d/branch"; exit 0 ;;
"api repos/{owner}/{repo}/rules/branches/"*) cat "$d/rules"; [ -e "$d/rules.fail" ] && exit 1; exit 0 ;;
esac
case "$1 $2" in
"pr view")
	case "$*" in
	*" state "*) echo OPEN ;;
	*isDraft*) echo false ;;
	*messageHeadline*) echo "Fix the widget" ;;
	*headRefName*) echo feat-x ;;
	*title*) echo "Fix the widget" ;;
	*" body "*) echo "Refs #3" ;;
	*" url "*) echo "https://github.com/o/r/pull/99999" ;;
	esac ;;
"pr diff") echo a.txt ;;
"api graphql")
	case "$*" in
	*squashMergeCommitTitle*) echo '{"title":"COMMIT_OR_PR_TITLE","message":"COMMIT_MESSAGES"}' ;;
	*parents*) echo '{"message":"Fix the widget","parents":1}' ;;
	esac ;;
"issue view") echo OPEN ;;
"pr merge") printf '%s\n' "$@" > "$d/argv" ;;
esac
exit 0
`

// rollup is a one-page checks answer for fakeHead holding nodes.
func rollup(nodes ...string) string {
	if len(nodes) == 0 {
		return `{"data":{"resource":{"headRefOid":"` + fakeHead + `","baseRefName":"main","commits":{"nodes":[{"commit":{"oid":"` + fakeHead + `","statusCheckRollup":null}}]}}}}`
	}
	return `{"data":{"resource":{"headRefOid":"` + fakeHead + `","baseRefName":"main","commits":{"nodes":[{"commit":{"oid":"` + fakeHead +
		`","statusCheckRollup":{"contexts":{"pageInfo":{"hasNextPage":false,"endCursor":"MQ"},"nodes":[` + strings.Join(nodes, ",") + `]}}}}]}}}}`
}

func runNode(name, status, conclusion string) string {
	c := "null"
	if conclusion != "" {
		c = `"` + conclusion + `"`
	}
	return `{"__typename":"CheckRun","databaseId":1,"name":"` + name + `","status":"` + status + `","conclusion":` + c +
		`,"checkSuite":{"workflowRun":{"event":"pull_request","workflow":{"name":"CI"}}}}`
}

const (
	unprotected  = `{"name":"main","protected":false,"protection":{"enabled":false,"required_status_checks":{"enforcement_level":"off","contexts":[],"checks":[]}}}`
	requiresLint = `{"name":"main","protected":true,"protection":{"enabled":true,"required_status_checks":{"enforcement_level":"non_admins","contexts":["lint"],"checks":[{"context":"lint","app_id":null}]}}}`
	planNoRules  = `{"message":"Upgrade to GitHub Pro or make this repository public to enable this feature.","documentation_url":"https://docs.github.com/rest/repos/rules#get-rules-for-a-branch","status":"403"}`
)

// TestCmdMergePR_checksGate drives the whole merge-pr command over the checks
// gate (#179) in a scratch repo against checksGh: what it refuses, what it
// merges, and the commit it pins the merge to. Same safety as the other
// fake-gh tests: the fake is first and alone on PATH (with git's dir), the
// test stops unless "gh" resolves to it, and the repo has no GitHub remote.
func TestCmdMergePR_checksGate(t *testing.T) {
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
	} {
		if out, err := exec.Command(gitBin, append([]string{"-C", repo}, args...)...).CombinedOutput(); err != nil {
			t.Fatalf("git %v: %v\n%s", args, err, out)
		}
	}
	ghDir := t.TempDir()
	if err := os.WriteFile(filepath.Join(ghDir, "gh"), []byte(checksGh), 0o755); err != nil {
		t.Fatal(err)
	}
	sep := string(os.PathListSeparator)
	t.Setenv("PATH", ghDir+sep+filepath.Dir(gitBin)+sep+"/usr/bin"+sep+"/bin")
	if got, err := exec.LookPath("gh"); err != nil || got != filepath.Join(ghDir, "gh") {
		t.Fatalf("gh resolves to %q (%v), not the fake", got, err)
	}
	t.Setenv("HOME", t.TempDir())
	t.Chdir(repo)
	postMergeSleep = func(time.Duration) {} // the PR stays OPEN: no merge to wait for
	liveChecksReads.sleep = func(time.Duration) {}
	t.Cleanup(func() { postMergeSleep, liveChecksReads.sleep = time.Sleep, time.Sleep })

	green := rollup(runNode("test", "COMPLETED", "SUCCESS"), runNode("lint", "COMPLETED", "SKIPPED"))
	pending := rollup(runNode("test", "COMPLETED", "SUCCESS"), runNode("lint", "QUEUED", ""))
	pinned := []string{"--match-head-commit", fakeHead}
	cases := []struct {
		name     string
		args     []string
		env      map[string]string
		rollup   string // "" = gh fails to read the checks
		branch   string
		rules    string
		rulesErr bool
		code     int
		merged   []string // gh's argv after `pr merge 99999 --squash`; nil = gh never merged
		stdout   []string
		stderr   []string
		notOut   []string // in neither stream
	}{
		{name: "pending: refused", args: []string{"99999", "--keep"}, rollup: pending, code: 1,
			stdout: []string{"checks=blocked"}, stderr: []string{"pending: lint [QUEUED]", "refusing to merge PR #99999 — 1 check pending."}},
		{name: "failed, with --admin: refused", args: []string{"99999", "--keep", "--admin"},
			rollup: rollup(runNode("test", "COMPLETED", "FAILURE")), code: 1,
			stdout: []string{"--admin bypasses GitHub's required checks too"}, stderr: []string{"failed: test [FAILURE]"}},
		{name: "a required check never reported: refused", args: []string{"99999", "--keep"},
			rollup: rollup(runNode("test", "COMPLETED", "SUCCESS")), branch: requiresLint, code: 1,
			stderr: []string{"required, but never reported on 0123456789ab: lint"}},
		{name: "green: merges, pinned to the head it read", args: []string{"99999", "--keep"}, rollup: green, code: 0,
			merged: pinned, stdout: []string{"checks=green on 0123456789ab: 1 passed, 1 skipped; no required checks"}},
		{name: "green with --admin: the pin and --admin in front", args: []string{"99999", "--keep", "--admin", "--", "-d"},
			rollup: green, code: 0, merged: append(append([]string{}, pinned...), "--admin", "-d")},
		{name: "unreadable: refused with the bypass", args: []string{"99999", "--keep"}, rollup: "", code: 1,
			stdout: []string{"checks=unread", "pass --checks-ok to merge without them"}},
		{name: "--checks-ok past pending: merges, loudly", args: []string{"99999", "--keep", "--checks-ok"}, rollup: pending, code: 0,
			merged: pinned, stderr: []string{"--checks-ok set — merging PR #99999 anyway: 1 check pending."}},
		{name: "--checks-ok when unreadable: merges unpinned", args: []string{"99999", "--keep", "--checks-ok"}, rollup: "", code: 0,
			merged: []string{}, stderr: []string{"--checks-ok set — merging PR #99999 anyway: its checks could not be read."}},
		{name: "--bypass is not --checks-ok", args: []string{"99999", "--keep", "--bypass"}, rollup: pending, code: 1,
			stdout: []string{"--bypass does not cover the checks gate; --checks-ok does"}},
		{name: "--dry-run prints the summary and what a real merge would do", args: []string{"99999", "--dry-run"}, rollup: pending, code: 0,
			stdout: []string{"checks=blocked on 0123456789ab: 1 passed, 1 pending", "verdict=ok"},
			stderr: []string{"--dry-run: a real merge would REFUSE here"}},
		{name: "a clean --dry-run read the checks, green", args: []string{"99999", "--dry-run"}, rollup: green, code: 0,
			stdout: []string{"checks=green on 0123456789ab", "verdict=ok"}, notOut: []string{"REFUSE"}},
		{name: "no checks, nothing required: merges, and says so", args: []string{"99999", "--keep"}, rollup: rollup(), code: 0,
			merged: pinned, stderr: []string{"no check ran on PR #99999 (no checks reported)"}},
		{name: "no checks under merge_min_checks: refused", args: []string{"99999", "--keep"}, rollup: rollup(), code: 1,
			env:    map[string]string{"WT_MERGE_MIN_CHECKS": "1"},
			stderr: []string{"only 0 check(s) ran, fewer than merge_min_checks = 1"}},
		{name: "merge_min_checks that is not a count: refused", args: []string{"99999", "--keep"}, rollup: green, code: 1,
			env:    map[string]string{"WT_MERGE_MIN_CHECKS": "five"},
			stderr: []string{`merge_min_checks = "five" is not a count`}},
		{name: "rulesets unavailable on this plan: an answer, merges", args: []string{"99999", "--keep"}, rollup: green,
			rules: planNoRules, rulesErr: true, code: 0, merged: pinned},
		// the checks gate runs before the deploy confirm: a pending merge never asks for "deploy"
		{name: "merge_is_deploy: refused at the checks gate first", args: []string{"99999", "--keep"}, rollup: pending, code: 1,
			env:    map[string]string{"WT_MERGE_IS_DEPLOY": "1"},
			stderr: []string{"refusing to merge PR #99999 — 1 check pending."}, notOut: []string{"AUTO-APPLIES", "prod deploy not confirmed"}},
		{name: "merge_is_deploy, green: the deploy gate still decides", args: []string{"99999", "--keep"}, rollup: green, code: 1,
			env:    map[string]string{"WT_MERGE_IS_DEPLOY": "1"},
			stdout: []string{"checks=green"}, stderr: []string{"prod deploy not confirmed"}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			for k, v := range tc.env {
				t.Setenv(k, v)
			}
			if tc.branch == "" {
				tc.branch = unprotected
			}
			if tc.rules == "" {
				tc.rules = "[]"
			}
			files := map[string]string{"rollup": tc.rollup, "branch": tc.branch, "rules": tc.rules}
			for name, text := range files {
				if err := os.WriteFile(filepath.Join(ghDir, name), []byte(text+"\n"), 0o644); err != nil {
					t.Fatal(err)
				}
			}
			for name, on := range map[string]bool{"rollup.fail": tc.rollup == "", "rules.fail": tc.rulesErr, "argv": false} {
				_ = os.Remove(filepath.Join(ghDir, name))
				if on {
					if err := os.WriteFile(filepath.Join(ghDir, name), nil, 0o644); err != nil {
						t.Fatal(err)
					}
				}
			}
			var code int
			stdout, stderr := captureT(t, func() { code = cmdMergePR(tc.args) })
			if code != tc.code {
				t.Errorf("cmdMergePR(%q) = %d, want %d\nstdout: %s\nstderr: %s", tc.args, code, tc.code, stdout, stderr)
			}
			for _, w := range tc.stdout {
				if !strings.Contains(stdout, w) {
					t.Errorf("stdout = %q\nwant it to say %q", stdout, w)
				}
			}
			for _, w := range tc.stderr {
				if !strings.Contains(stderr, w) {
					t.Errorf("stderr = %q\nwant it to say %q", stderr, w)
				}
			}
			for _, w := range tc.notOut {
				if strings.Contains(stdout+stderr, w) {
					t.Errorf("output says %q, want it not to\nstdout: %s\nstderr: %s", w, stdout, stderr)
				}
			}
			argv, err := os.ReadFile(filepath.Join(ghDir, "argv"))
			switch {
			case tc.merged == nil && err == nil:
				t.Errorf("gh pr merge ran (%q), want no merge", argv)
			case tc.merged != nil && err != nil:
				t.Errorf("gh pr merge never ran: %v", err)
			case tc.merged != nil:
				want := append([]string{"pr", "merge", "99999", "--squash"}, tc.merged...)
				if got := strings.Split(strings.TrimSuffix(string(argv), "\n"), "\n"); !reflect.DeepEqual(got, want) {
					t.Errorf("gh argv = %q, want %q", got, want)
				}
			}
		})
	}
}
