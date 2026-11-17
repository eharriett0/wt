package gitx

import (
	"os"
	"path/filepath"
	"reflect"
	"testing"
)

// parseDeletedRanges reads the lines each -U0 hunk removed or replaced, on
// the old side (#237); a pure insertion deletes nothing.
func TestParseDeletedRanges(t *testing.T) {
	diff := "diff --git a/f b/f\n--- a/f\n+++ b/f\n" +
		"@@ -2 +2 @@\n-a\n+A\n" + // one line replaced
		"@@ -5,3 +4,0 @@\n-b\n-c\n-d\n" + // three deleted
		"@@ -9,0 +8,2 @@\n+e\n+f\n" // inserted: no deletion
	want := []LineRange{{Start: 2, End: 2}, {Start: 5, End: 7}}
	if got := parseDeletedRanges(diff); !reflect.DeepEqual(got, want) {
		t.Errorf("parseDeletedRanges = %v, want %v", got, want)
	}
}

const docBase = "# Plan\n## Alpha\na1\na2\n## Beta\nb1\nb2\nb3\n"

func docRepo(t *testing.T) string {
	t.Helper()
	dir := gitRepo(t)
	writeFile(t, dir, "PLAN.md", docBase)
	runGit(t, dir, "add", "-A")
	runGit(t, dir, "commit", "-qm", "base")
	return dir
}

// ChangedDocEdits is ChangedRangesNew's one diff with both sides (#237): the
// new-frame ranges as before, the lines deleted in the old copy's numbers,
// insertions' old-side gaps, and that copy (as stored) when it holds a claim.
func TestChangedDocEdits(t *testing.T) {
	dir := docRepo(t)
	for _, tc := range []struct {
		name          string
		content       *string // nil: the file is removed
		deleted, gaps []LineRange
	}{
		{"Beta deleted", strPtr("# Plan\n## Alpha\na1\na2\n"), []LineRange{{Start: 5, End: 8}}, nil},
		{"emptied", strPtr(""), []LineRange{{Start: 1, End: 8}}, nil},
		{"removed", nil, []LineRange{{Start: 1, End: 8}}, nil},
		{"a line inserted only", strPtr("# Plan\n## Alpha\na1\nNEW\na2\n## Beta\nb1\nb2\nb3\n"), nil, []LineRange{gapAfter(3)}},
		{"a line modified", strPtr("# Plan\n## Alpha\nXa1\na2\n## Beta\nb1\nb2\nb3\n"), []LineRange{{Start: 3, End: 3}}, nil},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if tc.content == nil {
				if err := os.Remove(filepath.Join(dir, "PLAN.md")); err != nil {
					t.Fatal(err)
				}
			} else {
				writeFile(t, dir, "PLAN.md", *tc.content)
			}
			defer writeFile(t, dir, "PLAN.md", docBase)
			e := ChangedDocEdits(dir, "main", "PLAN.md")
			if !reflect.DeepEqual(e.Deleted, tc.deleted) || !reflect.DeepEqual(e.Gaps, tc.gaps) || e.All {
				t.Errorf("Deleted = %v, Gaps = %v, All = %v; want %v, %v, false", e.Deleted, e.Gaps, e.All, tc.deleted, tc.gaps)
			}
			if wantOld := map[bool]string{true: docBase, false: ""}[tc.deleted != nil || tc.gaps != nil]; e.Old != wantOld {
				t.Errorf("Old = %q, want %q", e.Old, wantOld)
			}
			if got := ChangedRangesNew(dir, "main", "PLAN.md"); !reflect.DeepEqual(got, e.New) {
				t.Errorf("New = %v, ChangedRangesNew = %v", e.New, got)
			}
		})
	}
}

