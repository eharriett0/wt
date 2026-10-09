package ghx

import (
	"errors"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
)

// The fixtures in testdata/checks are real answers, captured read-only on
// 2026-10-09 with gh 2.68.1 and the query and endpoints the code uses (a few
// long ones trimmed to fewer nodes, and the branch answers to the fields wt
// reads):
//
//	cli-14629-required-missing   cli/cli#14629: only the triage workflow ran; trunk requires three builds
//	cli-14148-failed             FAILURE ×9, NEUTRAL, SUCCESS; a CodeQL run with no workflow
//	cli-14044-reruns             21 runs of 9 checks (label events re-run the triage workflow)
//	k8s-142872-statuses          commit statuses only (Prow): PENDING, ERROR, SUCCESS
//	k8s-142872-paged             the same PR read 5 per page: three pages
//	ha-185493-in-progress        IN_PROGRESS runs (conclusion null) and statuses
//	vscode-340739-queued         QUEUED and IN_PROGRESS; a CodeQL run from another app
//	cpython-159048-cancelled     CANCELLED
//	wt-205-no-checks             statusCheckRollup null: no check reported
//	wt-999999-no-pr              no such PR: {"data":{"resource":null}}, exit 0
//	wt-179-issue-url             an issue's URL: {"data":{"resource":{}}}, exit 0
func fixture(t *testing.T, name string) string {
	t.Helper()
	b, err := os.ReadFile(filepath.Join("testdata", "checks", name))
	if err != nil {
		t.Fatal(err)
	}
	return string(b)
}

func runCheck(name, workflow, event, state string) Check {
	return Check{Name: name, Workflow: workflow, Event: event, State: state}
}

func statusCheck(name, state string) Check { return Check{Name: name, State: state, Status: true} }

