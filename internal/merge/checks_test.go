package merge

import (
	"errors"
	"fmt"
	"reflect"
	"testing"

	"github.com/eharriett0/wt/internal/ghx"
)

// TestBucketOf covers every state GitHub reports for a check, as
// ghx.Check.State carries it: the commit status states (StatusState), a run's
// status until it completes (CheckStatusState) and its conclusion after
// (CheckConclusionState). Only SUCCESS passes; SKIPPED and NEUTRAL satisfy a
// required check; every other finished state, CANCELLED and STALE included,
// is failed; and a state wt does not know is never green.
func TestBucketOf(t *testing.T) {
	cases := map[string]CheckBucket{
		// StatusState
		"SUCCESS": BucketPassed, "PENDING": BucketPending, "EXPECTED": BucketPending,
		"FAILURE": BucketFailed, "ERROR": BucketFailed,
		// CheckStatusState, before COMPLETED
		"QUEUED": BucketPending, "IN_PROGRESS": BucketPending, "WAITING": BucketPending, "REQUESTED": BucketPending,
		// CheckConclusionState
		"NEUTRAL": BucketSkipped, "SKIPPED": BucketSkipped,
		"CANCELLED": BucketFailed, "TIMED_OUT": BucketFailed, "ACTION_REQUIRED": BucketFailed,
		"STARTUP_FAILURE": BucketFailed, "STALE": BucketFailed,
		// read loosely, as GitHub's own casing may vary
		"success": BucketPassed, " in_progress ": BucketPending,
		// never green
		"":              BucketUnknown, // a COMPLETED run with no conclusion
		"COMPLETED":     BucketUnknown, // a status without its conclusion: cannot happen via RunState, and is not a pass
		"SOMETHING_NEW": BucketUnknown,
	}
	for state, want := range cases {
		if got := BucketOf(state); got != want {
			t.Errorf("BucketOf(%q) = %s, want %s", state, got, want)
		}
	}
}

func chk(name, state string) ghx.Check { return ghx.Check{Name: name, State: state} }

// req is checks any app may report.
func req(names ...string) []ghx.RequiredCheck {
	var out []ghx.RequiredCheck
	for _, n := range names {
		out = append(out, ghx.RequiredCheck{Name: n})
	}
	return out
}

