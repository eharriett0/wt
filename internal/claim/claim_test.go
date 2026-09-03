package claim

import "testing"

func TestSlugFromTitle(t *testing.T) {
	cases := map[string]string{
		"Add a logout button":             "add-a-logout-button",
		"Fix #123: the THING is Broken!!": "fix-123-the-thing-is-broken",
		"   leading/trailing   ":          "leading-trailing",
		"CamelCase With Spaces":           "camelcase-with-spaces",
		"a______b":                        "a-b",
		"":                                "",
		"!!!":                             "",
		"this title is intentionally very long and should be truncated at forty": "this-title-is-intentionally-very-long-an",
	}
	for in, want := range cases {
		if got := SlugFromTitle(in); got != want {
			t.Errorf("SlugFromTitle(%q) = %q, want %q (len %d)", in, got, want, len(got))
		}
	}
}

func TestSlugFromTitle_LenCap(t *testing.T) {
	got := SlugFromTitle("this title is intentionally very long and should be truncated at forty")
	if len(got) > 40 {
		t.Errorf("slug len %d exceeds 40: %q", len(got), got)
	}
}

func TestBranchName(t *testing.T) {
	if got := BranchName("feat-", "918", "do-the-thing"); got != "feat-918-do-the-thing" {
		t.Errorf("BranchName = %q", got)
	}
	if got := BranchName("feat-", "918", ""); got != "feat-918" {
		t.Errorf("empty-slug BranchName = %q, want feat-918 (no trailing dash)", got)
	}
	if got := BranchName("wip-", "5", "x"); got != "wip-5-x" {
		t.Errorf("custom prefix BranchName = %q", got)
	}
}

// #134: issueFromBranch inverts BranchName so `wt adopt` can key the active-work
// record by the issue a PR branch encodes — and returns "" (record-by-branch)
// when the branch isn't issue-shaped.
func TestIssueFromBranch(t *testing.T) {
	cases := []struct {
		prefix, branch, want string
	}{
		{"feat-", "feat-134-claim-dup", "134"}, // prefix + issue + slug
		{"feat-", "feat-134", "134"},           // prefix + issue, no slug
		{"feat-", "feat-9", "9"},
		{"feat-", "spike/x", ""},        // non-issue branch → "" (adopt keys by branch)
		{"feat-", "hotfix-2-thing", ""}, // different prefix, no leading digits after trim
		{"", "51-bare", "51"},           // empty prefix, issue-led branch
		{"feat-", "feat-abc", ""},       // prefix but no digits
	}
	for _, c := range cases {
		if got := issueFromBranch(c.prefix, c.branch); got != c.want {
			t.Errorf("issueFromBranch(%q, %q) = %q, want %q", c.prefix, c.branch, got, c.want)
		}
	}
}
