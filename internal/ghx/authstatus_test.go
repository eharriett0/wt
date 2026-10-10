package ghx

import (
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"runtime"
	"strconv"
	"strings"
	"testing"
)

// ghTwoHosts is `gh auth status` on the #183 reporter's machine: github.com
// logged in, a second Enterprise host unreachable. Byte-for-byte what gh 2.68.1
// and 2.102.0 print (pkg/cmd/auth/status), all of it to STDERR with exit 1,
// because one account failed.
const ghTwoHosts = `github.com
  ✓ Logged in to github.com account octo (keyring)
  - Active account: true
  - Git operations protocol: ssh
  - Token: gho_************************************
  - Token scopes: 'gist', 'read:org', 'repo', 'workflow'

ghe.example.com
  X Timeout trying to log in to ghe.example.com account octo (keyring)
  - Active account: true
`

// TestParseAuthStatus pins the per-host reading of `gh auth status` (#183). With
// no forge host to scope to, its exit code is an aggregate over every host, so a
// github.com login that is fine read as "NOT authenticated" when any other host
// failed. The rows that matter most: a timeout is NOT a failed login, the ACTIVE
// account decides a host, and output wt cannot read is parsed=false (it proves
// nothing), never a host that failed.
func TestParseAuthStatus(t *testing.T) {
	ok := func(h string) HostAuth { return HostAuth{Host: h, State: HostAuthOK} }
	failed := func(h string) HostAuth { return HostAuth{Host: h, State: HostAuthFailed} }
	timeout := func(h string) HostAuth { return HostAuth{Host: h, State: HostAuthTimeout} }

	cases := []struct {
		name   string
		out    string
		want   []HostAuth
		parsed bool
	}{
		{"#183: github.com ok, the Enterprise host times out", ghTwoHosts,
			[]HostAuth{ok("github.com"), timeout("ghe.example.com")}, true},
		{"both hosts ok", `github.com
  ✓ Logged in to github.com account octo (keyring)
  - Active account: true

ghe.example.com
  ✓ Logged in to ghe.example.com account octo (keyring)
  - Active account: true
`, []HostAuth{ok("github.com"), ok("ghe.example.com")}, true},
		{"none ok: both fail to log in", `github.com
  X Failed to log in to github.com account octo (keyring)
  - Active account: true
  - The token in keyring is invalid.
  - To re-authenticate, run: gh auth login -h github.com
  - To forget about this account, run: gh auth logout -h github.com -u octo

ghe.example.com
  X Failed to log in to ghe.example.com account octo (keyring)
  - Active account: true
  - The token in keyring is invalid.
`, []HostAuth{failed("github.com"), failed("ghe.example.com")}, true},
		{"tokens from the environment print 'using token'", `github.com
  X Failed to log in to github.com using token (GH_TOKEN)
  - Active account: true
  - The token in GH_TOKEN is invalid.

ghe.example.com
  X Timeout trying to log in to ghe.example.com using token (GH_ENTERPRISE_TOKEN)
  - Active account: true
`, []HostAuth{failed("github.com"), timeout("ghe.example.com")}, true},
		{"an inactive account with a dead token does not fail the host", `github.com
  ✓ Logged in to github.com account octo (keyring)
  - Active account: true
  - Git operations protocol: https

  X Failed to log in to github.com account old-octo (keyring)
  - Active account: false
  - The token in keyring is invalid.
`, []HostAuth{ok("github.com")}, true},
		{"the active account failing fails the host, whatever an inactive one says", `github.com
  X Failed to log in to github.com account octo (keyring)
  - Active account: true
  - The token in keyring is invalid.

  ✓ Logged in to github.com account other-octo (keyring)
  - Active account: false
`, []HostAuth{failed("github.com")}, true},
		{"the Active marker outranks list order", `github.com
  ✓ Logged in to github.com account other-octo (keyring)
  - Active account: false

  X Timeout trying to log in to github.com account octo (keyring)
  - Active account: true
`, []HostAuth{timeout("github.com")}, true},
		{"gh before 2.40: one entry per host, no Active marker", `github.com
  ✓ Logged in to github.com as octo (oauth_token)
  ✓ Git operations for github.com configured to use https protocol.
  ✓ Token: *******************

ghe.example.com
  X ghe.example.com: timeout trying to log in

old.example.com
  X old.example.com: authentication failed
  - The old.example.com token in oauth_token is no longer valid.
`, []HostAuth{ok("github.com"), timeout("ghe.example.com"), failed("old.example.com")}, true},
		{"a host or login NAMED like a timeout is still a failed login", `timeout.example.com
  X Failed to log in to timeout.example.com account timeout-bot (keyring)
  - Active account: true
  - The token in keyring is invalid.
`, []HostAuth{failed("timeout.example.com")}, true},
		{"a missing-scopes warning under a ✓ entry does not fail the host", `github.com
  ✓ Logged in to github.com account octo (keyring)
  - Active account: true
  - Token scopes: 'repo'
  ! Missing required token scopes: 'read:org'
  - To request missing scopes, run: gh auth refresh -h github.com
`, []HostAuth{ok("github.com")}, true},
		{"colored output (GH_FORCE_TTY) reads the same",
			"\x1b[0;1;39mgithub.com\x1b[0m\n  \x1b[0;32m✓\x1b[0m Logged in to github.com account \x1b[0;1;39mocto\x1b[0m (keyring)\n  - Active account: \x1b[0;1;39mtrue\x1b[0m\n\n\x1b[0;1;39mghe.example.com\x1b[0m\n  \x1b[0;31mX\x1b[0m Timeout trying to log in to ghe.example.com account \x1b[0;1;39mocto\x1b[0m (keyring)\n  - Active account: \x1b[0;1;39mtrue\x1b[0m\n",
			[]HostAuth{ok("github.com"), timeout("ghe.example.com")}, true},
		{"CRLF line endings", strings.ReplaceAll(ghTwoHosts, "\n", "\r\n"),
			[]HostAuth{ok("github.com"), timeout("ghe.example.com")}, true},
		{"noise that is not a section is skipped", "A new release of gh is available: 2.68.1 → 2.102.0\nhttps://github.com/cli/cli/releases/tag/v2.102.0\n  ✓ stray indented line before any host\n" + ghTwoHosts,
			[]HostAuth{ok("github.com"), timeout("ghe.example.com")}, true},
		{"a section with no account line wt can read is unknown, never failed", `ghe.example.com
  ? some line a future gh might print
`, []HostAuth{{Host: "ghe.example.com", State: HostAuthUnknown}}, true},
		{"no host at all is readable, and definitively none",
			"You are not logged into any GitHub hosts. To log in, run: gh auth login\n", nil, true},
		{"empty output proves nothing", "", nil, false},
		{"an error that is not a host section proves nothing",
			"failed to read configuration: open /home/u/.config/gh/config.yml: permission denied\n", nil, false},
	}
	for _, c := range cases {
		got, parsed := parseAuthStatus(c.out)
		if !reflect.DeepEqual(got, c.want) || parsed != c.parsed {
			t.Errorf("%s:\n parseAuthStatus = (%+v, %v)\n want             (%+v, %v)", c.name, got, parsed, c.want, c.parsed)
		}
	}
}

