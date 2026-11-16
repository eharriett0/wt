package ghx

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"
)

// What gh printed during the #217 incident (the request id and timestamp
// shortened), by read.
const (
	// `gh api …`: the body on stdout, this on stderr.
	restLimitAPI = `gh: API rate limit exceeded for user ID 1. If you reach out to GitHub Support for help, please include the request ID CF90:1 and timestamp 2026-10-10 20:53:16 UTC. For more on scraping GitHub and how it may affect your rights, please review our Terms of Service (https://docs.github.com/en/site-policy/github-terms/github-terms-of-service) (HTTP 403)`
	// `gh pr diff`.
	restLimitDiff = `could not find pull request diff: HTTP 403: API rate limit exceeded for user ID 1. If you reach out to GitHub Support for help, please include the request ID CF9D:1 and timestamp 2026-10-10 20:53:22 UTC. For more on scraping GitHub and how it may affect your rights, please review our Terms of Service (https://docs.github.com/en/site-policy/github-terms/github-terms-of-service)`
	// The body `gh api` prints on stdout.
	restLimitBody = `{"message":"API rate limit exceeded for user ID 1. If you reach out to GitHub Support for help, please include the request ID CF7D:1 and timestamp 2026-10-10 20:53:03 UTC.","documentation_url":"https://docs.github.com/en/rest/using-the-rest-api/getting-started-with-the-rest-api#rate-limiting","status":"403"}`
)

func TestRateLimitKind(t *testing.T) {
	cases := []struct {
		name, text, api string
		secondary, ok   bool
	}{
		{"gh api's stderr", restLimitAPI, "REST", false, true},
		{"gh pr diff's stderr", restLimitDiff, "REST", false, true},
		{"the body's message", "API rate limit exceeded for user ID 1.", "REST", false, true},
		{"GraphQL, as gh prints it", "GraphQL: API rate limit exceeded for user ID 1.", "GraphQL", false, true},
		{"GraphQL's 'already exceeded'", "GraphQL: API rate limit already exceeded for user ID 1.", "GraphQL", false, true},
		{"GraphQL's error type", `{"errors":[{"type":"RATE_LIMITED","message":"API rate limit exceeded for user ID 1."}]}`, "GraphQL", false, true},
		{"the secondary limit", "gh: You have exceeded a secondary rate limit. Please wait a few minutes before you try again. (HTTP 403)", "REST", true, true},
		{"the secondary limit over GraphQL", "GraphQL: You have exceeded a secondary rate limit.", "GraphQL", true, true},
		{"case does not matter", "api RATE LIMIT exceeded", "REST", false, true},
		{"a not-found", "gh: Not Found (HTTP 404)", "", false, false},
		{"a suspended account's 403", "gh: Sorry. Your account was suspended. (HTTP 403)", "", false, false},
		{"a 'rate' that is no limit", "gh: rate this repository (HTTP 422)", "", false, false},
		{"no text", "", "", false, false},
	}
	for _, c := range cases {
		api, secondary, ok := rateLimitKind(c.text)
		if api != c.api || secondary != c.secondary || ok != c.ok {
			t.Errorf("%s: rateLimitKind = (%q, %v, %v), want (%q, %v, %v)", c.name, api, secondary, ok, c.api, c.secondary, c.ok)
		}
	}
}

