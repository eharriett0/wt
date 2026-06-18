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
