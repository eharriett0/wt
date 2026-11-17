package merge

import (
	"strings"
	"testing"
)

// gh takes a PR as a number or a URL; the PR-head fetch needs the number
// (#230 re-review: a URL got no fetch and a broken fetch command).
func TestPRNumber(t *testing.T) {
	for _, tc := range []struct{ pr, want string }{
		{"5", "5"},
		{"https://github.com/acme/repo/pull/5", "5"},
		{"https://github.com/acme/repo/pull/5/", "5"},
		{"feat-x", ""},
		{"", ""},
	} {
		if got := PRNumber(tc.pr); got != tc.want {
			t.Errorf("PRNumber(%q) = %q, want %q", tc.pr, got, tc.want)
		}
	}
	const head = "1111111111111111111111111111111111111111"
	if got := KeptLaneMessage(TipUnknown, "feat-x", "https://github.com/acme/repo/pull/5", -1, "t", head); !strings.Contains(got, "`git fetch origin refs/pull/5/head`") {
		t.Errorf("a PR given as a URL: %q, want the fetch command for PR 5", got)
	}
	if got := KeptLaneMessage(TipUnknown, "feat-x", "feat-x", -1, "t", head); strings.Contains(got, "refs/pull/") {
		t.Errorf("no PR number: %q, want no fetch command", got)
	}
}