// TestParsePRChecks reads real answers. The expected checks were derived
// outside wt (jq: newest run per name/workflow/event), and for cli/cli#14044
// they are what gh pr checks itself lists for that PR.
func TestParsePRChecks(t *testing.T) {
	triage := func(name, state string) Check { return runCheck(name, "PR Triaging", "pull_request_target", state) }
	cases := []struct {
		file       string
		head, base string
		checks     []Check // nil: compare only the per-state counts below
		states     map[string]int
	}{
		{"cli-14629-required-missing.json", "57bfea5f807ffb0c78336679d052ab56217aafde", "trunk", []Check{
			triage("check-requirements / check-requirements", "SUCCESS"),
			triage("check-requirements / close-unmet-requirements", "SKIPPED"),
			triage("close-from-default-branch / close-from-default-branch", "SKIPPED"),
			triage("close-no-help-wanted", "SKIPPED"),
			triage("close-unmet-requirements", "SKIPPED"),
			triage("label-external / label_issues", "SUCCESS"),
			triage("ready-for-review", "SKIPPED"),
		}, nil},
		// gh pr checks 14044 -R cli/cli lists exactly these nine.
		{"cli-14044-reruns.json", "1432a0e3c280cf056ced6787886b70f618f5ef88", "trunk", []Check{
			triage("check-requirements / check-requirements", "SKIPPED"), // newest of SUCCESS, SKIPPED, SKIPPED
			triage("check-requirements / close-unmet-requirements", "SKIPPED"),
			triage("close-from-default-branch", "SKIPPED"),
			triage("close-from-default-branch / close-from-default-branch", "SKIPPED"),
			triage("close-no-help-wanted", "SKIPPED"),
			triage("close-unmet-requirements", "SKIPPED"),
			triage("label-external", "SKIPPED"),
			triage("label-external / label_issues", "SUCCESS"),
			triage("ready-for-review", "SKIPPED"),
		}, nil},
		{"k8s-142872-statuses.json", "b9a8348a23b81129c58abf2a8d53bb40730ea648", "master", []Check{
			statusCheck("EasyCLA", "SUCCESS"),
			statusCheck("pull-kubernetes-cmd", "PENDING"),
			statusCheck("pull-kubernetes-conformance-kind-ga-only-parallel", "PENDING"),
			statusCheck("pull-kubernetes-dependencies", "SUCCESS"),
			statusCheck("pull-kubernetes-e2e-gce", "PENDING"),
			statusCheck("pull-kubernetes-e2e-kind", "PENDING"),
			statusCheck("pull-kubernetes-integration", "PENDING"),
			statusCheck("pull-kubernetes-linter-hints", "PENDING"),
			statusCheck("pull-kubernetes-node-e2e-containerd", "PENDING"),
			statusCheck("pull-kubernetes-typecheck", "SUCCESS"),
			statusCheck("pull-kubernetes-unit", "ERROR"),
			statusCheck("pull-kubernetes-verify", "PENDING"),
			statusCheck("tide", "PENDING"),
		}, nil},
		{"vscode-340739-queued.json", "7300c42a7377d72022b939e40da9f115ce613426", "main", []Check{
			runCheck("Analyze (javascript-typescript)", "CodeQL", "pull_request", "IN_PROGRESS"),
			runCheck("Analyze (rust)", "CodeQL", "pull_request", "IN_PROGRESS"),
			runCheck("Check metadata", "Telemetry", "pull_request", "SUCCESS"),
			runCheck("CodeQL", "", "", "QUEUED"), // another app's run: no workflow
			runCheck("Compile & Hygiene", "Code OSS", "pull_request", "IN_PROGRESS"),
			runCheck("Monaco Editor checks", "Monaco Editor checks", "pull_request", "SUCCESS"),
			runCheck("Playwright Fixture Tests", "Component Fixtures", "pull_request", "IN_PROGRESS"),
			runCheck("chat-lib tests (macos-latest)", "chat-lib tests", "pull_request", "SUCCESS"),
			runCheck("chat-lib tests (ubuntu-latest)", "chat-lib tests", "pull_request", "SUCCESS"),
		}, nil},
		{"cli-14148-failed.json", "82b0f3e997c6ba18f429de3772bc2867cf759ed8", "trunk", nil,
			map[string]int{"FAILURE": 9, "NEUTRAL": 1, "SUCCESS": 1}},
		{"ha-185493-in-progress.json", "29473f6009a45e8cb9a68bb56cce55036514dcd6", "dev", nil,
			map[string]int{"IN_PROGRESS": 3, "SKIPPED": 1, "SUCCESS": 12}},
		{"cpython-159048-cancelled.json", "f50e78d9b6511aa3792beb8e04890a667a1e4253", "3.15", nil,
			map[string]int{"CANCELLED": 1, "SUCCESS": 7}},
		{"wt-205-no-checks.json", "514ab1d18f91a254ee91350e90955a83412d1c1c", "main", []Check{}, nil},
	}
	for _, tc := range cases {
		got, err := parsePRChecks(fixture(t, tc.file))
		if err != nil {
			t.Errorf("%s: parsePRChecks: %v", tc.file, err)
			continue
		}
		if got.Head != tc.head || got.Base != tc.base {
			t.Errorf("%s: head, base = %q, %q, want %q, %q", tc.file, got.Head, got.Base, tc.head, tc.base)
		}
		if tc.checks != nil {
			if len(got.Checks) != 0 || len(tc.checks) != 0 {
				if !reflect.DeepEqual(got.Checks, tc.checks) {
					t.Errorf("%s: checks =\n%+v\nwant\n%+v", tc.file, got.Checks, tc.checks)
				}
			}
			continue
		}
		states := map[string]int{}
		for _, c := range got.Checks {
			states[c.State]++
		}
		if !reflect.DeepEqual(states, tc.states) {
			t.Errorf("%s: states = %v, want %v", tc.file, states, tc.states)
		}
	}
}

