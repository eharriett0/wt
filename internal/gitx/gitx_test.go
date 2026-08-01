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

func TestParseHunkRangesOld(t *testing.T) {
	// Same synthetic headers, but graded on the OLD (base) side (#29).
	cases := []struct {
		name string
		diff string
		want []LineRange
	}{
		{"single-line edit", "@@ -5 +5 @@ ctx", []LineRange{{5, 5}}},
		{"multi-line edit", "@@ -10,3 +10,4 @@", []LineRange{{10, 12}}},
		{"count omitted = 1", "@@ -7 +1 @@", []LineRange{{7, 7}}},
		// Pure addition: 0 lines on the OLD side → span the insertion-gap neighbors.
		{"pure addition spans gap", "@@ -5,0 +6,3 @@", []LineRange{{5, 6}}},
		{"pure deletion", "@@ -2,3 +1,0 @@", []LineRange{{2, 4}}},
		{"empty", "", nil},
	}
	for _, c := range cases {
		got := parseHunkRangesOld(c.diff)
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

// The #29 fix in miniature: window A inserted lines above a shared region so its
// NEW-side numbers are shifted; both windows edit BASE line 200. New-side coords
// grade them disjoint (the latent false-negative); base-side coords overlap.
func TestHunkRanges_BaseFrameCatchesShiftedConflict(t *testing.T) {
	aHunk := "@@ -200 +300 @@" // A edits base 200; +100 lines inserted above → new 300
	bHunk := "@@ -200 +200 @@" // B edits base 200, no shift

	aNew, bNew := parseHunkRanges(aHunk), parseHunkRanges(bHunk)
	if aNew[0].Overlaps(bNew[0]) {
		t.Fatal("precondition: NEW-side ranges should be disjoint (that's the bug)")
	}
	aOld, bOld := parseHunkRangesOld(aHunk), parseHunkRangesOld(bHunk)
	if !aOld[0].Overlaps(bOld[0]) {
		t.Fatalf("base-side ranges must overlap: A=%+v B=%+v", aOld, bOld)
	}
}
