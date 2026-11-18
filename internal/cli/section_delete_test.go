package cli

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/eharriett0/wt/internal/collide"
	"github.com/eharriett0/wt/internal/config"
)

const deletePlan = "# Plan\n## Alpha\na1\na2\na3\n## Beta\nb1\nb2\nb3\nb4\nb5\nb6\nb7\n"

// #237: the section grade read each window's edits in its own copy's line
// numbers, where a deleted line has no line. Deleting another window's
// section, emptying the doc or deleting it read "disjoint" in `wt check` and
// pre-push while git conflicts. Every shape here is held against `git
// merge-tree` from both windows: a conflict is HIGH in `wt check` and blocks
// pre-push. The section grade is coarser than git on purpose (two edits far
// apart in one section are HIGH and merge cleanly), so a shape git merges
// cleanly says what the grade does (high), and never the other way round.
func TestSectionDeletions_MatchMergeTree(t *testing.T) {
	const notes = "# Plan\n## Notes\nm1\nm2\n## Alpha\na1\n## Notes\nn1\nn2\nn3\nn4\n"
	edit := func(old, new string) func(t *testing.T, dir string) {
		return func(t *testing.T, dir string) {
			t.Helper()
			cur := readT(t, dir, "PLAN.md")
			if !strings.Contains(cur, old) {
				t.Fatalf("precondition: %q not in %s's PLAN.md", old, dir)
			}
			writeT(t, dir, "PLAN.md", strings.Replace(cur, old, new, 1))
		}
	}
	remove := func(t *testing.T, dir string) {
		t.Helper()
		if err := os.Remove(filepath.Join(dir, "PLAN.md")); err != nil {
			t.Fatal(err)
		}
	}
	cases := []struct {
		name     string
		plan     string
		main     func(*testing.T, string) // main moves on after wa forks: wa is behind
		wa, wb   []func(*testing.T, string)
		conflict bool // git merge-tree's verdict, re-checked
		high     bool // wt check from both sides and pre-push from both
	}{
		{name: "wb deletes the section wa edits", plan: deletePlan,
			wa:       []func(*testing.T, string){edit("b2\n", "Xb2\n")},
			wb:       []func(*testing.T, string){edit("## Beta\nb1\nb2\nb3\nb4\nb5\nb6\nb7\n", "")},
			conflict: true, high: true},
		{name: "wb empties the doc", plan: deletePlan,
			wa:       []func(*testing.T, string){edit("b2\n", "Xb2\n")},
			wb:       []func(*testing.T, string){edit(deletePlan, "")},
			conflict: true, high: true},
		{name: "wb deletes the doc", plan: deletePlan,
			wa:       []func(*testing.T, string){edit("b2\n", "Xb2\n")},
			wb:       []func(*testing.T, string){remove},
			conflict: true, high: true},
		{name: "wb deletes lines of its own section only", plan: deletePlan,
			wa: []func(*testing.T, string){edit("b4\n", "Xb4\n")},
			wb: []func(*testing.T, string){edit("a2\n", "")}},
		{name: "wb deletes a heading wa edits right under", plan: deletePlan,
			wa:       []func(*testing.T, string){edit("b1\n", "Xb1\n")},
			wb:       []func(*testing.T, string){edit("## Beta\n", "")},
			conflict: true, high: true},
		{name: "wb renames the heading wa edits right under", plan: deletePlan,
			wa:       []func(*testing.T, string){edit("b1\n", "Xb1\n")},
			wb:       []func(*testing.T, string){edit("## Beta\n", "## Beta two\n")},
			conflict: true, high: true},
		// Coarser than git, on purpose: the renamed heading is Beta's, and wa
		// edits Beta; git merges an edit four lines down cleanly.
		{name: "wb renames the heading, wa edits far below", plan: deletePlan,
			wa:   []func(*testing.T, string){edit("b5\n", "Xb5\n")},
			wb:   []func(*testing.T, string){edit("## Beta\n", "## Beta two\n")},
			high: true},
		{name: "duplicate headings: wb deletes the Notes wa edits", plan: notes,
			wa:       []func(*testing.T, string){edit("n2\n", "Xn2\n")},
			wb:       []func(*testing.T, string){edit("## Notes\nn1\nn2\nn3\nn4\n", "")},
			conflict: true, high: true},
		// Sections are identified by heading text, so the other Notes is "the
		// same section": coarser than git again.
		{name: "duplicate headings: wb deletes the other Notes", plan: notes,
			wa:   []func(*testing.T, string){edit("n2\n", "Xn2\n")},
			wb:   []func(*testing.T, string){edit("## Notes\nm1\nm2\n", "")},
			high: true},
		// #236's third review: the first Notes renamed and edited, then its
		// heading restored as base has it and the second Notes deleted. The
		// heading count is what it was, so a count could not see it.
		{name: "two-step history: rename, restore, delete the other Notes", plan: notes,
			wa: []func(*testing.T, string){edit("n2\n", "Xn2\n")},
			wb: []func(*testing.T, string){
				edit("## Notes\nm1\n", "## Old notes\nXm1\n"),
				edit("## Old notes\n", "## Notes\n"),
				edit("## Notes\nn1\nn2\nn3\nn4\n", ""),
			},
			conflict: true, high: true},
		// #237 review: the other window's only edit is a NEW section. Its
		// lines have no section in the old copy; it claims the line it lands
		// after (S1-S4). And a renamed heading against an edit of the line
		// above it (S5), and two new sections appended at the end (S6).
		{name: "S1: wa appends a section, wb deletes the doc", plan: deletePlan,
			wa:       []func(*testing.T, string){edit("b7\n", "b7\n## Delta\nd1\n")},
			wb:       []func(*testing.T, string){remove},
			conflict: true, high: true},
		{name: "S2: wa appends a section, wb empties the doc", plan: deletePlan,
			wa:       []func(*testing.T, string){edit("b7\n", "b7\n## Delta\nd1\n")},
			wb:       []func(*testing.T, string){edit(deletePlan, "")},
			conflict: true, high: true},
		{name: "S3: wa appends a section, wb deletes the last section", plan: deletePlan,
			wa:       []func(*testing.T, string){edit("b7\n", "b7\n## Delta\nd1\n")},
			wb:       []func(*testing.T, string){edit("## Beta\nb1\nb2\nb3\nb4\nb5\nb6\nb7\n", "")},
			conflict: true, high: true},
		{name: "S4: wa splits Beta with a new section, wb deletes Beta", plan: deletePlan,
			wa:       []func(*testing.T, string){edit("b1\n", "b1\n## Delta\nd1\n")},
			wb:       []func(*testing.T, string){edit("## Beta\nb1\nb2\nb3\nb4\nb5\nb6\nb7\n", "")},
			conflict: true, high: true},
		{name: "S5: wa edits the line above ## Beta, wb renames it", plan: deletePlan,
			wa:       []func(*testing.T, string){edit("a3\n", "Xa3\n")},
			wb:       []func(*testing.T, string){edit("## Beta\n", "## Beta two\n")},
			conflict: true, high: true},
		{name: "S6: both append a different section at the end", plan: deletePlan,
			wa:       []func(*testing.T, string){edit("b7\n", "b7\n## Delta\nd1\n")},
			wb:       []func(*testing.T, string){edit("b7\n", "b7\n## Epsilon\ne1\n")},
			conflict: true, high: true},
		// The lanes case structured docs exist for: wa adds a section at the
		// end of Alpha, right before ## Beta, while wb edits Beta's body.
		// git merges it; the insertion claims the line it lands after (a3),
		// never the next lane.
		{name: "wa adds a section at the end of Alpha, wb edits Beta's body", plan: deletePlan,
			wa: []func(*testing.T, string){edit("a3\n", "a3\n## Delta\nd1\n")},
			wb: []func(*testing.T, string){edit("b4\n", "Xb4\n")}},
		// Base has no PLAN.md and each window adds its own (#244 review):
		// git conflicts (add/add); a doc the measured revision lacks is
		// every section.
		{name: "N1: both windows add the doc", plan: "",
			wa:       []func(*testing.T, string){func(t *testing.T, dir string) { writeT(t, dir, "PLAN.md", "# Plan\n## A\na1\n") }},
			wb:       []func(*testing.T, string){func(t *testing.T, dir string) { writeT(t, dir, "PLAN.md", "# Plan\n## B\nb1\n") }},
			conflict: true, high: true},
		// wa is behind main (main inserted lines in Alpha after wa forked):
		// its deletion is read in the merge base's copy. main inserts more
		// lines than Beta has, so read in base's copy instead, the deleted
		// span would land in Alpha (#237 review: with a two-line insert that
		// mistake still read Beta).
		{name: "behind base: wa deletes the section wb edits", plan: deletePlan,
			main:     edit("a1\n", "a1\nM1\nM2\nM3\nM4\nM5\nM6\nM7\nM8\nM9\nM10\n"),
			wa:       []func(*testing.T, string){edit("## Beta\nb1\nb2\nb3\nb4\nb5\nb6\nb7\n", "")},
			wb:       []func(*testing.T, string){edit("b6\n", "Xb6\n")},
			conflict: true, high: true},
		// #247: main changes a heading after wa forks, so wa's own copies put
		// its edit under the heading it forked with, while wb's copy (current)
		// has main's. Each held against git from both windows.
		{name: "R1: main renames Beta; behind wa and current wb edit b2", plan: deletePlan,
			main:     edit("## Beta\n", "## Beta v2\n"),
			wa:       []func(*testing.T, string){edit("b2\n", "Xb2\n")},
			wb:       []func(*testing.T, string){edit("b2\n", "Yb2\n")},
			conflict: true, high: true},
		{name: "R2: main renames Beta; behind wa deletes b2-b3, wb edits b2", plan: deletePlan,
			main:     edit("## Beta\n", "## Beta v2\n"),
			wa:       []func(*testing.T, string){edit("b2\nb3\n", "")},
			wb:       []func(*testing.T, string){edit("b2\n", "Yb2\n")},
			conflict: true, high: true},
		{name: "SPLIT: main adds a heading before b2; wa and wb edit b3", plan: deletePlan,
			main:     edit("b2\n", "## Split\nb2\n"),
			wa:       []func(*testing.T, string){edit("b3\n", "Xb3\n")},
			wb:       []func(*testing.T, string){edit("b3\n", "Yb3\n")},
			conflict: true, high: true},
		{name: "MERGE: main removes ## Beta; wa and wb edit b3", plan: deletePlan,
			main:     edit("## Beta\n", ""),
			wa:       []func(*testing.T, string){edit("b3\n", "Xb3\n")},
			wb:       []func(*testing.T, string){edit("b3\n", "Yb3\n")},
			conflict: true, high: true},
		// And base's frame claims no more than the edit's own section there.
		{name: "main renames Beta; behind wa edits Alpha, wb edits Beta", plan: deletePlan,
			main: edit("## Beta\n", "## Beta v2\n"),
			wa:   []func(*testing.T, string){edit("a2\n", "Xa2\n")},
			wb:   []func(*testing.T, string){edit("b5\n", "Yb5\n")}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			root, wa, wb := sectionDeleteWindows(t, tc.plan, tc.main, tc.wa, tc.wb)
			if got := mergeTreeConflicts(t, root, tc.main != nil); got != tc.conflict {
				t.Fatalf("precondition: git merge-tree conflict=%v, the case says %v", got, tc.conflict)
			}
			if tc.conflict && !tc.high {
				t.Fatal("a case git conflicts on must be HIGH")
			}
			for _, w := range []struct{ dir, branch, other string }{{wa, "wa", "wb"}, {wb, "wb", "wa"}} {
				e := planEntry(t, w.dir, w.other)
				if (e.Category == CatBlocking) != tc.high {
					t.Errorf("wt check from %s: %s %v, want HIGH=%v (git merge-tree conflict=%v)", w.branch, e.Category, e.SharedSections, tc.high, tc.conflict)
				}
				if got := prePushBlocks(t, w.dir, w.branch); got != tc.high {
					t.Errorf("pre-push from %s blocks=%v, want %v (git merge-tree conflict=%v)", w.branch, got, tc.high, tc.conflict)
				}
			}
		})
	}
}

