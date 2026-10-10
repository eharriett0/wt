package cli

import (
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"runtime"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
	"unicode/utf8"

	"github.com/eharriett0/wt/internal/merge"
)

// TestMain keeps every test in this package off the real status page (#178):
// a test that reaches merge-pr's deploy gate on a github.com PR reads
// WT_GITHUB_STATUS_URL, and this one fails at once without touching the
// network (the gate then prints its "could not check" line and goes on). A
// test that wants a page sets its own.
func TestMain(m *testing.M) {
	os.Setenv(githubStatusURLEnv, "wt-test://no-network/summary.json")
	os.Exit(m.Run())
}

// statusPage is a stand-in for GitHub's status page: it answers with the
// fixture its mode names, or, in mode "slow", not before the client gives up,
// or "503". It counts the requests it gets.
type statusPage struct {
	mu    sync.Mutex
	mode  string
	pages map[string][]byte
	hits  atomic.Int32
}

func (p *statusPage) set(mode string) {
	p.mu.Lock()
	p.mode = mode
	p.mu.Unlock()
	p.hits.Store(0)
}

func (p *statusPage) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	p.hits.Add(1)
	p.mu.Lock()
	mode := p.mode
	p.mu.Unlock()
	switch mode {
	case "slow":
		select {
		case <-r.Context().Done():
		case <-time.After(10 * time.Second):
		}
		return
	case "503":
		http.Error(w, "upstream unavailable", http.StatusServiceUnavailable)
		return
	}
	w.Header().Set("Content-Type", "application/json")
	_, _ = w.Write(p.pages[mode])
}

// newStatusPage serves the fixtures in internal/merge/testdata/githubstatus,
// read now (a test may chdir away later).
func newStatusPage(t *testing.T) (*statusPage, *httptest.Server) {
	t.Helper()
	p := &statusPage{pages: map[string][]byte{"garbage": []byte("<html>a captive portal</html>")}}
	for _, name := range []string{"operational", "degraded-performance", "partial-outage"} {
		b, err := os.ReadFile(filepath.Join("..", "merge", "testdata", "githubstatus", name+".json"))
		if err != nil {
			t.Fatal(err)
		}
		p.pages[name] = b
	}
	srv := httptest.NewServer(p)
	t.Cleanup(srv.Close)
	return p, srv
}

// closedURL is the URL of a server that is gone: nothing listens there.
func closedURL(t *testing.T) string {
	t.Helper()
	srv := httptest.NewServer(http.NotFoundHandler())
	u := srv.URL + "/api/v2/summary.json"
	srv.Close()
	return u
}

func TestReadGitHubStatus(t *testing.T) {
	p, srv := newStatusPage(t)
	cases := []struct {
		name, mode, url string
		timeout         time.Duration
		body            string // a prefix of the body; "" = an error
		err             string // in the error
		notErr          string // not in it
	}{
		{name: "200: the body", mode: "operational", body: `{"page":`},
		{name: "503: an error naming it", mode: "503", err: "HTTP 503 Service Unavailable"},
		{name: "slow: no answer within the timeout", mode: "slow", timeout: 150 * time.Millisecond, err: "no answer within 150ms"},
		{name: "nothing listening: the connection error, without the URL or address", url: closedURL(t),
			err: "connect: connection refused", notErr: "127.0.0.1"},
		{name: "not http: an error", url: "wt-test://no-network/summary.json", err: "unsupported protocol scheme"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			p.set(tc.mode)
			u, timeout := srv.URL+"/api/v2/summary.json", 2*time.Second
			if tc.url != "" {
				u = tc.url
			}
			if tc.timeout != 0 {
				timeout = tc.timeout
			}
			start := time.Now()
			body, err := readGitHubStatus(u, timeout)
			if took := time.Since(start); took > timeout+time.Second {
				t.Errorf("took %s, past the %s timeout", took, timeout)
			}
			switch {
			case tc.body != "" && (err != nil || !strings.HasPrefix(string(body), tc.body)):
				t.Errorf("readGitHubStatus = %.40q, %v; want a body starting %q", body, err, tc.body)
			case tc.body == "" && (err == nil || !strings.Contains(err.Error(), tc.err)):
				t.Errorf("readGitHubStatus err = %v, want one saying %q", err, tc.err)
			case err != nil && strings.Contains(err.Error(), u):
				t.Errorf("err = %v repeats the URL", err)
			case err != nil && tc.notErr != "" && strings.Contains(err.Error(), tc.notErr):
				t.Errorf("err = %v, want it not to say %q", err, tc.notErr)
			}
		})
	}
}