// The pending measurement IS the written one, both sides (#226, #237).
func TestChangedDocEditsWith_IsChangedDocEditsOnceWritten(t *testing.T) {
	dir := docRepo(t)
	for _, content := range []string{
		"# Plan\n## Alpha\na1\na2\n",
		"",
		"# Plan\n## Alpha\nXa1\na2\n## Beta two\nb1\nb3\n",
		docBase,
	} {
		pending, ok := ChangedDocEditsWith(dir, "main", "PLAN.md", []byte(content))
		if !ok {
			t.Fatalf("%q: not measured", content)
		}
		writeFile(t, dir, "PLAN.md", content)
		written := ChangedDocEdits(dir, "main", "PLAN.md")
		writeFile(t, dir, "PLAN.md", docBase)
		if !reflect.DeepEqual(pending, written) {
			t.Errorf("%q:\n pending %+v\n written %+v", content, pending, written)
		}
	}
}

// A measurement that fails reads as every section (All), never as fewer:
// the diff failing, or the old copy it deleted from unreadable (#237).
func TestChangedDocEdits_FailureIsAll(t *testing.T) {
	dir := docRepo(t)
	writeFile(t, dir, "PLAN.md", "# Plan\n## Alpha\na1\na2\n")
	failGit(t, func(args []string) bool { return len(args) > 0 && args[0] == "cat-file" && args[1] == "blob" })
	if e := ChangedDocEdits(dir, "main", "PLAN.md"); !e.All || len(e.Deleted) == 0 {
		t.Errorf("old copy unreadable: %+v, want All with the deletions", e)
	}
	if e, ok := ChangedDocEditsWith(dir, "main", "PLAN.md", []byte("# Plan\n")); !ok || !e.All {
		t.Errorf("pending, old copy unreadable: %+v, %v; want All", e, ok)
	}
	failGit(t, func(args []string) bool { return len(args) > 0 && args[0] == "diff" })
	if e := ChangedDocEdits(dir, "main", "PLAN.md"); !e.All || !reflect.DeepEqual(e.New, wholeFile) {
		t.Errorf("diff failing: %+v, want All and the whole file", e)
	}
}

func strPtr(s string) *string { return &s }

// A window behind base is diffed from its merge base, so its deletions and
// insertion gaps are numbered in the merge base's copy, and Old is that copy,
// not base's (#237 review: reading base's copy survived every test).
func TestChangedDocEdits_BehindReadsTheMergeBase(t *testing.T) {
	dir := docRepo(t)
	wt := filepath.Join(t.TempDir(), "wa")
	runGit(t, dir, "worktree", "add", "-q", "-b", "wa", wt)
	writeFile(t, dir, "PLAN.md", "# Plan\n## Alpha\na1\nM1\nM2\nM3\nM4\na2\n## Beta\nb1\nb2\nb3\n")
	runGit(t, dir, "commit", "-qam", "main inserts in Alpha")
	writeFile(t, wt, "PLAN.md", "# Plan\n## Alpha\na1\na2\n## Delta\nd1\n")
	e := ChangedDocEdits(wt, "main", "PLAN.md")
	if e.Old != docBase || !reflect.DeepEqual(e.Deleted, []LineRange{{Start: 5, End: 8}}) {
		t.Errorf("behind: Old = %q, Deleted = %v; want the merge base's copy and [5-8]", e.Old, e.Deleted)
	}
	p, ok := ChangedDocEditsWith(wt, "main", "PLAN.md", []byte("# Plan\n## Alpha\na1\na2\n## Delta\nd1\n"))
	if !ok || !reflect.DeepEqual(p, e) {
		t.Errorf("pending, behind: %+v, want %+v", p, e)
	}
}

// parseInsertGaps reads each pure insertion's old-side gap (#237 review).
func TestParseInsertGaps(t *testing.T) {
	diff := "@@ -0,0 +1 @@\n+top\n@@ -2 +3 @@\n-a\n+A\n@@ -9,0 +10,2 @@\n+e\n+f\n"
	want := []LineRange{gapAfter(0), gapAfter(9)}
	if got := parseInsertGaps(diff); !reflect.DeepEqual(got, want) {
		t.Errorf("parseInsertGaps = %v, want %v", got, want)
	}
}
