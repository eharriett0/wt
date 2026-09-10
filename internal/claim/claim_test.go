package claim

import (
	"strings"
	"testing"

	"github.com/eharriett0/wt/internal/config"
)

// #157 (+review): non-interactive ALWAYS proceeds (agents never hang); the
// interactive y/N branch — the actual wrong-number catch — only y/yes proceeds.
func TestConfirmNewClaim(t *testing.T) {
	// non-interactive: proceeds regardless of what stdin would say
	if !confirmNewClaim("42", "some issue title", false, strings.NewReader("n\n")) {
		t.Error("non-interactive confirmNewClaim must proceed (true), never block a scripted claim")
	}
	// interactive: only y/yes (any case) proceed; everything else aborts
	for _, tc := range []struct {
		in   string
		want bool
	}{
		{"y\n", true}, {"yes\n", true}, {"Y\n", true}, {"  yes  \n", true},
		{"n\n", false}, {"no\n", false}, {"\n", false}, {"", false}, {"nope\n", false},
	} {
		if got := confirmNewClaim("42", "t", true, strings.NewReader(tc.in)); got != tc.want {
			t.Errorf("confirmNewClaim(interactive, %q) = %v, want %v", tc.in, got, tc.want)
		}
	}
}

// #156: the identity recorded in the active-work file must be the STABLE
// worktree-based coord.WindowID (matching wt doctor), not a per-shell hostname-PID.
func TestWindowID_StableAndMatchesContract(t *testing.T) {
	c := &config.Config{Root: "/home/u/worktree-x"}
	// WT_WINDOW wins — an explicit, restart-stable label
	t.Setenv("WT_WINDOW", "term-7")
	if got := windowID(c); got != "term-7" {
		t.Errorf("windowID with WT_WINDOW = %q, want term-7", got)
	}
	// unset → the worktree toplevel PATH (stable across shell restarts; what
	// coord.WindowID / wt doctor produce), never a hostname-PID
	t.Setenv("WT_WINDOW", "")
	if got := windowID(c); got != "/home/u/worktree-x" {
		t.Errorf("windowID = %q, want the stable worktree path /home/u/worktree-x", got)
	}
}

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
