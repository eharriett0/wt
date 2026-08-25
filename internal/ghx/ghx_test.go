package ghx

import (
	"strings"
	"testing"
)

// TestPRCreateArgs pins the `wt claim` draft-PR regression: PR creation must
// pass --head AND --base explicitly so it does not depend on the invoking
// cwd's current branch. `wt claim` runs from the main checkout (on the base
// branch); without --head, gh infers head=base → "no commits between base and
// base" → the draft PR silently never gets created (the bug this guards).
func TestPRCreateArgs(t *testing.T) {
	draft := strings.Join(PRCreateArgs(true, "feat-42-slug", "main", "WIP: #42", "body"), " ")
	for _, want := range []string{
		"pr create", "--head feat-42-slug", "--base main", "--draft", "--title WIP: #42", "--body body",
	} {
		if !strings.Contains(draft, want) {
			t.Errorf("PRCreateArgs(draft) missing %q\n got: %s", want, draft)
		}
	}

	// Non-draft must omit --draft (but still carry --head/--base).
	nd := strings.Join(PRCreateArgs(false, "b", "trunk", "t", "y"), " ")
	if strings.Contains(nd, "--draft") {
		t.Errorf("non-draft must omit --draft: %s", nd)
	}
	if !strings.Contains(nd, "--head b") || !strings.Contains(nd, "--base trunk") {
		t.Errorf("non-draft must still carry --head/--base: %s", nd)
	}
}

// TestHostFromRemoteURL pins the parsing behind the host-scoped auth check
// (#100). The scp-style form is the one worth care: it has no "://" and is
// distinguished from a local path only by a colon appearing before any slash.
func TestHostFromRemoteURL(t *testing.T) {
	cases := []struct{ url, want string }{
		{"git@github.com:owner/repo.git", "github.com"},
		{"https://github.com/owner/repo.git", "github.com"},
		{"http://github.com/owner/repo", "github.com"},
		{"ssh://git@ghe.example.com:2222/owner/repo.git", "ghe.example.com"},
		{"ssh://ghe.example.com/owner/repo.git", "ghe.example.com"},
		{"https://user:token@ghe.example.com/o/r", "ghe.example.com"},
		{"git@ghe.example.com:o/r", "ghe.example.com"},
		{"https://github.com", "github.com"},
		{"  git@github.com:o/r.git  ", "github.com"},

		// No host to scope to — the caller must fall back to gh's own default
		// rather than guess github.com, or it would ask about the wrong forge.
		{"/srv/git/repo.git", ""},
		{"../sibling-repo", ""},
		{"./repo", ""},
		{"", ""},
	}
	for _, c := range cases {
		if got := hostFromRemoteURL(c.url); got != c.want {
			t.Errorf("hostFromRemoteURL(%q) = %q, want %q", c.url, got, c.want)
		}
	}
}

// TestAuthStatusArgs is the actual #100 regression. Bare `gh auth status` exits
// non-zero when ANY configured host fails, so an unrelated unreachable host made
// `wt doctor` report "gh — found but NOT authenticated" on a machine whose
// github.com login was fine. The fix is that a known host MUST be passed through
// as --hostname; without it the check asks a broader question than it needs
// answered and cannot distinguish "you are broken" from "the check is broken".
func TestAuthStatusArgs(t *testing.T) {
	scoped := strings.Join(authStatusArgs("github.com"), " ")
	if scoped != "auth status --hostname github.com" {
		t.Errorf("host must be scoped: got %q", scoped)
	}

	// Unknown host: fall back to gh's default rather than inventing one. This is
	// the pre-#100 behaviour, deliberately retained ONLY for the can't-tell case.
	unscoped := strings.Join(authStatusArgs(""), " ")
	if unscoped != "auth status" {
		t.Errorf("unknown host must not invent a --hostname: got %q", unscoped)
	}

	// An enterprise host scopes to itself — the mirror-image false NEGATIVE the
	// old code had, where such a repo passed because github.com happened to be
	// authed while the host it actually needs was not.
	ghe := strings.Join(authStatusArgs("ghe.example.com"), " ")
	if !strings.Contains(ghe, "--hostname ghe.example.com") {
		t.Errorf("enterprise host must scope to itself: got %q", ghe)
	}
}