// fakeGH puts a gh shim first on PATH (only /usr/bin and /bin after it, and the
// test refuses to run unless gh resolves to it), empties the auth memo, and
// restores both afterwards. The shim logs each run's arguments. Its `--json`
// form answers per jsonMode; its human form prints text to STDERR and exits 1,
// gh's shape once any account fails.
//   - "unsupported": gh before 2.81, verbatim from 2.68.1: "unknown flag: --json"
//   - "json": gh 2.81+: jsonOut on stdout, an update notice on stderr, exit 0
//   - "fatal": gh failing for another reason (its config unreadable), exit 1
func fakeGH(t *testing.T, jsonMode, jsonOut, text string) (runs func() []string) {
	t.Helper()
	if runtime.GOOS == "windows" {
		t.Skip("shell-script gh shim")
	}
	dir := t.TempDir()
	write := func(name, body string, mode os.FileMode) string {
		p := filepath.Join(dir, name)
		if err := os.WriteFile(p, []byte(body), mode); err != nil {
			t.Fatal(err)
		}
		return p
	}
	textFile := write("text.txt", text, 0o644)
	jsonFile := write("json.txt", jsonOut, 0o644)
	calls := filepath.Join(dir, "calls")
	var jsonForm string
	switch jsonMode {
	case "unsupported":
		jsonForm = `printf 'unknown flag: --json\n\nUsage:  gh auth status [flags]\n\nFlags:\n  -a, --active            Display the active account only\n' >&2; exit 1`
	case "json":
		jsonForm = `cat '` + jsonFile + `'; echo 'A new release of gh is available: 2.81.0 -> 2.102.0' >&2; exit 0`
	case "fatal":
		jsonForm = `echo 'failed to read configuration: open /home/u/.config/gh/config.yml: permission denied' >&2; exit 1`
	default:
		t.Fatalf("fakeGH: unknown jsonMode %q", jsonMode)
	}
	write("gh", "#!/bin/sh\necho \"$*\" >> '"+calls+"'\ncase \" $* \" in *\" --json \"*) "+jsonForm+" ;; esac\ncat '"+textFile+"' >&2\nexit 1\n", 0o755)
	t.Setenv("PATH", dir+string(os.PathListSeparator)+"/usr/bin"+string(os.PathListSeparator)+"/bin")
	if p, err := exec.LookPath("gh"); err != nil || p != filepath.Join(dir, "gh") {
		t.Fatalf("gh resolves to %q (%v), not the shim", p, err)
	}

	authMu.Lock()
	savedMemo, savedNoJSON := authMemo, authNoJSON
	authMemo, authNoJSON = map[string]authAnswer{}, false
	authMu.Unlock()
	t.Cleanup(func() {
		authMu.Lock()
		authMemo, authNoJSON = savedMemo, savedNoJSON
		authMu.Unlock()
	})
	return func() []string {
		b, err := os.ReadFile(calls)
		if os.IsNotExist(err) {
			return nil
		}
		if err != nil {
			t.Fatal(err)
		}
		return strings.Split(strings.TrimSpace(string(b)), "\n")
	}
}

