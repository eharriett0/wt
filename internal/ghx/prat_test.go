package ghx

import (
	"strings"
	"testing"
)

// parsePRAt reads ResolvePRIn's "<number> <url>" into the PR's repository
// (#228), and the head repository on the next line (#228 review), and refuses
// anything that isn't that PR's URL.
func TestParsePRAt(t *testing.T) {
	for _, tc := range []struct {
		out                 string
		ok                  bool
		host, owner, name   string
		headOwner, headName string
	}{
		{"5 https://github.com/other/repo/pull/5\nother/repo", true, "github.com", "other", "repo", "other", "repo"},
		{"5 https://github.com/other/repo/pull/5\nfork-owner/repo-fork", true, "github.com", "other", "repo", "fork-owner", "repo-fork"},
		{"12 https://git.acme.test/team/svc/pull/12\nteam/svc", true, "git.acme.test", "team", "svc", "team", "svc"},
		// A deleted fork: gh prints "/" (or no head line at all).
		{"5 https://github.com/other/repo/pull/5\n/", true, "github.com", "other", "repo", "", ""},
		{"5 https://github.com/other/repo/pull/5", true, "github.com", "other", "repo", "", ""},
		{"5 https://github.com/other/repo/pull/5\nowner/", true, "github.com", "other", "repo", "", ""},
		{"5 https://github.com/other/repo/pull/5\na/b/c", true, "github.com", "other", "repo", "", ""},
		{"5 https://github.com/other/repo/pull/6\nother/repo", false, "", "", "", "", ""},
		{"5 https://github.com/other/pull/5", false, "", "", "", "", ""},
		{"5 https://github.com/other/repo/issues/5", false, "", "", "", "", ""},
		{"null null\n/", false, "", "", "", "", ""},
		{"", false, "", "", "", "", ""},
	} {
		p, err := parsePRAt(tc.out)
		if (err == nil) != tc.ok || p.Host != tc.host || p.Owner != tc.owner || p.Name != tc.name ||
			p.HeadOwner != tc.headOwner || p.HeadName != tc.headName {
			t.Errorf("parsePRAt(%q) = %+v, %v; want ok=%v %s %s/%s head %s/%s",
				tc.out, p, err, tc.ok, tc.host, tc.owner, tc.name, tc.headOwner, tc.headName)
		}
	}
	p, _ := parsePRAt("5 https://github.com/other/repo/pull/5\nfork/repo")
	if got := p.IssueURL("3"); got != "https://github.com/other/repo/issues/3" {
		t.Errorf("IssueURL = %q", got)
	}
	if p.Repo() != "other/repo" || p.HeadRepo() != "fork/repo" {
		t.Errorf("Repo = %q, HeadRepo = %q", p.Repo(), p.HeadRepo())
	}
	if (PRAt{}).HeadRepo() != "" {
		t.Error("an unknown head repository must read as none")
	}
}

func TestIsPRURL(t *testing.T) {
	for s, want := range map[string]bool{
		"https://github.com/o/r/pull/5":           true,
		"https://github.com/o/r/pull/5/files":     true,
		"http://git.acme.test/o/r/pull/12":        true,
		" https://github.com/o/r/pull/5 ":         true,
		"https://github.com/o/r/pull/0":           false,
		"https://github.com/o/r/pull/x":           false,
		"https://github.com/o/r/issues/5":         false,
		"https://github.com/pull/5":               false,
		"github.com/o/r/pull/5":                   false,
		"ftp://github.com/o/r/pull/5":             false,
		"5":                                       false,
		"#5":                                      false,
		"feat/pull/5":                             false,
		"https://github.com/o/r/pull/5?w=1":       true,
		"https://github.com/o/r/pull/5#discussio": true,
		// #228 review: gh's finder matches the path as a prefix, so these
		// are PR 5 to gh, and must be resolved; a path with anything before
		// the owner is not a PR URL to gh (it reads the selector as a branch).
		"https://github.com/o/r/pull/5.diff":  true,
		"https://github.com/o/r/pull/5.patch": true,
		"https://github.com/o/r/pull/5abc":    true,
		"https://github.com/x/o/r/pull/5":     false,
		"https://github.com/o/r/blob/pull/5":  false,
	} {
		if got := IsPRURL(s); got != want {
			t.Errorf("IsPRURL(%q) = %v, want %v", s, got, want)
		}
	}
}

// The GraphQL reads that take a PR's URL go to that URL's host (#228 review:
// bare, gh asks its default host, which answers a GitHub Enterprise URL with a
// null resource).
func TestWithURLHost(t *testing.T) {
	base := []string{"api", "graphql"}
	if got := strings.Join(withURLHost(base, " https://git.acme.test/o/r/pull/5\n"), " "); got != "api graphql --hostname git.acme.test" {
		t.Errorf("GHES URL: %q", got)
	}
	if got := strings.Join(withURLHost(base, "not a url"), " "); got != "api graphql" {
		t.Errorf("no host: %q, want no --hostname", got)
	}
}

func TestResolveWhere(t *testing.T) {
	for _, tc := range []struct{ pr, repo, want string }{
		{"5", "o/r", "#5 in o/r"},
		{"#5", "o/r", "#5 in o/r"},
		{"https://github.com/o/r/pull/5", "", "https://github.com/o/r/pull/5"},
		{"https://github.com/o/r/pull/5", "x/y", "https://github.com/o/r/pull/5 in x/y"},
	} {
		if got := resolveWhere(tc.pr, tc.repo); got != tc.want {
			t.Errorf("resolveWhere(%q, %q) = %q, want %q", tc.pr, tc.repo, got, tc.want)
		}
	}
}

func TestSelectorHost(t *testing.T) {
	t.Setenv("GH_HOST", "")
	for _, tc := range []struct{ pr, repo, want string }{
		{"https://git.acme.test/o/r/pull/5", "", "git.acme.test"},
		{"https://git.acme.test/o/r/pull/5", "other/repo", "git.acme.test"},
		{"5", "git.acme.test/o/r", "git.acme.test"},
		{"5", "o/r", "github.com"},
	} {
		if got := selectorHost(tc.pr, tc.repo); got != tc.want {
			t.Errorf("selectorHost(%q, %q) = %q, want %q", tc.pr, tc.repo, got, tc.want)
		}
	}
	t.Setenv("GH_HOST", "git.acme.test")
	if got := selectorHost("5", "o/r"); got != "git.acme.test" {
		t.Errorf("GH_HOST set: %q", got)
	}
}
