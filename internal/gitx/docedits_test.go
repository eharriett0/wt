package gitx

import (
	"os"
	"path/filepath"
	"reflect"
	"strings"
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

// A window behind base also carries its edits in base's frame, with base's
// copy (#247): base renamed or added a heading after it forked, so its own
// copies put its edits under headings a current window's copy lacks. The
// claims are ChangedRanges' (mapHunksToBase), an insertion as a Gap.
func TestChangedDocEdits_BehindAlsoInBasesFrame(t *testing.T) {
	for _, tc := range []struct {
		name, main, mine string
		base             []LineRange
	}{
		{"main renames Beta; wa edits b2",
			"# Plan\n## Alpha\na1\na2\n## Beta two\nb1\nb2\nb3\n",
			"# Plan\n## Alpha\na1\na2\n## Beta\nb1\nXb2\nb3\n",
			[]LineRange{{Start: 7, End: 7}}},
		{"main splits Beta before b2; wa edits b3",
			"# Plan\n## Alpha\na1\na2\n## Beta\nb1\n## Split\nb2\nb3\n",
			"# Plan\n## Alpha\na1\na2\n## Beta\nb1\nb2\nXb3\n",
			[]LineRange{{Start: 9, End: 9}}},
		{"main splits Beta; wa inserts after b3",
			"# Plan\n## Alpha\na1\na2\n## Beta\nb1\n## Split\nb2\nb3\n",
			"# Plan\n## Alpha\na1\na2\n## Beta\nb1\nb2\nb3\nb4\n",
			[]LineRange{{Start: 9, End: 10, Gap: true}}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			dir := docRepo(t)
			wt := filepath.Join(t.TempDir(), "wa")
			runGit(t, dir, "worktree", "add", "-q", "-b", "wa", wt)
			writeFile(t, dir, "PLAN.md", tc.main)
			runGit(t, dir, "commit", "-qam", "main moves on")
			writeFile(t, wt, "PLAN.md", tc.mine)
			e := ChangedDocEdits(wt, "main", "PLAN.md")
			if !reflect.DeepEqual(e.Base, tc.base) || e.BaseCopy != tc.main || e.All {
				t.Errorf("Base = %v, BaseCopy = %q, All = %v; want %v, main's copy, false", e.Base, e.BaseCopy, e.All, tc.base)
			}
			if e.Old != docBase {
				t.Errorf("Old = %q, want the merge base's copy still", e.Old)
			}
			if p, ok := ChangedDocEditsWith(wt, "main", "PLAN.md", []byte(tc.mine)); !ok || !reflect.DeepEqual(p, e) {
				t.Errorf("pending: %+v, want %+v", p, e)
			}
		})
	}
}

// Only a behind window gets base-frame claims, and only while base has the
// doc; base's own diff failing is All (#247).
func TestChangedDocEdits_BaseClaimsOnlyWhenBehind(t *testing.T) {
	dir := docRepo(t)
	writeFile(t, dir, "PLAN.md", "# Plan\n## Alpha\nXa1\na2\n## Beta\nb1\nb2\nb3\n")
	if e := ChangedDocEdits(dir, "main", "PLAN.md"); e.Base != nil || e.BaseCopy != "" {
		t.Errorf("up to date with base: Base = %v, BaseCopy = %q; want none", e.Base, e.BaseCopy)
	}
	writeFile(t, dir, "PLAN.md", docBase)
	wt := filepath.Join(t.TempDir(), "wa")
	runGit(t, dir, "worktree", "add", "-q", "-b", "wa", wt)
	runGit(t, dir, "rm", "-q", "PLAN.md")
	runGit(t, dir, "commit", "-qm", "main deletes the doc")
	writeFile(t, wt, "PLAN.md", "# Plan\n## Alpha\nXa1\na2\n## Beta\nb1\nb2\nb3\n")
	if e := ChangedDocEdits(wt, "main", "PLAN.md"); e.Base != nil || e.All || len(e.New) == 0 {
		t.Errorf("base deleted the doc: %+v; want the window's own edits, no base claims, not All", e)
	}
	runGit(t, dir, "checkout", "-q", "HEAD~1", "--", "PLAN.md")
	writeFile(t, dir, "PLAN.md", "# Plan\n## Alpha\na1\na2\n## Beta two\nb1\nb2\nb3\n")
	runGit(t, dir, "add", "PLAN.md")
	runGit(t, dir, "commit", "-qm", "main restores it, renamed")
	ok := ChangedDocEdits(wt, "main", "PLAN.md")
	if ok.All || len(ok.Base) == 0 {
		t.Fatalf("precondition: base renamed Beta, wa edits a1: %+v, want base claims", ok)
	}
	// Base's copy, its listing or its diff unreadable: All, never "no such
	// doc" (#252 review: an unreadable copy read as the pre-#247 miss).
	baseBlob := gitOut(t, dir, "rev-parse", "main:PLAN.md")
	for name, fail := range map[string]func([]string) bool{
		"copy": func(a []string) bool {
			return len(a) > 2 && a[0] == "cat-file" && a[1] == "blob" && a[2] == baseBlob
		},
		"listing": func(a []string) bool { return len(a) > 0 && a[0] == "ls-tree" },
		"diff":    func(a []string) bool { return len(a) > 3 && a[0] == "diff" && a[3] == "main" },
	} {
		t.Run(name, func(t *testing.T) { // the subtest's Cleanup lifts the failure
			failGit(t, fail)
			if e := ChangedDocEdits(wt, "main", "PLAN.md"); !e.All {
				t.Errorf("base's %s unreadable: %+v, want All", name, e)
			}
		})
	}
}