// TestAuthStatusForReadsBothStreams pins the gh plumbing under the parsers for a
// gh before 2.81 (#183/#203). It refuses the JSON form ("unknown flag: --json"),
// so the same check falls back to the human form, whose host sections gh writes
// to STDERR once any account fails: a stdout-only capture would read the #183
// two-host case as "could not be verified". The boolean and the per-host
// breakdown come from that one memoized check (#172), and the refusal is
// remembered: the next host asks the human form directly.
func TestAuthStatusForReadsBothStreams(t *testing.T) {
	runs := fakeGH(t, "unsupported", "", ghTwoHosts)

	if AuthedFor("") {
		t.Fatal(`AuthedFor("") = true with the Enterprise host timing out`)
	}
	got := AuthStatusFor("")
	want := []HostAuth{{Host: "github.com", State: HostAuthOK}, {Host: "ghe.example.com", State: HostAuthTimeout}}
	if got.OK || !got.Parsed || !reflect.DeepEqual(got.Hosts, want) {
		t.Errorf("AuthStatusFor(\"\") = %+v, want OK=false Parsed=true Hosts=%+v (the sections are on stderr)", got, want)
	}
	wantRuns := []string{"auth status --json hosts --active", "auth status"}
	if r := runs(); !reflect.DeepEqual(r, wantRuns) {
		t.Errorf("gh runs for AuthedFor + AuthStatusFor = %q, want %q (one JSON probe, one human check, memoized: #172)", r, wantRuns)
	}

	AuthedFor("github.com")
	wantRuns = append(wantRuns, "auth status --hostname github.com")
	if r := runs(); !reflect.DeepEqual(r, wantRuns) {
		t.Errorf("gh runs after a second host = %q, want %q (this gh has no JSON form: don't probe again)", r, wantRuns)
	}
}

// TestAuthCheckJSONForm: gh 2.81+ answers in ONE run, read from stdout alone (gh
// writes notes such as an update notice to stderr even then), and a host gh
// could not reach reads as unreachable, not as the dead token gh's human output
// calls it (#203). `--active`: only the account gh uses there is checked.
func TestAuthCheckJSONForm(t *testing.T) {
	doc := ghDoc(map[string][]ghEntry{"github.com": {errAcct("github.com", "octo", true, "error", errDNS)}})
	runs := fakeGH(t, "json", doc, "the human form must not be asked\n")

	if AuthedFor("github.com") {
		t.Error(`AuthedFor("github.com") = true for a host gh could not reach`)
	}
	st := AuthStatusFor("github.com")
	want := []HostAuth{{Host: "github.com", State: HostAuthUnreachable, Error: errDNS}}
	if st.OK || !st.Parsed || st.Host != "github.com" || !reflect.DeepEqual(st.Hosts, want) {
		t.Errorf("AuthStatusFor = %+v, want OK=false Parsed=true Hosts=%+v", st, want)
	}
	if r := runs(); !reflect.DeepEqual(r, []string{"auth status --json hosts --active --hostname github.com"}) {
		t.Errorf("gh runs = %q, want the one JSON check", r)
	}
	st.Hosts[0].State = HostAuthOK // a caller editing its copy must not edit the memo
	if again := AuthStatusFor("github.com"); !reflect.DeepEqual(again.Hosts, want) {
		t.Errorf("after a caller edited its hosts, AuthStatusFor = %+v, want %+v (the memo is shared)", again.Hosts, want)
	}
}

