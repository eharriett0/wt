package doctor

import (
	"encoding/json"
	"reflect"
	"sort"
	"strings"
	"testing"

	"github.com/eharriett0/wt/internal/collide"
	"github.com/eharriett0/wt/internal/ghx"
)

func TestClassifyStaleCheckout(t *testing.T) {
	for _, tc := range []struct {
		name    string
		behind  int
		wantSev string
	}{
		{"at threshold → warn", collide.StaleBaseBehindThreshold, "warn"},
		{"far past threshold → warn", 144, "warn"},
		{"just below threshold → nothing", collide.StaleBaseBehindThreshold - 1, ""},
		{"current → nothing", 0, ""},
		{"uncomputable (-1) → nothing (git error never manufactures a warning)", -1, ""},
	} {
		t.Run(tc.name, func(t *testing.T) {
			_, sev := classifyStaleCheckout(tc.behind)
			if sev != tc.wantSev {
				t.Errorf("classifyStaleCheckout(%d) severity = %q, want %q", tc.behind, sev, tc.wantSev)
			}
		})
	}
}

// #138: severity for a base-tracking branch turns on push.default, NOT
// merge_is_deploy. Under simple (git's default) a bare push refuses on the name
// mismatch, so the old hard ✗ over-claimed — it must be info, not fail. Only
// upstream/tracking aim a bare push at the base (and the pre-push guard still
// blocks even that), so those warn.
func TestClassifyUpstream(t *testing.T) {
	for _, tc := range []struct {
		name         string
		mergeRef     string
		base         string
		pushDefault  string
		wantIssue    string
		wantSeverity string
	}{
		{"no upstream", "", "main", "", "no_upstream", "info"},
		{"no upstream, push.default=upstream", "", "main", "upstream", "no_upstream", "info"},
		{"tracks own branch — fine", "refs/heads/feat/x", "main", "", "", ""},
		{"tracks a different base name — fine", "refs/heads/develop", "main", "upstream", "", ""},

		// THE #138 FIX: default config (simple/unset) is INFO, never fail.
		{"tracks base, push.default unset (simple) — INFO not fail", "refs/heads/main", "main", "", "tracks_base", "info"},
		{"tracks base, push.default=simple — INFO", "refs/heads/main", "main", "simple", "tracks_base", "info"},
		{"tracks base, push.default=current — INFO", "refs/heads/main", "main", "current", "tracks_base", "info"},
		{"tracks base, push.default=nothing — INFO", "refs/heads/main", "main", "nothing", "tracks_base", "info"},

		// Only upstream/tracking aim a bare push at the base → warn (guard still blocks).
		{"tracks base, push.default=upstream — warn", "refs/heads/main", "main", "upstream", "tracks_base", "warn"},
		{"tracks base, push.default=tracking (alias) — warn", "refs/heads/main", "main", "tracking", "tracks_base", "warn"},
		{"tracks base by bare merge name, upstream — warn", "main", "main", "upstream", "tracks_base", "warn"},
		{"non-main base, upstream — warn", "refs/heads/develop", "develop", "upstream", "tracks_base", "warn"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			issue, sev := classifyUpstream(tc.mergeRef, tc.base, tc.pushDefault)
			if issue != tc.wantIssue || sev != tc.wantSeverity {
				t.Errorf("classifyUpstream(%q,%q,%q) = (%q,%q), want (%q,%q)",
					tc.mergeRef, tc.base, tc.pushDefault, issue, sev, tc.wantIssue, tc.wantSeverity)
			}
		})
	}
}

// #138: only upstream/tracking make a bare `git push` follow the branch's
// upstream ref (→ the base); every other value keys off the branch name.
func TestPushDefaultFollowsUpstream(t *testing.T) {
	for _, v := range []string{"upstream", "tracking", " upstream "} {
		if !pushDefaultFollowsUpstream(v) {
			t.Errorf("pushDefaultFollowsUpstream(%q) = false, want true", v)
		}
	}
	for _, v := range []string{"", "simple", "current", "matching", "nothing", "garbage"} {
		if pushDefaultFollowsUpstream(v) {
			t.Errorf("pushDefaultFollowsUpstream(%q) = true, want false (bare push keys off the branch name)", v)
		}
	}
}

