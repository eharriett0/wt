package ghx

import (
	"regexp"
	"strings"
)

// HostAuth is one host's line in a `gh auth status` run (#183).
type HostAuth struct {
	Host  string `json:"host"`
	State string `json:"state"` // HostAuthOK | HostAuthFailed | HostAuthTimeout | HostAuthUnknown
}

// The states a host can be in. Only OK and Failed are claims gh makes about the
// login; a timeout and an unreadable section are claims about nothing.
const (
	HostAuthOK      = "ok"      // "✓ Logged in to <host> account …"
	HostAuthFailed  = "failed"  // "X Failed to log in to <host> …": gh calls the token invalid (it also says so for a refused or unresolvable host)
	HostAuthTimeout = "timeout" // "X Timeout trying to log in to <host> …": the host never answered
	HostAuthUnknown = "unknown" // a host section with no account line wt can read
)

// AuthStatus is one `gh auth status` answer, broken down per host (#183).
type AuthStatus struct {
	Host   string     // the host the check was scoped to; "" = unscoped, every configured host
	OK     bool       // gh exited 0 (AuthedFor's answer)
	Hosts  []HostAuth // per host, in gh's order
	Parsed bool       // false = gh's output could not be read, so it proves nothing
}

// AuthStatusFor is AuthedFor with the per-host breakdown. It reads the SAME
// memoized gh run, so asking for the detail never costs a second check (#172).
func AuthStatusFor(host string) AuthStatus {
	a := authCheck(host)
	hosts, parsed := parseAuthStatus(a.out)
	return AuthStatus{Host: host, OK: a.ok, Hosts: hosts, Parsed: parsed}
}

// RepoAuthStatus is AuthStatusFor the host Authed checks: the repo's forge host,
// or "" (unscoped) when there is none, which includes being outside a repo.
func RepoAuthStatus() AuthStatus { return AuthStatusFor(RepoHost()) }

var (
	// ansiSeq: gh colors its output when it thinks it is on a terminal
	// (GH_FORCE_TTY, CLICOLOR_FORCE), so strip escapes before reading it.
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
	}
	if len(hosts) > 0 {
		return hosts, true
	}
	if strings.Contains(strings.ToLower(clean), "not logged into any") {
		return nil, true
	}
	return nil, false
}