// A doc named like pathspec magic (":PLAN.md") is listed in base's tree as a
// literal path; listed as magic it read as missing and dropped the base
// claims (#252 review).
func TestChangedDocEdits_BehindLiteralPath(t *testing.T) {
	const doc, main = ":PLAN.md", "# Plan\n## Alpha\na1\na2\n## Beta two\nb1\nb2\nb3\n"
	dir := gitRepo(t)
	writeFile(t, dir, doc, docBase)
	runGit(t, dir, "add", "-A")
	runGit(t, dir, "commit", "-qm", "base")
	wt := filepath.Join(t.TempDir(), "wa")
	runGit(t, dir, "worktree", "add", "-q", "-b", "wa", wt)
	writeFile(t, dir, doc, main)
	runGit(t, dir, "commit", "-qam", "main renames Beta")
	mine := "# Plan\n## Alpha\na1\na2\n## Beta\nb1\nXb2\nb3\n"
	writeFile(t, wt, doc, mine)
	e := ChangedDocEdits(wt, "main", doc)
	if want := []LineRange{{Start: 7, End: 7}}; !reflect.DeepEqual(e.Base, want) || e.BaseCopy != main || e.All {
		t.Errorf("%s: Base = %v, BaseCopy = %q, All = %v; want %v, main's copy, false", doc, e.Base, e.BaseCopy, e.All, want)
	}
	if p, ok := ChangedDocEditsWith(wt, "main", doc, []byte(mine)); !ok || !reflect.DeepEqual(p, e) {
		t.Errorf("pending: %+v, want %+v", p, e)
	}
}

// Base's entry for the doc that is no regular file has no sections to read:
// All, never the link target's name read as base's doc (#252 review).
func TestChangedDocEdits_BaseEntryNotAFileIsAll(t *testing.T) {
	const mine = "# Plan\n## Alpha\na1\na2\n## Beta\nb1\nXb2\nb3\n"
	for name, replace := range map[string]func(t *testing.T, dir string){
		"symlink": func(t *testing.T, dir string) {
			writeFile(t, dir, "REAL.md", docBase)
			if err := os.Symlink("REAL.md", filepath.Join(dir, "PLAN.md")); err != nil {
				t.Fatal(err)
			}
		},
		"directory": func(t *testing.T, dir string) {
			if err := os.Mkdir(filepath.Join(dir, "PLAN.md"), 0o755); err != nil {
				t.Fatal(err)
			}
			writeFile(t, dir, "PLAN.md/x", "x\n")
		},
	} {
		t.Run(name, func(t *testing.T) {
			dir := docRepo(t)
			wt := filepath.Join(t.TempDir(), "wa")
			runGit(t, dir, "worktree", "add", "-q", "-b", "wa", wt)
			runGit(t, dir, "rm", "-q", "PLAN.md")
			replace(t, dir)
			runGit(t, dir, "add", "-A")
			runGit(t, dir, "commit", "-qm", "main replaces the doc")
			writeFile(t, wt, "PLAN.md", mine)
			if e := ChangedDocEdits(wt, "main", "PLAN.md"); !e.All {
				t.Errorf("base's PLAN.md a %s: %+v, want All", name, e)
			}
			if p, ok := ChangedDocEditsWith(wt, "main", "PLAN.md", []byte(mine)); !ok || !p.All {
				t.Errorf("pending, base's PLAN.md a %s: %+v, %v; want All", name, p, ok)
			}
		})
	}
}

