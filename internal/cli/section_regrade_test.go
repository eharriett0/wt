package cli

import (
	"encoding/json"
	"io"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"github.com/eharriett0/wt/internal/collide"
	"github.com/eharriett0/wt/internal/config"
	"github.com/eharriett0/wt/internal/gitx"
)

const sectionPlan = "# Plan\n## Alpha\na1\na2\na3\n## Beta\nb1\nb2\nb3\nb4\nb5\nb6\nb7\n"

// The edit hooks re-grade a structured shared doc by section, as `wt check`
// will once the edit is made (#226). wc edits Alpha and wa edits Beta, which
// `wt check` grades advisory. Before #226 the hooks dropped that advisory
// without re-grading it, so an edit moving wc INTO Beta was silent while
// `wt check` then graded it HIGH ("same section"); and an edit moving wc OUT
// of a shared Beta kept a heads-up (a deny under block) on a collision the
// edit resolves. Each case holds the hook to `wt check` after the edit.
func TestPreEditHooks_StructuredDocSections(t *testing.T) {
	wc := sectionHookRepo(t)
	cases := []struct {
		name, old, new string
		prep           func()
		high           bool
	}{
		{name: "into Beta", old: "b6\n", new: "Xb6\n", high: true},
		{name: "stays in Alpha", old: "a2\n", new: "Xa2\n"},
		// wc's own two-line insert in Alpha moves b1 from line 7 to line 9 of
		// its copy. Read in base-frame numbers, the edit's line 7 is a3, in
		// Alpha: the hook must read sections in the copy's own frame (#123).
		{name: "into Beta past wc's insert", old: "b1\n", new: "Xb1\n", high: true, prep: func() {
			writeT(t, wc, "PLAN.md", strings.Replace(readT(t, wc, "PLAN.md"), "Wa1\n", "Wa1\nINS1\nINS2\n", 1))
			gitT(t, wc, "commit", "-qam", "wc inserts two lines in Alpha")
		}},
		{name: "out of Beta", old: "Wb6\n", new: "b6\n", prep: func() {
			writeT(t, wc, "PLAN.md", strings.Replace(readT(t, wc, "PLAN.md"), "b6\n", "Wb6\n", 1))
			gitT(t, wc, "commit", "-qam", "wc edits b6 too")
			if e := sectionGradeOf(t, wc); e.Category != CatBlocking {
				t.Fatalf("precondition: wc and wa both in Beta: wt check says %s, want HIGH", e.Category)
			}
		}},
	}
	for _, tc := range cases {
		if tc.prep != nil {
			tc.prep()
		}
		post := postEditSectionGrade(t, wc, tc.old, tc.new)
		if (post.Category == CatBlocking) != tc.high {
			t.Fatalf("%s: precondition: wt check after the edit says %s %v, want HIGH=%v", tc.name, post.Category, post.SharedSections, tc.high)
		}
		if tc.high && !reflect.DeepEqual(post.SharedSections, []string{"## Beta"}) {
			t.Fatalf("%s: precondition: wt check's shared sections %v, want [## Beta]", tc.name, post.SharedSections)
		}
		for _, h := range []struct {
			kind    string
			hook    func(io.Reader) int
			payload string
			blockEV string
		}{
			{"claude", hookClaudeEdit, sectionClaudePayload(wc, tc.old, tc.new), "WT_CLAUDE_HOOK_BLOCK"},
			{"codex", hookCodexEdit, sectionCodexPayload(wc, tc.old, tc.new), "WT_CODEX_HOOK_BLOCK"},
		} {
			for _, block := range []bool{false, true} {
				t.Setenv(h.blockEV, map[bool]string{false: "0", true: "1"}[block])
				out := runHookCapture(t, h.hook, h.payload)
				got := hookVerdict(t, out)
				want := "silent"
				if tc.high {
					want = map[bool]string{false: "overlap", true: "deny"}[block]
				}
				if got != want {
					t.Errorf("%s %s block=%v: hook says %s, want %s (wt check after the edit: %s)\n%s", tc.name, h.kind, block, got, want, post.Category, out)
				}
				if tc.high && !strings.Contains(out, "## Beta") {
					t.Errorf("%s %s block=%v: the warning doesn't name the shared section, as wt check does:\n%s", tc.name, h.kind, block, out)
				}
				if strings.Contains(out, "OVERLAPS hunks") {
					t.Errorf("%s %s block=%v: a section grade overlaps no hunk:\n%s", tc.name, h.kind, block, out)
				}
			}
		}
	}
}

