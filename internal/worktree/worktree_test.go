package worktree

import "testing"

func TestShippedVerdict(t *testing.T) {
	// merged PR → shipped regardless of cherry (the #37 core)
	if !ShippedVerdict(5, false, true) {
		t.Error("merged PR should be shipped even with unshipped cherry commits")
	}
	if !ShippedVerdict(3, true, true) {
		t.Error("merged PR should be shipped even when cherry FAILED")
	}
	// no merged PR: cherry decides
	if !ShippedVerdict(0, false, false) {
		t.Error("cherry==0, no PR → shipped")
	}
	if ShippedVerdict(2, false, false) {
		t.Error("cherry!=0, no PR → not shipped")
	}
	if ShippedVerdict(0, true, false) {
		t.Error("cherry failed + no PR → can't tell → not shipped")
	}
}

func TestIsAbandonedBranch(t *testing.T) {
	wip := []string{"WIP: claim #42 — foo"}
	real := []string{"WIP: claim #42 — foo", "feat: real work"}
	cases := []struct {
		name             string
		subjects         []string
		prOpen, prMerged bool
		want             bool
	}{
		{"empty, no PR", nil, false, false, true},
		{"wip-only, no PR", wip, false, false, true},
		{"wip-only but open PR", wip, true, false, false},
		{"wip-only but merged PR", wip, false, true, false},
		{"has real commit", real, false, false, false},
	}
	for _, tc := range cases {
		if got := IsAbandonedBranch(tc.subjects, tc.prOpen, tc.prMerged); got != tc.want {
			t.Errorf("%s: IsAbandonedBranch = %v, want %v", tc.name, got, tc.want)
		}
	}
}