func TestRateReset(t *testing.T) {
	now := time.Date(2026, 10, 10, 20, 53, 0, 0, time.UTC)
	reset := time.Unix(1791676380, 0) // 2026-10-10 21:13:00Z
	resp := func(status string, headers ...string) string {
		return "HTTP/2.0 " + status + "\r\n" + strings.Join(headers, "\r\n") + "\r\n\r\n" + restLimitBody + "\n"
	}
	cases := []struct {
		name string
		out  string
		want time.Time
		ok   bool
	}{
		{"a refused call: no quota left, the reset", resp("403 Forbidden", "X-Ratelimit-Limit: 5000", "X-Ratelimit-Remaining: 0", "X-Ratelimit-Reset: 1791676380", "X-Ratelimit-Resource: core"), reset, true},
		{"header names in any case, LF line ends", strings.ReplaceAll(resp("403 Forbidden", "x-ratelimit-remaining: 0", "X-RATELIMIT-RESET: 1791676380"), "\r", ""), reset, true},
		{"Retry-After wins (the secondary limit)", resp("403 Forbidden", "Retry-After: 60", "X-Ratelimit-Remaining: 0", "X-Ratelimit-Reset: 1791676380"), now.Add(time.Minute), true},
		{"Retry-After alone", resp("429 Too Many Requests", "Retry-After: 120"), now.Add(2 * time.Minute), true},
		{"quota left: the call was not refused for it", resp("200 OK", "X-Ratelimit-Remaining: 4912", "X-Ratelimit-Reset: 1791676380"), time.Time{}, false},
		{"no remaining header: not known", resp("403 Forbidden", "X-Ratelimit-Reset: 1791676380"), time.Time{}, false},
		{"an unreadable reset", resp("403 Forbidden", "X-Ratelimit-Remaining: 0", "X-Ratelimit-Reset: soon"), time.Time{}, false},
		{"a Retry-After that is a date is not read as seconds", resp("403 Forbidden", "Retry-After: Wed, 21 Oct 2026 07:28:00 GMT"), time.Time{}, false},
		{"headers in the BODY do not count", "HTTP/2.0 403 Forbidden\r\n\r\nX-Ratelimit-Remaining: 0\r\nX-Ratelimit-Reset: 1791676380\r\n", time.Time{}, false},
		{"not a response (no status line)", "X-Ratelimit-Remaining: 0\nX-Ratelimit-Reset: 1791676380\n", time.Time{}, false},
		{"nothing printed", "", time.Time{}, false},
	}
	for _, c := range cases {
		got, ok := rateReset(c.out, now)
		if !got.Equal(c.want) || ok != c.ok {
			t.Errorf("%s: rateReset = (%v, %v), want (%v, %v)", c.name, got, ok, c.want, c.ok)
		}
	}
}

func TestRateLimitError(t *testing.T) {
	now := time.Date(2026, 10, 10, 20, 53, 30, 0, time.UTC)
	cases := []struct {
		e    RateLimitError
		want string
	}{
		{RateLimitError{API: "REST", Reset: now.Add(23 * time.Minute), Now: now},
			"GitHub's REST API rate limit for this account is used up (it lifts at 21:16Z, in 23m)"},
		{RateLimitError{API: "REST", Reset: now.Add(10 * time.Second), Now: now},
			"GitHub's REST API rate limit for this account is used up (it lifts at 20:53Z, in 1m)"},
		{RateLimitError{API: "REST", Reset: now.Add(75 * time.Minute), Now: now},
			"GitHub's REST API rate limit for this account is used up (it lifts at 22:08Z, in 1h15m)"},
		{RateLimitError{API: "REST", Reset: now.Add(-time.Minute), Now: now},
			"GitHub's REST API rate limit for this account is used up (it lifted at 20:52Z)"},
		{RateLimitError{API: "REST", Now: now},
			"GitHub's REST API rate limit for this account is used up (when it lifts is not known)"},
		{RateLimitError{API: "GraphQL", Now: now},
			"GitHub's GraphQL API rate limit for this account is used up (when it lifts is not known)"},
		{RateLimitError{API: "REST", Secondary: true, Reset: now.Add(time.Minute), Now: now},
			"GitHub's secondary rate limit refused the call (too many requests in a short time) (it lifts at 20:54Z, in 1m)"},
	}
	for _, c := range cases {
		if got := c.e.Error(); got != c.want {
			t.Errorf("%+v.Error() =\n %q, want\n %q", c.e, got, c.want)
		}
	}
	wrapped := fmt.Errorf("the REST diff read failed (%w)", &RateLimitError{API: "REST"})
	if !IsRateLimit(wrapped) || IsRateLimit(errors.New("API rate limit exceeded")) || IsRateLimit(nil) {
		t.Error("IsRateLimit: want true only for a *RateLimitError, wrapped or not")
	}
}