// TestAuthCheckFallsBackOnce: a gh failure that is not a refusal of the JSON
// form (its config unreadable, say) falls back to the human form for that check
// and is NOT remembered, so the next check tries the JSON form again.
func TestAuthCheckFallsBackOnce(t *testing.T) {
	runs := fakeGH(t, "fatal", "", ghTwoHosts)

	st := AuthStatusFor("")
	if !st.Parsed || len(st.Hosts) != 2 {
		t.Errorf("AuthStatusFor(\"\") = %+v, want the human form's two hosts", st)
	}
	AuthStatusFor("github.com")
	want := []string{
		"auth status --json hosts --active", "auth status",
		"auth status --json hosts --active --hostname github.com", "auth status --hostname github.com",
	}
	if r := runs(); !reflect.DeepEqual(r, want) {
		t.Errorf("gh runs = %q, want %q", r, want)
	}
}

// ghEntry mirrors gh's authEntry (pkg/cmd/auth/status, identical in 2.81.0,
// which added `--json`, and 2.102.0), tags and all, so a fixture marshalled from
// it is what gh's encoder writes for `gh auth status --json hosts`.
type ghEntry struct {
	State       string `json:"state"`
	Error       string `json:"error,omitempty"`
	Active      bool   `json:"active"`
	Host        string `json:"host"`
	Login       string `json:"login"`
	TokenSource string `json:"tokenSource"`
	Token       string `json:"token,omitempty"`
	Scopes      string `json:"scopes,omitempty"`
	GitProtocol string `json:"gitProtocol"`
}

// ghDoc is gh's authStatus document, {"hosts": {"<host>": [authEntry, …]}}.
func ghDoc(hosts map[string][]ghEntry) string {
	b, err := json.Marshal(map[string]any{"hosts": hosts})
	if err != nil {
		panic(err)
	}
	return string(b) + "\n"
}

func okAcct(host, login string, active bool) ghEntry {
	return ghEntry{State: "success", Active: active, Host: host, Login: login, TokenSource: "keyring",
		Scopes: "gist, read:org, repo, workflow", GitProtocol: "ssh"}
}

func errAcct(host, login string, active bool, state, err string) ghEntry {
	return ghEntry{State: state, Error: err, Active: active, Host: host, Login: login, TokenSource: "keyring", GitProtocol: "ssh"}
}

// gh's error texts, as `--json` records them: built the way gh 2.81+ builds them
// (net/http's *url.Error, go-gh's HTTPError, cli/shurcooL-graphql's non-200).
const (
	errDNS     = `Get "https://api.github.com/": dial tcp: lookup api.github.com: no such host`
	errGHEDNS  = `Get "https://ghe.example.com/api/v3/": dial tcp: lookup ghe.example.com: no such host`
	errRefused = `Get "https://ghe.example.com/api/v3/": dial tcp 10.0.0.5:443: connect: connection refused`
	err401     = `HTTP 401: Bad credentials (https://api.github.com/)`
	err401Env  = `non-200 OK status code: 401 Unauthorized body: "{\"message\":\"Bad credentials\",\"documentation_url\":\"https://docs.github.com/graphql\",\"status\":\"401\"}"`
	errTimeout = `Get "https://api.github.com/": context deadline exceeded` // gh's own status_test fixture
	err403     = `HTTP 403: API rate limit exceeded for user ID 1. (https://api.github.com/)`
)