// With no base, the section grade reads one diff from HEAD, both sides
// (#247): deletions count, in HEAD's copy, staged and unstaged alike.
func TestChangedDocEdits_NoBaseCountsDeletions(t *testing.T) {
	dir := docRepo(t)
	writeFile(t, dir, "PLAN.md", "# Plan\n## Alpha\nXa1\na2\n## Beta\nb1\nb2\nb3\n")
	runGit(t, dir, "add", "PLAN.md")
	const content = "# Plan\n## Alpha\nXa1\na2\n"
	writeFile(t, dir, "PLAN.md", content) // Beta deleted, unstaged
	e := ChangedDocEdits(dir, "no-such-base", "PLAN.md")
	want := []LineRange{{Start: 3, End: 3}, {Start: 5, End: 8}}
	if !reflect.DeepEqual(e.Deleted, want) || e.Old != docBase || e.All {
		t.Errorf("no base: Deleted = %v, Old = %q, All = %v; want %v, HEAD's copy, false", e.Deleted, e.Old, e.All, want)
	}
	if p, ok := ChangedDocEditsWith(dir, "no-such-base", "PLAN.md", []byte(content)); !ok || !reflect.DeepEqual(p, e) {
		t.Errorf("pending, no base: %+v, want %+v", p, e)
	}
	// A doc new since HEAD is added whole, as from a base (#244).
	writeFile(t, dir, "NEW.md", "# New\n## A\na1\n")
	runGit(t, dir, "add", "NEW.md")
	if e := ChangedDocEdits(dir, "no-such-base", "NEW.md"); !e.All {
		t.Errorf("a doc new since HEAD: %+v, want All", e)
	}
	// HEAD that can't be read is no unborn branch: All (#252 review).
	t.Run("HEAD unreadable", func(t *testing.T) { // the subtest's Cleanup lifts the failure
		failGit(t, func(a []string) bool { return len(a) > 1 && a[0] == "rev-parse" && a[len(a)-1] == "HEAD^{commit}" })
		if e := ChangedDocEdits(dir, "no-such-base", "PLAN.md"); !e.All {
			t.Errorf("HEAD unreadable: %+v, want All", e)
		}
		if e, ok := ChangedDocEditsWith(dir, "no-such-base", "PLAN.md", []byte(content)); !ok || !e.All {
			t.Errorf("pending, HEAD unreadable: %+v, %v; want All", e, ok)
		}
	})
	// No HEAD either (an unborn branch): the uncommitted ranges, as before.
	unborn := t.TempDir()
	runGit(t, unborn, "init", "-q", "-b", "main")
	writeFile(t, unborn, "PLAN.md", docBase)
	runGit(t, unborn, "add", "PLAN.md")
	if e := ChangedDocEdits(unborn, "no-such-base", "PLAN.md"); e.All || !reflect.DeepEqual(e.New, []LineRange{{Start: 1, End: 8}}) {
		t.Errorf("no HEAD: %+v, want the staged ranges [1-8]", e)
	}
}

// A HEAD git can't read is no unborn branch (#252 review): rev-parse
// --verify HEAD^{commit} exits 1 for each of these, as for an unborn one,
// and read as unborn they dropped the deletions. Each is All, written and
// pending (the unborn reading is TestChangedDocEdits_NoBaseCountsDeletions').
func TestChangedDocEdits_BrokenHEADIsAll(t *testing.T) {
	const content = "# Plan\n## Alpha\na1\na2\n" // Beta deleted
	gitFile := func(t *testing.T, dir, name, text string) {
		writeFile(t, dir, gitOut(t, dir, "rev-parse", "--git-path", name), text)
	}
	for name, corrupt := range map[string]func(t *testing.T, dir string){
		"branch holds a missing object": func(t *testing.T, dir string) {
			gitFile(t, dir, "refs/heads/main", strings.Repeat("1", 40)+"\n")
		},
		"branch holds a tree": func(t *testing.T, dir string) {
			gitFile(t, dir, "refs/heads/main", gitOut(t, dir, "rev-parse", "HEAD^{tree}")+"\n")
		},
		"branch holds garbage": func(t *testing.T, dir string) {
			gitFile(t, dir, "refs/heads/main", "not-a-sha\n")
		},
		"HEAD is a bad symref": func(t *testing.T, dir string) {
			gitFile(t, dir, "HEAD", "ref: refs/heads/..bad\n")
		},
		"detached at a missing object": func(t *testing.T, dir string) {
			gitFile(t, dir, "HEAD", strings.Repeat("2", 40)+"\n")
		},
	} {
		t.Run(name, func(t *testing.T) {
			dir := docRepo(t)
			writeFile(t, dir, "PLAN.md", content)
			corrupt(t, dir)
			if e := ChangedDocEdits(dir, "no-such-base", "PLAN.md"); !e.All {
				t.Errorf("%s: %+v, want All", name, e)
			}
			if e, ok := ChangedDocEditsWith(dir, "no-such-base", "PLAN.md", []byte(content)); !ok || !e.All {
				t.Errorf("pending, %s: %+v, %v; want All", name, e, ok)
			}
		})
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

// A doc the measured revision lacks is added whole: All, in both
// measurements alike (#244 review). git conflicts two windows' adds of one
// path (add/add); and the pending measurement names the missing side by the
// empty blob, which this repo holds (an empty file is committed), so reading
// it "succeeded" and the hook would have disagreed with wt check.
func TestChangedDocEdits_NewDocIsAll(t *testing.T) {
	dir := gitRepo(t)
	writeFile(t, dir, "empty.txt", "")
	runGit(t, dir, "add", "-A")
	runGit(t, dir, "commit", "-qm", "base, with an empty file")
	const doc = "# Plan\n## A\na1\n"
	writeFile(t, dir, "PLAN.md", doc)
	runGit(t, dir, "add", "PLAN.md")
	written := ChangedDocEdits(dir, "main", "PLAN.md")
	pending, ok := ChangedDocEditsWith(dir, "main", "PLAN.md", []byte(doc))
	if !ok || !written.All || !reflect.DeepEqual(pending, written) {
		t.Errorf("a new doc: written %+v, pending %+v (ok %v); want both All, equal", written, pending, ok)
	}
}
