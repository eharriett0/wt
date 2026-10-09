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
		{"no checks, a required one: never started", ChecksInput{Required: []string{"test"}},
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
		{"required and passed", ChecksInput{Checks: []ghx.Check{chk("test", "SUCCESS")}, Required: []string{"test"}},
			want{status: ChecksGreen, ran: 1}},
		{"required and skipped: GitHub counts it satisfied", ChecksInput{Checks: []ghx.Check{chk("test", "SKIPPED"), chk("lint", "SUCCESS")},
			Required: []string{"test"}}, want{status: ChecksGreen, ran: 1}},
		{"required and pending: pending, not missing", ChecksInput{Checks: []ghx.Check{chk("test", "IN_PROGRESS")}, Required: []string{"test"}},
			want{status: ChecksBlocked, pending: 1, ran: 1}},
		{"required names match exactly", ChecksInput{Checks: []ghx.Check{chk("Test", "SUCCESS")}, Required: []string{"test"}},
			want{status: ChecksBlocked, missing: []string{"test"}, ran: 1}},
		{"cli/cli#14629: the triage ran, the required builds never started", ChecksInput{Checks: triageOnly, Required: builds},
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
		Required:       []string{"a", "x", "y"},
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
// require, and which of them could not be read. A server with no rulesets for
// the repo is an answer (none), not a failed read.
func TestRequiredChecks(t *testing.T) {
	boom := errors.New("Server Error (HTTP 502)")
	cases := []struct {
		name                  string
		prot                  []string
		protErr               error
		rules                 []string
		rulesErr              error
		wantNames, wantUnread []string
	}{
		{"both read", []string{"b", "a"}, nil, []string{"c", "a"}, nil, []string{"a", "b", "c"}, nil},
		{"neither requires anything", nil, nil, nil, nil, nil, nil},
		{"no rulesets on this server or plan", []string{"a"}, nil, nil, ghx.ErrRulesetsUnavailable, []string{"a"}, nil},
		{"no rulesets, wrapped", nil, nil, nil, fmt.Errorf("rules: %w", ghx.ErrRulesetsUnavailable), nil, nil},
		{"the rulesets could not be read", []string{"a"}, nil, nil, boom, []string{"a"}, []string{"rulesets: " + boom.Error()}},
		{"the protection could not be read", nil, boom, []string{"c"}, nil, []string{"c"}, []string{"branch protection: " + boom.Error()}},
		{"neither could be read", nil, boom, nil, boom, nil,
			[]string{"branch protection: " + boom.Error(), "rulesets: " + boom.Error()}},
	}
	for _, tc := range cases {
		names, unread := RequiredChecks(tc.prot, tc.protErr, tc.rules, tc.rulesErr)
		if !reflect.DeepEqual(names, tc.wantNames) || !reflect.DeepEqual(unread, tc.wantUnread) {
			t.Errorf("%s: RequiredChecks = %q, %q; want %q, %q", tc.name, names, unread, tc.wantNames, tc.wantUnread)
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