// #183: with no forge host to scope to (outside a repo, or a local-path origin)
// the gh check is a bare `gh auth status`, whose exit code fails if ANY host does.
// One unreachable Enterprise host made doctor say "NOT authenticated" for a
// github.com login that was fine: #100 again, because #102 only fixed it in a
// repo. The unscoped check is now read per host. #203: so is a scoped check that
// did not pass, because a timeout or an unreachable repo host proves nothing
// about the login either; and offline (gh 2.81+ says so) is not a dead token.
func TestGHAuth(t *testing.T) {
	ok := ghx.HostAuth{Host: "github.com", State: ghx.HostAuthOK}
	gheTimeout := ghx.HostAuth{Host: "ghe.example.com", State: ghx.HostAuthTimeout}
	ghFailed := ghx.HostAuth{Host: "github.com", State: ghx.HostAuthFailed}
	gheFailed := ghx.HostAuth{Host: "ghe.example.com", State: ghx.HostAuthFailed}
	unreadable := ghx.HostAuth{Host: "ghe.example.com", State: ghx.HostAuthUnknown}
	ghTimeout := ghx.HostAuth{Host: "github.com", State: ghx.HostAuthTimeout}
	ghOffline := ghx.HostAuth{Host: "github.com", State: ghx.HostAuthUnreachable, Error: `Get "https://api.github.com/": dial tcp: lookup api.github.com: no such host`}
	gheOffline := ghx.HostAuth{Host: "ghe.example.com", State: ghx.HostAuthUnreachable, Error: `Get "https://ghe.example.com/api/v3/": dial tcp: lookup ghe.example.com: no such host`}

	cases := []struct {
		name    string
		st      ghx.AuthStatus
		authed  bool
		hosts   []ghx.HostAuth
		unknown bool
	}{
		{"in a repo: the scoped check passes", ghx.AuthStatus{Host: "github.com", OK: true, Parsed: true, Hosts: []ghx.HostAuth{ok}},
			true, nil, false},
		{"#203: in a repo, the repo's host timed out → unknown, never NOT authenticated",
			ghx.AuthStatus{Host: "github.com", Parsed: true, Hosts: []ghx.HostAuth{ghTimeout}},
			false, []ghx.HostAuth{ghTimeout}, true},
		{"#203: in a repo, gh 2.81+ could not reach the host → unknown",
			ghx.AuthStatus{Host: "github.com", Parsed: true, Hosts: []ghx.HostAuth{ghOffline}},
			false, []ghx.HostAuth{ghOffline}, true},
		{"in a repo: the active account's token is dead → NOT authenticated, as before",
			ghx.AuthStatus{Host: "github.com", Parsed: true, Hosts: []ghx.HostAuth{ghFailed}},
			false, []ghx.HostAuth{ghFailed}, false},
		{"in a repo: gh has no account on the repo's host → NOT authenticated",
			ghx.AuthStatus{Host: "github.com", Parsed: true},
			false, nil, false},
		{"#203: in a repo, output wt cannot read → unknown, naming the repo's host",
			ghx.AuthStatus{Host: "github.com"},
			false, []ghx.HostAuth{{Host: "github.com", State: ghx.HostAuthUnknown}}, true},
		{"unscoped and gh passed: authenticated, as before", ghx.AuthStatus{OK: true, Parsed: true, Hosts: []ghx.HostAuth{ok}},
			true, nil, false},
		{"#183: unscoped, github.com ok and the Enterprise host timed out → authenticated",
			ghx.AuthStatus{Parsed: true, Hosts: []ghx.HostAuth{ok, gheTimeout}},
			true, []ghx.HostAuth{ok, gheTimeout}, false},
		{"#183: the authenticated host listed after a failing one",
			ghx.AuthStatus{Parsed: true, Hosts: []ghx.HostAuth{gheTimeout, ok}},
			true, []ghx.HostAuth{gheTimeout, ok}, false},
		{"unscoped, every host failed to log in → NOT authenticated",
			ghx.AuthStatus{Parsed: true, Hosts: []ghx.HostAuth{ghFailed, gheFailed}},
			false, []ghx.HostAuth{ghFailed, gheFailed}, false},
		{"unscoped, gh has no host at all → NOT authenticated", ghx.AuthStatus{Parsed: true},
			false, nil, false},
		{"unscoped, every host timed out → unknown, never NOT authenticated",
			ghx.AuthStatus{Parsed: true, Hosts: []ghx.HostAuth{{Host: "github.com", State: ghx.HostAuthTimeout}, gheTimeout}},
			false, []ghx.HostAuth{{Host: "github.com", State: ghx.HostAuthTimeout}, gheTimeout}, true},
		{"unscoped, one failed and one timed out → unknown: the timeout may be the login",
			ghx.AuthStatus{Parsed: true, Hosts: []ghx.HostAuth{ghFailed, gheTimeout}},
			false, []ghx.HostAuth{ghFailed, gheTimeout}, true},
		{"unscoped, a host section wt cannot read → unknown",
			ghx.AuthStatus{Parsed: true, Hosts: []ghx.HostAuth{ghFailed, unreadable}},
			false, []ghx.HostAuth{ghFailed, unreadable}, true},
		{"unscoped, output wt cannot read at all → unknown", ghx.AuthStatus{},
			false, nil, true},
		{"#203: offline, gh 2.81+ reaches no host → unknown, never NOT authenticated",
			ghx.AuthStatus{Parsed: true, Hosts: []ghx.HostAuth{gheOffline, ghOffline}},
			false, []ghx.HostAuth{gheOffline, ghOffline}, true},
		{"#203: github.com ok, the Enterprise host unreachable → authenticated",
			ghx.AuthStatus{Parsed: true, Hosts: []ghx.HostAuth{gheOffline, ok}},
			true, []ghx.HostAuth{gheOffline, ok}, false},
	}
	for _, c := range cases {
		authed, hosts, unknown := ghAuth(c.st)
		if authed != c.authed || !reflect.DeepEqual(hosts, c.hosts) || unknown != c.unknown {
			t.Errorf("%s:\n ghAuth = (%v, %+v, %v)\n want    (%v, %+v, %v)", c.name, authed, hosts, unknown, c.authed, c.hosts, c.unknown)
		}
	}
}

