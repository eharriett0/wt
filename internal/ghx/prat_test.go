package ghx

import "testing"

// parsePRAt reads ResolvePRIn's "<number> <url>" into the PR's repository
// (#228), and refuses anything that isn't that PR's URL.
func TestParsePRAt(t *testing.T) {
	for _, tc := range []struct {
		out         string
		ok          bool
		owner, name string
	}{
		{"5 https://github.com/other/repo/pull/5", true, "other", "repo"},
		{"12 https://git.acme.test/team/svc/pull/12", true, "team", "svc"},
		{"5 https://github.com/other/repo/pull/6", false, "", ""},
		{"5 https://github.com/other/pull/5", false, "", ""},
		{"5 https://github.com/other/repo/issues/5", false, "", ""},
		{"null null", false, "", ""},
		{"", false, "", ""},
	} {
		p, err := parsePRAt(tc.out)
		if (err == nil) != tc.ok || p.Owner != tc.owner || p.Name != tc.name {
			t.Errorf("parsePRAt(%q) = %+v, %v; want ok=%v %s/%s", tc.out, p, err, tc.ok, tc.owner, tc.name)
		}
	}
	p, _ := parsePRAt("5 https://github.com/other/repo/pull/5")
	if got := p.IssueURL("3"); got != "https://github.com/other/repo/issues/3" {
		t.Errorf("IssueURL = %q", got)
	}
	if p.Repo() != "other/repo" {
		t.Errorf("Repo = %q", p.Repo())
	}
}