// TestParsePRChecks_pages: the same PR read 5 per page (three JSON objects in a
// row, as `gh api graphql --paginate` prints them) reads as read in one page.
func TestParsePRChecks_pages(t *testing.T) {
	one, err := parsePRChecks(fixture(t, "k8s-142872-statuses.json"))
	if err != nil {
		t.Fatal(err)
	}
	paged := fixture(t, "k8s-142872-paged.json")
	if n := strings.Count(paged, `{"data":`); n != 3 {
		t.Fatalf("the paged fixture has %d pages, want 3", n)
	}
	got, err := parsePRChecks(paged)
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(got, one) {
		t.Errorf("paged read = %+v\nwant %+v", got, one)
	}
}

// TestParsePRChecks_neverAPlaceholder: what is not a PR with a head commit is
// an error, never "no checks" (#168). The two real ones come back with exit 0.
func TestParsePRChecks_neverAPlaceholder(t *testing.T) {
	const head = "514ab1d18f91a254ee91350e90955a83412d1c1c"
	const other = "0123456789abcdef0123456789abcdef01234567"
	page := func(headRef, oid, rollup string) string {
		return `{"data":{"resource":{"headRefOid":"` + headRef + `","baseRefName":"main","commits":{"nodes":[{"commit":{"oid":"` + oid + `","statusCheckRollup":` + rollup + `}}]}}}}`
	}
	contexts := func(next bool, nodes string) string {
		return `{"contexts":{"pageInfo":{"hasNextPage":` + map[bool]string{true: "true", false: "false"}[next] + `,"endCursor":"MQ"},"nodes":[` + nodes + `]}}`
	}
	const okRun = `{"__typename":"CheckRun","databaseId":1,"name":"test","status":"COMPLETED","conclusion":"SUCCESS","checkSuite":null}`
	bad := map[string]string{
		"no such PR (real)":        fixture(t, "wt-999999-no-pr.json"),
		"an issue's URL (real)":    fixture(t, "wt-179-issue-url.json"),
		"empty output":             "",
		"null":                     "null",
		"not JSON":                 "no checks reported on the 'x' branch",
		"no base branch":           strings.Replace(page(head, head, "null"), `"baseRefName":"main"`, `"baseRefName":""`, 1),
		"an abbreviated head":      page(head[:12], head[:12], "null"),
		"head is not the commit":   page(head, other, "null"),
		"no commit":                `{"data":{"resource":{"headRefOid":"` + head + `","baseRefName":"main","commits":{"nodes":[]}}}}`,
		"rollup without contexts":  page(head, head, `{}`),
		"contexts without pages":   page(head, head, `{"contexts":{"nodes":[]}}`),
		"a run with no name":       page(head, head, contexts(false, `{"__typename":"CheckRun","status":"QUEUED"}`)),
		"a run with no status":     page(head, head, contexts(false, `{"__typename":"CheckRun","name":"test"}`)),
		"a status with no state":   page(head, head, contexts(false, `{"__typename":"StatusContext","context":"ci"}`)),
		"a kind wt does not know":  page(head, head, contexts(false, `{"__typename":"SomethingNew"}`)),
		"gh stopped early":         page(head, head, contexts(true, okRun)),
		"the head moved":           page(head, head, contexts(true, okRun)) + page(other, other, contexts(false, okRun)),
		"a page after the last":    page(head, head, contexts(false, okRun)) + page(head, head, contexts(false, okRun)),
		"a null rollup, then more": page(head, head, "null") + page(head, head, contexts(false, okRun)),
		"a good page, then junk":   page(head, head, contexts(false, okRun)) + "\nnull",
	}
	for name, out := range bad {
		if got, err := parsePRChecks(out); err == nil {
			t.Errorf("%s: parsePRChecks = %+v, want an error", name, got)
		}
	}
	// A run that finished with no conclusion is read, with no state: the gate
	// reads that as unknown, never green.
	got, err := parsePRChecks(page(head, head, contexts(false, `{"__typename":"CheckRun","databaseId":1,"name":"test","status":"COMPLETED","conclusion":null}`)))
	if err != nil || len(got.Checks) != 1 || got.Checks[0].State != "" {
		t.Errorf("a COMPLETED run with no conclusion = %+v (err %v), want one check with no state", got, err)
	}
}