// TestParseAuthStatusJSON pins the reading of `gh auth status --json hosts`
// (#203). Its point: gh's human output says "Failed to log in … token invalid"
// for every error, a host it never reached included, so offline doctor said "NOT
// authenticated"; the JSON keeps gh's error, and only a 401 is a dead token. The
// ACTIVE account decides a host, as in the human form.
func TestParseAuthStatusJSON(t *testing.T) {
	ok := func(h string) HostAuth { return HostAuth{Host: h, State: HostAuthOK} }
	with := func(h, state, err string) HostAuth { return HostAuth{Host: h, State: state, Error: err} }
	d := func(s string) string { return "\x1b[1;37m" + s + "\x1b[m" } // jsoncolor's delimiters
	k := func(s string) string { return "\x1b[1;34m" + strconv.Quote(s) + "\x1b[m" + d(":") + " " }
	v := func(s string) string { return "\x1b[32m" + strconv.Quote(s) + "\x1b[m" }
	colored := d("{") + "\n  " + k("hosts") + d("{") + "\n    " + k("github.com") + d("[") + "\n      " + d("{") + "\n        " +
		k("state") + v("error") + d(",") + "\n        " + k("error") + v(errDNS) + d(",") + "\n        " +
		k("active") + "\x1b[33mtrue\x1b[m" + "\n      " + d("}") + "\n    " + d("]") + "\n  " + d("}") + "\n" + d("}") + "\n"

	cases := []struct {
		name   string
		out    string
		want   []HostAuth
		parsed bool
	}{
		{"active ok, an inactive account's token is dead: the host works",
			ghDoc(map[string][]ghEntry{"github.com": {okAcct("github.com", "octo", true), errAcct("github.com", "old-octo", false, "error", err401)}}),
			[]HostAuth{ok("github.com")}, true},
		{"the active account decides, wherever it is listed",
			ghDoc(map[string][]ghEntry{"github.com": {okAcct("github.com", "other-octo", false), errAcct("github.com", "octo", true, "error", err401)}}),
			[]HostAuth{with("github.com", HostAuthFailed, err401)}, true},
		{"#203: offline, the active account's host does not resolve → unreachable, NOT a dead token",
			ghDoc(map[string][]ghEntry{
				"github.com":      {errAcct("github.com", "octo", true, "error", errDNS)},
				"ghe.example.com": {errAcct("ghe.example.com", "octo", true, "error", errGHEDNS)}}),
			[]HostAuth{with("ghe.example.com", HostAuthUnreachable, errGHEDNS), with("github.com", HostAuthUnreachable, errDNS)}, true},
		{"connection refused → unreachable",
			ghDoc(map[string][]ghEntry{"ghe.example.com": {errAcct("ghe.example.com", "octo", true, "error", errRefused)}}),
			[]HostAuth{with("ghe.example.com", HostAuthUnreachable, errRefused)}, true},
		{"a timeout: gh's own fixture, verbatim",
			`{"hosts":{"github.com":[{"state":"timeout","error":"Get \"https://api.github.com/\": context deadline exceeded","active":true,"host":"github.com","login":"monalisa","tokenSource":"GH_CONFIG_DIR/hosts.yml","gitProtocol":"https"}]}}` + "\n",
			[]HostAuth{with("github.com", HostAuthTimeout, errTimeout)}, true},
		{"an invalid token: the host answered 401",
			ghDoc(map[string][]ghEntry{"github.com": {errAcct("github.com", "octo", true, "error", err401)}}),
			[]HostAuth{with("github.com", HostAuthFailed, err401)}, true},
		{"an invalid GH_TOKEN: gh's GraphQL read says 401 in its own words",
			ghDoc(map[string][]ghEntry{"github.com": {{State: "error", Error: err401Env, Active: true, Host: "github.com", TokenSource: "GH_TOKEN", GitProtocol: "https"}}}),
			[]HostAuth{with("github.com", HostAuthFailed, err401Env)}, true},
		{"rate limited: the host answered, but not that the token is invalid",
			ghDoc(map[string][]ghEntry{"github.com": {errAcct("github.com", "octo", true, "error", err403)}}),
			[]HostAuth{with("github.com", HostAuthUnknown, err403)}, true},
		{"logged in nowhere, or not on the host asked about: definitively no host (gh's fixture)",
			"{\"hosts\":{}}\n", nil, true},
		{"gh's own all-valid fixture: hosts by name, the inactive account ignored",
			`{"hosts":{"ghe.io":[{"state":"success","active":true,"host":"ghe.io","login":"monalisa-ghe","tokenSource":"GH_CONFIG_DIR/hosts.yml","scopes":"repo, read:org","gitProtocol":"https"}],"github.com":[{"state":"success","active":true,"host":"github.com","login":"monalisa2","tokenSource":"GH_CONFIG_DIR/hosts.yml","scopes":"repo, read:org","gitProtocol":"https"},{"state":"success","active":false,"host":"github.com","login":"monalisa","tokenSource":"GH_CONFIG_DIR/hosts.yml","scopes":"repo, read:org","gitProtocol":"https"}]}}` + "\n",
			[]HostAuth{ok("ghe.io"), ok("github.com")}, true},
		{"hosts come in name order, whatever the document's",
			`{"hosts":{"zeta.example.com":[{"state":"success","active":true}],"alpha.example.com":[{"state":"timeout","error":"x","active":true}]}}`,
			[]HostAuth{with("alpha.example.com", HostAuthTimeout, "x"), ok("zeta.example.com")}, true},
		{"no account marked active is not gh's answer (gh always marks one): unknown, never ok",
			ghDoc(map[string][]ghEntry{"github.com": {okAcct("github.com", "octo", false), okAcct("github.com", "other-octo", false)}}),
			[]HostAuth{with("github.com", HostAuthUnknown, "")}, true},
		{"a state wt does not know is unknown, never ok",
			ghDoc(map[string][]ghEntry{"github.com": {errAcct("github.com", "octo", true, "expired", "token expired")}}),
			[]HostAuth{with("github.com", HostAuthUnknown, "token expired")}, true},
		{"an error with no text is unknown, not a dead token",
			ghDoc(map[string][]ghEntry{"github.com": {errAcct("github.com", "octo", true, "error", "")}}),
			[]HostAuth{with("github.com", HostAuthUnknown, "")}, true},
		{"a host with no accounts is unknown", `{"hosts":{"github.com":[]}}`,
			[]HostAuth{with("github.com", HostAuthUnknown, "")}, true},
		{"colored output (GH_FORCE_TTY) reads the same", colored,
			[]HostAuth{with("github.com", HostAuthUnreachable, errDNS)}, true},
		{"gh before 2.81: the human form is not this document", ghTwoHosts, nil, false},
		{"gh's flag error is not this document", "unknown flag: --json\n", nil, false},
		{"JSON without hosts", "{}\n", nil, false},
		{"hosts: null", `{"hosts":null}`, nil, false},
		{"empty output", "", nil, false},
	}
	for _, c := range cases {
		got, parsed := parseAuthStatusJSON(c.out)
		if !reflect.DeepEqual(got, c.want) || parsed != c.parsed {
			t.Errorf("%s:\n parseAuthStatusJSON = (%+v, %v)\n want                 (%+v, %v)", c.name, got, parsed, c.want, c.parsed)
		}
	}
}