// TestGHLine pins doctor's gh line. The three pre-#183 lines are what a check that
// passed or definitely failed prints, so they are pinned byte-for-byte; none of
// the others may say "NOT authenticated" when a host is authenticated or nothing
// was proven (#183/#203).
func TestGHLine(t *testing.T) {
	ok := ghx.HostAuth{Host: "github.com", State: ghx.HostAuthOK}
	gheTimeout := ghx.HostAuth{Host: "ghe.example.com", State: ghx.HostAuthTimeout}
	dns := `Get "https://api.github.com/": dial tcp: lookup api.github.com: no such host`
	gheDNS := `Get "https://ghe.example.com/api/v3/": dial tcp: lookup ghe.example.com: no such host`
	cases := []struct {
		name string
		rep  Report
		warn bool
		msg  string
	}{
		{"gh not on PATH (unchanged)", Report{},
			true, "gh — not found (claim/release/merge-pr need it; new/clean/status/check/hooks don't)"},
		{"authenticated (unchanged)", Report{GH: true, GHAuthed: true},
			false, "gh — authenticated"},
		{"NOT authenticated (unchanged)", Report{GH: true},
			true, "gh — found but NOT authenticated (claim/release/merge-pr need `gh auth login`)"},
		{"#183: github.com authenticated, the Enterprise host timed out",
			Report{GH: true, GHAuthed: true, GHHosts: []ghx.HostAuth{ok, gheTimeout}},
			false, "gh — authenticated on github.com (1 other host failing: ghe.example.com timed out)"},
		{"several failing hosts are counted and named",
			Report{GH: true, GHAuthed: true, GHHosts: []ghx.HostAuth{ok, gheTimeout, {Host: "old.example.com", State: ghx.HostAuthFailed}}},
			false, "gh — authenticated on github.com (2 other hosts failing: ghe.example.com timed out, old.example.com failed to log in)"},
		{"every host authenticated, read per host",
			Report{GH: true, GHAuthed: true, GHHosts: []ghx.HostAuth{ok, {Host: "ghe.example.com", State: ghx.HostAuthOK}}},
			false, "gh — authenticated on github.com, ghe.example.com"},
		{"nothing proven: every host timed out",
			Report{GH: true, GHAuthUnknown: true, GHHosts: []ghx.HostAuth{{Host: "github.com", State: ghx.HostAuthTimeout}, gheTimeout}},
			true, "gh — found, but auth could not be verified: github.com timed out, ghe.example.com timed out"},
		{"nothing proven: output wt could not read",
			Report{GH: true, GHAuthUnknown: true},
			true, "gh — found, but auth could not be verified: with no repository host to scope it to, `gh auth status` checks every configured host, and it failed with output wt could not read (run it to see why)"},
		{"#203: in a repo, the repo's host timed out",
			Report{GH: true, GHAuthUnknown: true, GHHosts: []ghx.HostAuth{{Host: "github.com", State: ghx.HostAuthTimeout}}},
			true, "gh — found, but auth could not be verified: github.com timed out"},
		{"#203: offline, and gh 2.81+ says so",
			Report{GH: true, GHAuthUnknown: true, GHHosts: []ghx.HostAuth{
				{Host: "github.com", State: ghx.HostAuthUnreachable, Error: dns}, {Host: "ghe.example.com", State: ghx.HostAuthUnreachable, Error: gheDNS}}},
			true, "gh — found, but auth could not be verified: couldn't reach github.com, couldn't reach ghe.example.com"},
		{"#203: authenticated, the other host could not be reached",
			Report{GH: true, GHAuthed: true, GHHosts: []ghx.HostAuth{ok, {Host: "ghe.example.com", State: ghx.HostAuthUnreachable, Error: gheDNS}}},
			false, "gh — authenticated on github.com (1 other host failing: couldn't reach ghe.example.com)"},
		{"#203: the host answered, but not about the token: gh's words, first line",
			Report{GH: true, GHAuthUnknown: true, GHHosts: []ghx.HostAuth{{Host: "github.com", State: ghx.HostAuthUnknown,
				Error: "HTTP 403: API rate limit exceeded for user ID 1. (https://api.github.com/)\nmore"}}},
			true, "gh — found, but auth could not be verified: github.com could not be checked (gh: HTTP 403: API rate limit exceeded for user ID 1. (https://api.github.com/))"},
		{"#203: a check wt cannot read names its host",
			Report{GH: true, GHAuthUnknown: true, GHHosts: []ghx.HostAuth{{Host: "github.com", State: ghx.HostAuthUnknown}}},
			true, "gh — found, but auth could not be verified: wt could not read gh's answer for github.com"},
		{"#203: gh 2.81+ says the token is dead (an HTTP 401): plainly NOT authenticated",
			Report{GH: true, GHHosts: []ghx.HostAuth{{Host: "github.com", State: ghx.HostAuthFailed, Error: "HTTP 401: Bad credentials (https://api.github.com/)"}}},
			true, "gh — found but NOT authenticated (claim/release/merge-pr need `gh auth login`)"},
		{"#203: gh before 2.81 prints that failure for an unreachable host too: say so",
			Report{GH: true, GHHosts: []ghx.HostAuth{{Host: "github.com", State: ghx.HostAuthFailed}}},
			true, "gh — found but NOT authenticated, or offline: gh before 2.81 reports a host it cannot reach as a failed login (claim/release/merge-pr need `gh auth login`)"},
	}
	for _, c := range cases {
		warn, msg := ghLine(&c.rep)
		if warn != c.warn || msg != c.msg {
			t.Errorf("%s:\n ghLine = (%v, %q)\n want    (%v, %q)", c.name, warn, msg, c.warn, c.msg)
		}
		if (c.rep.GHAuthed || c.rep.GHAuthUnknown) && strings.Contains(msg, "NOT authenticated") {
			t.Errorf("%s: claims NOT authenticated for a login that is authenticated or unproven: %q", c.name, msg)
		}
	}
}