// TestReadGitHubStatus_slowBody: a page that sends its headers, then stalls,
// is cut off by the same timeout (it covers reading the body too). The stall
// ends by itself after 5s, so a read with no timeout fails instead of hanging.
func TestReadGitHubStatus_slowBody(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(`{"components":[`))
		w.(http.Flusher).Flush()
		select {
		case <-r.Context().Done():
		case <-time.After(5 * time.Second):
		}
	}))
	defer srv.Close()
	start := time.Now()
	_, err := readGitHubStatus(srv.URL, 150*time.Millisecond)
	if err == nil || !strings.Contains(err.Error(), "no answer within 150ms") {
		t.Errorf("err = %v, want a timeout", err)
	}
	if took := time.Since(start); took > 2*time.Second {
		t.Errorf("took %s", took)
	}
}

func TestReadGitHubStatus_tooLarge(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte(strings.Repeat(" ", githubStatusMaxBytes+1)))
	}))
	defer srv.Close()
	if _, err := readGitHubStatus(srv.URL, 2*time.Second); err == nil || !strings.Contains(err.Error(), "an answer over") {
		t.Errorf("err = %v, want the size cap", err)
	}
}

func TestGitHubStatusURL(t *testing.T) {
	t.Setenv(githubStatusURLEnv, "")
	if got := githubStatusURL(); got != "https://www.githubstatus.com/api/v2/summary.json" {
		t.Errorf("githubStatusURL() = %q with %s unset", got, githubStatusURLEnv)
	}
	t.Setenv(githubStatusURLEnv, " http://127.0.0.1:9/x.json ")
	if got := githubStatusURL(); got != "http://127.0.0.1:9/x.json" {
		t.Errorf("githubStatusURL() = %q", got)
	}
}

func TestActionsStatusLines(t *testing.T) {
	now := time.Date(2026, 10, 9, 14, 25, 0, 0, time.UTC)
	updated := time.Date(2026, 10, 9, 14, 2, 30, 0, time.UTC)
	const tail = "If this deploy runs on GitHub Actions, a job started now can wait for a runner and be cancelled without running: " +
		"merge once Actions recovers, or check afterwards that the deploy ran."
	cases := []struct {
		name string
		s    merge.ActionsStatus
		warn []string
		note string
	}{
		{name: "operational: nothing", s: merge.ActionsStatus{Health: merge.ActionsOperational,
			Components: []merge.ActionsComponent{{Name: "Actions", Status: "operational"}}}},
		{name: "unknown: one line", s: merge.ActionsStatus{Health: merge.ActionsUnknown, Why: "no answer within 3s"},
			note: "could not check GitHub's status for Actions (www.githubstatus.com: no answer within 3s); continuing without it."},
		{name: "the component only", s: merge.ActionsStatus{Health: merge.ActionsDegraded,
			Components: []merge.ActionsComponent{{Name: "Actions", Status: "partial_outage"}}},
			warn: []string{"GitHub's status page reports trouble with Actions (www.githubstatus.com):", "  Actions: partial outage", tail}},
		{name: "an incident: name, status, impact, last update, link", s: merge.ActionsStatus{Health: merge.ActionsDegraded,
			Components: []merge.ActionsComponent{{Name: "Actions", Status: "degraded_performance"}},
			Incidents: []merge.ActionsIncident{{Name: "Incident with Actions", Status: "investigating", Impact: "major",
				Link: "https://stspg.io/abc", Updated: updated}}},
			warn: []string{"GitHub's status page reports trouble with Actions (www.githubstatus.com):",
				"  Actions: degraded performance",
				`  incident: "Incident with Actions" (investigating, impact major), last update 2026-10-09 14:02 UTC (22 min ago) https://stspg.io/abc`,
				tail}},
		{name: "an incident while the component reads operational, or is not listed", s: merge.ActionsStatus{Health: merge.ActionsDegraded,
			Incidents: []merge.ActionsIncident{{Name: "Actions Job Delays", UpdatedText: "yesterday-ish"}}},
			warn: []string{"GitHub's status page reports trouble with Actions (www.githubstatus.com):",
				"  Actions: not listed as a component",
				`  incident: "Actions Job Delays" (status not given), last update yesterday-ish`, tail}},
		{name: "a component with no status", s: merge.ActionsStatus{Health: merge.ActionsDegraded,
			Components: []merge.ActionsComponent{{Name: "Actions"}}, Incidents: []merge.ActionsIncident{{Name: "Incident with Actions", Status: "identified"}}},
			warn: []string{"GitHub's status page reports trouble with Actions (www.githubstatus.com):",
				"  Actions: no status given", `  incident: "Incident with Actions" (identified)`, tail}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			warn, note := actionsStatusLines(tc.s, "www.githubstatus.com", now)
			if !reflect.DeepEqual(warn, tc.warn) {
				t.Errorf("warn =\n%q\nwant\n%q", warn, tc.warn)
			}
			if note != tc.note {
				t.Errorf("note = %q, want %q", note, tc.note)
			}
		})
	}
}