// TestClassifyAuthError pins how gh's error text decides an account in state
// "error" (#203): only a 401 says the token is invalid; no HTTP answer at all is
// an unreachable host; anything else proves nothing about the token.
func TestClassifyAuthError(t *testing.T) {
	cases := []struct{ name, msg, want string }{
		{"REST 401 (a token from gh's store)", err401, HostAuthFailed},
		{"GraphQL 401 (a token from GH_TOKEN)", err401Env, HostAuthFailed},
		{"a 401 with no message (a non-JSON body)", "HTTP 401 (https://ghe.example.com/api/v3/)", HostAuthFailed},
		{"DNS failure", errDNS, HostAuthUnreachable},
		{"connection refused", errRefused, HostAuthUnreachable},
		{"TLS failure", `Get "https://ghe.example.com/api/v3/": tls: failed to verify certificate: x509: certificate signed by unknown authority`, HostAuthUnreachable},
		{"connection dropped", `Get "https://ghe.example.com/api/v3/": EOF`, HostAuthUnreachable},
		{"GH_TOKEN's GraphQL read, no network", `Post "https://api.github.com/graphql": dial tcp: lookup api.github.com: no such host`, HostAuthUnreachable},
		{"a network failure not wrapped as a URL error", "dial tcp: lookup ghe.example.com on 10.0.0.1:53: server misbehaving", HostAuthUnreachable},
		{"surrounding space", "  " + errDNS + "\n", HostAuthUnreachable},
		{"a proxy's 502 naming a refused connection: an HTTP answer, so unproven", "HTTP 502: connection refused (https://ghe.example.com/api/v3/)", HostAuthUnknown},
		{"rate limited: the token may be fine", err403, HostAuthUnknown},
		{"a 5xx", "HTTP 503: 503 Service Unavailable (https://ghe.example.com/api/v3/)", HostAuthUnknown},
		{"gh's own 'bad token' fixture is a 400", "HTTP 400 (https://ghe.io/api/v3/)", HostAuthUnknown},
		{"gh's own GraphQL fixture has no status", `non-200 OK status code:  body: "no bueno"`, HostAuthUnknown},
		{"a GraphQL error", "GraphQL: Resource not accessible by integration (viewer)", HostAuthUnknown},
		{"a 2xx other than 200 (gh 2.102)", "unexpected HTTP 204 for GET https://api.github.com/", HostAuthUnknown},
		{"no text", "", HostAuthUnknown},
	}
	for _, c := range cases {
		if got := classifyAuthError(c.msg); got != c.want {
			t.Errorf("%s: classifyAuthError(%q) = %q, want %q", c.name, c.msg, got, c.want)
		}
	}
}