// TestDecideChecks is the gate's decision table (#179).
func TestDecideChecks(t *testing.T) {
	// cli/cli#14629, read with the ghx fixture of that name: only the triage
	// workflow ran, and trunk's protection requires three builds. GitHub's own
	// rollup state for it was SUCCESS.
	triageOnly := []ghx.Check{
		chk("check-requirements / check-requirements", "SUCCESS"),
		chk("check-requirements / close-unmet-requirements", "SKIPPED"),
		chk("close-from-default-branch / close-from-default-branch", "SKIPPED"),
		chk("close-no-help-wanted", "SKIPPED"),
		chk("close-unmet-requirements", "SKIPPED"),
		chk("label-external / label_issues", "SUCCESS"),
		chk("ready-for-review", "SKIPPED"),
	}
	builds := []string{"build (macos-latest)", "build (ubuntu-latest)", "build (windows-latest)"}

	type want struct {
		status  ChecksStatus
		pending int
		failed  int
		unknown int
		missing []string
		ran     int
		floor   int
	}
	cases := []struct {
		name string
		in   ChecksInput
		want want
	}{
		{"unreadable", ChecksInput{ReadErr: "gh: HTTP 502", Checks: []ghx.Check{chk("a", "SUCCESS")}}, want{status: ChecksBlocked}},
		{"no checks, nothing required, no floor: merge, and say so", ChecksInput{}, want{status: ChecksNone}},
		{"no checks, a required one: never started", ChecksInput{Required: req("test")},
			want{status: ChecksBlocked, missing: []string{"test"}}},
		{"no checks, a floor", ChecksInput{MinChecks: 1}, want{status: ChecksBlocked, floor: 1}},
		{"all passed", ChecksInput{Checks: []ghx.Check{chk("a", "SUCCESS"), chk("b", "SUCCESS")}}, want{status: ChecksGreen, ran: 2}},
		{"passed and skipped", ChecksInput{Checks: []ghx.Check{chk("a", "SUCCESS"), chk("b", "SKIPPED"), chk("c", "SKIPPED")}},
			want{status: ChecksGreen, ran: 1}},
		{"every check skipped: none ran", ChecksInput{Checks: []ghx.Check{chk("a", "SKIPPED"), chk("b", "SKIPPED")}}, want{status: ChecksNone}},
		{"NEUTRAL ran", ChecksInput{Checks: []ghx.Check{chk("CodeQL", "NEUTRAL")}}, want{status: ChecksGreen, ran: 1}},
		{"one queued among passes", ChecksInput{Checks: []ghx.Check{chk("a", "SUCCESS"), chk("b", "QUEUED")}},
			want{status: ChecksBlocked, pending: 1, ran: 2}},
		{"a status pending", ChecksInput{Checks: []ghx.Check{chk("tide", "PENDING")}}, want{status: ChecksBlocked, pending: 1, ran: 1}},
		{"a status expected", ChecksInput{Checks: []ghx.Check{chk("ci", "EXPECTED")}}, want{status: ChecksBlocked, pending: 1, ran: 1}},
		{"waiting and requested", ChecksInput{Checks: []ghx.Check{chk("a", "WAITING"), chk("b", "REQUESTED"), chk("c", "IN_PROGRESS")}},
			want{status: ChecksBlocked, pending: 3, ran: 3}},
		{"failed", ChecksInput{Checks: []ghx.Check{chk("a", "SUCCESS"), chk("b", "FAILURE")}}, want{status: ChecksBlocked, failed: 1, ran: 2}},
		{"cancelled, timed out, action required, startup failure, stale, error",
			ChecksInput{Checks: []ghx.Check{chk("a", "CANCELLED"), chk("b", "TIMED_OUT"), chk("c", "ACTION_REQUIRED"),
				chk("d", "STARTUP_FAILURE"), chk("e", "STALE"), chk("f", "ERROR")}},
			want{status: ChecksBlocked, failed: 6, ran: 6}},
		{"a run that finished with no conclusion", ChecksInput{Checks: []ghx.Check{chk("a", "")}},
			want{status: ChecksBlocked, unknown: 1, ran: 1}},
		{"a state wt does not know", ChecksInput{Checks: []ghx.Check{chk("a", "SOMETHING_NEW")}},
			want{status: ChecksBlocked, unknown: 1, ran: 1}},
		{"required and passed", ChecksInput{Checks: []ghx.Check{chk("test", "SUCCESS")}, Required: req("test")},
			want{status: ChecksGreen, ran: 1}},
		{"required and skipped: GitHub counts it satisfied", ChecksInput{Checks: []ghx.Check{chk("test", "SKIPPED"), chk("lint", "SUCCESS")},
			Required: req("test")}, want{status: ChecksGreen, ran: 1}},
		{"required and pending: pending, not missing", ChecksInput{Checks: []ghx.Check{chk("test", "IN_PROGRESS")}, Required: req("test")},
			want{status: ChecksBlocked, pending: 1, ran: 1}},
		{"required names match exactly", ChecksInput{Checks: []ghx.Check{chk("Test", "SUCCESS")}, Required: req("test")},
			want{status: ChecksBlocked, missing: []string{"test"}, ran: 1}},
		{"cli/cli#14629: the triage ran, the required builds never started", ChecksInput{Checks: triageOnly, Required: req(builds...)},
			want{status: ChecksBlocked, missing: builds, ran: 2}},
		{"the floor met exactly", ChecksInput{Checks: []ghx.Check{chk("a", "SUCCESS"), chk("b", "NEUTRAL")}, MinChecks: 2},
			want{status: ChecksGreen, ran: 2}},
		{"one short of the floor", ChecksInput{Checks: []ghx.Check{chk("a", "SUCCESS")}, MinChecks: 2},
			want{status: ChecksBlocked, ran: 1, floor: 2}},
		{"skipped checks do not meet the floor", ChecksInput{Checks: []ghx.Check{chk("a", "SUCCESS"), chk("b", "SKIPPED"), chk("c", "SKIPPED")}, MinChecks: 2},
			want{status: ChecksBlocked, ran: 1, floor: 2}},
		{"running checks count toward the floor", ChecksInput{Checks: []ghx.Check{chk("a", "QUEUED"), chk("b", "SUCCESS")}, MinChecks: 2},
			want{status: ChecksBlocked, pending: 1, ran: 2}},
		{"merge_min_checks that is not a count", ChecksInput{Checks: []ghx.Check{chk("a", "SUCCESS")}, MinChecksBad: "five"},
			want{status: ChecksBlocked, ran: 1}},
		{"the required checks could not be read", ChecksInput{Checks: []ghx.Check{chk("a", "SUCCESS")}, RequiredUnread: []string{"rulesets: HTTP 502"}},
			want{status: ChecksBlocked, ran: 1}},
		{"no checks, and the required ones could not be read", ChecksInput{RequiredUnread: []string{"branch protection: HTTP 502"}},
			want{status: ChecksBlocked}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			v := DecideChecks(tc.in)
			got := want{v.Status, len(v.Pending), len(v.Failed), len(v.Unknown), v.Missing, v.Ran, v.Floor}
			if !reflect.DeepEqual(got, tc.want) {
				t.Errorf("DecideChecks = %+v, want %+v", got, tc.want)
			}
			if (v.Status == ChecksBlocked) != (len(v.Reasons()) > 0) {
				t.Errorf("status %s with reasons %q: a blocked verdict must say why, and only a blocked one", v.Status, v.Reasons())
			}
		})
	}
}

