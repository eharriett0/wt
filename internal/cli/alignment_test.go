package cli

import (
	"encoding/json"
	"fmt"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/eharriett0/wt/internal/config"
	"github.com/eharriett0/wt/internal/hooks"
)

// #199 review: graded by git's rule, an insertion meets only an edit next to
// its gap, so WHERE a diff puts an edit decides the grade. Where an edit can sit
// in more than one place (an inserted "}" beside an equal one, a rewrite among
// repeated lines), a plain `git diff` (myers, indent heuristic) and git's merge
// (merge-ort: histogram, no indent heuristic) put it in different gaps: wt
// graded two insertions one gap apart while git puts them in one gap and
// conflicts, so check, status and pre-push all let it through. Each shape here
// is a real repo, the verdict git merge-tree's on the two windows (through
// main for a window behind it), and every grader, from both sides, must say it.
func TestGrade_FollowsMergeAlignment(t *testing.T) {
	cases := []struct {
		name       string
		base, main string // main: what main holds after a forked ("" = a is current)
		a, b       string
		conflict   bool // git merge-tree's verdict, re-checked
	}{
		// The indent heuristic reports b's ")\n}" after the blank line; a merge
		// puts it after "}", the gap a inserts into.
		{"indent heuristic", "a\n\n}\n\t}\n", "", "a\n\n}\nX\n\t}\n", "a\n\n}\n)\n}\n\t}\n", true},
		{"indent heuristic, a gap apart", "a\n\n}\n\t}\n", "", "a\nX\n\n}\n\t}\n", "a\n\n}\n)\n}\n\t}\n", false},
		// myers reports a's edit as an insertion after line 5 and a rewrite of
		// line 8; histogram as a rewrite of line 6 and an insertion after line 9.
		{"myers vs histogram", "b\na\nc\nb\na\nb\nb\nb\nc\n", "", "b\na\nc\nb\na\nx\nb\nb\nc\nc\n", "b\na\nc\nb\na\nb\nZ\nb\nb\nc\n", true},
		// The first shape from a window behind main (#184's merge-base mapping).
		{"indent heuristic, behind main", "a\n\n}\n\t}\n", "top\na\n\n}\n\t}\n", "a\n\n}\nX\n\t}\n", "top\na\n\n}\n)\n}\n\t}\n", true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			root, wa, wb := alignmentWindows(t, tc.base, tc.main, tc.a, tc.b)
			if got := mergeTreeConflicts(t, root, tc.main != ""); got != tc.conflict {
				t.Fatalf("precondition: git merge-tree conflict=%v, the case says %v", got, tc.conflict)
			}
			want := map[bool]string{true: "HIGH", false: "low"}[tc.conflict]
			if e := checkEntryFor(t, wa, "wb"); e.Severity != want {
				t.Errorf("wt check from wa: %s, git merge-tree conflict=%v (wb's ranges %v)", e.Severity, tc.conflict, e.OtherRanges)
			}
			if e := checkEntryFor(t, wb, "wa"); e.Severity != want {
				t.Errorf("wt check from wb: %s, git merge-tree conflict=%v (wa's ranges %v)", e.Severity, tc.conflict, e.OtherRanges)
			}
			if got := statusSeverity(t, root); got != want {
				t.Errorf("wt status: %s, git merge-tree conflict=%v", got, tc.conflict)
			}
			for _, w := range []struct{ dir, branch string }{{wa, "wa"}, {wb, "wb"}} {
				if got := prePushBlocks(t, w.dir, w.branch); got != tc.conflict {
					t.Errorf("pre-push from %s blocks=%v, git merge-tree conflict=%v", w.branch, got, tc.conflict)
				}
			}
		})
	}
}