// TestActionsStatusLines_termSafe: what the page supplied cannot drive the
// terminal or forge a line.
func TestActionsStatusLines_termSafe(t *testing.T) {
	esc := "\x1b[2J\x1b]0;pwned\x07"
	s := merge.ActionsStatus{Health: merge.ActionsDegraded,
		Components: []merge.ActionsComponent{{Name: "Actions" + esc, Status: "partial_outage\n⚠ all clear"}},
		Incidents: []merge.ActionsIncident{{Name: "Incident with Actions" + esc, Status: "investigating\r", Impact: "major" + esc,
			Link: "https://stspg.io/x" + esc, UpdatedText: "soon\n"}}}
	warn, _ := actionsStatusLines(s, "www.githubstatus.com", time.Now())
	_, note := actionsStatusLines(merge.ActionsStatus{Health: merge.ActionsUnknown, Why: "its Actions" + esc + " component has no status"},
		"www.githubstatus.com", time.Now())
	for _, l := range append(warn, note) {
		if strings.ContainsAny(l, "\x1b\x07\n\r") {
			t.Errorf("line %q carries a control character", l)
		}
	}
	if got := warn[1]; got != "  Actions[2J]0;pwned: partial outage⚠ all clear" {
		t.Errorf("component line = %q", got)
	}
	if long := termSafe(strings.Repeat("é", termSafeMax+5)); utf8.RuneCountInString(long) != termSafeMax+1 || !strings.HasSuffix(long, "…") {
		t.Errorf("termSafe kept %d runes of a %d-rune string", utf8.RuneCountInString(long), termSafeMax+5)
	}
}

func TestAgoText(t *testing.T) {
	for _, c := range []struct {
		d    time.Duration
		want string
	}{
		{-5 * time.Minute, "just now"}, // a page clock ahead of ours
		{0, "just now"},
		{59 * time.Second, "just now"},
		{time.Minute, "1 min ago"},
		{84 * time.Minute, "1h24m ago"},
		{47*time.Hour + 59*time.Minute, "47h59m ago"},
		{72 * time.Hour, "3 days ago"},
	} {
		if got := agoText(c.d); got != c.want {
			t.Errorf("agoText(%s) = %q, want %q", c.d, got, c.want)
		}
	}
}