func TestChecksVerdictText(t *testing.T) {
	v := DecideChecks(ChecksInput{
		Checks: []ghx.Check{chk("a", "SUCCESS"), chk("b", "SUCCESS"), chk("c", "QUEUED"), chk("d", "FAILURE"),
			chk("e", "SKIPPED"), chk("f", "WEIRD")},
		Required:       req("a", "x", "y"),
		MinChecks:      9,
		RequiredUnread: []string{"rulesets: HTTP 502"},
	})
	if got, want := v.CountsText(), "2 passed, 1 pending, 1 failed, 1 unknown, 1 skipped"; got != want {
		t.Errorf("CountsText = %q, want %q", got, want)
	}
	want := []string{"1 check failed", "1 check pending", "1 check in a state wt does not know",
		"2 required checks never reported on this head", "fewer checks ran than merge_min_checks",
		"the required checks could not be read"}
	if got := v.Reasons(); !reflect.DeepEqual(got, want) {
		t.Errorf("Reasons = %q, want %q", got, want)
	}
	if got := DecideChecks(ChecksInput{}).CountsText(); got != "no checks reported" {
		t.Errorf("CountsText with no checks = %q", got)
	}
	if got := DecideChecks(ChecksInput{ReadErr: "x"}).Reasons(); !reflect.DeepEqual(got, []string{"its checks could not be read"}) {
		t.Errorf("Reasons when unread = %q", got)
	}
	if got := DecideChecks(ChecksInput{MinChecksBad: "-1"}).Reasons(); !reflect.DeepEqual(got, []string{"merge_min_checks is not a count"}) {
		t.Errorf("Reasons with a bad floor = %q", got)
	}
}

// TestRequiredChecks: the union of what branch protection and the rulesets
// require (a check pinned to an app stays apart from the same name for any app;
// required workflows come from the rulesets), and which of them could not be
// read. A server with no rulesets for the repo is an answer (none), not a
// failed read.
func TestRequiredChecks(t *testing.T) {
	boom := errors.New("Server Error (HTTP 502)")
	wf := ghx.RequiredWorkflow{Path: ".github/workflows/scan.yml", RepoID: 9}
	cases := []struct {
		name       string
		prot       ghx.Required
		protErr    error
		rules      ghx.Required
		rulesErr   error
		want       ghx.Required
		wantUnread []string
	}{
		{"both read", ghx.Required{Checks: req("b", "a")}, nil, ghx.Required{Checks: append(req("c", "a"), ghx.RequiredCheck{Name: "a", App: 7}), Workflows: []ghx.RequiredWorkflow{wf}}, nil,
			ghx.Required{Checks: append(req("a"), ghx.RequiredCheck{Name: "a", App: 7}, ghx.RequiredCheck{Name: "b"}, ghx.RequiredCheck{Name: "c"}), Workflows: []ghx.RequiredWorkflow{wf}}, nil},
		{"neither requires anything", ghx.Required{}, nil, ghx.Required{}, nil, ghx.Required{}, nil},
		{"no rulesets on this server or plan", ghx.Required{Checks: req("a")}, nil, ghx.Required{}, ghx.ErrRulesetsUnavailable, ghx.Required{Checks: req("a")}, nil},
		{"no rulesets, wrapped", ghx.Required{}, nil, ghx.Required{}, fmt.Errorf("rules: %w", ghx.ErrRulesetsUnavailable), ghx.Required{}, nil},
		{"the rulesets could not be read", ghx.Required{Checks: req("a")}, nil, ghx.Required{}, boom, ghx.Required{Checks: req("a")}, []string{"rulesets: " + boom.Error()}},
		{"the protection could not be read", ghx.Required{}, boom, ghx.Required{Checks: req("c")}, nil, ghx.Required{Checks: req("c")}, []string{"branch protection: " + boom.Error()}},
		{"neither could be read", ghx.Required{}, boom, ghx.Required{}, boom, ghx.Required{},
			[]string{"branch protection: " + boom.Error(), "rulesets: " + boom.Error()}},
	}
	for _, tc := range cases {
		got, unread := RequiredChecks(tc.prot, tc.protErr, tc.rules, tc.rulesErr)
		if !reflect.DeepEqual(got, tc.want) || !reflect.DeepEqual(unread, tc.wantUnread) {
			t.Errorf("%s: RequiredChecks = %+v, %q; want %+v, %q", tc.name, got, unread, tc.want, tc.wantUnread)
		}
	}
}