// alignmentWindows: main holds base in data.txt; wa forks, main then holds
// mainText (when given), wb forks; each commits its text. Returns the main
// checkout and the two worktrees.
func alignmentWindows(t *testing.T, base, mainText, aText, bText string) (root, wa, wb string) {
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
	writeT(t, root, "data.txt", base)
	gitT(t, root, "add", "data.txt")
	gitT(t, root, "commit", "-qm", "base")
	dirs := t.TempDir()
	wa, wb = filepath.Join(dirs, "wa"), filepath.Join(dirs, "wb")
	gitT(t, root, "worktree", "add", "-q", "-b", "wa", wa, "main")
	if mainText != "" {
		writeT(t, root, "data.txt", mainText)
		gitT(t, root, "commit", "-qam", "main moves on")
	}
	gitT(t, root, "worktree", "add", "-q", "-b", "wb", wb, "main")
	writeT(t, wa, "data.txt", aText)
	gitT(t, wa, "commit", "-qam", "wa's edit", "--allow-empty")
	writeT(t, wb, "data.txt", bText)
	gitT(t, wb, "commit", "-qam", "wb's edit")
	return root, wa, wb
}

// mergeTreeConflicts is git's verdict on landing wa and wb: merge-tree of the
// two, with wa first merged into main when it is behind (it must merge
// cleanly, or the shape isn't the one meant).
func mergeTreeConflicts(t *testing.T, root string, aBehind bool) bool {
	t.Helper()
	mt := func(x, y string) (string, bool) {
		cmd := exec.Command("git", "merge-tree", "--write-tree", x, y)
		cmd.Dir = root
		out, err := cmd.Output()
		if err != nil {
			if ee, ok := err.(*exec.ExitError); ok && ee.ExitCode() == 1 {
				return "", true
			}
			t.Fatalf("git merge-tree %s %s: %v", x, y, err)
		}
		return strings.SplitN(string(out), "\n", 2)[0], false
	}
	a := "wa"
	if aBehind {
		tree, conflict := mt("main", "wa")
		if conflict {
			t.Fatal("precondition: wa conflicts with main")
		}
		cmd := exec.Command("git", "commit-tree", tree, "-p", "main", "-p", "wa", "-m", "wa landed")
		cmd.Dir = root
		out, err := cmd.Output()
		if err != nil {
			t.Fatalf("git commit-tree: %v", err)
		}
		a = strings.TrimSpace(string(out))
	}
	_, conflict := mt(a, "wb")
	return conflict
}

// prePushBlocks runs the pre-push hook in dir for a push of branch's tip.
func prePushBlocks(t *testing.T, dir, branch string) bool {
	t.Helper()
	cmd := exec.Command("git", "rev-parse", "HEAD")
	cmd.Dir = dir
	sha, err := cmd.Output()
	if err != nil {
		t.Fatal(err)
	}
	t.Chdir(dir)
	c, err := config.Load()
	if err != nil {
		t.Fatal(err)
	}
	line := fmt.Sprintf("refs/heads/%s %s refs/heads/%s %s\n", branch, strings.TrimSpace(string(sha)), branch, strings.Repeat("0", 40))
	return hooks.HookPrePush(c, strings.NewReader(line)) != 0
}

