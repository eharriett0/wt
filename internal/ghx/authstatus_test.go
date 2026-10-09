package ghx

import (
	"os"
	"path/filepath"
	"reflect"
	"runtime"
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

// TestAuthStatusForReadsBothStreams pins the gh plumbing under the parser: once
// any account fails, gh writes EVERY host section to stderr and exits 1, so a
// stdout-only capture would read the #183 two-host case as "could not be
// verified". It also pins #172's single check: the boolean and the per-host
// breakdown come from one memoized gh run.
func TestAuthStatusForReadsBothStreams(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("shell-script gh shim")
	}
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "out.txt"), []byte(ghTwoHosts), 0o644); err != nil {
		t.Fatal(err)
	}
	calls := filepath.Join(dir, "calls")
	shim := "#!/bin/sh\necho run >> '" + calls + "'\ncat '" + filepath.Join(dir, "out.txt") + "' >&2\nexit 1\n"
	if err := os.WriteFile(filepath.Join(dir, "gh"), []byte(shim), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", dir+string(os.PathListSeparator)+"/usr/bin"+string(os.PathListSeparator)+"/bin")

	authMu.Lock()
	saved := authMemo
	authMemo = map[string]authAnswer{}
	authMu.Unlock()
	t.Cleanup(func() {
		authMu.Lock()
		authMemo = saved
		authMu.Unlock()
	})

	if AuthedFor("") {
		t.Fatal("AuthedFor(\"\") = true for a gh that exited 1")
	}
	got := AuthStatusFor("")
	want := []HostAuth{{Host: "github.com", State: HostAuthOK}, {Host: "ghe.example.com", State: HostAuthTimeout}}
	if got.OK || !got.Parsed || !reflect.DeepEqual(got.Hosts, want) {
		t.Errorf("AuthStatusFor(\"\") = %+v, want OK=false Parsed=true Hosts=%+v (the sections are on stderr)", got, want)
	}
	b, err := os.ReadFile(calls)
	if err != nil {
		t.Fatal(err)
	}
	if n := strings.Count(string(b), "run"); n != 1 {
		t.Errorf("gh ran %d times for AuthedFor + AuthStatusFor, want 1 (one memoized check, #172)", n)
	}
}