// TestWarnActionsStatus: the page is read for a github.com PR only, and what
// it says is printed (a warning, one line, or nothing).
func TestWarnActionsStatus(t *testing.T) {
	p, srv := newStatusPage(t)
	t.Setenv(githubStatusURLEnv, srv.URL+"/api/v2/summary.json")
	cases := []struct {
		name, host, mode string
		hits             int32
		stdout, stderr   []string
		quiet            bool // prints nothing at all
	}{
		{name: "github.com, operational: nothing", host: "github.com", mode: "operational", hits: 1, quiet: true},
		{name: "github.com, an incident: the warning", host: "github.com", mode: "degraded-performance", hits: 1,
			stderr: []string{"reports trouble with Actions (127.0.0.1:", "Actions: degraded performance", `incident: "Actions Job Delays" (investigating, impact minor), last update 2026-10-01 14:54 UTC`}},
		{name: "github.com, garbage: one line", host: "github.com", mode: "garbage", hits: 1,
			stdout: []string{"could not check GitHub's status for Actions", "its answer is not the status summary's JSON"}},
		{name: "a GHE host: no read, nothing", host: "ghe.example.com", mode: "degraded-performance", hits: 0, quiet: true},
		{name: "GHE.com: no read, nothing", host: "octocorp.ghe.com", mode: "degraded-performance", hits: 0, quiet: true},
		{name: "no host: no read, nothing", host: "", mode: "degraded-performance", hits: 0, quiet: true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			p.set(tc.mode)
			stdout, stderr := captureT(t, func() { warnActionsStatus(tc.host) })
			if got := p.hits.Load(); got != tc.hits {
				t.Errorf("the page got %d request(s), want %d", got, tc.hits)
			}
			if tc.quiet && stdout+stderr != "" {
				t.Errorf("printed %q / %q, want nothing", stdout, stderr)
			}
			for _, w := range tc.stdout {
				if !strings.Contains(stdout, w) {
					t.Errorf("stdout = %q, want it to say %q", stdout, w)
				}
			}
			for _, w := range tc.stderr {
				if !strings.Contains(stderr, w) {
					t.Errorf("stderr = %q, want it to say %q", stderr, w)
				}
			}
		})
	}
}

// TestDeployStatusHost: the PR's host wins; with none, origin's.
func TestDeployStatusHost(t *testing.T) {
	repo := t.TempDir()
	gitT(t, repo, "init", "-q", "-b", "main")
	gitT(t, repo, "remote", "add", "origin", "git@github.com:o/r.git")
	t.Chdir(repo)
	for _, c := range []struct{ pr, want string }{
		{"github.com", "github.com"},
		{"ghe.example.com", "ghe.example.com"},
		{"", "github.com"},
	} {
		if got := deployStatusHost(c.pr); got != c.want {
			t.Errorf("deployStatusHost(%q) = %q, want %q", c.pr, got, c.want)
		}
	}
}

// actionsGh is a fake gh for the deploy gate's status read (#178): open PR <n>
// in o/r on the host in the file "host", not a draft unless the file "draft"
// says true, one changed file (a.txt), green checks (gh fails the checks read
// when "rollup.fail" exists), closing nothing. Every call is appended to
// "calls" and every `pr merge` argv recorded, one per line, in "argv".
const actionsGh = `#!/bin/sh
d=$(dirname "$0")
h=$(cat "$d/host")
printf '%s\n' "$*" >> "$d/calls"
case "$*" in
*"--json number,url"*) echo "$3 https://$h/o/r/pull/$3"; exit 0 ;;
*statusCheckRollup*) [ -e "$d/rollup.fail" ] && { echo "gh: Something went wrong (HTTP 502)" >&2; exit 1; }; echo '` + greenRollup + `'; exit 0 ;;
"api --hostname $h repos/o/r/branches/"*) echo '` + unprotected + `'; exit 0 ;;
"api --hostname $h repos/o/r/rules/branches/"*) echo '[]'; exit 0 ;;
esac
case "$1 $2" in
"pr view")
	case "$*" in
	*" state "*) echo OPEN ;;
	*isDraft*) cat "$d/draft" ;;
	*messageHeadline*) echo "Deploy the widget" ;;
	*headRefName*) echo feat-x ;;
	*title*) echo "Deploy the widget" ;;
	*" body "*) echo "Refs #3" ;;
	*" url "*) echo "https://$h/o/r/pull/99999" ;;
	esac ;;
"pr diff") echo a.txt ;;
"api graphql")
	case "$*" in
	*squashMergeCommitTitle*) echo '{"title":"COMMIT_OR_PR_TITLE","message":"COMMIT_MESSAGES"}' ;;
	*parents*) echo '{"message":"Deploy the widget","parents":1}' ;;
	esac ;;
"issue view") echo OPEN ;;
"pr merge") printf '%s\n' "$@" > "$d/argv" ;;
esac
exit 0
`

