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