// #247: with no base branch (the default branch is neither main nor master,
// no origin/HEAD, no `base` in .wt.conf), a window's uncommitted edits are
// read from HEAD, deletions included (B2: deleting the section the other
// window edits read advisory). Committed changes aren't compared there at
// all (B1: no entry), and `wt check` says so instead of a silent clear.
func TestSectionGrade_NoBase(t *testing.T) {
	offlineGH(t)
	t.Setenv("HOME", t.TempDir())
	t.Setenv("WT_SKIP_COLLISION", "")
	t.Chdir(t.TempDir())
	root := t.TempDir()
	gitT(t, root, "init", "-q", "-b", "trunk")
	gitT(t, root, "config", "user.email", "t@t.test")
	gitT(t, root, "config", "user.name", "t")
	conf := "shared_docs = PLAN.md\nstructured_doc.PLAN.md = ^##\\s\n"
	writeT(t, root, ".wt.conf", conf)
	writeT(t, root, "PLAN.md", deletePlan)
	gitT(t, root, "add", "-A")
	gitT(t, root, "commit", "-qm", "base")
	dirs := t.TempDir()
	wa, wb := filepath.Join(dirs, "wa"), filepath.Join(dirs, "wb")
	gitT(t, root, "worktree", "add", "-q", "-b", "wa", wa, "trunk")
	gitT(t, root, "worktree", "add", "-q", "-b", "wb", wb, "trunk")
	writeT(t, wa, "PLAN.md", strings.Replace(deletePlan, "b2\n", "Xb2\n", 1))
	writeT(t, wb, "PLAN.md", strings.Replace(deletePlan, "## Beta\nb1\nb2\nb3\nb4\nb5\nb6\nb7\n", "", 1))

	// B2: both uncommitted.
	for _, w := range []struct{ dir, branch, other string }{{wa, "wa", "wb"}, {wb, "wb", "wa"}} {
		if e := planEntry(t, w.dir, w.other); e.Category != CatBlocking {
			t.Errorf("B2, wt check from %s: %s %v, want HIGH", w.branch, e.Category, e.SharedSections)
		}
	}

	// B1: both committed. git conflicts; wt has no base to compare commits
	// against, so it says so (text and JSON) rather than a bare clear.
	for _, w := range []string{wa, wb} {
		gitT(t, w, "commit", "-qam", "change")
	}
	if !mergeTreeConflicts(t, root, false) {
		t.Fatal("precondition: git merge-tree doesn't conflict wa and wb")
	}
	t.Chdir(wa)
	_, errOut := captureOutputForTest(t, func() { cmdCheck([]string{"PLAN.md"}) })
	if !strings.Contains(errOut, "no base branch resolves here") {
		t.Errorf("B1: wt check doesn't say committed changes aren't compared:\n%s", errOut)
	}
	out, _ := captureOutputForTest(t, func() { cmdCheck([]string{"--json", "PLAN.md"}) })
	if !strings.Contains(out, `"base_unresolved": true`) {
		t.Errorf("B1: wt check --json lacks base_unresolved:\n%s", out)
	}
	c, err := config.Load()
	if err != nil {
		t.Fatal(err)
	}
	if text, isErr := mcpCheck(c, []string{"PLAN.md"}, false, false); isErr || !strings.Contains(text, `"base_unresolved": true`) {
		t.Errorf("B1: MCP wt_check lacks base_unresolved (isError %v):\n%s", isErr, text)
	}

	// With a base, no note, and the committed shape grades HIGH.
	writeT(t, wa, ".wt.conf", conf+"base = trunk\n")
	writeT(t, wb, ".wt.conf", conf+"base = trunk\n")
	_, errOut = captureOutputForTest(t, func() { cmdCheck([]string{"PLAN.md"}) })
	if strings.Contains(errOut, "no base branch") {
		t.Errorf("with base = trunk, wt check still says no base:\n%s", errOut)
	}
	if out, _ := captureOutputForTest(t, func() { cmdCheck([]string{"--json", "PLAN.md"}) }); strings.Contains(out, "base_unresolved") {
		t.Errorf("with base = trunk, --json carries base_unresolved:\n%s", out)
	}
	if e := planEntry(t, wa, "wb"); e.Category != CatBlocking {
		t.Errorf("with base = trunk, committed: %s %v, want HIGH", e.Category, e.SharedSections)
	}
}