// TestDecideChecks_requiredApps: a required check pinned to an app is reported
// only by a check GitHub marks required for this PR, or a run of that app
// (#179 review: matching names alone let another app's check stand in).
func TestDecideChecks_requiredApps(t *testing.T) {
	run := func(name string, app int64, required bool) ghx.Check {
		return ghx.Check{Name: name, State: "SUCCESS", App: app, Required: required}
	}
	status := func(name string, required bool) ghx.Check {
		return ghx.Check{Name: name, State: "SUCCESS", Status: true, Required: required}
	}
	pinned := []ghx.RequiredCheck{{Name: "build", App: 15368}}
	cases := []struct {
		name     string
		checks   []ghx.Check
		required []ghx.RequiredCheck
		status   ChecksStatus
		otherApp []string
		count    int // Required
	}{
		{"pinned, reported by that app", []ghx.Check{run("build", 15368, false)}, pinned, ChecksGreen, nil, 1},
		{"pinned, GitHub marks the check required", []ghx.Check{run("build", 99, true)}, pinned, ChecksGreen, nil, 1},
		{"pinned, only another app reported it", []ghx.Check{run("build", 99, false)}, pinned, ChecksBlocked, []string{"build"}, 1},
		{"pinned, a status of that name GitHub does not mark required", []ghx.Check{status("build", false)}, pinned, ChecksBlocked, []string{"build"}, 1},
		{"pinned status (home-assistant's cla-bot): GitHub marks it required", []ghx.Check{status("cla-bot", true)},
			[]ghx.RequiredCheck{{Name: "cla-bot", App: 97978}}, ChecksGreen, nil, 1},
		{"any app: any check of the name", []ghx.Check{run("build", 99, false)}, req("build"), ChecksGreen, nil, 1},
		{"the name pinned AND for any app (two rules): both must hold", []ghx.Check{run("build", 99, false)},
			append(req("build"), pinned...), ChecksBlocked, []string{"build"}, 1},
		{"GitHub requires a check the rules endpoints did not name: counted", []ghx.Check{run("lint", 1, true)}, nil, ChecksGreen, nil, 1},
	}
	for _, tc := range cases {
		v := DecideChecks(ChecksInput{Checks: tc.checks, Required: tc.required})
		if v.Status != tc.status || !reflect.DeepEqual(v.OtherApp, tc.otherApp) || len(v.Missing) != 0 || v.Required != tc.count {
			t.Errorf("%s: status %s, otherApp %q, missing %q, required %d; want %s, %q, none, %d", tc.name, v.Status, v.OtherApp, v.Missing, v.Required, tc.status, tc.otherApp, tc.count)
		}
	}
}