// TestLiveProbeRateReset: the reset probe is one `gh api --include /` at the
// host, read from the refused call's stdout (gh exits 1 for it); asked once
// per host per authTTL, and asked again once that has passed, so a long-lived
// `wt mcp` never keeps a reset that is over.
func TestLiveProbeRateReset(t *testing.T) {
	dir := fakeGh(t, `echo "$*" >> "$d/calls"
printf 'HTTP/2.0 403 Forbidden\r\nX-Ratelimit-Remaining: 0\r\nX-Ratelimit-Reset: 4102444800\r\n\r\n{}\n'; exit 1
`)
	resetMu.Lock()
	saved := resetMemo
	resetMemo = map[string]resetAnswer{}
	resetMu.Unlock()
	t.Cleanup(func() { resetMu.Lock(); resetMemo = saved; resetMu.Unlock() })
	calls := func() string {
		b, _ := os.ReadFile(filepath.Join(dir, "calls"))
		return string(b)
	}
	want := time.Unix(4102444800, 0)
	for i := 0; i < 2; i++ {
		if got := liveProbeRateReset("ghe.example.com"); !got.Equal(want) {
			t.Fatalf("probe %d = %v, want %v", i, got, want)
		}
	}
	if got := liveProbeRateReset(""); !got.Equal(want) {
		t.Fatalf("probe of gh's own host = %v, want %v", got, want)
	}
	if c := calls(); c != "api --include / --hostname ghe.example.com\napi --include /\n" {
		t.Errorf("calls = %q, want one probe per host", c)
	}
	resetMu.Lock()
	a := resetMemo["ghe.example.com"]
	a.at = a.at.Add(-authTTL)
	resetMemo["ghe.example.com"] = a
	resetMu.Unlock()
	liveProbeRateReset("ghe.example.com")
	if n := strings.Count(calls(), "--hostname ghe.example.com"); n != 2 {
		t.Errorf("after authTTL the host was probed %d times in all, want 2", n)
	}
}

// stubProbe swaps the reset probe for one answering reset, and returns the
// hosts it was asked about.
func stubProbe(t *testing.T, reset time.Time) *[]string {
	t.Helper()
	var asked []string
	saved := probeRateReset
	probeRateReset = func(host string) time.Time { asked = append(asked, host); return reset }
	t.Cleanup(func() { probeRateReset = saved })
	return &asked
}

// TestRateLimitedErrors: a gh failure GitHub refused for a rate limit comes
// back as a *RateLimitError naming when it lifts (#217), from stderr and from
// a `gh api` body alike; a REST limit is probed for its reset at the host the
// call went to, a GraphQL one is not (its quota has its own clock); anything
// else keeps gh's own first line.
func TestRateLimitedErrors(t *testing.T) {
	reset := time.Now().Add(20 * time.Minute)
	asked := stubProbe(t, reset)
	exitErr := func(stderr string) error {
		t.Helper()
		dir := fakeGh(t, `cat "$d/stderr" >&2; exit 1`)
		if err := os.WriteFile(filepath.Join(dir, "stderr"), []byte(stderr), 0o644); err != nil {
			t.Fatal(err)
		}
		_, err := run("pr", "diff", "7")
		return err
	}

	var rl *RateLimitError
	if err := ghErrAt("ghe.example.com", exitErr(restLimitDiff+"\nsecond line\n")); !errors.As(err, &rl) || rl.API != "REST" || !rl.Reset.Equal(reset) {
		t.Errorf("gh pr diff refused: err = %v, want a REST *RateLimitError with the probed reset", err)
	}
	if !reflect.DeepEqual(*asked, []string{"ghe.example.com"}) {
		t.Errorf("probed %q, want the call's host", *asked)
	}
	*asked = nil
	if err := ghErrAt("", exitErr("GraphQL: API rate limit exceeded for user ID 1.")); !errors.As(err, &rl) || rl.API != "GraphQL" || !rl.Reset.IsZero() || len(*asked) != 0 {
		t.Errorf("GraphQL refused: err = %v (probed %q), want a GraphQL *RateLimitError, no probe", err, *asked)
	}
	if err := ghErrAt("", exitErr("GraphQL: Could not resolve to a PullRequest\nmore\n")); IsRateLimit(err) || err.Error() != "GraphQL: Could not resolve to a PullRequest" {
		t.Errorf("another failure: err = %v, want gh's first line", err)
	}

	*asked = nil
	if err := apiErr("github.com", restLimitBody, exitErr(restLimitAPI)); !errors.As(err, &rl) || !rl.Reset.Equal(reset) || !reflect.DeepEqual(*asked, []string{"github.com"}) {
		t.Errorf("gh api refused: err = %v (probed %q), want a *RateLimitError from the body", err, *asked)
	}
	if err := apiErr("github.com", `{"message":"Not Found","status":"404"}`, exitErr("gh: Not Found (HTTP 404)")); IsRateLimit(err) || err.Error() != "Not Found (HTTP 404)" {
		t.Errorf("gh api 404: err = %v, want the body's message and status", err)
	}
}

