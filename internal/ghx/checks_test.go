package ghx

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
)

// The fixtures in testdata/checks are real answers, captured read-only with gh
// 2.68.1 and the query and endpoints the code uses (a few long ones trimmed to
// fewer nodes, and the branch answers to the fields wt reads). The first ten
// were captured on 2026-10-09 with the first version of the query, then given
// the fields it reads since (baseRepository; each run's check suite, app and
// workflow file; isRequired), read from the SAME head commit, so the states
// they caught (queued, in progress) are kept:
//
//	cli-14629-required-missing   cli/cli#14629: only the triage workflow ran; trunk requires three builds
//	cli-14148-failed             FAILURE ×9, NEUTRAL, SUCCESS; a CodeQL run with no workflow
//	cli-14044-reruns             21 runs of 9 checks: label events ran the triage workflow three times
//	k8s-142872-statuses          commit statuses only (Prow): PENDING, ERROR, SUCCESS
//	k8s-142872-paged             the same PR read 5 per page: three pages
//	ha-185493-in-progress        IN_PROGRESS runs (conclusion null) and statuses
//	vscode-340739-queued         QUEUED and IN_PROGRESS; a CodeQL run from another app
//	cpython-159048-cancelled     CANCELLED
//	wt-205-no-checks             statusCheckRollup null: no check reported
//	wt-999999-no-pr              no such PR: {"data":{"resource":null}}, exit 0
//	wt-179-issue-url             an issue's URL: {"data":{"resource":{}}}, exit 0
//
// Captured on 2026-10-09 with the current query:
//
//	dotnet-135496-two-apps                      two apps' runs of one name; a required check FAILED
//	pytorch-200419-one-name-two-jobs            two jobs of one name in ONE workflow run, twice
//	grafana-134667-cancelled-and-required-workflows  CANCELLED runs a later run replaced; two
//	                                            required workflows from grafana/security-github-actions
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

// brief is checks without what only some tests look at: the app, GitHub's
// isRequired, the workflow file.
func brief(cs []Check) []Check {
	out := make([]Check, len(cs))
	for i, c := range cs {
		out[i] = Check{Name: c.Name, State: c.State, Workflow: c.Workflow, Event: c.Event, Status: c.Status}
	}
	return out
}