// #199 review: the pre-edit hooks grade the file the edit will produce, so they
// say exactly what `wt check` says once it lands, and that is git's verdict.
// wb committed its edit; wa (wx) is about to make one, as a Claude Edit or a Codex
// apply_patch producing `after`. The hooks (WT_*_HOOK_BLOCK=1) must deny iff
// `wt check` from wx says HIGH after the edit, iff git merge-tree conflicts.
// Shapes: what apply_patch does beyond the '+'/'-' lines (a file without a
// final newline gains one, so its last line changes too), and insertions or
// deletions a diff can place in more than one gap (beside an equal line, at the
// top or the end of the file), where claiming every gap the edit could take
// denied edits `wt check` grades low.
func TestPreEditHooks_GradeTheFileTheEditProduces(t *testing.T) {
	cases := []struct {
		name     string
		base, y  string
		old, new string // the Claude Edit ("" = none)
		patch    string // the Codex patch body ("" = none)
		after    string // wx's file once the edit is made
		conflict bool
	}{
		{"codex, no final newline: the last line changes, touching y's", "a\nb\nc", "a\nB\nc", "", "", "@@\n c\n+d\n", "a\nb\nc\nd\n", true},
		{"codex, no final newline, edit far from it", "a\nb\nc\nd\ne", "a\nb\nc\nD\ne", "", "", "@@\n a\n-b\n+B\n c\n", "a\nB\nc\nd\ne\n", true},
		{"an insertion the indent heuristic moves, beside y's line", "a\n\n}\n\t}\n", "a\n\n}\n\t} //y\n", "\n}\n", "\n}\n)\n}\n", "@@\n }\n+)\n+}\n \t}\n", "a\n\n}\n)\n}\n\t}\n", true},
		{"the same insertion, y a gap away", "a\n\n}\n\t}\n", "a\n\nQ\n}\n\t}\n", "\n}\n", "\n}\n)\n}\n", "@@\n }\n+)\n+}\n \t}\n", "a\n\n}\n)\n}\n\t}\n", false},
		{"inserting a copy of the last line, y appends", "a\nb\nx\n", "a\nb\nx\ny\n", "b\n", "b\nx\n", "@@\n b\n+x\n x\n", "a\nb\nx\nx\n", true},
		{"deleting a copy of the last line, y appends", "a\nx\nx\n", "a\nx\nx\ny\n", "a\nx\n", "a\n", "@@\n a\n-x\n x\n", "a\nx\n", true},
		{"inserting a copy of the first line, y prepends", "x\nb\n", "y\nx\nb\n", "x\nb", "x\nx\nb", "@@\n x\n+x\n b\n", "x\nx\nb\n", false},
		{"deleting a copy of the first line, y prepends", "x\nx\nb\n", "y\nx\nx\nb\n", "x\nx\n", "x\n", "@@\n-x\n x\n b\n", "x\nb\n", false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			root, wx, _ := alignmentWindows(t, tc.base, "", tc.base, tc.y) // wa: about to edit; wb: committed y
			gitT(t, wx, "reset", "-q", "--hard", "main")                   // wx: no edit yet
			t.Setenv("WT_CLAUDE_HOOK_BLOCK", "1")
			t.Setenv("WT_CODEX_HOOK_BLOCK", "1")
			verdicts := map[string]string{}
			if tc.old != "" {
				if got := strings.Replace(tc.base, tc.old, tc.new, 1); got != tc.after {
					t.Fatalf("precondition: the Edit produces %q, the case says %q", got, tc.after)
				}
				p, _ := json.Marshal(map[string]any{"cwd": wx, "tool_name": "Edit", "tool_input": map[string]string{
					"file_path": filepath.Join(wx, "data.txt"), "old_string": tc.old, "new_string": tc.new}})
				verdicts["claude"] = confirmedVerdict(t, runHookCapture(t, hookClaudeEdit, string(p)))
			}
			if tc.patch != "" {
				patch := "*** Begin Patch\n*** Update File: data.txt\n" + tc.patch + "*** End Patch\n"
				if got, err := emulateCodex(map[string]string{"data.txt": tc.base}, patch); err != nil || got["data.txt"] != tc.after {
					t.Fatalf("precondition: apply_patch produces %q (%v), the case says %q", got["data.txt"], err, tc.after)
				}
				p, _ := json.Marshal(map[string]any{"cwd": wx, "tool_name": "apply_patch", "tool_input": map[string]string{"command": patch}})
				verdicts["codex"] = confirmedVerdict(t, runHookCapture(t, hookCodexEdit, string(p)))
			}
			writeT(t, wx, "data.txt", tc.after)
			check := checkEntryFor(t, wx, "wb")
			gitT(t, wx, "commit", "-qam", "wx's edit")
			if got := mergeTreeConflicts(t, root, false); got != tc.conflict {
				t.Fatalf("precondition: git merge-tree conflict=%v, the case says %v", got, tc.conflict)
			}
			if got := check.Category == CatBlocking; got != tc.conflict {
				t.Errorf("wt check after the edit: %s, git merge-tree conflict=%v", check.Severity, tc.conflict)
			}
			want := map[bool]string{true: "deny", false: "silent"}[tc.conflict]
			for kind, got := range verdicts {
				if got != want {
					t.Errorf("%s hook: %s, want %s (wt check after the edit: %s)", kind, got, want, check.Severity)
				}
			}
		})
	}
}

// confirmedVerdict is hookVerdict, a deny marked when it is only a file-level
// heads-up (Claude denies those too under WT_CLAUDE_HOOK_BLOCK): the edits here
// are all predictable, so a deny must be a computed overlap.
func confirmedVerdict(t *testing.T, out string) string {
	t.Helper()
	v := hookVerdict(t, out)
	if v == "deny" && !strings.Contains(out, "OVERLAPS") {
		return "deny (file-level)"
	}
	return v
}