// TestDecideChecks_workflows: a ruleset's required workflow counts as run when a
// check came from that workflow file in that repository; with none, it blocks
// as missing, or as unverifiable when some run does not say its file.
func TestDecideChecks_workflows(t *testing.T) {
	scan := ghx.RequiredWorkflow{Path: ".github/workflows/scan.yml", RepoID: 9, Repo: "org/security"}
	run := func(path, repo, state string) ghx.Check {
		return ghx.Check{Name: "job", State: state, Workflow: "W", Event: "pull_request", WorkflowPath: path, WorkflowRepo: repo, App: 15368}
	}
	cases := []struct {
		name               string
		checks             []ghx.Check
		wf                 ghx.RequiredWorkflow
		status             ChecksStatus
		missing, unverifed int
	}{
		{"it ran", []ghx.Check{run(".github/workflows/scan.yml", "org/security", "SUCCESS")}, scan, ChecksGreen, 0, 0},
		{"it ran, owner/name in another case", []ghx.Check{run(".github/workflows/scan.yml", "Org/Security", "SUCCESS")}, scan, ChecksGreen, 0, 0},
		{"its run is pending: pending, not missing", []ghx.Check{run(".github/workflows/scan.yml", "org/security", "QUEUED")}, scan, ChecksBlocked, 0, 0},
		{"only its runs were skipped: it ran", []ghx.Check{run(".github/workflows/scan.yml", "org/security", "SKIPPED"), run("ci.yml", "o/r", "SUCCESS")}, scan, ChecksGreen, 0, 0},
		{"the same file in the PR's own repository is another workflow", []ghx.Check{run(".github/workflows/scan.yml", "o/r", "SUCCESS")}, scan, ChecksBlocked, 1, 0},
		{"never ran", []ghx.Check{run(".github/workflows/ci.yml", "o/r", "SUCCESS")}, scan, ChecksBlocked, 1, 0},
		{"no checks at all", nil, scan, ChecksBlocked, 1, 0},
		{"a run that does not say its file: cannot be ruled out", []ghx.Check{run("", "", "SUCCESS")}, scan, ChecksBlocked, 0, 1},
		{"a run that does not say its file, and the workflow's run: it ran", []ghx.Check{run("", "", "SUCCESS"), run(".github/workflows/scan.yml", "org/security", "SUCCESS")}, scan, ChecksGreen, 0, 0},
		{"another app's run and a status are not workflows", []ghx.Check{{Name: "x", State: "SUCCESS", App: 5}, {Name: "y", State: "SUCCESS", Status: true}}, scan, ChecksBlocked, 1, 0},
		{"the workflow's repository was never named", []ghx.Check{run(".github/workflows/scan.yml", "org/security", "SUCCESS")}, ghx.RequiredWorkflow{Path: scan.Path, RepoID: 9}, ChecksBlocked, 0, 1},
	}
	for _, tc := range cases {
		v := DecideChecks(ChecksInput{Checks: tc.checks, Workflows: []ghx.RequiredWorkflow{tc.wf}})
		if v.Status != tc.status || len(v.MissingWorkflows) != tc.missing || len(v.Unverifiable) != tc.unverifed || v.Required != 1 {
			t.Errorf("%s: status %s, missing %q, unverifiable %q, required %d; want %s, %d, %d, 1", tc.name, v.Status, v.MissingWorkflows, v.Unverifiable, v.Required, tc.status, tc.missing, tc.unverifed)
		}
	}
	v := DecideChecks(ChecksInput{Workflows: []ghx.RequiredWorkflow{scan, {Path: "x.yml", RepoID: 3}}})
	if want := []string{"2 required workflows never ran on this head"}; !reflect.DeepEqual(v.Reasons(), []string{"1 required workflow never ran on this head", "1 required workflow that wt cannot verify"}) {
		t.Errorf("Reasons = %q (not %q)", v.Reasons(), want)
	}
	if !reflect.DeepEqual(v.MissingWorkflows, []string{"org/security:.github/workflows/scan.yml"}) || !reflect.DeepEqual(v.Unverifiable, []string{"repository 3:x.yml"}) {
		t.Errorf("labels = %q, %q", v.MissingWorkflows, v.Unverifiable)
	}
}

// TestBucketOf_sharesStateKind: the gate's buckets and latestChecks' kinds are
// one reading of GitHub's states (a failure latestChecks keeps must be one the
// gate blocks on).
func TestBucketOf_sharesStateKind(t *testing.T) {
	want := map[ghx.Kind]CheckBucket{
		ghx.KindPassed: BucketPassed, ghx.KindQuiet: BucketSkipped, ghx.KindPending: BucketPending,
		ghx.KindFailed: BucketFailed, ghx.KindVoid: BucketFailed, ghx.KindUnknown: BucketUnknown,
	}
	for _, s := range []string{"SUCCESS", "SKIPPED", "NEUTRAL", "PENDING", "EXPECTED", "QUEUED", "IN_PROGRESS", "WAITING", "REQUESTED",
		"FAILURE", "ERROR", "TIMED_OUT", "ACTION_REQUIRED", "STARTUP_FAILURE", "CANCELLED", "STALE", "", "NEW_STATE"} {
		if got := BucketOf(s); got != want[ghx.StateKind(s)] {
			t.Errorf("BucketOf(%q) = %s, but StateKind says %v", s, got, ghx.StateKind(s))
		}
	}
}

