package ghx

import (
	"encoding/json"
	"regexp"
	"slices"
	"sort"
	"strings"
)

// HostAuth is one host's line in a `gh auth status` run (#183).
type HostAuth struct {
	Host  string `json:"host"`
	State string `json:"state"` // HostAuthOK | HostAuthFailed | HostAuthTimeout | HostAuthUnreachable | HostAuthUnknown
	// Error is gh's own error for the host's active account. Only `--json`
	// (gh 2.81+) carries one; gh's human output never does (#203).
	Error string `json:"error,omitempty"`
}

// The states a host can be in. Only OK and Failed are claims about the login; a
// timeout, an unreachable host and an unreadable answer are claims about nothing.
const (
	HostAuthOK = "ok" // the active account's token works
	// HostAuthFailed: the host rejected the token. From `--json` that is an HTTP
	// 401 (and Error holds gh's text). From the human output ("X Failed to log in
	// to <host> …", no Error) it is NOT proof: gh prints that same line, "token
	// invalid" and all, for a host it could not reach at all (#203).
	HostAuthFailed      = "failed"
	HostAuthTimeout     = "timeout"     // the host never answered
	HostAuthUnreachable = "unreachable" // no HTTP answer at all: DNS failure, connection refused, TLS (`--json` only, #203)
	HostAuthUnknown     = "unknown"     // nothing wt can read as one of the above
)

// AuthStatus is one `gh auth status` answer, broken down per host (#183).
type AuthStatus struct {
	Host string // the host the check was scoped to; "" = unscoped, every configured host
	// OK is AuthedFor's answer (authOK; textAuthStatus for the human form): gh
	// can use every host it checked, judged by each host's ACTIVE account (#203).
	OK     bool
	Hosts  []HostAuth // per host: gh's order (human output), by name (`--json`)
	Parsed bool       // false = gh's output could not be read, so it proves nothing
}

// AuthStatusFor is AuthedFor with the per-host breakdown. It reads the SAME
// memoized gh run, so asking for the detail never costs a second check (#172).
// The hosts are a copy: a caller can't edit the memo.
func AuthStatusFor(host string) AuthStatus {
	st := authCheck(host)
	st.Hosts = append([]HostAuth(nil), st.Hosts...)
	return st
}

// RepoAuthStatus is AuthStatusFor the host Authed checks: the repo's forge host,
// or "" (unscoped) when there is none, which includes being outside a repo.
func RepoAuthStatus() AuthStatus { return AuthStatusFor(RepoHost()) }

// authOK decides AuthedFor from one check (#203). Pure. Exit code 0 from the
// human form is a yes: gh says every account it checked works. Otherwise the
// ACTIVE account of every host gh reported must work, because that is the one gh
// uses there: an inactive account with a dead token fails `gh auth status
// --hostname H` (the #100 shape) without stopping a single gh call to H. The
// `--json` form always exits 0, so its callers pass exitOK=false. Unreadable
// output, or no host at all, is a no: "unknown" never reads as authenticated.
func authOK(exitOK bool, hosts []HostAuth, parsed bool) bool {
	if exitOK {
		return true
	}
	if !parsed || len(hosts) == 0 {
		return false
	}
	for _, h := range hosts {
		if h.State != HostAuthOK {
			return false
		}
	}
	return true
}

// textAuthStatus reads the human form: the exit code plus the host sections. Pure.
// A non-zero exit says some account failed. With every host's active account
// working, only an INACTIVE account's failure explains it, so the yes needs one
// in a section wt read: a failure wt cannot see (a section it can't read) leaves
// AuthedFor a no. That yes is what lets liveness trust "no PR" (PRChecked, which
// dormancy needs), so it is never given on output wt only partly understood.
func textAuthStatus(host string, exitOK bool, out string) AuthStatus {
	hosts, parsed, inactiveFailed := readAuthStatus(out)
	ok := exitOK || (inactiveFailed && authOK(false, hosts, parsed))
	return AuthStatus{Host: host, OK: ok, Hosts: hosts, Parsed: parsed}
}

// jsonAuthOutcome decides what one `gh auth status --json hosts` run means
// (#203). Pure.
//   - its stdout reads as the JSON → that is the answer, whatever the exit code
//     (gh documents exit 0 for every auth outcome in this form)
//   - gh rejected the flag (before 2.81: "unknown flag: --json") → fall back to
//     the human form, and noJSON: this gh has none, don't ask again
//   - exit 0 without JSON wt can read → the same (a form wt does not know)
//   - any other failure (gh could not read its config, say) → fall back once,
//     not remembered: the human form reports the failure as it always has
func jsonAuthOutcome(host string, exitOK bool, stdout, stderr string) (st AuthStatus, fallback, noJSON bool) {
	if hosts, parsed := parseAuthStatusJSON(stdout); parsed {
		return AuthStatus{Host: host, OK: authOK(false, hosts, true), Hosts: hosts, Parsed: true}, false, false
	}
	if jsonFormRejected(stderr) {
		return AuthStatus{Host: host}, true, true
	}
	return AuthStatus{Host: host}, true, exitOK
}

