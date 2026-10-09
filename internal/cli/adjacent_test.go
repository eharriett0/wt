package cli

import (
	"encoding/json"
	"fmt"
	"io"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"github.com/eharriett0/wt/internal/collide"
	"github.com/eharriett0/wt/internal/config"
	"github.com/eharriett0/wt/internal/gitx"
)

// #199: two windows editing TOUCHING lines (no unchanged line between) are one
// conflict region in git's 3-way merge, so the second merge conflicts. `wt check`
// (from either side) and `wt status` graded them disjoint. Real worktrees, both
// up to date with main, each with one committed edit of a 20-line file; the
// verdicts are git merge-tree's on the same pairs (the #199 repro). An insertion
// meets only a change of a line next to its gap, or another insertion there.
func TestCheck_TouchingEditsAreOneConflict(t *testing.T) {
	change := func(n int) func([]string) []string {
		return func(ls []string) []string { ls[n-1] = "changed " + ls[n-1]; return ls }
	}
	insertAfter := func(n int) func([]string) []string {
		return func(ls []string) []string { return append(ls[:n:n], append([]string{"inserted"}, ls[n:]...)...) }
	}
	cases := []struct {
		name  string
		a, b  func([]string) []string
		high  bool
		spans []gitx.LineRange // from b's side
	}{
		{"A edits L10, B edits L11", change(10), change(11), true, []gitx.LineRange{{Start: 10, End: 11}}},
		{"A edits L11, B edits L10", change(11), change(10), true, []gitx.LineRange{{Start: 10, End: 11}}},
		{"one unchanged line between", change(10), change(12), false, nil},
		{"insertion after L10 vs a change of L11", insertAfter(10), change(11), true, []gitx.LineRange{{Start: 11, End: 11}}},
		{"insertion after L10 vs a change of L12", insertAfter(10), change(12), false, nil},
		{"insertions after L10 and after L11", insertAfter(10), insertAfter(11), false, nil},
		{"insertions both after L10", insertAfter(10), insertAfter(10), true, []gitx.LineRange{{Start: 10, End: 11}}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			wa, wb := twoCurrentWindows(t, tc.a, tc.b)
			fromB := checkEntryFor(t, wb, "wa")
			fromA := checkEntryFor(t, wa, "wb")
			if got := fromB.Category == CatBlocking; got != tc.high {
				t.Errorf("wt check from wb: %s/%s, want HIGH=%v", fromB.Category, fromB.Severity, tc.high)
			}
			if got := fromA.Category == CatBlocking; got != tc.high {
				t.Errorf("wt check from wa: %s/%s, want HIGH=%v", fromA.Category, fromA.Severity, tc.high)
			}
			if !reflect.DeepEqual(fromB.OverlapSpans, tc.spans) {
				t.Errorf("overlap spans from wb = %v, want %v", fromB.OverlapSpans, tc.spans)
			}
			if got := statusSeverity(t, wb) == "HIGH"; got != tc.high {
				t.Errorf("wt status: HIGH=%v, want %v", got, tc.high)
			}
		})
	}
}

// twoCurrentWindows: main holds a 20-line data.txt; worktrees wa and wb branch
// from it and each commit one edit (a, b). Returns their paths.
func twoCurrentWindows(t *testing.T, a, b func([]string) []string) (wa, wb string) {
	t.Helper()
	offlineGH(t)
	t.Setenv("HOME", t.TempDir())
	t.Setenv("WT_SKIP_COLLISION", "")
	t.Setenv("HOOK_DISABLE_MULTIWINDOW_CHECK", "")
	t.Chdir(t.TempDir())
	root := t.TempDir()
	gitT(t, root, "init", "-q", "-b", "main")
	gitT(t, root, "config", "user.email", "t@t.test")
	gitT(t, root, "config", "user.name", "t")
	var lines []string
	for i := 1; i <= 20; i++ {
		lines = append(lines, fmt.Sprintf("l%02d", i))
	}
	writeT(t, root, "data.txt", strings.Join(lines, "\n")+"\n")
	gitT(t, root, "add", "data.txt")
	gitT(t, root, "commit", "-qm", "base")
	base := t.TempDir()
	for _, w := range []struct {
		name string
		edit func([]string) []string
		dir  *string
	}{{"wa", a, &wa}, {"wb", b, &wb}} {
		*w.dir = filepath.Join(base, w.name)
		gitT(t, root, "worktree", "add", "-q", "-b", w.name, *w.dir, "main")
		edited := w.edit(append([]string(nil), lines...))
		writeT(t, *w.dir, "data.txt", strings.Join(edited, "\n")+"\n")
		gitT(t, *w.dir, "commit", "-qam", w.name+"'s edit")
	}
	return wa, wb
}