func TestRunState(t *testing.T) {
	for _, c := range []struct{ status, conclusion, want string }{
		{"COMPLETED", "SUCCESS", "SUCCESS"},
		{"COMPLETED", "CANCELLED", "CANCELLED"},
		{"completed", "FAILURE", "FAILURE"},
		{"COMPLETED", "", ""}, // no conclusion: unknown, never green
		{"IN_PROGRESS", "", "IN_PROGRESS"},
		{"QUEUED", "", "QUEUED"},
		{"WAITING", "", "WAITING"},
		{"PENDING", "", "PENDING"},
		{"REQUESTED", "", "REQUESTED"},
	} {
		if got := RunState(c.status, c.conclusion); got != c.want {
			t.Errorf("RunState(%q, %q) = %q, want %q", c.status, c.conclusion, got, c.want)
		}
	}
}

// TestLatestChecks: the newest run of each check counts (a re-run that passed
// after a failure is green, and one that failed after a pass is red), runs and
// statuses never merge, and one name in two workflows is two checks.
func TestLatestChecks(t *testing.T) {
	r := func(id int64, name, workflow, state string) rawCheck {
		return rawCheck{Check: runCheck(name, workflow, "pull_request", state), id: id}
	}
	s := func(at, name, state string) rawCheck {
		return rawCheck{Check: statusCheck(name, state), at: at}
	}
	got := latestChecks([]rawCheck{
		r(7, "test", "CI", "FAILURE"),
		r(9, "test", "CI", "SUCCESS"), // the re-run
		r(8, "test", "CI", "CANCELLED"),
		r(5, "lint", "CI", "SUCCESS"),
		r(6, "lint", "CI", "FAILURE"), // failed after a pass
		r(3, "test", "Nightly", "QUEUED"),
		s("2026-10-09T10:00:00Z", "ci/x", "SUCCESS"),
		s("2026-10-09T11:00:00Z", "ci/x", "PENDING"),
		s("2026-10-09T09:00:00Z", "ci/x", "FAILURE"),
		r(4, "ci/x", "", "SUCCESS"), // a run named like the status: its own check
	})
	want := []Check{ // sorted by name, workflow, event: the status's event is ""
		statusCheck("ci/x", "PENDING"),
		runCheck("ci/x", "", "pull_request", "SUCCESS"),
		runCheck("lint", "CI", "pull_request", "FAILURE"),
		runCheck("test", "CI", "pull_request", "SUCCESS"),
		runCheck("test", "Nightly", "pull_request", "QUEUED"),
	}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("latestChecks =\n%+v\nwant\n%+v", got, want)
	}
	if got := latestChecks(nil); len(got) != 0 {
		t.Errorf("latestChecks(nil) = %+v, want none", got)
	}
}