// TestAuthOK pins AuthedFor's decision from one check: the exit code × what the
// output says (#203). The exit code fails for an INACTIVE account's dead token
// although gh calls to the host work, so Authed() gated every gh call off; the
// host's active account decides instead. Nothing unreadable reads as a yes.
func TestAuthOK(t *testing.T) {
	ok := HostAuth{Host: "github.com", State: HostAuthOK}
	st := func(h, s string) HostAuth { return HostAuth{Host: h, State: s} }
	cases := []struct {
		name   string
		exitOK bool
		hosts  []HostAuth
		parsed bool
		want   bool
	}{
		{"exit 0: gh says every account it checked works", true, nil, false, true},
		{"#203: exit 1, but the host's ACTIVE account works (an inactive one's token is dead)", false, []HostAuth{ok}, true, true},
		{"exit 1, the active account timed out", false, []HostAuth{st("github.com", HostAuthTimeout)}, true, false},
		{"exit 1, the active account's token is dead", false, []HostAuth{st("github.com", HostAuthFailed)}, true, false},
		{"the host could not be reached", false, []HostAuth{st("github.com", HostAuthUnreachable)}, true, false},
		{"a host wt cannot read", false, []HostAuth{st("github.com", HostAuthUnknown)}, true, false},
		{"gh has no account on the host", false, nil, true, false},
		{"output wt cannot read", false, nil, false, false},
		{"unreadable output wins over a host in it", false, []HostAuth{ok}, false, false},
		{"unscoped: every host's active account works", false, []HostAuth{ok, st("ghe.example.com", HostAuthOK)}, true, true},
		{"unscoped: one host timed out", false, []HostAuth{ok, st("ghe.example.com", HostAuthTimeout)}, true, false},
	}
	for _, c := range cases {
		if got := authOK(c.exitOK, c.hosts, c.parsed); got != c.want {
			t.Errorf("%s: authOK(%v, %+v, %v) = %v, want %v", c.name, c.exitOK, c.hosts, c.parsed, got, c.want)
		}
	}
}

// TestTextAuthStatus pins the scoped check read from gh's human output, which
// every gh before 2.81 answers (#203): `gh auth status --hostname github.com`
// exits 1 in every row but the first, and the exit code alone said "NOT
// authenticated" for all of them.
func TestTextAuthStatus(t *testing.T) {
	const inactiveDead = `github.com
  ✓ Logged in to github.com account octo (keyring)
  - Active account: true
  - Git operations protocol: https
  - Token: gho_************************************

  X Failed to log in to github.com account old-octo (keyring)
  - Active account: false
  - The token in keyring is invalid.
  - To re-authenticate, run: gh auth login -h github.com
  - To forget about this account, run: gh auth logout -h github.com -u old-octo
`
	ok := []HostAuth{{Host: "github.com", State: HostAuthOK}}
	cases := []struct {
		name   string
		exitOK bool
		out    string
		wantOK bool
		hosts  []HostAuth
		parsed bool
	}{
		{"passes", true, "github.com\n  ✓ Logged in to github.com account octo (keyring)\n  - Active account: true\n", true, ok, true},
		{"#203: an inactive account's dead token fails the exit code; the active one decides", false, inactiveDead, true, ok, true},
		{"the host timed out: not authed (doctor: could not be verified)", false,
			"github.com\n  X Timeout trying to log in to github.com account octo (keyring)\n  - Active account: true\n",
			false, []HostAuth{{Host: "github.com", State: HostAuthTimeout}}, true},
		{"the active account's token is dead (or, before gh 2.81, the host unreachable)", false,
			"github.com\n  X Failed to log in to github.com account octo (keyring)\n  - Active account: true\n  - The token in keyring is invalid.\n",
			false, []HostAuth{{Host: "github.com", State: HostAuthFailed}}, true},
		{"gh has no account on the host", false, "You are not logged into any accounts on github.com\n", false, nil, true},
		{"output wt cannot read", false, "failed to read configuration: permission denied\n", false, nil, false},
		{"exit 1 that no inactive account explains: the failure is somewhere wt did not read → no", false,
			"github.com\n  ✓ Logged in to github.com account octo (keyring)\n  - Active account: true\n\nhost_with_underscore.example.com\n  X Failed to log in to host_with_underscore.example.com account octo (keyring)\n  - Active account: true\n",
			false, ok, true},
		{"gh before 2.40 (no Active marker): exit 1 with every host OK is unexplained → no", false,
			"github.com\n  ✓ Logged in to github.com as octo (oauth_token)\n  ✓ Token: *******************\n", false, ok, true},
		{"gh before 2.40: a failing line in a section with no Active marker is no known inactive account → no", false,
			"github.com\n  ✓ Logged in to github.com as octo (oauth_token)\n  X github.com: the token in oauth_token is missing required scope 'read:org'\n",
			false, ok, true},
		{"an inactive account that timed out explains the exit code too", false,
			"github.com\n  ✓ Logged in to github.com account octo (keyring)\n  - Active account: true\n\n  X Timeout trying to log in to github.com account old-octo (keyring)\n  - Active account: false\n",
			true, ok, true},
	}
	for _, c := range cases {
		got := textAuthStatus("github.com", c.exitOK, c.out)
		want := AuthStatus{Host: "github.com", OK: c.wantOK, Hosts: c.hosts, Parsed: c.parsed}
		if !reflect.DeepEqual(got, want) {
			t.Errorf("%s:\n textAuthStatus = %+v\n want            %+v", c.name, got, want)
		}
	}
}