// checkEntryFor is `wt check data.txt` run in dir: the entry for window other.
func checkEntryFor(t *testing.T, dir, other string) CheckEntry {
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
	for _, e := range buildCheckReport(c, ws, dir, collide.ExactQueries([]string{"data.txt"}), false) {
		if e.Window == other {
			return e
		}
	}
	t.Fatalf("no %s entry in the check report from %s", other, dir)
	return CheckEntry{}
}

// statusSeverity is `wt status`'s grade of data.txt, run in dir.
func statusSeverity(t *testing.T, dir string) string {
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
	for _, o := range gradeStatusOverlaps(c, ws, collide.Overlaps(ws)) {
		if o.File == "data.txt" {
			return o.Severity
		}
	}
	t.Fatal("no data.txt overlap in wt status")
	return ""
}

// #199 through the pre-edit hooks, which must say what `wt check` says once the
// edit is made (#92/#108): in behindHookRepo, wa edited "l20" (base line 23) and
// this window wc is behind base by 3 lines. An edit of "l21" or "l19" touches
// wa's line, so it is one conflict region in git; "l22" has "l21" between. A
// Codex patch that only INSERTS a line is graded too, as the insertion git will
// see: after "l20" or "l19" it meets wa's change of "l20"; after "l21" it
// doesn't. (Before #199 a pure insertion contributed no range, so the hook
// couldn't grade it at all.) So is a Claude Edit that only appends a line: it
// claims the insertion, not its old_string's line, which touches wa's.
func TestPreEditHooks_TouchingEdits(t *testing.T) {
	wc := behindHookRepo(t)
	replace := func(target string) func(string) string {
		return func(s string) string { return strings.Replace(s, target+"\n", "X"+target+"\n", 1) }
	}
	insertAfter := func(target string) func(string) string {
		return func(s string) string { return strings.Replace(s, target+"\n", target+"\nINS\n", 1) }
	}
	codexInsert := func(target string) string {
		p, _ := json.Marshal(map[string]any{"cwd": wc, "tool_name": "apply_patch", "tool_input": map[string]string{
			"command": "*** Begin Patch\n*** Update File: data.txt\n@@\n " + target + "\n+INS\n*** End Patch\n"}})
		return string(p)
	}
	claudeInsert := func(target string) string {
		p, _ := json.Marshal(map[string]any{"cwd": wc, "tool_name": "Edit", "tool_input": map[string]string{
			"file_path": filepath.Join(wc, "data.txt"), "old_string": target + "\n", "new_string": target + "\nINS\n"}})
		return string(p)
	}
	for _, tc := range []struct {
		name    string
		edit    func(string) string
		payload map[string]string // hook kind → payload
		high    bool
	}{
		{"edit l21", replace("l21"), map[string]string{"claude": claudeEditPayload(wc, "l21"), "codex": codexEditPayload(wc, "l21")}, true},
		{"edit l19", replace("l19"), map[string]string{"claude": claudeEditPayload(wc, "l19"), "codex": codexEditPayload(wc, "l19")}, true},
		{"edit l22", replace("l22"), map[string]string{"claude": claudeEditPayload(wc, "l22"), "codex": codexEditPayload(wc, "l22")}, false},
		{"insert after l20", insertAfter("l20"), map[string]string{"claude": claudeInsert("l20"), "codex": codexInsert("l20")}, true},
		{"insert after l19", insertAfter("l19"), map[string]string{"claude": claudeInsert("l19"), "codex": codexInsert("l19")}, true},
		{"insert after l21", insertAfter("l21"), map[string]string{"claude": claudeInsert("l21"), "codex": codexInsert("l21")}, false},
	} {
		if post := postEditHigh(t, wc, tc.edit); post != tc.high {
			t.Fatalf("%s: precondition: wt check after the edit HIGH=%v, want %v", tc.name, post, tc.high)
		}
		for kind, payload := range tc.payload {
			hook := map[string]func(io.Reader) int{"claude": hookClaudeEdit, "codex": hookCodexEdit}[kind]
			t.Setenv("WT_CLAUDE_HOOK_BLOCK", "1")
			t.Setenv("WT_CODEX_HOOK_BLOCK", "1")
			want := map[bool]string{true: "deny", false: "silent"}[tc.high]
			if got := hookVerdict(t, runHookCapture(t, hook, payload)); got != want {
				t.Errorf("%s: %s hook says %s, want %s (wt check after the edit: HIGH=%v)", tc.name, kind, got, want, tc.high)
			}
		}
	}
}

// postEditHigh makes edit in wc, asks the `wt check` grader whether wa's entry
// is HIGH, and undoes the edit.
func postEditHigh(t *testing.T, wc string, edit func(string) string) bool {
	t.Helper()
	orig := readT(t, wc, "data.txt")
	writeT(t, wc, "data.txt", edit(orig))
	defer writeT(t, wc, "data.txt", orig)
	return checkEntryFor(t, wc, "wa").Category == CatBlocking
}