// TestPassthroughReaders: the -R/--repo, the flags that merge nothing, and
// --auto, read from a merge-pr passthrough the way gh's parser reads it (#179).
func TestPassthroughReaders(t *testing.T) {
	for _, c := range []struct {
		args       []string
		repo       string
		nonMerging string
		auto       bool
	}{
		{nil, "", "", false},
		{[]string{"-R", "o/r"}, "o/r", "", false},
		{[]string{"--repo=o/r", "-R", "x/y"}, "x/y", "", false}, // the last wins
		{[]string{"-Ro/r"}, "o/r", "", false},
		{[]string{"-R=o/r"}, "o/r", "", false},
		{[]string{"-dR", "o/r"}, "o/r", "", false},
		{[]string{"--subject", "-R", "x"}, "", "", false}, // -R is the subject
		{[]string{"--", "-R", "o/r"}, "", "", false},      // after --: positionals
		{[]string{"-R"}, "", "", false},                   // dangling: gh fails on it
		{[]string{"--disable-auto"}, "", "--disable-auto", false},
		{[]string{"--disable-auto=false"}, "", "", false},
		{[]string{"--disable-auto=nope"}, "", "--disable-auto", false}, // gh fails: merges nothing
		{[]string{"--help"}, "", "--help", false},
		{[]string{"-h"}, "", "--help", false},
		{[]string{"-dh"}, "", "--help", false},
		{[]string{"--body", "--help"}, "", "", false}, // the body is "--help"
		{[]string{"-b", "-h"}, "", "", false},
		{[]string{"--auto"}, "", "", true},
		{[]string{"--auto", "-R", "o/r"}, "o/r", "", true},
		{[]string{"--auto=false"}, "", "", false},
		{[]string{"-t", "--auto"}, "", "", false}, // the subject is "--auto"
	} {
		if got := ParseForwardedRepo(c.args); got != c.repo {
			t.Errorf("ParseForwardedRepo(%q) = %q, want %q", c.args, got, c.repo)
		}
		if got := NonMerging(c.args); got != c.nonMerging {
			t.Errorf("NonMerging(%q) = %q, want %q", c.args, got, c.nonMerging)
		}
		if got := ForwardsAuto(c.args); got != c.auto {
			t.Errorf("ForwardsAuto(%q) = %v, want %v", c.args, got, c.auto)
		}
	}
}

// TestVerdictField: a dry run the checks would refuse never reads as a bare
// verdict=ok (#179).
func TestVerdictField(t *testing.T) {
	for _, c := range []struct {
		v      Verdict
		checks string
		want   string
	}{
		{VerdictOK, "", "verdict=ok"},
		{VerdictOK, "blocked", "verdict=ok checks=blocked"},
		{VerdictOK, "green", "verdict=ok checks=green"},
		{VerdictEmptyDiff, "unread", "verdict=block:empty_diff checks=unread"},
	} {
		if got := VerdictField(c.v, c.checks); got != c.want {
			t.Errorf("VerdictField(%s, %q) = %q, want %q", c.v, c.checks, got, c.want)
		}
	}
}

// TestWithMatchHead: the pin goes in FRONT of everything else (#180), so an
// operator's own forwarded --match-head-commit wins, and nothing is pinned
// when the checks were not read. The caller's slice is never touched.
func TestWithMatchHead(t *testing.T) {
	const head = "0123456789abcdef0123456789abcdef01234567"
	cases := []struct {
		head string
		args []string
		want []string
	}{
		{"", nil, nil},
		{"", []string{"--admin"}, []string{"--admin"}},
		{head, nil, []string{"--match-head-commit", head}},
		{head, []string{"--admin", "-F", "-"}, []string{"--match-head-commit", head, "--admin", "-F", "-"}},
		{head, []string{"--match-head-commit", "abc"}, []string{"--match-head-commit", head, "--match-head-commit", "abc"}},
	}
	for _, tc := range cases {
		if got := WithMatchHead(tc.head, tc.args); !reflect.DeepEqual(got, tc.want) {
			t.Errorf("WithMatchHead(%q, %q) = %q, want %q", tc.head, tc.args, got, tc.want)
		}
	}
	args := make([]string, 1, 4)
	args[0] = "--admin"
	_ = WithMatchHead(head, args)
	if args[0] != "--admin" || len(args) != 1 {
		t.Errorf("caller slice mutated: %q", args)
	}
}