// jsonFormRejected reports whether gh refused the `--json hosts` form itself:
// cobra's "unknown flag: --json" (gh before 2.81), or "Unknown JSON field" from
// a gh whose JSON form has no "hosts". Pure.
func jsonFormRejected(stderr string) bool {
	s := strings.ToLower(stderr)
	return strings.Contains(s, "unknown flag") || strings.Contains(s, "unknown json field")
}

var (
	// ansiSeq: gh colors its output when it thinks it is on a terminal
	// (GH_FORCE_TTY, CLICOLOR_FORCE), the JSON form included, so strip escapes
	// before reading it.
	ansiSeq = regexp.MustCompile("\x1b\\[[0-9;?]*[ -/]*[@-~]")
	// hostLine: a section header is an unindented host name, optionally :port.
	hostLine = regexp.MustCompile(`^[A-Za-z0-9.-]+(:[0-9]+)?$`)
)

// parseAuthStatus reads the per-host sections of `gh auth status` output. Pure,
// so the per-host classification is table-testable (#183).
//
// A section is a host name on an unindented line, then one indented entry per
// account: "✓ Logged in to …", "X Failed to log in to …" or "X Timeout trying to
// log in to …", each followed by "- Active account: true|false". The ACTIVE
// account decides the host, because it is the one gh uses there: an inactive
// account with a dead token does not stop the host from working. gh lists the
// active account first, so the first entry stands in when no line marks it (gh
// before 2.40 printed none, and one entry per host).
//
// A section with no readable entry is HostAuthUnknown, never Failed. parsed=false
// means there was no section at all; gh's "not logged into any GitHub hosts" is
// the one hostless output that is readable, as definitively no host: (nil, true).
func parseAuthStatus(out string) (hosts []HostAuth, parsed bool) {
	hosts, parsed, _ = readAuthStatus(out)
	return hosts, parsed
}

// readAuthStatus is parseAuthStatus, plus whether an account marked inactive
// failed in a section it read (#203): the failure that leaves every host's
// active account working. Pure.
func readAuthStatus(out string) (hosts []HostAuth, parsed, inactiveFailed bool) {
	type section struct {
		host   string
		states []string // one per account entry, in gh's order
		active int      // index of the entry marked active; -1 = none marked
	}
	clean := ansiSeq.ReplaceAllString(out, "")
	var secs []*section
	var cur *section
	for _, ln := range strings.Split(clean, "\n") {
		ln = strings.TrimRight(ln, "\r")
		t := strings.TrimSpace(ln)
		if t == "" {
			continue
		}
		if ln[0] != ' ' && ln[0] != '\t' { // unindented: a host header, or not ours
			cur = nil
			if hostLine.MatchString(t) {
				cur = &section{host: t, active: -1}
				secs = append(secs, cur)
			}
			continue
		}
		if cur == nil {
			continue
		}
		switch {
		case strings.HasPrefix(t, "✓"):
			cur.states = append(cur.states, HostAuthOK)
		case strings.HasPrefix(t, "X "):
			// gh's phrase, in both formats ("X Timeout trying to log in to <host>",
			// pre-2.40 "X <host>: timeout trying to log in"). Not a bare "timeout":
			// a host or login can contain that word, never this phrase (no spaces).
			if strings.Contains(strings.ToLower(t), "timeout trying to log in") {
				cur.states = append(cur.states, HostAuthTimeout)
			} else {
				cur.states = append(cur.states, HostAuthFailed)
			}
		case strings.HasPrefix(t, "- Active account:"):
			marked := strings.TrimSpace(strings.TrimPrefix(t, "- Active account:")) == "true"
			if marked && cur.active < 0 && len(cur.states) > 0 {
				cur.active = len(cur.states) - 1
			}
		}
	}
	for _, s := range secs {
		st := HostAuthUnknown
		switch {
		case s.active >= 0:
			st = s.states[s.active]
		case len(s.states) > 0:
			st = s.states[0]
		}
		hosts = append(hosts, HostAuth{Host: s.host, State: st})
		for j, entry := range s.states {
			if s.active >= 0 && j != s.active && entry != HostAuthOK {
				inactiveFailed = true
			}
		}
	}
	if len(hosts) > 0 {
		return hosts, true, inactiveFailed
	}
	if strings.Contains(strings.ToLower(clean), "not logged into any") {
		return nil, true, false
	}
	return nil, false, false
}

