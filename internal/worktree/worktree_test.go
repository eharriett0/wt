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

func TestReapVerdict(t *testing.T) {
	cases := []struct {
		name                                       string
		unshipped                                  int
		cherryFailed, prMerged, hasUpstream, grace bool
		want                                       bool
	}{
		// The #61 data-loss cases — must all be KEEP (false):
		{"fresh commitless wt new (no upstream)", 0, false, false, false, false, false},
		{"within grace window", 0, false, false, true, true, false},
		{"merged PR but still in grace", 0, false, true, true, true, false},
		{"pushed, unshipped commits", 2, false, false, true, false, false},
		{"no upstream even if cherry says 0", 0, false, false, false, false, false},
		// Legit reaps (true):
		{"merged PR, past grace", 0, false, true, true, false, true},
		{"pushed + patch-equivalent on base", 0, false, false, true, false, true},
		// cherry failed, no merged PR, has upstream → can't prove shipped → keep
		{"cherry failed, no PR", 0, true, false, true, false, false},
	}
	for _, tc := range cases {
		got := ReapVerdict(tc.unshipped, tc.cherryFailed, tc.prMerged, tc.hasUpstream, tc.grace)
		if got != tc.want {
			t.Errorf("%s: ReapVerdict = %v, want %v", tc.name, got, tc.want)
		}
	}
}

func TestStaleIndexReap(t *testing.T) {
	cases := []struct {
		name                                 string
		prState                              string
		prOK, dirty, staleIndex, grace, want bool
	}{
		// Fires: merged/closed PR + dirty index + flag + past grace.
		{"merged + dirty index + flag → reap", "MERGED", true, true, true, false, true},
		{"closed + dirty index + flag → reap", "CLOSED", true, true, true, false, true},
		// Must NOT fire — the safety gates:
		{"flag off → never (even merged+dirty)", "MERGED", true, true, false, false, false},
		{"clean index → nothing to reclaim", "MERGED", true, false, true, false, false},
		{"within grace → protected", "MERGED", true, true, true, true, false},
		{"open PR → active, keep", "OPEN", true, true, true, false, false},
		{"no PR resolved → not shippable, keep", "", false, true, true, false, false},
		{"unknown state → keep", "SOMETHING", true, true, true, false, false},
	}
	for _, tc := range cases {
		got := StaleIndexReap(tc.prState, tc.prOK, tc.dirty, tc.staleIndex, tc.grace)
		if got != tc.want {
			t.Errorf("%s: StaleIndexReap(%q,%v,%v,%v,%v) = %v, want %v",
				tc.name, tc.prState, tc.prOK, tc.dirty, tc.staleIndex, tc.grace, got, tc.want)
		}
	}
}