// captureCombined runs f with os.Stdout and os.Stderr sent to ONE file, so the
// order of what both streams print can be checked.
func captureCombined(t *testing.T, f func()) string {
	t.Helper()
	out, err := os.Create(filepath.Join(t.TempDir(), "out"))
	if err != nil {
		t.Fatal(err)
	}
	oldOut, oldErr := os.Stdout, os.Stderr
	os.Stdout, os.Stderr = out, out
	func() {
		defer func() { os.Stdout, os.Stderr = oldOut, oldErr }()
		f()
	}()
	out.Close()
	b, _ := os.ReadFile(out.Name())
	return string(b)
}

// TestCmdMergePR_actionsStatus drives the whole merge-pr command through the
// deploy gate (#178) against a stand-in status page: it warns about an Actions
// incident right before the confirm and never blocks on it, says in one line
// when the page cannot be read (down, slow, garbage), and reads nothing for a
// merge that is not a deploy, a PR on another host, or a draft. Same safety as
// the other fake-gh tests: the fake is first and alone on PATH (with git's dir),
// the test stops unless "gh" resolves to it, and the repo has no GitHub remote.
func TestCmdMergePR_actionsStatus(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("the fake gh is a shell script")
	}
	gitBin, err := exec.LookPath("git")
	if err != nil {
		t.Skip("no git")
	}
	page, srv := newStatusPage(t)
	pageURL, downURL := srv.URL+"/api/v2/summary.json", closedURL(t)
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
	if err := os.WriteFile(filepath.Join(ghDir, "gh"), []byte(actionsGh), 0o755); err != nil {
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
	tty, realTTY := false, stdinIsTTY
	stdinIsTTY = func() bool { return tty }
	realTimeout := githubStatusTimeout
	githubStatusTimeout = 300 * time.Millisecond
	t.Cleanup(func() {
		postMergeSleep, liveChecksReads.sleep = time.Sleep, time.Sleep
		stdinIsTTY, githubStatusTimeout = realTTY, realTimeout
	})

	const (
		banner  = "merge_is_deploy — merging PR #99999 AUTO-APPLIES to prod"
		warning = "GitHub's status page reports trouble with Actions (127.0.0.1:"
		comp    = "  Actions: degraded performance"
		inc     = `  incident: "Actions Job Delays" (investigating, impact minor), last update 2026-10-01 14:54 UTC (`
		link    = "https://stspg.io/kdqxfjn5qg6q"
		proceed = "--confirm-deploy set — proceeding with the prod deploy."
		prompt  = "Type deploy to merge PR #99999 to prod"
		noCheck = "could not check GitHub's status for Actions (127.0.0.1:"
	)
	deploy := map[string]string{"WT_MERGE_IS_DEPLOY": "1"}
	pinned := []string{"--match-head-commit", fakeHead}
	cases := []struct {
		name    string
		args    []string
		env     map[string]string
		host    string // the PR's host; "" = github.com
		draft   bool
		unread  bool // gh fails the checks read
		mode    string
		url     string // the status URL; "" = the stand-in page
		tty     bool
		stdin   string
		code    int
		merged  []string // gh's argv after `pr merge 99999 --squash`; nil = no merge
		hits    int32    // requests the stand-in page got
		inOrder []string // in the combined output, in this order
		notOut  []string
	}{
		{name: "operational: nothing extra, the confirm decides", args: []string{"99999", "--keep", "--confirm-deploy"}, env: deploy,
			mode: "operational", code: 0, merged: pinned, hits: 1, inOrder: []string{banner, proceed},
			notOut: []string{"status page", "Actions", "could not check"}},
		{name: "an Actions incident: warned right before --confirm-deploy proceeds", args: []string{"99999", "--keep", "--confirm-deploy"},
			env: deploy, mode: "degraded-performance", code: 0, merged: pinned, hits: 1,
			inOrder: []string{banner, warning, comp, inc, link, "If this deploy runs on GitHub Actions", proceed}},
		{name: "an Actions incident, no terminal, no --confirm-deploy: warned, refused as before", args: []string{"99999", "--keep"},
			env: deploy, mode: "degraded-performance", code: 1, hits: 1,
			inOrder: []string{banner, warning, inc, "prod deploy not confirmed"}},
		{name: "an Actions incident at a terminal: warned before the prompt, deploy typed: merges", args: []string{"99999", "--keep"},
			env: deploy, mode: "degraded-performance", tty: true, stdin: "deploy\n", code: 0, merged: pinned, hits: 1,
			inOrder: []string{banner, warning, inc, prompt}},
		{name: "an Actions incident at a terminal, anything else typed: aborted", args: []string{"99999", "--keep"},
			env: deploy, mode: "degraded-performance", tty: true, stdin: "no\n", code: 1, hits: 1,
			inOrder: []string{warning, prompt, "aborted — deploy not confirmed."}},
		{name: "the component down, no incident yet: warned", args: []string{"99999", "--keep", "--confirm-deploy"},
			env: deploy, mode: "partial-outage", code: 0, merged: pinned, hits: 1,
			inOrder: []string{banner, warning, "  Actions: partial outage", proceed}, notOut: []string{"incident:"}},
		{name: "--dry-run during an incident: the same lines, no merge", args: []string{"99999", "--dry-run"},
			env: deploy, mode: "degraded-performance", code: 0, hits: 1,
			inOrder: []string{"--dry-run: a real merge stops at the deploy gate", warning, comp, inc, "verdict=ok checks=green"}},
		{name: "status page down: one line, merges", args: []string{"99999", "--keep", "--confirm-deploy"},
			env: deploy, url: downURL, code: 0, merged: pinned,
			inOrder: []string{banner, noCheck, "connection refused", proceed}, notOut: []string{"reports trouble"}},
		{name: "status page slow: gives up within the timeout, merges", args: []string{"99999", "--keep", "--confirm-deploy"},
			env: deploy, mode: "slow", code: 0, merged: pinned, hits: 1,
			inOrder: []string{banner, noCheck, "no answer within 300ms", proceed}, notOut: []string{"reports trouble"}},
		{name: "status page answers 503: one line, merges", args: []string{"99999", "--keep", "--confirm-deploy"},
			env: deploy, mode: "503", code: 0, merged: pinned, hits: 1,
			inOrder: []string{noCheck, "HTTP 503 Service Unavailable", proceed}},
		{name: "status page answers garbage: one line, merges", args: []string{"99999", "--keep", "--confirm-deploy"},
			env: deploy, mode: "garbage", code: 0, merged: pinned, hits: 1,
			inOrder: []string{noCheck, "not the status summary's JSON", proceed}},
		{name: "not a deploy repo: no read, no output", args: []string{"99999", "--keep"},
			mode: "degraded-performance", code: 0, merged: pinned, hits: 0, notOut: []string{"status", "Actions"}},
		{name: "a PR outside merge_is_deploy_paths: no read", args: []string{"99999", "--keep"},
			env:  map[string]string{"WT_MERGE_IS_DEPLOY": "1", "WT_MERGE_IS_DEPLOY_PATHS": "infra/**"},
			mode: "degraded-performance", code: 0, merged: pinned, hits: 0, notOut: []string{"reports trouble", "could not check"}},
		{name: "a PR on another host: no read, no output", args: []string{"99999", "--keep", "--confirm-deploy"},
			env: deploy, host: "ghe.example.com", mode: "degraded-performance", code: 0, merged: pinned, hits: 0,
			inOrder: []string{banner, proceed}, notOut: []string{"status page", "Actions", "could not check"}},
		{name: "--dry-run of a PR on another host: no read", args: []string{"99999", "--dry-run"},
			env: deploy, host: "ghe.example.com", mode: "degraded-performance", code: 0, hits: 0,
			notOut: []string{"reports trouble", "could not check"}},
		{name: "a draft: refused before the read", args: []string{"99999", "--keep", "--confirm-deploy"},
			env: deploy, draft: true, mode: "degraded-performance", code: 1, hits: 0,
			inOrder: []string{"is a DRAFT"}, notOut: []string{"reports trouble"}},
		{name: "--dry-run of a draft: no read either", args: []string{"99999", "--dry-run"},
			env: deploy, draft: true, mode: "degraded-performance", code: 0, hits: 0,
			inOrder: []string{"a real merge would REFUSE here: PR #99999 is a DRAFT"}, notOut: []string{"reports trouble"}},
		// The checks gate read no PR, and this repo's origin is a local path:
		// no host is known, so nothing is read.
		{name: "checks unreadable, --checks-ok, no forge origin: no read", args: []string{"99999", "--keep", "--confirm-deploy", "--checks-ok"},
			env: deploy, unread: true, mode: "degraded-performance", code: 0, merged: []string{}, hits: 0,
			notOut: []string{"reports trouble", "could not check"}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			for k, v := range tc.env {
				t.Setenv(k, v)
			}
			host := tc.host
			if host == "" {
				host = "github.com"
			}
			files := map[string]string{"host": host, "draft": map[bool]string{true: "true", false: "false"}[tc.draft]}
			for name, text := range files {
				if err := os.WriteFile(filepath.Join(ghDir, name), []byte(text+"\n"), 0o644); err != nil {
					t.Fatal(err)
				}
			}
			for _, name := range []string{"argv", "calls", "rollup.fail"} {
				_ = os.Remove(filepath.Join(ghDir, name))
			}
			if tc.unread {
				if err := os.WriteFile(filepath.Join(ghDir, "rollup.fail"), nil, 0o644); err != nil {
					t.Fatal(err)
				}
			}
			u := pageURL
			if tc.url != "" {
				u = tc.url
			}
			t.Setenv(githubStatusURLEnv, u)
			page.set(tc.mode)
			in, err := os.Open(writeTemp(t, tc.stdin))
			if err != nil {
				t.Fatal(err)
			}
			defer in.Close()
			oldStdin := os.Stdin
			os.Stdin = in
			defer func() { os.Stdin = oldStdin }()
			tty = tc.tty

			var code int
			start := time.Now()
			out := captureCombined(t, func() { code = cmdMergePR(tc.args) })
			if took := time.Since(start); took > 10*time.Second {
				t.Errorf("merge-pr took %s", took)
			}
			if code != tc.code {
				t.Errorf("cmdMergePR(%q) = %d, want %d\n%s", tc.args, code, tc.code, out)
			}
			if got := page.hits.Load(); got != tc.hits {
				t.Errorf("the status page got %d request(s), want %d\n%s", got, tc.hits, out)
			}
			at := 0
			for _, w := range tc.inOrder {
				i := strings.Index(out[at:], w)
				if i < 0 {
					t.Errorf("output does not say %q after byte %d\n%s", w, at, out)
					break
				}
				at += i + len(w)
			}
			for _, w := range tc.notOut {
				if strings.Contains(out, w) {
					t.Errorf("output says %q, want it not to\n%s", w, out)
				}
			}
			argv, err := os.ReadFile(filepath.Join(ghDir, "argv"))
			switch {
			case tc.merged == nil && err == nil:
				t.Errorf("gh pr merge ran (%q), want no merge", argv)
			case tc.merged != nil && err != nil:
				t.Errorf("gh pr merge never ran: %v\n%s", err, out)
			case tc.merged != nil:
				want := append([]string{"pr", "merge", "99999", "--squash"}, tc.merged...)
				if got := strings.Split(strings.TrimSuffix(string(argv), "\n"), "\n"); !reflect.DeepEqual(got, want) {
					t.Errorf("gh argv = %q, want %q", got, want)
				}
			}
		})
	}
}

// writeTemp writes content to a new temp file and returns its path.
func writeTemp(t *testing.T, content string) string {
	t.Helper()
	p := filepath.Join(t.TempDir(), "f")
	if err := os.WriteFile(p, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
	return p
}
