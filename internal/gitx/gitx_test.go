package gitx

import (
	"reflect"
	"testing"
)

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

// span is a change of lines s..e; gapAt the insertion below line p (#199).
func span(s, e int) LineRange { return LineRange{Start: s, End: e} }
func gapAt(p int) LineRange   { return gapAfter(p) }

func TestParseHunkRanges(t *testing.T) {
	// Synthetic `git diff -U0` headers → expected NEW-side ranges.
	cases := []struct {
		name string
		diff string
		want []LineRange
	}{
		{"single-line edit", "@@ -5 +5 @@ ctx", []LineRange{span(5, 5)}},
		{"multi-line edit", "@@ -10,3 +10,4 @@", []LineRange{span(10, 13)}},
		{"count omitted = 1", "@@ -1 +7 @@", []LineRange{span(7, 7)}},
		{"pure addition", "@@ -5,0 +6,3 @@", []LineRange{span(6, 8)}},
		// Pure deletion: git reports the surviving line before the gap; we span
		// [start, start+1] so an edit of the removed region overlaps.
		{"pure deletion spans gap", "@@ -2 +1,0 @@", []LineRange{span(1, 2)}},
		{"non-header lines ignored", "diff --git a/x b/x\n+added\n@@ -3 +3 @@\n-removed", []LineRange{span(3, 3)}},
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
		{"single-line edit", "@@ -5 +5 @@ ctx", []LineRange{span(5, 5)}},
		{"multi-line edit", "@@ -10,3 +10,4 @@", []LineRange{span(10, 12)}},
		{"count omitted = 1", "@@ -7 +1 @@", []LineRange{span(7, 7)}},
		// Pure addition: 0 lines on the OLD side → an insertion into the gap after
		// line 5 (#199: it meets only edits of line 5 or 6, or another at that gap).
		{"pure addition is a gap", "@@ -5,0 +6,3 @@", []LineRange{gapAt(5)}},
		{"addition at the very top", "@@ -0,0 +1,2 @@", []LineRange{gapAt(0)}},
		{"pure deletion", "@@ -2,3 +1,0 @@", []LineRange{span(2, 4)}},
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

func TestAllZeroSHA(t *testing.T) {
	cases := map[string]bool{
		"0000000000000000000000000000000000000000":                         true, // 40-hex zero (SHA-1)
		"0000000000000000000000000000000000000000000000000000000000000000": true, // SHA-256
		"  0000000000000000000000000000000000000000  ":                     true, // trimmed
		"be912e0aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa":                        false,
		"0000000000000000000000000000000000000001":                         false,
		"": false, // empty is NOT the zero sentinel
	}
	for in, want := range cases {
		if got := AllZeroSHA(in); got != want {
			t.Errorf("AllZeroSHA(%q) = %v, want %v", in, got, want)
		}
	}
}

// `git merge-tree --write-tree --name-only -z` output (#200): NUL-terminated
// records, every path verbatim. The message records are the -z ones git 2.39
// writes ("<n paths>\0<path>\0<type>\0<message>\0").
func TestParseMergeTreeConflictPaths(t *testing.T) {
	const oid = "0123456789abcdef0123456789abcdef01234567"
	for _, tc := range []struct {
		name, out string
		want      []string
	}{
		// a clean merge exits 0 and never reaches the parser, but an OID alone is
		// tolerated
		{"tree OID only", oid + "\x00", nil},
		{"empty", "", nil},
		{"conflicts, then the messages after an empty record",
			oid + "\x00internal/hooks/hooks.go\x00internal/gitx/gitx.go\x00\x00" +
				"1\x00internal/hooks/hooks.go\x00Auto-merging\x00Auto-merging internal/hooks/hooks.go\n\x00" +
				"1\x00internal/hooks/hooks.go\x00CONFLICT (contents)\x00CONFLICT (content): Merge conflict in internal/hooks/hooks.go\n\x00",
			[]string{"internal/hooks/hooks.go", "internal/gitx/gitx.go"}},
		{"--no-messages: no empty record", oid + "\x00a.go\x00", []string{"a.go"}},
		{"names git would C-quote stay verbatim",
			oid + "\x00café.md\x00dír é/new\nline.md\x00 lead \"q\" \\.md\x00\x00",
			[]string{"café.md", "dír é/new\nline.md", " lead \"q\" \\.md"}},
	} {
		if got := parseMergeTreeConflictPaths(tc.out); !reflect.DeepEqual(got, tc.want) {
			t.Errorf("%s: paths = %q, want %q", tc.name, got, tc.want)
		}
	}
}

// splitNUL reads `git diff --name-only -z` (#200): every path verbatim, which
// a line read could not do (git C-quotes "unusual" names, and a trim eats a
// leading or trailing space).
func TestSplitNUL(t *testing.T) {
	for _, tc := range []struct {
		name, out string
		want      []string
	}{
		{"empty", "", nil},
		{"one", "a.go\x00", []string{"a.go"}},
		{"several, in git's order", "b.go\x00a/c.go\x00", []string{"b.go", "a/c.go"}},
		{"non-ASCII", "café.md\x00naïve/ü.txt\x00", []string{"café.md", "naïve/ü.txt"}},
		{"space, tab, quote, backslash, newline",
			"a b.md\x00tab\tname.md\x00q\"uote.md\x00back\\slash.md\x00new\nline.md\x00",
			[]string{"a b.md", "tab\tname.md", "q\"uote.md", "back\\slash.md", "new\nline.md"}},
		{"leading and trailing spaces kept", " lead.md\x00trail.md \x00", []string{" lead.md", "trail.md "}},
		{"a missing final NUL keeps the last path", "a.go\x00b.go", []string{"a.go", "b.go"}},
	} {
		if got := splitNUL(tc.out); !reflect.DeepEqual(got, tc.want) {
			t.Errorf("%s: splitNUL = %q, want %q", tc.name, got, tc.want)
		}
	}
}

// parsePorcelainZ reads `git status --porcelain -z` (#200). A rename or copy
// is "XY <new>\0<orig>\0", new FIRST (the line format's "orig -> new"
// reversed); both are wanted (#28).
func TestParsePorcelainZ(t *testing.T) {
	for _, tc := range []struct {
		name, out string
		want      []string
	}{
		{"clean", "", nil},
		{"modified, staged, untracked, deleted",
			" M a.go\x00M  b.go\x00?? new/c.go\x00 D d.go\x00AM e.go\x00",
			[]string{"a.go", "b.go", "new/c.go", "d.go", "e.go"}},
		{"staged rename: new, then the original",
			"R  pkg/README.md\x00README.md\x00", []string{"pkg/README.md", "README.md"}},
		{"rename then more edits in the worktree", "RM new.go\x00old.go\x00", []string{"new.go", "old.go"}},
		{"intent-to-add rename, in the worktree column",
			" R moved.md\x00a b.md\x00", []string{"moved.md", "a b.md"}},
		{"copy", "C  copy.go\x00orig.go\x00", []string{"copy.go", "orig.go"}},
		{"the original is not read as a status record",
			// "M  x" as an ORIGINAL path would otherwise parse as status "M " path "x"
			"R  y.go\x00M  x\x00 M z.go\x00", []string{"y.go", "M  x", "z.go"}},
		{"a short original path", "R  long-name.go\x00a\x00?? b\x00", []string{"long-name.go", "a", "b"}},
		{"unmerged", "UU both.go\x00AA added.go\x00", []string{"both.go", "added.go"}},
		{"names git would C-quote (porcelain quotes a space too)",
			" M café.md\x00?? dír é/ü.md\x00M  q\"uote.md\x00 M back\\slash.md\x00?? tab\tname.md\x00 M new\nline.md\x00",
			[]string{"café.md", "dír é/ü.md", "q\"uote.md", "back\\slash.md", "tab\tname.md", "new\nline.md"}},
		{"leading and trailing spaces kept", " M  lead.md\x00?? trail.md \x00", []string{" lead.md", "trail.md "}},
		{"a rename of odd names", "R  dír é/moved ü.md\x00q\"uote.md\x00", []string{"dír é/moved ü.md", "q\"uote.md"}},
		{"a truncated rename keeps its new path", "R  new.go\x00", []string{"new.go"}},
	} {
		if got := parsePorcelainZ(tc.out); !reflect.DeepEqual(got, tc.want) {
			t.Errorf("%s: parsePorcelainZ = %q, want %q", tc.name, got, tc.want)
		}
	}
}

func TestScopedEnv(t *testing.T) {
	t.Setenv("GIT_DIR", "/some/.git")
	t.Setenv("GIT_INDEX_FILE", "/some/.git/index")
	t.Setenv("GIT_WORK_TREE", "/some")
	t.Setenv("GIT_LITERAL_PATHSPECS", "1") // #204: it would turn literalPath's magic into part of the name
	t.Setenv("GIT_ICASE_PATHSPECS", "1")
	t.Setenv("WT_KEEP_ME", "yes")
	env := scopedEnv()
	has := func(prefix string) bool {
		for _, kv := range env {
			if len(kv) >= len(prefix) && kv[:len(prefix)] == prefix {
				return true
			}
		}
		return false
	}
	for _, dropped := range []string{"GIT_DIR=", "GIT_INDEX_FILE=", "GIT_WORK_TREE=", "GIT_LITERAL_PATHSPECS=", "GIT_ICASE_PATHSPECS="} {
		if has(dropped) {
			t.Errorf("scopedEnv did not strip %s", dropped)
		}
	}
	if !has("WT_KEEP_ME=") {
		t.Error("scopedEnv wrongly stripped a non-git var")
	}
}