// When this window's copy can't be measured, `wt check` after the edit may
// say HIGH, so every section-graded entry stays a file-level heads-up, an
// advisory included (#226 review: dropping the advisory failed open).
func TestPreEditHooks_StructuredDocUnmeasurable(t *testing.T) {
	wc := sectionHookRepo(t)
	orig := pendingRangesNew
	t.Cleanup(func() { pendingRangesNew = orig })
	pendingRangesNew = func(dir, base, file string, content []byte) ([]gitx.LineRange, bool) { return nil, false }
	t.Setenv("WT_CLAUDE_HOOK_BLOCK", "0")
	if got := hookVerdict(t, runHookCapture(t, hookClaudeEdit, sectionClaudePayload(wc, "b6\n", "Xb6\n"))); got != "file-level" {
		t.Errorf("advisory, copy unmeasurable: claude says %s, want a file-level heads-up", got)
	}
	writeT(t, wc, "PLAN.md", strings.Replace(readT(t, wc, "PLAN.md"), "b6\n", "Wb6\n", 1))
	gitT(t, wc, "commit", "-qam", "wc edits b6 too")
	if got := hookVerdict(t, runHookCapture(t, hookClaudeEdit, sectionClaudePayload(wc, "Wb6\n", "b6\n"))); got != "file-level" {
		t.Errorf("section HIGH, copy unmeasurable: claude says %s, want a file-level heads-up", got)
	}
}

// sectionGraded picks exactly the entries gradeEntry's shared-doc branch
// grades by section; sectionGrade is one of them once the edit is made, and
// falls back as `wt check` does when the sections can't be graded (#226).
func TestSectionGrade(t *testing.T) {
	adv := CheckEntry{Window: "wa", Category: CatAdvisory, Severity: "low"}
	high := CheckEntry{Window: "wa", Category: CatBlocking, Severity: "HIGH", SharedSections: []string{"## Beta"}}
	for _, tc := range []struct {
		e    CheckEntry
		want bool
	}{
		{adv, true},
		{high, true},
		{CheckEntry{Category: CatBlocking, Severity: "HIGH"}, false}, // hunk-graded HIGH: regradePending's
		{CheckEntry{Category: CatFYI, Severity: "low", Untracked: true}, false},
		{CheckEntry{Category: CatFYI, Severity: "low", AlreadyMerged: true}, false},
		{CheckEntry{Category: CatStale, Severity: "low"}, false},
	} {
		if got := sectionGraded(tc.e); got != tc.want {
			t.Errorf("sectionGraded(%s %v) = %v, want %v", tc.e.Category, tc.e.SharedSections, got, tc.want)
		}
	}

	for _, tc := range []struct {
		name      string
		e         CheckEntry
		shared    []string
		graded    bool
		gone      bool // the edit deletes a shared heading
		keep      bool
		confirmed bool
		sections  []string
	}{
		{"advisory, now a shared section", adv, []string{"## Alpha"}, true, false, true, true, []string{"## Alpha"}},
		{"advisory, still disjoint", adv, nil, true, false, false, false, nil},
		{"HIGH, still shared, by another section", high, []string{"## Alpha"}, true, false, true, true, []string{"## Alpha"}},
		{"HIGH, now disjoint", high, nil, true, false, false, false, nil},
		{"HIGH, its heading deleted: a heads-up (#237)", high, nil, true, true, true, false, []string{"## Beta"}},
		{"advisory, a heading deleted: dropped, as wt check grades it", adv, nil, true, true, false, false, nil},
		{"advisory, ungradable", adv, nil, false, false, false, false, nil},
		{"HIGH, ungradable: a heads-up, never dropped", high, nil, false, false, true, false, []string{"## Beta"}},
	} {
		g, keep := sectionGrade(tc.e, tc.shared, tc.graded, tc.gone)
		if keep != tc.keep {
			t.Errorf("%s: keep = %v, want %v", tc.name, keep, tc.keep)
			continue
		}
		if !keep {
			continue
		}
		if g.confirmed != tc.confirmed || !reflect.DeepEqual(g.entry.SharedSections, tc.sections) {
			t.Errorf("%s: confirmed=%v sections=%v, want %v %v", tc.name, g.confirmed, g.entry.SharedSections, tc.confirmed, tc.sections)
		}
		if g.confirmed && (g.entry.Category != CatBlocking || g.entry.Severity != "HIGH") {
			t.Errorf("%s: a confirmed section grade reads %s %s, want blocking HIGH", tc.name, g.entry.Category, g.entry.Severity)
		}
	}
}

