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