func TestParseBranchProtection(t *testing.T) {
	ok := []struct {
		name, out string
		want      []string
	}{
		{"cli/cli trunk (real): protected, three builds required",
			fixture(t, "cli-trunk.branch.json"), []string{"build (macos-latest)", "build (ubuntu-latest)", "build (windows-latest)"}},
		{"eharriett0/wt main (real): protected, required checks off", fixture(t, "wt-main.branch.json"), nil},
		{"eharriett0/awesome-o main (real): not protected", fixture(t, "awesome-o-main.branch.json"), nil},
		{"contexts and checks are one list", `{"protected":true,"protection":{"required_status_checks":{"enforcement_level":"everyone","contexts":["b","a"],"checks":[{"context":"a","app_id":1},{"context":"c","app_id":null}]}}}`,
			[]string{"a", "b", "c"}},
		{"not protected: whatever else it says", `{"protected":false,"protection":{"required_status_checks":{"enforcement_level":"everyone","contexts":["a"]}}}`, nil},
		{"required checks switched off: whatever they list", `{"protected":true,"protection":{"required_status_checks":{"enforcement_level":"off","contexts":["a"],"checks":[{"context":"a"}]}}}`, nil},
	}
	for _, tc := range ok {
		got, err := parseBranchProtection(tc.out)
		if err != nil || !reflect.DeepEqual(got, tc.want) {
			t.Errorf("%s: parseBranchProtection = %q (err %v), want %q", tc.name, got, err, tc.want)
		}
	}
	for name, out := range map[string]string{
		"empty":                         "",
		"null":                          "null",
		"no protected field":            `{"name":"main"}`,
		"protected, no protection":      `{"protected":true}`,
		"protected, no required checks": `{"protected":true,"protection":{"enabled":true}}`,
		"protected, no enforcement":     `{"protected":true,"protection":{"required_status_checks":{"contexts":["a"]}}}`,
		"an error body":                 `{"message":"Branch not found","status":"404"}`,
	} {
		if got, err := parseBranchProtection(out); err == nil {
			t.Errorf("%s: parseBranchProtection(%q) = %q, want an error", name, out, got)
		}
	}
}

func TestParseRulesets(t *testing.T) {
	ok := []struct {
		name, out string
		want      []string
	}{
		{"home-assistant/core dev (real): a required_status_checks ruleset", fixture(t, "ha-dev.rules.json"), []string{
			"Check all requirements", "Check hassfest", "Collect information & changes data",
			"blocking-label-awaiting-frontend", "cla-bot", "code-owner-approval", "docs-missing", "required-labels"}},
		{"cli/cli trunk (real): a ruleset that requires no check", fixture(t, "cli-trunk.rules.json"), nil},
		{"eharriett0/wt main (real): no rules", fixture(t, "wt-main.rules.json"), nil},
		{"pages run together", `[{"type":"required_status_checks","parameters":{"required_status_checks":[{"context":"b"}]}}]` +
			`[{"type":"required_status_checks","parameters":{"required_status_checks":[{"context":"a"},{"context":"b"}]}}]`, []string{"a", "b"}},
	}
	for _, tc := range ok {
		got, err := parseRulesets(tc.out)
		if err != nil || !reflect.DeepEqual(got, tc.want) {
			t.Errorf("%s: parseRulesets = %q (err %v), want %q", tc.name, got, err, tc.want)
		}
	}
	for name, out := range map[string]string{
		"empty":                    "",
		"null":                     "null",
		"an error body":            fixture(t, "awesome-o-main.rules-403.json"),
		"a rule with no type":      `[{"parameters":{}}]`,
		"a status rule, no list":   `[{"type":"required_status_checks"}]`,
		"a status rule, null list": `[{"type":"required_status_checks","parameters":{"required_status_checks":null}}]`,
		"a list, then junk":        `[]` + "\nnope",
	} {
		if got, err := parseRulesets(out); err == nil {
			t.Errorf("%s: parseRulesets(%q) = %q, want an error", name, out, got)
		}
	}
}

// TestRulesetsUnavailable: the two answers that mean the server has no
// rulesets for the repo (both real), and the failed reads that do not.
func TestRulesetsUnavailable(t *testing.T) {
	for _, c := range []struct {
		name, out string
		want      bool
	}{
		{"a private repo on a plan without rulesets (real 403)", fixture(t, "awesome-o-main.rules-403.json"), true},
		{"no such endpoint (real 404)", fixture(t, "no-repo.rules-404.json"), true},
		{"rate limited: a 403 that is a failed read", `{"message":"API rate limit exceeded for user ID 1.","status":"403"}`, false},
		{"SSO: a 403 that is a failed read", `{"message":"Resource protected by organization SAML enforcement.","status":"403"}`, false},
		{"a server error", `{"message":"Server Error","status":"502"}`, false},
		{"nothing printed", "", false},
		{"not JSON", "gh: Not Found (HTTP 404)", false},
	} {
		if got := rulesetsUnavailable(c.out); got != c.want {
			t.Errorf("%s: rulesetsUnavailable = %v, want %v", c.name, got, c.want)
		}
	}
}