// sectionHookRepo: main has a .wt.conf making PLAN.md a structured shared doc
// and PLAN.md (sectionPlan); wa edits b2 (Beta), wc edits a1 (Alpha). Returns
// wc.
func sectionHookRepo(t *testing.T) (wc string) {
	t.Helper()
	offlineGH(t) // no real GitHub calls
	t.Setenv("HOME", t.TempDir())
	t.Setenv("WT_SKIP_COLLISION", "")
	t.Setenv("HOOK_DISABLE_MULTIWINDOW_CHECK", "")
	t.Chdir(t.TempDir()) // the hooks chdir into the payload's cwd; restored after

	root := t.TempDir()
	gitT(t, root, "init", "-q", "-b", "main")
	gitT(t, root, "config", "user.email", "t@t.test")
	gitT(t, root, "config", "user.name", "t")
	writeT(t, root, ".wt.conf", "shared_docs = PLAN.md\nstructured_doc.PLAN.md = ^##\\s\n")
	writeT(t, root, "PLAN.md", sectionPlan)
	gitT(t, root, "add", "-A")
	gitT(t, root, "commit", "-qm", "base")
	gitT(t, root, "branch", "wa")
	gitT(t, root, "branch", "wc")

	wa := filepath.Join(t.TempDir(), "wa")
	wc = filepath.Join(t.TempDir(), "wc")
	gitT(t, root, "worktree", "add", "-q", wa, "wa")
	gitT(t, root, "worktree", "add", "-q", wc, "wc")
	writeT(t, wa, "PLAN.md", strings.Replace(sectionPlan, "b2\n", "Wb2\n", 1))
	gitT(t, wa, "commit", "-qam", "wa edits b2")
	writeT(t, wc, "PLAN.md", strings.Replace(sectionPlan, "a1\n", "Wa1\n", 1))
	gitT(t, wc, "commit", "-qam", "wc edits a1")
	if e := sectionGradeOf(t, wc); e.Category != CatAdvisory {
		t.Fatalf("precondition: Alpha vs Beta: wt check says %s %v, want advisory", e.Category, e.SharedSections)
	}
	return wc
}

func sectionClaudePayload(wc, old, new string) string {
	p, _ := json.Marshal(map[string]any{"cwd": wc, "tool_name": "Edit", "tool_input": map[string]string{
		"file_path": filepath.Join(wc, "PLAN.md"), "old_string": old, "new_string": new}})
	return string(p)
}

func sectionCodexPayload(wc, old, new string) string {
	p, _ := json.Marshal(map[string]any{"cwd": wc, "tool_name": "apply_patch", "tool_input": map[string]string{
		"command": "*** Begin Patch\n*** Update File: PLAN.md\n@@\n-" + strings.TrimSuffix(old, "\n") + "\n+" + strings.TrimSuffix(new, "\n") + "\n*** End Patch\n"}})
	return string(p)
}

// postEditSectionGrade makes the edit in wc, asks `wt check` for wa's entry,
// and undoes the edit.
func postEditSectionGrade(t *testing.T, wc, old, new string) CheckEntry {
	t.Helper()
	orig := readT(t, wc, "PLAN.md")
	if !strings.Contains(orig, old) {
		t.Fatalf("precondition: %q not in wc's PLAN.md", old)
	}
	writeT(t, wc, "PLAN.md", strings.Replace(orig, old, new, 1))
	defer writeT(t, wc, "PLAN.md", orig)
	return sectionGradeOf(t, wc)
}

// sectionGradeOf is `wt check PLAN.md`'s entry for wa, run from wc.
func sectionGradeOf(t *testing.T, wc string) CheckEntry {
	t.Helper()
	t.Chdir(wc)
	c, err := config.Load()
	if err != nil {
		t.Fatal(err)
	}
	ws, err := collide.Scan(c)
	if err != nil {
		t.Fatal(err)
	}
	for _, e := range buildCheckReport(c, ws, wc, collide.ExactQueries([]string{"PLAN.md"})) {
		if e.Window == "wa" {
			return e
		}
	}
	t.Fatal("no wa entry in the check report for PLAN.md")
	return CheckEntry{}
}

// An edit the hook can't predict (Claude Code would refuse it: old_string is
// not in the file) keeps a structured doc's advisory as a heads-up (#226
// review): what the file becomes is unknown, and `wt check` may say HIGH.
func TestPreEditHooks_StructuredDocUnpredictedKeepsHeadsUp(t *testing.T) {
	wc := sectionHookRepo(t)
	t.Setenv("WT_CLAUDE_HOOK_BLOCK", "0")
	if got := hookVerdict(t, runHookCapture(t, hookClaudeEdit, sectionClaudePayload(wc, "not in the file\n", "x\n"))); got != "file-level" {
		t.Errorf("unpredicted edit: claude says %s, want a file-level heads-up", got)
	}
}