// sectionDeleteWindows makes a repo whose PLAN.md is a structured shared doc
// holding plan, forks wa, lets main move on (when main is given), forks wb,
// and commits each window's changes in turn (one commit per change).
func sectionDeleteWindows(t *testing.T, plan string, main func(*testing.T, string), aChanges, bChanges []func(*testing.T, string)) (root, wa, wb string) {
	t.Helper()
	offlineGH(t)
	t.Setenv("HOME", t.TempDir())
	t.Setenv("WT_SKIP_COLLISION", "")
	t.Setenv("HOOK_DISABLE_MULTIWINDOW_CHECK", "")
	t.Setenv("HOOK_DISABLE_MAIN_PUSH", "")
	t.Chdir(t.TempDir())
	root = t.TempDir()
	gitT(t, root, "init", "-q", "-b", "main")
	gitT(t, root, "config", "user.email", "t@t.test")
	gitT(t, root, "config", "user.name", "t")
	writeT(t, root, ".wt.conf", "shared_docs = PLAN.md\nstructured_doc.PLAN.md = ^##\\s\n")
	if plan != "" { // "": base has no PLAN.md
		writeT(t, root, "PLAN.md", plan)
	}
	gitT(t, root, "add", "-A")
	gitT(t, root, "commit", "-qm", "base")
	dirs := t.TempDir()
	wa, wb = filepath.Join(dirs, "wa"), filepath.Join(dirs, "wb")
	gitT(t, root, "worktree", "add", "-q", "-b", "wa", wa, "main")
	if main != nil {
		main(t, root)
		gitT(t, root, "commit", "-qam", "main moves on")
	}
	gitT(t, root, "worktree", "add", "-q", "-b", "wb", wb, "main")
	for _, w := range []struct {
		dir     string
		changes []func(*testing.T, string)
	}{{wa, aChanges}, {wb, bChanges}} {
		for i, change := range w.changes {
			change(t, w.dir)
			gitT(t, w.dir, "add", "-A")
			gitT(t, w.dir, "commit", "-qm", "change "+string(rune('1'+i)))
		}
	}
	return root, wa, wb
}

// planEntry is `wt check PLAN.md`'s entry for window other, run from dir.
func planEntry(t *testing.T, dir, other string) CheckEntry {
	t.Helper()
	t.Chdir(dir)
	c, err := config.Load()
	if err != nil {
		t.Fatal(err)
	}
	ws, err := collide.Scan(c)
	if err != nil {
		t.Fatal(err)
	}
	for _, e := range buildCheckReport(c, ws, dir, collide.ExactQueries([]string{"PLAN.md"})) {
		if e.Window == other {
			return e
		}
	}
	t.Fatalf("no %s entry in the check report for PLAN.md from %s", other, dir)
	return CheckEntry{}
}
