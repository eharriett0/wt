package gitx

import "testing"

func TestDefaultBranchFromRef(t *testing.T) {
	cases := map[string]string{
		"origin/main":          "main",
		"origin/master":        "master",
		"origin/trunk":         "trunk",
		"origin/feature/x":     "feature/x",
		"main":                 "main",
		"":                     "",
		"  origin/develop  \n": "develop",
	}
	for in, want := range cases {
		if got := DefaultBranchFromRef(in); got != want {
			t.Errorf("DefaultBranchFromRef(%q) = %q, want %q", in, got, want)
		}
	}
}

func TestParseHunkRanges(t *testing.T) {
	// Synthetic `git diff -U0` headers → expected NEW-side ranges.
	cases := []struct {
		name string
		diff string
		want []LineRange
	}{
		{"single-line edit", "@@ -5 +5 @@ ctx", []LineRange{{5, 5}}},
		{"multi-line edit", "@@ -10,3 +10,4 @@", []LineRange{{10, 13}}},
		{"count omitted = 1", "@@ -1 +7 @@", []LineRange{{7, 7}}},
		{"pure addition", "@@ -5,0 +6,3 @@", []LineRange{{6, 8}}},
		// Pure deletion: git reports the surviving line before the gap; we span
		// [start, start+1] so an edit of the removed region overlaps.
		{"pure deletion spans gap", "@@ -2 +1,0 @@", []LineRange{{1, 2}}},
		{"non-header lines ignored", "diff --git a/x b/x\n+added\n@@ -3 +3 @@\n-removed", []LineRange{{3, 3}}},
		{"empty", "", nil},
	}
	for _, c := range cases {
		got := parseHunkRanges(c.diff)
		if len(got) != len(c.want) {
			t.Errorf("%s: got %+v, want %+v", c.name, got, c.want)
			continue
		}
		for i := range got {
			if got[i] != c.want[i] {
				t.Errorf("%s: got %+v, want %+v", c.name, got, c.want)
				break
			}
		}
	}
}
