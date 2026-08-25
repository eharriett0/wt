package collide

import "testing"

// mergedVerdict is the fail-safe core of the #109 already-merged detection:
// merged only when the worktree (and the index, if the path is staged) is
// byte-identical to upstream; unknown when upstream or the worktree blob can't
// be read (so the caller keeps the collision HIGH — never clear on can't-tell).
func TestMergedVerdict(t *testing.T) {
	const up, other = "aaaaaaaa", "bbbbbbbb"
	cases := []struct {
		name                  string
		up                    string
		upOK                  bool
		wt                    string
		wtOK                  bool
		staged                string
		stagedOK              bool
		wantMerged, wantKnown bool
	}{
		{"worktree + index both equal upstream → already merged", up, true, up, true, up, true, true, true},
		{"a conflict path with NO index entry (staged deletion) → unknown, keep HIGH", up, true, up, true, "", false, false, false},
		{"worktree differs from upstream → real content", up, true, other, true, up, true, false, true},
		{"worktree equals but index differs → real content", up, true, up, true, other, true, false, true},
		{"no upstream ref for the path → unknown (keep HIGH)", "", false, up, true, up, true, false, false},
		{"worktree blob unreadable → unknown (keep HIGH)", up, true, "", false, up, true, false, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			m, k := mergedVerdict(tc.up, tc.upOK, tc.wt, tc.wtOK, tc.staged, tc.stagedOK)
			if m != tc.wantMerged || k != tc.wantKnown {
				t.Fatalf("mergedVerdict = (merged=%v, known=%v), want (merged=%v, known=%v)",
					m, k, tc.wantMerged, tc.wantKnown)
			}
		})
	}
}