func TestParseFileCount(t *testing.T) {
	for _, c := range []struct {
		out  string
		want int
		ok   bool
	}{
		{"6\n", 6, true},
		{"0", 0, true},
		{" 12 ", 12, true},
		{"null", 0, false}, // #168: a placeholder is an unread answer, never 0
		{"", 0, false},
		{"-1", 0, false},
		{"six", 0, false},
	} {
		got, err := parseFileCount(c.out)
		if got != c.want || (err == nil) != c.ok {
			t.Errorf("parseFileCount(%q) = (%d, %v), want (%d, ok=%v)", c.out, got, err, c.want, c.ok)
		}
	}
}

func TestParsePRFiles(t *testing.T) {
	page := func(total int, paths ...string) string {
		q := make([]string, len(paths))
		for i, p := range paths {
			q[i] = fmt.Sprintf("%q", p)
		}
		return fmt.Sprintf(`{"total":%d,"paths":[%s]}`, total, strings.Join(q, ","))
	}
	cases := []struct {
		name string
		out  string
		want []string
		err  string
	}{
		{"one page", page(2, "a.md", "infra/café.yaml") + "\n", []string{"a.md", "infra/café.yaml"}, ""},
		{"three pages make the whole list", page(5, "a", "b") + page(5, "c", "d") + "\n" + page(5, "e"), []string{"a", "b", "c", "d", "e"}, ""},
		{"an empty PR lists nothing, and says so", page(0), []string{}, ""},
		{"a short list is not the list", page(285, "a", "b"), nil, "GraphQL listed 2 of the PR's 285 changed files"},
		{"the total moved between pages", page(3, "a", "b") + page(4, "c"), nil, "changed while it was read"},
		{"a page with no total", `{"paths":["a"]}`, nil, "no file count or no paths"},
		{"a page with null paths (not a PR)", `{"total":null,"paths":null}`, nil, "no file count or no paths"},
		{"nothing printed", "", nil, "printed no file list"},
		{"not JSON", "GraphQL: Something went wrong", nil, "unreadable file list"},
	}
	for _, c := range cases {
		got, err := parsePRFiles(c.out)
		if c.err == "" {
			if err != nil || !reflect.DeepEqual(got, c.want) {
				t.Errorf("%s: parsePRFiles = (%q, %v), want %q", c.name, got, err, c.want)
			}
			continue
		}
		if err == nil || !strings.Contains(err.Error(), c.err) {
			t.Errorf("%s: parsePRFiles = (%q, %v), want an error containing %q", c.name, got, err, c.err)
		}
	}
}