// Deleting the shared section, or emptying the file, reads disjoint in `wt
// check` (its new-frame ranges can't show a deletion, #237) while git
// conflicts. main kept a heads-up for the section HIGH, and so do the hooks
// until the grader sees deletions (#226 review).
func TestPreEditHooks_StructuredDocDeletionKeepsHeadsUp(t *testing.T) {
	wc := sectionHookRepo(t)
	writeT(t, wc, "PLAN.md", strings.Replace(readT(t, wc, "PLAN.md"), "b6\n", "Wb6\n", 1))
	gitT(t, wc, "commit", "-qam", "wc edits b6 too")
	if e := sectionGradeOf(t, wc); e.Category != CatBlocking {
		t.Fatalf("precondition: wc and wa both in Beta: wt check says %s, want HIGH", e.Category)
	}
	beta := readT(t, wc, "PLAN.md")[strings.Index(readT(t, wc, "PLAN.md"), "## Beta\n"):]
	t.Setenv("WT_CLAUDE_HOOK_BLOCK", "1")
	for _, tc := range []struct{ name, old, new string }{
		{"the whole Beta section deleted", beta, ""},
		{"the file emptied", readT(t, wc, "PLAN.md"), ""},
	} {
		if got := hookVerdict(t, runHookCapture(t, hookClaudeEdit, sectionClaudePayload(wc, tc.old, tc.new))); got != "deny" {
			t.Errorf("%s, claude, block: hook says %s, want the file-level heads-up's deny (as on main)", tc.name, got)
		}
	}
	// Codex: deleting the file.
	p, _ := json.Marshal(map[string]any{"cwd": wc, "tool_name": "apply_patch", "tool_input": map[string]string{
		"command": "*** Begin Patch\n*** Delete File: PLAN.md\n*** End Patch\n"}})
	t.Setenv("WT_CODEX_HOOK_BLOCK", "0")
	if got := hookVerdict(t, runHookCapture(t, hookCodexEdit, string(p))); got != "file-level" {
		t.Errorf("codex deletes PLAN.md: hook says %s, want a file-level heads-up", got)
	}
}

// A structured doc base doesn't have (each window adds its own) is measured as
// `wt check` measures it: the whole copy is new (#226 review: it read as
// unmeasurable and the hook dropped the entry while `wt check` said HIGH).
func TestPreEditHooks_StructuredDocNotInBase(t *testing.T) {
	offlineGH(t)
	t.Setenv("HOME", t.TempDir())
	t.Setenv("WT_SKIP_COLLISION", "")
	t.Setenv("HOOK_DISABLE_MULTIWINDOW_CHECK", "")
	t.Chdir(t.TempDir())
	root := t.TempDir()
	gitT(t, root, "init", "-q", "-b", "main")
	gitT(t, root, "config", "user.email", "t@t.test")
	gitT(t, root, "config", "user.name", "t")
	writeT(t, root, ".wt.conf", "shared_docs = PLAN.md\nstructured_doc.PLAN.md = ^##\\s\n")
	gitT(t, root, "add", "-A")
	gitT(t, root, "commit", "-qm", "base")
	gitT(t, root, "branch", "wa")
	gitT(t, root, "branch", "wc")
	wa := filepath.Join(t.TempDir(), "wa")
	wc := filepath.Join(t.TempDir(), "wc")
	gitT(t, root, "worktree", "add", "-q", wa, "wa")
	gitT(t, root, "worktree", "add", "-q", wc, "wc")
	writeT(t, wa, "PLAN.md", "# Plan\n## Beta\nb1\n")
	gitT(t, wa, "add", "PLAN.md")
	gitT(t, wa, "commit", "-qm", "wa adds Beta")
	writeT(t, wc, "PLAN.md", "# Plan\n## Alpha\na1\n")
	gitT(t, wc, "add", "PLAN.md")
	gitT(t, wc, "commit", "-qm", "wc adds Alpha")

	post := postEditSectionGrade(t, wc, "a1\n", "a1\n## Beta\nb9\n")
	if post.Category != CatBlocking {
		t.Fatalf("precondition: wt check after wc adds Beta says %s, want HIGH", post.Category)
	}
	t.Setenv("WT_CLAUDE_HOOK_BLOCK", "0")
	if got := hookVerdict(t, runHookCapture(t, hookClaudeEdit, sectionClaudePayload(wc, "a1\n", "a1\n## Beta\nb9\n"))); got != "overlap" {
		t.Errorf("claude says %s, want overlap, as wt check after the edit", got)
	}
}