// TestChecksReadsThroughGh drives the three reads against a fake gh, so the
// argv they send and the way a failure comes back are pinned: the GraphQL
// read's error carries gh's own message, the rules read tells "unavailable"
// apart from "failed", and a branch name is escaped into the path.
func TestChecksReadsThroughGh(t *testing.T) {
	dir := fakeGh(t, `printf '%s\n' "$@" >> "$d/argv"
case "$*" in
"auth status"*) exit 0 ;;
"pr view 7 --json url --jq .url") echo "https://github.com/o/r/pull/7" ;;
"api graphql --paginate"*) [ -e "$d/gql.fail" ] && { echo "GraphQL: Something went wrong" >&2; exit 1; }; cat "$d/gql" ;;
"api repos/{owner}/{repo}/branches/"*) cat "$d/branch" ;;
"api repos/{owner}/{repo}/rules/branches/"*) cat "$d/rules"; [ -e "$d/rules.fail" ] && { echo "gh: failed (HTTP 403)" >&2; exit 1; }; exit 0 ;;
esac
`)
	write := func(name, s string) {
		t.Helper()
		if err := os.WriteFile(filepath.Join(dir, name), []byte(s), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	write("gql", fixture(t, "cli-14629-required-missing.json"))
	got, err := PRChecks("7")
	if err != nil || got.Base != "trunk" || len(got.Checks) != 7 {
		t.Fatalf("PRChecks = %+v (err %v), want cli/cli#14629's seven checks", got, err)
	}
	argv := readFile(t, filepath.Join(dir, "argv"))
	if !strings.Contains(argv, "query="+prChecksQuery+"\n") || !strings.Contains(argv, "url=https://github.com/o/r/pull/7\n") {
		t.Errorf("gh argv = %q, want the checks query for the PR's URL", argv)
	}
	write("gql.fail", "")
	if _, err := PRChecks("7"); err == nil || !strings.Contains(err.Error(), "GraphQL: Something went wrong") {
		t.Errorf("PRChecks with gh failing: err = %v, want gh's message", err)
	}

	write("branch", fixture(t, "cli-trunk.branch.json"))
	if names, err := BranchProtectionChecks("release/1.0#x"); err != nil || len(names) != 3 {
		t.Errorf("BranchProtectionChecks = %q (err %v), want three", names, err)
	}
	if argv := readFile(t, filepath.Join(dir, "argv")); !strings.Contains(argv, "repos/{owner}/{repo}/branches/release%2F1.0%23x\n") {
		t.Errorf("gh argv = %q, want the branch escaped into the path", argv)
	}

	write("rules", fixture(t, "ha-dev.rules.json"))
	if names, err := RulesetChecks("dev"); err != nil || len(names) != 8 {
		t.Errorf("RulesetChecks = %q (err %v), want eight", names, err)
	}
	write("rules.fail", "")
	write("rules", fixture(t, "awesome-o-main.rules-403.json"))
	if _, err := RulesetChecks("main"); !errors.Is(err, ErrRulesetsUnavailable) {
		t.Errorf("RulesetChecks on a plan without rulesets: err = %v, want ErrRulesetsUnavailable", err)
	}
	write("rules", `{"message":"API rate limit exceeded for user ID 1.","status":"403"}`)
	_, err = RulesetChecks("main")
	if err == nil || errors.Is(err, ErrRulesetsUnavailable) || !strings.Contains(err.Error(), "rate limit") {
		t.Errorf("RulesetChecks rate limited: err = %v, want a failed read naming it", err)
	}
}