// ghAuthEntry is one account in `gh auth status --json hosts` (gh 2.81+,
// cli/cli#11544): pkg/cmd/auth/status's authEntry, the fields wt reads. The
// document is {"hosts": {"<host>": [authEntry, …]}}, the active account first.
type ghAuthEntry struct {
	State  string `json:"state"` // "success" | "timeout" | "error"
	Error  string `json:"error"` // gh's error text; omitted on success
	Active bool   `json:"active"`
}

// parseAuthStatusJSON reads `gh auth status --json hosts`. Pure (#203).
//
// Each host is decided by its ACTIVE account, as in parseAuthStatus: "success"
// is OK, "timeout" a timeout, and "error" is classified by gh's error text
// (classifyAuthError), which is the point of this form: the human output says
// "token invalid" for every error, a host it never reached included. Hosts come
// in name order (gh's encoder sorts the map). parsed=false unless stdout is that
// document: {"hosts": {}} (logged in nowhere, or not on the host asked about) is
// (nil, true).
func parseAuthStatusJSON(out string) (hosts []HostAuth, parsed bool) {
	var doc struct {
		Hosts *map[string][]ghAuthEntry `json:"hosts"` // nil: no "hosts" key, not this document
	}
	if err := json.Unmarshal([]byte(ansiSeq.ReplaceAllString(out, "")), &doc); err != nil || doc.Hosts == nil {
		return nil, false
	}
	names := make([]string, 0, len(*doc.Hosts))
	for name := range *doc.Hosts {
		names = append(names, name)
	}
	sort.Strings(names)
	for _, name := range names {
		hosts = append(hosts, jsonHostAuth(name, (*doc.Hosts)[name]))
	}
	return hosts, true
}

// jsonHostAuth is one host of the JSON form, read by its active account. Pure.
// gh marks the account it uses `"active": true` on every host (the first entry
// it builds), so a host with no entry marked is not gh's answer: unknown. Unlike
// the human form, there is no older JSON format to stand in for.
func jsonHostAuth(host string, entries []ghAuthEntry) HostAuth {
	i := slices.IndexFunc(entries, func(x ghAuthEntry) bool { return x.Active })
	if i < 0 {
		return HostAuth{Host: host, State: HostAuthUnknown}
	}
	e := entries[i]
	h := HostAuth{Host: host, Error: strings.TrimSpace(e.Error)}
	switch e.State {
	case "success":
		h.State, h.Error = HostAuthOK, ""
	case "timeout":
		h.State = HostAuthTimeout
	case "error":
		h.State = classifyAuthError(h.Error)
	default:
		h.State = HostAuthUnknown
	}
	return h
}

var (
	// httpStatusErr: the host answered with an HTTP error. gh's REST read (a
	// token from gh's own store) fails as "HTTP 401: Bad credentials
	// (https://api.github.com/)" (go-gh's HTTPError); its GraphQL read (a token
	// from GH_TOKEN and friends) as "non-200 OK status code: 401 Unauthorized
	// body: …" (cli/shurcooL-graphql).
	httpStatusErr = regexp.MustCompile(`^(?:HTTP |non-200 OK status code: )(\d{3})\b`)
	// urlErr: Go's *url.Error, `Get "https://api.github.com/": <cause>`, is how a
	// request that never got an HTTP answer fails.
	urlErr = regexp.MustCompile(`^[A-Z][a-z]+ "[^"]*": `)
	// netFailures: the Go and OS wording of a connection that never happened, for
	// an error that does not arrive as a *url.Error.
	netFailures = []string{
		"no such host", "server misbehaving", "temporary failure in name resolution",
		"connection refused", "network is unreachable", "no route to host",
		"connection reset", "dial tcp",
	}
)

// classifyAuthError reads gh's error for an account in state "error" (#203).
// Pure. Only an HTTP 401 says the token is invalid → HostAuthFailed. No HTTP
// answer at all (DNS, refused, TLS, reset) → HostAuthUnreachable. Anything else,
// another HTTP status included (a 403 rate limit or suspended account, a 5xx), or
// an empty error → HostAuthUnknown: the host's verdict on the token is unproven.
func classifyAuthError(msg string) string {
	m := strings.TrimSpace(msg)
	if sm := httpStatusErr.FindStringSubmatch(m); sm != nil {
		if sm[1] == "401" {
			return HostAuthFailed
		}
		return HostAuthUnknown
	}
	if urlErr.MatchString(m) {
		return HostAuthUnreachable
	}
	low := strings.ToLower(m)
	for _, p := range netFailures {
		if strings.Contains(low, p) {
			return HostAuthUnreachable
		}
	}
	return HostAuthUnknown
}