// TestJSONAuthOutcome pins what one `gh auth status --json hosts` run means
// (#203): the JSON when stdout has it, whatever the exit code; the human form
// when gh refused the flag (before 2.81), remembered; the human form once, not
// remembered, when gh failed for another reason.
func TestJSONAuthOutcome(t *testing.T) {
	works := ghDoc(map[string][]ghEntry{"github.com": {okAcct("github.com", "octo", true)}})
	offline := ghDoc(map[string][]ghEntry{"github.com": {errAcct("github.com", "octo", true, "error", errDNS)}})
	refused := "unknown flag: --json\n\nUsage:  gh auth status [flags]\n\nFlags:\n  -a, --active            Display the active account only\n"
	okHosts := []HostAuth{{Host: "github.com", State: HostAuthOK}}
	cases := []struct {
		name             string
		exitOK           bool
		stdout, stderr   string
		want             AuthStatus
		fallback, noJSON bool
	}{
		{"gh 2.81+: the JSON is the answer", true, works, "",
			AuthStatus{Host: "github.com", OK: true, Hosts: okHosts, Parsed: true}, false, false},
		{"#203: offline the JSON form still exits 0; the entries decide, never the exit code", true, offline, "",
			AuthStatus{Host: "github.com", Hosts: []HostAuth{{Host: "github.com", State: HostAuthUnreachable, Error: errDNS}}, Parsed: true}, false, false},
		{"gh's note on stderr beside the JSON doesn't matter", true, "{\"hosts\":{}}\n", "You are not logged into any accounts on github.com\n",
			AuthStatus{Host: "github.com", Parsed: true}, false, false},
		{"a non-zero exit with the JSON on stdout: still the answer", false, works, "",
			AuthStatus{Host: "github.com", OK: true, Hosts: okHosts, Parsed: true}, false, false},
		{"gh before 2.81 refuses the flag → the human form, and don't ask again", false, "", refused,
			AuthStatus{Host: "github.com"}, true, true},
		{"a gh whose JSON form has no hosts field → the same", false, "", "Unknown JSON field: \"hosts\"\nAvailable fields:\n  accounts\n",
			AuthStatus{Host: "github.com"}, true, true},
		{"exit 0 but not the document → the human form, remembered", true, "github.com\n  ✓ Logged in\n", "",
			AuthStatus{Host: "github.com"}, true, true},
		{"gh itself failed → the human form once, not remembered", false, "", "failed to read configuration: permission denied\n",
			AuthStatus{Host: "github.com"}, true, false},
	}
	for _, c := range cases {
		got, fallback, noJSON := jsonAuthOutcome("github.com", c.exitOK, c.stdout, c.stderr)
		if !reflect.DeepEqual(got, c.want) || fallback != c.fallback || noJSON != c.noJSON {
			t.Errorf("%s:\n jsonAuthOutcome = (%+v, fallback=%v, noJSON=%v)\n want             (%+v, fallback=%v, noJSON=%v)",
				c.name, got, fallback, noJSON, c.want, c.fallback, c.noJSON)
		}
	}
}

// TestAuthStatusJSONArgs: the JSON form keeps #100's scoping and asks only about
// the account gh uses on each host.
func TestAuthStatusJSONArgs(t *testing.T) {
	if got := strings.Join(authStatusJSONArgs("github.com"), " "); got != "auth status --json hosts --active --hostname github.com" {
		t.Errorf("scoped: %q", got)
	}
	if got := strings.Join(authStatusJSONArgs(""), " "); got != "auth status --json hosts --active" {
		t.Errorf("unscoped: %q", got)
	}
}