// A check that passed emits exactly the pre-#183 gh keys, so `wt doctor --json`
// in a repo whose host authenticates is unchanged; one that did not adds the
// breakdown, scoped (#203) or not (#183), with gh's error from gh 2.81+.
func TestReportJSONGHFields(t *testing.T) {
	keys := func(rep Report) []string {
		b, err := json.Marshal(rep)
		if err != nil {
			t.Fatal(err)
		}
		var m map[string]any
		if err := json.Unmarshal(b, &m); err != nil {
			t.Fatal(err)
		}
		var ks []string
		for k := range m {
			if strings.HasPrefix(k, "gh") {
				ks = append(ks, k)
			}
		}
		sort.Strings(ks)
		return ks
	}
	scoped := Report{GH: true}
	scoped.GHAuthed, scoped.GHHosts, scoped.GHAuthUnknown = ghAuth(ghx.AuthStatus{Host: "github.com", OK: true, Parsed: true,
		Hosts: []ghx.HostAuth{{Host: "github.com", State: ghx.HostAuthOK}}})
	if got := keys(scoped); !reflect.DeepEqual(got, []string{"gh", "gh_authed"}) {
		t.Errorf("scoped check that passed: JSON gh keys = %v, want [gh gh_authed] (unchanged)", got)
	}
	timedOut := Report{GH: true}
	timedOut.GHAuthed, timedOut.GHHosts, timedOut.GHAuthUnknown = ghAuth(ghx.AuthStatus{Host: "github.com", Parsed: true,
		Hosts: []ghx.HostAuth{{Host: "github.com", State: ghx.HostAuthTimeout}}})
	if b, _ := json.Marshal(timedOut); !strings.Contains(string(b), `"gh_authed":false,"gh_hosts":[{"host":"github.com","state":"timeout"}],"gh_auth_unknown":true`) {
		t.Errorf("#203 scoped timeout JSON = %s, want the host and gh_auth_unknown", b)
	}
	offline := Report{GH: true}
	offline.GHAuthed, offline.GHHosts, offline.GHAuthUnknown = ghAuth(ghx.AuthStatus{Host: "github.com", Parsed: true,
		Hosts: []ghx.HostAuth{{Host: "github.com", State: ghx.HostAuthUnreachable, Error: `Get "https://api.github.com/": dial tcp: lookup api.github.com: no such host`}}})
	if b, _ := json.Marshal(offline); !strings.Contains(string(b), `"gh_hosts":[{"host":"github.com","state":"unreachable","error":"Get \"https://api.github.com/\": dial tcp: lookup api.github.com: no such host"}],"gh_auth_unknown":true`) {
		t.Errorf("#203 offline JSON = %s, want state unreachable with gh's error", b)
	}
	unscoped := Report{GH: true}
	unscoped.GHAuthed, unscoped.GHHosts, unscoped.GHAuthUnknown = ghAuth(ghx.AuthStatus{Parsed: true, Hosts: []ghx.HostAuth{
		{Host: "github.com", State: ghx.HostAuthOK}, {Host: "ghe.example.com", State: ghx.HostAuthTimeout}}})
	b, _ := json.Marshal(unscoped)
	if !strings.Contains(string(b), `"gh_authed":true,"gh_hosts":[{"host":"github.com","state":"ok"},{"host":"ghe.example.com","state":"timeout"}]`) {
		t.Errorf("#183 unscoped JSON = %s, want gh_authed true + the per-host breakdown", b)
	}
}

// #163: doctor names this shell's session and where it came from — or, with no
// token at all, says so and how to fix it (it can't be told apart from another
// token-less session in the same checkout).
func TestSessionLine(t *testing.T) {
	if got := sessionLine("c1", "CLAUDE_CODE_SESSION_ID"); got != "c1 (from CLAUDE_CODE_SESSION_ID)" {
		t.Errorf("sessionLine(token) = %q", got)
	}
	// The text row shortens a raw session id (doctor --json keeps it whole).
	if got := sessionLine("24288609-c37a-43a9-8a82-80e93779324e", "CLAUDE_CODE_SESSION_ID"); got != "24288609… (from CLAUDE_CODE_SESSION_ID)" {
		t.Errorf("sessionLine(uuid) = %q, want the short form", got)
	}
	got := sessionLine("", "")
	for _, want := range []string{"none", "WT_SESSION", "CLAUDE_CODE_SESSION_ID", "terminal tab ids are deliberately not used", "can't be told apart"} {
		if !strings.Contains(got, want) {
			t.Errorf("token-less sessionLine missing %q: %q", want, got)
		}
	}
}
