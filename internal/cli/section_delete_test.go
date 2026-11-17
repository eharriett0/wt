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
		// wa is behind main (main inserted lines in Alpha after wa forked):
		// its deletion is read in the merge base's copy, where Beta starts
		// two lines earlier than in main's.
		{name: "behind base: wa deletes the section wb edits", plan: deletePlan,
			main:     edit("a1\n", "a1\nM1\nM2\n"),
			wa:       []func(*testing.T, string){edit("## Beta\nb1\nb2\nb3\nb4\nb5\nb6\nb7\n", "")},
			wb:       []func(*testing.T, string){edit("b6\n", "Xb6\n")},
			conflict: true, high: true},
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
	writeT(t, root, "PLAN.md", plan)
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
