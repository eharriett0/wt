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
// and that copy (as stored) when anything was deleted.
func TestChangedDocEdits(t *testing.T) {
	dir := docRepo(t)
	for _, tc := range []struct {
		name    string
		content *string // nil: the file is removed
		deleted []LineRange
	}{
		{"Beta deleted", strPtr("# Plan\n## Alpha\na1\na2\n"), []LineRange{{Start: 5, End: 8}}},
		{"emptied", strPtr(""), []LineRange{{Start: 1, End: 8}}},
		{"removed", nil, []LineRange{{Start: 1, End: 8}}},
		{"a line inserted only", strPtr("# Plan\n## Alpha\na1\nNEW\na2\n## Beta\nb1\nb2\nb3\n"), nil},
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
			if !reflect.DeepEqual(e.Deleted, tc.deleted) || e.All {
				t.Errorf("Deleted = %v, All = %v; want %v, false", e.Deleted, e.All, tc.deleted)
			}
			if wantOld := map[bool]string{true: docBase, false: ""}[tc.deleted != nil]; e.Old != wantOld {
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