// prReadsGh answers merge-pr's diff reads: the REST diff from "diff" (or
// fails with "diff.fail"'s text), GraphQL's count from "count" (or fails with
// "count.fail"'s), and the paged file list from "files" (or "files.fail"). It
// logs each call's argv to "calls".
const prReadsGh = `echo "$*" >> "$d/calls"
fail() { [ -e "$d/$1.fail" ] && { cat "$d/$1.fail" >&2; exit 1; }; }
case "$*" in
"pr diff 7 --name-only") fail diff; cat "$d/diff" ;;
"pr view 7 --json changedFiles"*) fail count; cat "$d/count" ;;
"pr view 7 --json url"*) echo "https://github.com/o/r/pull/7" ;;
"api graphql --paginate"*) fail files; cat "$d/files" ;;
*) echo "unexpected: $*" >&2; exit 2 ;;
esac
`

// TestPRDiffReads: merge-pr's two diff reads (#217). A REST diff that reads
// is the answer, with no GraphQL call; one that fails falls back to GraphQL,
// whose own quota answered throughout the incident; both failing is an error
// naming each, and never a count of 0 or an empty list.
func TestPRDiffReads(t *testing.T) {
	stubProbe(t, time.Time{})
	dir := fakeGh(t, prReadsGh)
	set := func(name, s string) {
		t.Helper()
		if err := os.WriteFile(filepath.Join(dir, name), []byte(s), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	unset := func(name string) { _ = os.Remove(filepath.Join(dir, name)) }
	calls := func() string {
		b, _ := os.ReadFile(filepath.Join(dir, "calls"))
		unset("calls")
		return string(b)
	}

	set("diff", "a.md\ninfra/x.yaml\n")
	if n, err := PRChangedFileCount("7"); n != 2 || err != nil {
		t.Errorf("REST reads: PRChangedFileCount = (%d, %v), want 2", n, err)
	}
	if files, err := PRChangedFiles("7"); !reflect.DeepEqual(files, []string{"a.md", "infra/x.yaml"}) || err != nil {
		t.Errorf("REST reads: PRChangedFiles = (%q, %v)", files, err)
	}
	if c := calls(); strings.Contains(c, "changedFiles") || strings.Contains(c, "graphql") {
		t.Errorf("REST read fine, but GraphQL was asked: %q", c)
	}
	set("diff", "")
	if n, err := PRChangedFileCount("7"); n != 0 || err != nil {
		t.Errorf("a REST diff that reads empty IS empty: PRChangedFileCount = (%d, %v), want (0, nil)", n, err)
	}

	set("diff.fail", restLimitDiff)
	set("count", "6\n")
	set("files", `{"total":2,"paths":["a.md"]}`+"\n"+`{"total":2,"paths":["infra/x.yaml"]}`+"\n")
	if n, err := PRChangedFileCount("7"); n != 6 || err != nil {
		t.Errorf("REST refused: PRChangedFileCount = (%d, %v), want GraphQL's 6", n, err)
	}
	if files, err := PRChangedFiles("7"); !reflect.DeepEqual(files, []string{"a.md", "infra/x.yaml"}) || err != nil {
		t.Errorf("REST refused: PRChangedFiles = (%q, %v), want GraphQL's two", files, err)
	}
	if c := calls(); !strings.Contains(c, "query="+prFilesQuery) || !strings.Contains(c, "url=https://github.com/o/r/pull/7") {
		t.Errorf("GraphQL file list: calls = %q, want the paged query for the PR's URL", c)
	}

	set("count", "null\n")
	set("files", `{"total":285,"paths":["a.md"]}`)
	n, err := PRChangedFileCount("7")
	if err == nil || n != 0 || !IsRateLimit(err) || !strings.Contains(err.Error(), "REST API rate limit") || !strings.Contains(err.Error(), `got "null"`) {
		t.Errorf("both reads fail: PRChangedFileCount = (%d, %v), want an error naming the rate limit and GraphQL's answer", n, err)
	}
	files, err := PRChangedFiles("7")
	if err == nil || files != nil || !IsRateLimit(err) || !strings.Contains(err.Error(), "1 of the PR's 285") {
		t.Errorf("both reads fail: PRChangedFiles = (%q, %v), want an error, never part of the list", files, err)
	}
	set("count.fail", "GraphQL: API rate limit exceeded for user ID 1.")
	if _, err := PRChangedFileCount("7"); err == nil || !strings.Contains(err.Error(), "GraphQL API rate limit") {
		t.Errorf("both quotas used up: err = %v, want both named", err)
	}
}

func TestSignedIn(t *testing.T) {
	ok := HostAuth{Host: "github.com", State: HostAuthOK}
	limited := HostAuth{Host: "github.com", State: HostAuthRateLimited, Error: "HTTP 403: API rate limit exceeded for user ID 1."}
	gheLimited := HostAuth{Host: "ghe.example.com", State: HostAuthRateLimited}
	failed := HostAuth{Host: "ghe.example.com", State: HostAuthFailed}
	unknown := HostAuth{Host: "github.com", State: HostAuthUnknown}
	cases := []struct {
		name string
		st   AuthStatus
		want bool
	}{
		{"authed", AuthStatus{OK: true}, true},
		{"rate limited: the token works", AuthStatus{Parsed: true, Hosts: []HostAuth{limited}}, true},
		{"one host fine, one rate limited", AuthStatus{Parsed: true, Hosts: []HostAuth{gheLimited, ok}}, true},
		{"rate limited beside a dead login", AuthStatus{Parsed: true, Hosts: []HostAuth{limited, failed}}, false},
		{"unknown is never signed in", AuthStatus{Parsed: true, Hosts: []HostAuth{unknown}}, false},
		{"no host", AuthStatus{Parsed: true}, false},
		{"unreadable", AuthStatus{Hosts: []HostAuth{limited}}, false},
	}
	for _, c := range cases {
		if got := signedIn(c.st); got != c.want {
			t.Errorf("%s: signedIn = %v, want %v", c.name, got, c.want)
		}
		// ⚠ AuthedFor stays strict: liveness trusts "no PR" only from it.
		if c.st.OK != authOK(false, c.st.Hosts, c.st.Parsed) && !c.st.OK {
			t.Errorf("%s: a rate-limited host made authOK true", c.name)
		}
	}
}

func TestAuthProblem(t *testing.T) {
	cases := []struct {
		st   AuthStatus
		want string
	}{
		{AuthStatus{}, "gh's login could not be verified: wt could not read `gh auth status` (run it to see why)"},
		{AuthStatus{Parsed: true}, "gh is not logged in (`gh auth login`)"},
		{AuthStatus{Parsed: true, Hosts: []HostAuth{{Host: "github.com", State: HostAuthFailed, Error: "HTTP 401: Bad credentials (https://api.github.com/)"}}},
			"gh cannot be used: github.com failed to log in"},
		{AuthStatus{Parsed: true, Hosts: []HostAuth{{Host: "github.com", State: HostAuthFailed}}},
			"gh cannot be used: github.com failed gh's login check, which gh before 2.81 also reports for a host it cannot reach or a used-up rate limit (gh 2.81+ says which)"},
		{AuthStatus{Parsed: true, Hosts: []HostAuth{{Host: "github.com", State: HostAuthRateLimited}, {Host: "ghe.example.com", State: HostAuthUnreachable}}},
			"gh cannot be used: couldn't reach ghe.example.com"},
		{AuthStatus{Parsed: true, Hosts: []HostAuth{{Host: "github.com", State: HostAuthUnknown, Error: "HTTP 503: down\nmore"}}},
			"gh cannot be used: github.com could not be checked (gh: HTTP 503: down)"},
	}
	for _, c := range cases {
		if got := authProblem(c.st); got != c.want {
			t.Errorf("authProblem(%+v) =\n %q, want\n %q", c.st, got, c.want)
		}
	}
	if got := (HostAuth{Host: "github.com", State: HostAuthRateLimited}).Phrase(); !strings.Contains(got, "rate limit") || !strings.Contains(got, "the token works") {
		t.Errorf("rate-limited Phrase = %q, want it named, and the login not doubted", got)
	}
}