// TestParsePRChecks reads real answers. The expected checks were derived
// outside wt (jq over the fixture, and the rules latestChecks documents).
func TestParsePRChecks(t *testing.T) {
	triage := func(name, state string) Check { return runCheck(name, "PR Triaging", "pull_request_target", state) }
	gf := func(name, workflow, state string) Check { return runCheck(name, workflow, "pull_request", state) }
	cases := []struct {
		file             string
		head, base, repo string
		checks           []Check // nil: compare only the per-state counts below
		states           map[string]int
	}{
		{"cli-14629-required-missing.json", "57bfea5f807ffb0c78336679d052ab56217aafde", "trunk", "cli/cli", []Check{
			triage("check-requirements / check-requirements", "SUCCESS"),
			triage("check-requirements / close-unmet-requirements", "SKIPPED"),
			triage("close-from-default-branch / close-from-default-branch", "SKIPPED"),
			triage("close-no-help-wanted", "SKIPPED"),
			triage("close-unmet-requirements", "SKIPPED"),
			triage("label-external / label_issues", "SUCCESS"),
			triage("ready-for-review", "SKIPPED"),
		}, nil},
		// gh pr checks 14044 -R cli/cli lists these nine too, but check-requirements
		// as SKIPPED: it keeps the newest run. Its job ran and passed in the first
		// workflow run; the two the label events started skipped it, which does not
		// undo a pass on the same head.
		{"cli-14044-reruns.json", "1432a0e3c280cf056ced6787886b70f618f5ef88", "trunk", "cli/cli", []Check{
			triage("check-requirements / check-requirements", "SUCCESS"), // SUCCESS, then SKIPPED, SKIPPED
			triage("check-requirements / close-unmet-requirements", "SKIPPED"),
			triage("close-from-default-branch", "SKIPPED"),
			triage("close-from-default-branch / close-from-default-branch", "SKIPPED"),
			triage("close-no-help-wanted", "SKIPPED"),
			triage("close-unmet-requirements", "SKIPPED"),
			triage("label-external", "SKIPPED"),
			triage("label-external / label_issues", "SUCCESS"),
			triage("ready-for-review", "SKIPPED"),
		}, nil},
		{"k8s-142872-statuses.json", "b9a8348a23b81129c58abf2a8d53bb40730ea648", "master", "kubernetes/kubernetes", []Check{
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
		{"vscode-340739-queued.json", "7300c42a7377d72022b939e40da9f115ce613426", "main", "microsoft/vscode", []Check{
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
		// Two apps post "Helix Queue Insights (preview)": two checks, not one.
		{"dotnet-135496-two-apps.json", "c19e482ce0ee80260d272f672013f691281d3a5e", "main", "dotnet/runtime", []Check{
			runCheck("Build Analysis", "", "", "FAILURE"),
			runCheck("Helix Queue Insights (preview)", "", "", "SUCCESS"),
			runCheck("Helix Queue Insights (preview)", "", "", "SUCCESS"),
		}, nil},
		// One name, two jobs, in one workflow run of "pull" and one of "trunk":
		// each job counts.
		{"pytorch-200419-one-name-two-jobs.json", "50b813808823b9f96b4d92dd211d416c17b7ebbe", "main", "pytorch/pytorch", []Check{
			runCheck("before-test / get-label-type / runner-determinator", "pull", "pull_request", "SUCCESS"),
			runCheck("before-test / get-label-type / runner-determinator", "pull", "pull_request", "SUCCESS"),
			runCheck("before-test / get-label-type / runner-determinator", "rocm-mi200", "push", "SUCCESS"),
			runCheck("before-test / get-label-type / runner-determinator", "rocm-mi300", "push", "SUCCESS"),
			runCheck("before-test / get-label-type / runner-determinator", "trunk", "push", "SUCCESS"),
			runCheck("before-test / get-label-type / runner-determinator", "trunk", "push", "SUCCESS"),
		}, nil},
		// Every CANCELLED run was replaced by a later run of its workflow (SUCCESS,
		// or SKIPPED for check-endpoint-migration).
		{"grafana-134667-cancelled-and-required-workflows.json", "878f3ff6e814082f4285a3ee66e01e46476a3ad9", "main", "grafana/grafana", []Check{
			gf("Check whether there are things to scan", "zizmor GitHub Actions static analysis", "SUCCESS"),
			gf("Run Trufflehog", "Trufflehog", "SUCCESS"),
			gf("Run zizmor / Delete branch with dangerous-trigger vulnerability", "zizmor GitHub Actions static analysis", "SKIPPED"),
			gf("Run zizmor / Generate and upload zizmor results 🌈", "zizmor GitHub Actions static analysis", "SUCCESS"),
			gf("Run zizmor / Send zizmor metrics to Prometheus via Grafana Bench", "zizmor GitHub Actions static analysis", "SUCCESS"),
			gf("Run zizmor / job-workflow-ref", "zizmor GitHub Actions static analysis", "SUCCESS"),
			gf("TruffleHog Secret Scan / Send TruffleHog metrics to Prometheus via Grafana Bench", "TruffleHog Secret Scanning", "SUCCESS"),
			gf("TruffleHog Secret Scan / trufflehog-scan", "TruffleHog Secret Scanning", "SUCCESS"),
			gf("check-endpoint-migration", "Endpoint Migration Feature Toggle Check", "SKIPPED"),
			gf("check-separation", "MT Service Compatibility", "SUCCESS"),
			gf("check-separation", "Unified Storage Compatibility", "SUCCESS"),
			gf("detect-changes", "Endpoint Migration Feature Toggle Check", "SUCCESS"),
			statusCheck("license/cla", "SUCCESS"),
			runCheck("main", "Auto-milestone", "pull_request_target", "SUCCESS"),
			gf("main", "PR Checks", "SUCCESS"),
			runCheck("main", "PR automation", "pull_request_target", "SUCCESS"),
			statusCheck("policy-bot", "SUCCESS"),
		}, nil},
		{"cli-14148-failed.json", "82b0f3e997c6ba18f429de3772bc2867cf759ed8", "trunk", "cli/cli", nil,
			map[string]int{"FAILURE": 9, "NEUTRAL": 1, "SUCCESS": 1}},
		{"ha-185493-in-progress.json", "29473f6009a45e8cb9a68bb56cce55036514dcd6", "dev", "home-assistant/core", nil,
			map[string]int{"IN_PROGRESS": 3, "SKIPPED": 1, "SUCCESS": 12}},
		{"cpython-159048-cancelled.json", "f50e78d9b6511aa3792beb8e04890a667a1e4253", "3.15", "python/cpython", nil,
			map[string]int{"CANCELLED": 1, "SUCCESS": 7}},
		{"wt-205-no-checks.json", "514ab1d18f91a254ee91350e90955a83412d1c1c", "main", "eharriett0/wt", []Check{}, nil},
	}
	for _, tc := range cases {
		got, err := parsePRChecks(fixture(t, tc.file))
		if err != nil {
			t.Errorf("%s: parsePRChecks: %v", tc.file, err)
			continue
		}
		if got.Head != tc.head || got.Base != tc.base || got.Repo != tc.repo || got.RepoID <= 0 {
			t.Errorf("%s: head, base, repo, id = %q, %q, %q, %d; want %q, %q, %q, an id", tc.file, got.Head, got.Base, got.Repo, got.RepoID, tc.head, tc.base, tc.repo)
		}
		if tc.checks != nil {
			if len(got.Checks) != 0 || len(tc.checks) != 0 {
				if b := brief(got.Checks); !reflect.DeepEqual(b, tc.checks) {
					t.Errorf("%s: checks =\n%+v\nwant\n%+v", tc.file, b, tc.checks)
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

// TestParsePRChecks_fields: what a run carries besides its name and state, as
// GitHub said it: the app (two apps, two checks), the workflow file and its
// repository (a required workflow from another repository), and isRequired.
func TestParsePRChecks_fields(t *testing.T) {
	dotnet, err := parsePRChecks(fixture(t, "dotnet-135496-two-apps.json"))
	if err != nil {
		t.Fatal(err)
	}
	var apps []int64
	for _, c := range dotnet.Checks {
		switch c.Name {
		case "Helix Queue Insights (preview)":
			apps = append(apps, c.App)
		case "Build Analysis":
			if !c.Required || c.App != 216604 {
				t.Errorf("Build Analysis = %+v, want required, from app 216604", c)
			}
		}
	}
	if len(apps) != 2 || apps[0] == apps[1] || apps[0] <= 0 || apps[1] <= 0 {
		t.Errorf("dotnet: Helix Queue Insights apps = %v, want two different ones", apps)
	}
	grafana, err := parsePRChecks(fixture(t, "grafana-134667-cancelled-and-required-workflows.json"))
	if err != nil {
		t.Fatal(err)
	}
	found := 0
	for _, c := range grafana.Checks {
		if c.Name == "TruffleHog Secret Scan / trufflehog-scan" {
			found++
			if c.WorkflowPath != ".github/workflows/org-required-trufflehog.yml" || c.WorkflowRepo != "grafana/security-github-actions" || c.App != 15368 {
				t.Errorf("trufflehog run = %+v, want its workflow file in grafana/security-github-actions, app 15368", c)
			}
		}
		if c.Status && !c.Required {
			t.Errorf("grafana status %q: isRequired false, want true (both are required)", c.Name)
		}
	}
	if found != 1 || grafana.RepoID != 15111821 {
		t.Errorf("grafana: trufflehog runs %d, repo id %d; want 1, 15111821", found, grafana.RepoID)
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

const (
	pHead  = "514ab1d18f91a254ee91350e90955a83412d1c1c"
	pOther = "0123456789abcdef0123456789abcdef01234567"
)

// pPage is one checks page for head (newest commit oid) with rollup.
func pPage(headRef, oid, rollup string) string {
	return `{"data":{"resource":{"headRefOid":"` + headRef + `","baseRefName":"main","baseRepository":{"nameWithOwner":"o/r","databaseId":42},` +
		`"commits":{"nodes":[{"commit":{"oid":"` + oid + `","statusCheckRollup":` + rollup + `}}]}}}}`
}

// pContexts is a rollup holding nodes.
func pContexts(next bool, nodes ...string) string {
	return `{"contexts":{"pageInfo":{"hasNextPage":` + fmt.Sprint(next) + `,"endCursor":"MQ"},"nodes":[` + strings.Join(nodes, ",") + `]}}`
}

// pRun is a check run: id, its workflow run (check suite) and app; workflow ""
// is another app's run, with no workflow.
func pRun(id, suite, app int64, workflow, event, name, status, conclusion string) string {
	c := "null"
	if conclusion != "" {
		c = `"` + conclusion + `"`
	}
	wr := "null"
	if workflow != "" {
		wr = `{"event":"` + event + `","file":{"path":".github/workflows/` + strings.ToLower(workflow) + `.yml","repositoryName":"o/r"},"workflow":{"name":"` + workflow + `"}}`
	}
	return fmt.Sprintf(`{"__typename":"CheckRun","databaseId":%d,"name":"%s","status":"%s","conclusion":%s,"isRequired":false,`+
		`"checkSuite":{"databaseId":%d,"app":{"databaseId":%d},"workflowRun":%s}}`, id, name, status, c, suite, app, wr)
}

// TestParsePRChecks_neverAPlaceholder: what is not a PR with a head commit is
// an error, never "no checks" (#168). The two real ones come back with exit 0.
func TestParsePRChecks_neverAPlaceholder(t *testing.T) {
	head, other, page, contexts := pHead, pOther, pPage, pContexts
	okRun := pRun(1, 10, 15368, "CI", "pull_request", "test", "COMPLETED", "SUCCESS")
	bad := map[string]string{
		"no such PR (real)":            fixture(t, "wt-999999-no-pr.json"),
		"an issue's URL (real)":        fixture(t, "wt-179-issue-url.json"),
		"empty output":                 "",
		"null":                         "null",
		"not JSON":                     "no checks reported on the 'x' branch",
		"no base branch":               strings.Replace(page(head, head, "null"), `"baseRefName":"main"`, `"baseRefName":""`, 1),
		"no base repository":           strings.Replace(page(head, head, "null"), `"baseRepository":{"nameWithOwner":"o/r","databaseId":42},`, ``, 1),
		"a base repository unnamed":    strings.Replace(page(head, head, "null"), `"nameWithOwner":"o/r"`, `"nameWithOwner":"r"`, 1),
		"a base repository with no id": strings.Replace(page(head, head, "null"), `,"databaseId":42`, ``, 1),
		"an abbreviated head":          page(head[:12], head[:12], "null"),
		"head is not the commit":       page(head, other, "null"),
		"no commit":                    `{"data":{"resource":{"headRefOid":"` + head + `","baseRefName":"main","baseRepository":{"nameWithOwner":"o/r","databaseId":42},"commits":{"nodes":[]}}}}`,
		"rollup without contexts":      page(head, head, `{}`),
		"contexts without pages":       page(head, head, `{"contexts":{"nodes":[]}}`),
		"a run with no name":           page(head, head, contexts(false, `{"__typename":"CheckRun","databaseId":1,"status":"QUEUED"}`)),
		"a run with no status":         page(head, head, contexts(false, `{"__typename":"CheckRun","databaseId":1,"name":"test"}`)),
		"a run with no id":             page(head, head, contexts(false, `{"__typename":"CheckRun","name":"test","status":"QUEUED"}`)),
		"a workflow's run with no check suite id": page(head, head, contexts(false,
			`{"__typename":"CheckRun","databaseId":1,"name":"test","status":"QUEUED","checkSuite":{"workflowRun":{"event":"push"}}}`)),
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
	// A run whose suite names no app is its own suite's app, never another's.
	got, err = parsePRChecks(page(head, head, contexts(false,
		`{"__typename":"CheckRun","databaseId":1,"name":"x","status":"COMPLETED","conclusion":"FAILURE","checkSuite":{"databaseId":7,"app":null,"workflowRun":null}}`,
		`{"__typename":"CheckRun","databaseId":2,"name":"x","status":"COMPLETED","conclusion":"SUCCESS","checkSuite":{"databaseId":8,"app":null,"workflowRun":null}}`)))
	if err != nil || len(got.Checks) != 2 {
		t.Errorf("two unnamed apps' runs of one name = %+v (err %v), want both: one may have FAILED", got.Checks, err)
	}
}

// TestParsePRChecks_review179 pins what the first version got wrong, end to end
// through the parser: a check that FAILED on the head read as passed (#179
// review: a label event's run skipped the job; another app's run of the same
// name passed).
func TestParsePRChecks_review179(t *testing.T) {
	cases := []struct {
		name  string
		nodes []string
		want  []Check
	}{
		{"a FAILURE, then a label event's run that skipped the job: still FAILED", []string{
			pRun(100, 10, 15368, "CI", "pull_request", "test", "COMPLETED", "FAILURE"),
			pRun(200, 20, 15368, "CI", "pull_request", "test", "COMPLETED", "SKIPPED"),
			pRun(150, 10, 15368, "CI", "pull_request", "build", "COMPLETED", "SUCCESS"),
		}, []Check{runCheck("build", "CI", "pull_request", "SUCCESS"), runCheck("test", "CI", "pull_request", "FAILURE")}},
		{"cli/cli#14044's shape: a SUCCESS, then two runs that skipped it: SUCCESS", []string{
			pRun(100, 10, 15368, "Triage", "pull_request_target", "check", "COMPLETED", "SUCCESS"),
			pRun(200, 20, 15368, "Triage", "pull_request_target", "check", "COMPLETED", "SKIPPED"),
			pRun(300, 30, 15368, "Triage", "pull_request_target", "check", "COMPLETED", "SKIPPED"),
		}, []Check{runCheck("check", "Triage", "pull_request_target", "SUCCESS")}},
		{"two apps' runs of one name (dotnet/runtime's shape): one FAILED", []string{
			pRun(113863283186, 102805493202, 2989492, "", "", "Helix Queue Insights (preview)", "COMPLETED", "FAILURE"),
			pRun(113928923729, 102805207456, 216604, "", "", "Helix Queue Insights (preview)", "COMPLETED", "SUCCESS"),
		}, []Check{runCheck("Helix Queue Insights (preview)", "", "", "FAILURE"), runCheck("Helix Queue Insights (preview)", "", "", "SUCCESS")}},
		{"two jobs of one name in one workflow run (pytorch's shape): one FAILED", []string{
			pRun(113914861037, 102848166679, 15368, "trunk", "push", "before-test", "COMPLETED", "FAILURE"),
			pRun(113917508130, 102848166679, 15368, "trunk", "push", "before-test", "COMPLETED", "SUCCESS"),
		}, []Check{runCheck("before-test", "trunk", "push", "FAILURE"), runCheck("before-test", "trunk", "push", "SUCCESS")}},
	}
	for _, tc := range cases {
		got, err := parsePRChecks(pPage(pHead, pHead, pContexts(false, tc.nodes...)))
		if err != nil {
			t.Errorf("%s: %v", tc.name, err)
			continue
		}
		if b := brief(got.Checks); !reflect.DeepEqual(b, tc.want) {
			t.Errorf("%s:\n got %+v\nwant %+v", tc.name, b, tc.want)
		}
	}
}

func TestParsePRRef(t *testing.T) {
	for _, c := range []struct {
		out  string
		want prRef
	}{
		{"14629 https://github.com/cli/cli/pull/14629", prRef{14629, "https://github.com/cli/cli/pull/14629", "github.com"}},
		{"7 https://ghe.example.com/o/r/pull/7\n", prRef{7, "https://ghe.example.com/o/r/pull/7", "ghe.example.com"}},
	} {
		if got, err := parsePRRef(c.out); err != nil || got != c.want {
			t.Errorf("parsePRRef(%q) = %+v (err %v), want %+v", c.out, got, err, c.want)
		}
	}
	for _, out := range []string{"", "null", "https://github.com/o/r/pull/7", "7", "0 https://github.com/o/r/pull/0",
		"7 https://github.com/o/r/pull/8", "7 https://github.com/o/r/issues/7", "7 /o/r/pull/7", "x https://github.com/o/r/pull/x"} {
		if got, err := parsePRRef(out); err == nil {
			t.Errorf("parsePRRef(%q) = %+v, want an error", out, got)
		}
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

func TestStateKind(t *testing.T) {
	want := map[Kind][]string{
		KindPassed:  {"SUCCESS", "success"},
		KindQuiet:   {"SKIPPED", "NEUTRAL"},
		KindPending: {"PENDING", "EXPECTED", "QUEUED", "IN_PROGRESS", "WAITING", "REQUESTED"},
		KindFailed:  {"FAILURE", "ERROR", "TIMED_OUT", "ACTION_REQUIRED", "STARTUP_FAILURE"},
		KindVoid:    {"CANCELLED", "STALE"},
		KindUnknown: {"", "COMPLETED", "SOMETHING_NEW"},
	}
	for k, states := range want {
		for _, s := range states {
			if got := StateKind(s); got != k {
				t.Errorf("StateKind(%q) = %v, want %v", s, got, k)
			}
		}
	}
}

// TestLatestChecks: which runs of a check still count (latestChecks' rules).
// Runs here are (id, workflow run, state); same app, workflow, event and name
// unless a case says otherwise.
func TestLatestChecks(t *testing.T) {
	type run struct {
		id, unit int64
		state    string
	}
	group := func(runs ...run) []rawCheck {
		var out []rawCheck
		for _, r := range runs {
			out = append(out, rawCheck{Check: Check{Name: "test", Workflow: "CI", Event: "pull_request", State: r.state, App: 15368}, id: r.id, unit: r.unit})
		}
		return out
	}
	states := func(cs []Check) []string {
		var out []string
		for _, c := range cs {
			out = append(out, c.State)
		}
		return out
	}
	cases := []struct {
		name string
		runs []rawCheck
		want []string
	}{
		{"a later workflow run passed after a failure", group(run{1, 10, "FAILURE"}, run{2, 20, "SUCCESS"}), []string{"SUCCESS"}},
		{"a failure, then a run that skipped it: the failure stands", group(run{1, 10, "FAILURE"}, run{2, 20, "SKIPPED"}), []string{"FAILURE"}},
		{"a failure, then a neutral run", group(run{1, 10, "ERROR"}, run{2, 20, "NEUTRAL"}), []string{"ERROR"}},
		{"a failure, then a cancelled run", group(run{1, 10, "TIMED_OUT"}, run{2, 20, "CANCELLED"}), []string{"CANCELLED", "TIMED_OUT"}},
		{"a failure, then one still running: both listed", group(run{1, 10, "FAILURE"}, run{2, 20, "IN_PROGRESS"}), []string{"IN_PROGRESS", "FAILURE"}},
		{"a failure, a pass, then a skip: passed", group(run{1, 10, "FAILURE"}, run{2, 20, "SUCCESS"}, run{3, 30, "SKIPPED"}), []string{"SUCCESS"}},
		{"a pass, a failure, then a skip: failed", group(run{1, 10, "SUCCESS"}, run{2, 20, "STARTUP_FAILURE"}, run{3, 30, "SKIPPED"}), []string{"STARTUP_FAILURE"}},
		{"a pass, then skips (#14044): passed", group(run{1, 10, "SUCCESS"}, run{2, 20, "SKIPPED"}, run{3, 30, "SKIPPED"}), []string{"SUCCESS"}},
		{"a pass, then a cancelled run: cancelled", group(run{1, 10, "SUCCESS"}, run{2, 20, "CANCELLED"}), []string{"CANCELLED"}},
		{"cancelled, then skipped (grafana): skipped", group(run{1, 10, "CANCELLED"}, run{2, 20, "SKIPPED"}), []string{"SKIPPED"}},
		{"stale, then passed", group(run{1, 10, "STALE"}, run{2, 20, "SUCCESS"}), []string{"SUCCESS"}},
		{"cancelled twice, then passed (grafana)", group(run{1, 10, "CANCELLED"}, run{2, 20, "CANCELLED"}, run{3, 30, "SUCCESS"}), []string{"SUCCESS"}},
		{"running, then a skip: still running", group(run{1, 10, "IN_PROGRESS"}, run{2, 20, "SKIPPED"}), []string{"IN_PROGRESS"}},
		{"no conclusion, then a skip: unknown", group(run{1, 10, ""}, run{2, 20, "SKIPPED"}), []string{""}},
		{"a pass, cancelled, then skipped: passed", group(run{1, 10, "SUCCESS"}, run{2, 20, "CANCELLED"}, run{3, 30, "SKIPPED"}), []string{"SUCCESS"}},
		{"two jobs of one name in one workflow run: both", group(run{1, 10, "FAILURE"}, run{2, 10, "SUCCESS"}), []string{"FAILURE", "SUCCESS"}},
		{"two jobs, then a run that skipped both: both", group(run{1, 10, "FAILURE"}, run{2, 10, "SUCCESS"}, run{3, 20, "SKIPPED"}, run{4, 20, "SKIPPED"}),
			[]string{"FAILURE", "SUCCESS"}},
		{"two jobs, then a run that passed both", group(run{1, 10, "FAILURE"}, run{2, 10, "SUCCESS"}, run{3, 20, "SUCCESS"}, run{4, 20, "SUCCESS"}),
			[]string{"SUCCESS", "SUCCESS"}},
		{"two failed jobs, then one passed and one skipped: both failures stand",
			group(run{1, 10, "FAILURE"}, run{2, 10, "FAILURE"}, run{3, 20, "SUCCESS"}, run{4, 20, "SKIPPED"}),
			[]string{"SUCCESS", "SKIPPED", "FAILURE", "FAILURE"}},
		{"the order runs come in does not matter", group(run{2, 20, "SKIPPED"}, run{1, 10, "FAILURE"}), []string{"FAILURE"}},
	}
	for _, tc := range cases {
		if got := states(latestChecks(tc.runs)); !reflect.DeepEqual(got, tc.want) {
			t.Errorf("%s: states = %q, want %q", tc.name, got, tc.want)
		}
	}

	// What makes two runs one check: app, workflow (name and file), event, name.
	r := func(id int64, name, workflow, path, event, state string, app int64) rawCheck {
		return rawCheck{Check: Check{Name: name, Workflow: workflow, WorkflowPath: path, Event: event, State: state, App: app}, id: id, unit: id}
	}
	s := func(at, name, state string) rawCheck {
		return rawCheck{Check: statusCheck(name, state), at: at}
	}
	got := latestChecks([]rawCheck{
		r(7, "test", "CI", "ci.yml", "pull_request", "FAILURE", 1),
		r(9, "test", "CI", "ci.yml", "pull_request", "SUCCESS", 1), // a later run passed
		r(8, "test", "CI", "ci.yml", "pull_request", "CANCELLED", 1),
		r(5, "lint", "CI", "ci.yml", "pull_request", "SUCCESS", 1),
		r(6, "lint", "CI", "ci.yml", "pull_request", "FAILURE", 1), // failed after a pass
		r(3, "test", "Nightly", "nightly.yml", "pull_request", "QUEUED", 1),
		r(10, "test", "CI", "ci.yml", "push", "FAILURE", 1),           // another event: its own check
		r(11, "test", "CI", "ci-2.yml", "pull_request", "FAILURE", 1), // another file of the same name: its own
		r(12, "test", "CI", "ci.yml", "pull_request", "FAILURE", 2),   // another app: its own
		s("2026-10-09T10:00:00Z", "ci/x", "SUCCESS"),
		s("2026-10-09T11:00:00Z", "ci/x", "PENDING"),
		s("2026-10-09T09:00:00Z", "ci/x", "FAILURE"),
		r(4, "ci/x", "", "", "", "SUCCESS", 3), // a run named like the status: its own check
	})
	want := []Check{ // sorted by name, workflow, event (stable): the status's event is ""
		statusCheck("ci/x", "PENDING"),
		{Name: "ci/x", State: "SUCCESS", App: 3},
		{Name: "lint", Workflow: "CI", WorkflowPath: "ci.yml", Event: "pull_request", State: "FAILURE", App: 1},
		{Name: "test", Workflow: "CI", WorkflowPath: "ci.yml", Event: "pull_request", State: "SUCCESS", App: 1},
		{Name: "test", Workflow: "CI", WorkflowPath: "ci-2.yml", Event: "pull_request", State: "FAILURE", App: 1},
		{Name: "test", Workflow: "CI", WorkflowPath: "ci.yml", Event: "pull_request", State: "FAILURE", App: 2},
		{Name: "test", Workflow: "CI", WorkflowPath: "ci.yml", Event: "push", State: "FAILURE", App: 1},
		{Name: "test", Workflow: "Nightly", WorkflowPath: "nightly.yml", Event: "pull_request", State: "QUEUED", App: 1},
	}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("latestChecks =\n%+v\nwant\n%+v", got, want)
	}
	if got := latestChecks(nil); len(got) != 0 {
		t.Errorf("latestChecks(nil) = %+v, want none", got)
	}
}

func TestParseBranchProtection(t *testing.T) {
	any := func(names ...string) []RequiredCheck {
		var out []RequiredCheck
		for _, n := range names {
			out = append(out, RequiredCheck{Name: n})
		}
		return out
	}
	ok := []struct {
		name, out string
		want      []RequiredCheck
	}{
		{"cli/cli trunk (real): protected, three builds required",
			fixture(t, "cli-trunk.branch.json"), any("build (macos-latest)", "build (ubuntu-latest)", "build (windows-latest)")},
		{"eharriett0/wt main (real): protected, required checks off", fixture(t, "wt-main.branch.json"), nil},
		{"eharriett0/awesome-o main (real): not protected", fixture(t, "awesome-o-main.branch.json"), nil},
		{"an app pin kept; a context only in the older list is any app's",
			`{"protected":true,"protection":{"required_status_checks":{"enforcement_level":"everyone","contexts":["b","a","c"],"checks":[{"context":"a","app_id":1},{"context":"c","app_id":null},{"context":"d","app_id":-1}]}}}`,
			[]RequiredCheck{{Name: "a", App: 1}, {Name: "b"}, {Name: "c"}, {Name: "d"}}},
		{"not protected: whatever else it says", `{"protected":false,"protection":{"required_status_checks":{"enforcement_level":"everyone","contexts":["a"]}}}`, nil},
		{"required checks switched off: whatever they list", `{"protected":true,"protection":{"required_status_checks":{"enforcement_level":"off","contexts":["a"],"checks":[{"context":"a"}]}}}`, nil},
	}
	for _, tc := range ok {
		got, err := parseBranchProtection(tc.out)
		if err != nil || !reflect.DeepEqual(got, tc.want) {
			t.Errorf("%s: parseBranchProtection = %+v (err %v), want %+v", tc.name, got, err, tc.want)
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
			t.Errorf("%s: parseBranchProtection(%q) = %+v, want an error", name, out, got)
		}
	}
}

func TestParseRulesets(t *testing.T) {
	ha := []RequiredCheck{
		{Name: "Check all requirements", App: 15368}, {Name: "Check hassfest", App: 15368}, {Name: "Collect information & changes data", App: 15368},
		{Name: "blocking-label-awaiting-frontend", App: 97978}, {Name: "cla-bot", App: 97978}, {Name: "code-owner-approval", App: 97978},
		{Name: "docs-missing", App: 97978}, {Name: "required-labels", App: 97978},
	}
	ok := []struct {
		name, out string
		want      Required
	}{
		{"home-assistant/core dev (real): checks pinned to two apps", fixture(t, "ha-dev.rules.json"), Required{Checks: ha}},
		{"grafana/grafana main (real): two required workflows and two checks", fixture(t, "grafana-main.rules.json"), Required{
			Checks: []RequiredCheck{{Name: "license/cla"}, {Name: "policy-bot", App: 3306711}},
			Workflows: []RequiredWorkflow{{Path: ".github/workflows/org-required-trufflehog.yml", RepoID: 499279390},
				{Path: ".github/workflows/self-zizmor.yaml", RepoID: 499279390}}}},
		{"cli/cli trunk (real): a ruleset that requires no check", fixture(t, "cli-trunk.rules.json"), Required{}},
		{"eharriett0/wt main (real): no rules", fixture(t, "wt-main.rules.json"), Required{}},
		{"pages run together", `[{"type":"required_status_checks","parameters":{"required_status_checks":[{"context":"b"}]}}]` +
			`[{"type":"required_status_checks","parameters":{"required_status_checks":[{"context":"a"},{"context":"b"},{"context":" "}]}}]`,
			Required{Checks: []RequiredCheck{{Name: "a"}, {Name: "b"}}}},
	}
	for _, tc := range ok {
		got, err := parseRulesets(tc.out)
		if err != nil || !reflect.DeepEqual(got, tc.want) {
			t.Errorf("%s: parseRulesets = %+v (err %v), want %+v", tc.name, got, err, tc.want)
		}
	}
	for name, out := range map[string]string{
		"empty":                     "",
		"null":                      "null",
		"an error body":             fixture(t, "awesome-o-main.rules-403.json"),
		"a rule with no type":       `[{"parameters":{}}]`,
		"a status rule, no list":    `[{"type":"required_status_checks"}]`,
		"a status rule, null list":  `[{"type":"required_status_checks","parameters":{"required_status_checks":null}}]`,
		"a workflows rule, no list": `[{"type":"workflows","parameters":{}}]`,
		"a workflow with no file":   `[{"type":"workflows","parameters":{"workflows":[{"repository_id":1,"path":""}]}}]`,
		"a workflow with no repo":   `[{"type":"workflows","parameters":{"workflows":[{"path":".github/workflows/x.yml"}]}}]`,
		"a list, then junk":         `[]` + "\nnope",
	} {
		if got, err := parseRulesets(out); err == nil {
			t.Errorf("%s: parseRulesets(%q) = %+v, want an error", name, out, got)
		}
	}
}

// TestUniqueChecks: names trimmed, blanks and repeats dropped (a blank
// "required" name would otherwise be missing forever), the same name pinned to
// two apps kept twice.
func TestUniqueChecks(t *testing.T) {
	got := uniqueChecks([]RequiredCheck{{Name: " b "}, {Name: ""}, {Name: "  "}, {Name: "a", App: 2}, {Name: "a"}, {Name: "b"}, {Name: "a", App: 2}})
	want := []RequiredCheck{{Name: "a"}, {Name: "a", App: 2}, {Name: "b"}}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("uniqueChecks = %+v, want %+v", got, want)
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

// TestChecksReadsThroughGh drives the reads against a fake gh, so the argv they
// send and the way a failure comes back are pinned: the PR is looked up in a
// forwarded -R repository; every read goes to the PR's host; the required
// checks are read from the PR's BASE repository, never the current one's
// {owner}/{repo}; the GraphQL read's error carries gh's own message; the rules
// read tells "unavailable" apart from "failed"; and a branch name is escaped
// into the path.
func TestChecksReadsThroughGh(t *testing.T) {
	dir := fakeGh(t, `printf '%s\n' "$@" >> "$d/argv"
case "$*" in
"auth status"*) exit 0 ;;
"pr view 7 --json number,url"*) echo "7 https://github.com/cli/cli/pull/7" ;;
"api graphql --hostname github.com --paginate"*) [ -e "$d/gql.fail" ] && { echo "GraphQL: Something went wrong" >&2; exit 1; }; cat "$d/gql" ;;
"api --hostname github.com repos/cli/cli/branches/"*) cat "$d/branch" ;;
"api --hostname github.com repos/cli/cli/rules/branches/"*) cat "$d/rules"; [ -e "$d/rules.fail" ] && { echo "gh: failed (HTTP 403)" >&2; exit 1; }; exit 0 ;;
"api --hostname github.com repositories/499279390 --jq .full_name") echo "grafana/security-github-actions" ;;
"api --hostname github.com repositories/"*) echo '{"message":"Not Found","status":"404"}'; exit 1 ;;
esac
`)
	write := func(name, s string) {
		t.Helper()
		if err := os.WriteFile(filepath.Join(dir, name), []byte(s), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	argv := func() string {
		t.Helper()
		s := readFile(t, filepath.Join(dir, "argv"))
		_ = os.Remove(filepath.Join(dir, "argv"))
		return s
	}
	write("gql", fixture(t, "cli-14629-required-missing.json"))
	got, err := PRChecks("7", "")
	if err != nil || got.Base != "trunk" || got.Repo != "cli/cli" || got.Host != "github.com" || got.Number != 7 || len(got.Checks) != 7 {
		t.Fatalf("PRChecks = %+v (err %v), want cli/cli#14629's seven checks, host and number", got, err)
	}
	if a := argv(); !strings.Contains(a, "query="+prChecksQuery+"\n") || !strings.Contains(a, "url=https://github.com/cli/cli/pull/7\n") ||
		!strings.Contains(a, "number=7\n") || strings.Contains(a, "-R\n") {
		t.Errorf("gh argv = %q, want the checks query for the PR's URL and number, no -R", a)
	}
	if _, err := PRChecks("7", "cli/cli"); err != nil {
		t.Fatal(err)
	}
	if a := argv(); !strings.Contains(a, "-R\ncli/cli\n") {
		t.Errorf("gh argv = %q, want the PR looked up in the forwarded -R repository", a)
	}
	write("gql.fail", "")
	if _, err := PRChecks("7", ""); err == nil || !strings.Contains(err.Error(), "GraphQL: Something went wrong") {
		t.Errorf("PRChecks with gh failing: err = %v, want gh's message", err)
	}

	write("branch", fixture(t, "cli-trunk.branch.json"))
	if req, err := BranchProtectionChecks("github.com", "cli/cli", "release/1.0#x"); err != nil || len(req.Checks) != 3 {
		t.Errorf("BranchProtectionChecks = %+v (err %v), want three", req, err)
	}
	if a := argv(); !strings.Contains(a, "repos/cli/cli/branches/release%2F1.0%23x\n") || strings.Contains(a, "{owner}") {
		t.Errorf("gh argv = %q, want the base repository and the branch escaped into the path", a)
	}
	if _, err := BranchProtectionChecks("github.com", "not-a-repo", "main"); err == nil {
		t.Error("BranchProtectionChecks with no owner/name: want an error")
	}

	write("rules", fixture(t, "ha-dev.rules.json"))
	if req, err := RulesetChecks("github.com", "cli/cli", "dev"); err != nil || len(req.Checks) != 8 {
		t.Errorf("RulesetChecks = %+v (err %v), want eight", req, err)
	}
	write("rules.fail", "")
	write("rules", fixture(t, "awesome-o-main.rules-403.json"))
	if _, err := RulesetChecks("github.com", "cli/cli", "main"); !errors.Is(err, ErrRulesetsUnavailable) {
		t.Errorf("RulesetChecks on a plan without rulesets: err = %v, want ErrRulesetsUnavailable", err)
	}
	write("rules", `{"message":"API rate limit exceeded for user ID 1.","status":"403"}`)
	_, err = RulesetChecks("github.com", "cli/cli", "main")
	if err == nil || errors.Is(err, ErrRulesetsUnavailable) || !strings.Contains(err.Error(), "rate limit") {
		t.Errorf("RulesetChecks rate limited: err = %v, want a failed read naming it", err)
	}

	if name, err := RepoName("github.com", 499279390); err != nil || name != "grafana/security-github-actions" {
		t.Errorf("RepoName = %q (err %v), want grafana/security-github-actions", name, err)
	}
	if name, err := RepoName("github.com", 1); err == nil || !strings.Contains(err.Error(), "Not Found") {
		t.Errorf("RepoName of a repository gh cannot see = %q (err %v), want gh's 404", name, err)
	}
}
