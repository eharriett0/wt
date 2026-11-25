package cli

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// #256: a window git can't run in (a locked worktree on an unmounted volume,
// here its directory moved away) is graded at the commit git lists for it,
// from this repository, exactly as in place: an overlap with what it
// committed is HIGH, a disjoint edit low, the same section of a structured doc
// HIGH and another section advisory, and pre-push blocks on the overlap. Its
// uncommitted edits stay named as not compared.
func TestUnreachableWindow_gradedAtItsCommit(t *testing.T) {
	offlineGH(t)
	t.Setenv("HOME", t.TempDir())
	t.Setenv("WT_SKIP_COLLISION", "")
	t.Setenv("HOOK_DISABLE_MULTIWINDOW_CHECK", "")
	real := func(p string) string {
		r, err := filepath.EvalSymlinks(p)
		if err != nil {
			t.Fatal(err)
		}
		return r
	}
	root := real(t.TempDir())
	gitT(t, root, "init", "-q", "-b", "main")
	gitT(t, root, "config", "user.email", "t@t.test")
	gitT(t, root, "config", "user.name", "t")
	var x strings.Builder
	for i := 1; i <= 10; i++ {
		fmt.Fprintf(&x, "line %d\n", i)
	}
	const plan = "# Plan\n## Alpha\na1\na2\na3\n## Beta\nb1\nb2\nb3\n"
	writeT(t, root, ".wt.conf", "shared_docs = PLAN.md\nstructured_doc.PLAN.md = ^##\\s\n")
	writeT(t, root, "x.go", x.String())
	writeT(t, root, "PLAN.md", plan)
	gitT(t, root, "add", "-A")
	gitT(t, root, "commit", "-qm", "base")
	dirs := real(t.TempDir())
	lane, me := filepath.Join(dirs, "lane"), filepath.Join(dirs, "me")
	gitT(t, root, "worktree", "add", "-q", "-b", "lane", lane, "main")
	gitT(t, root, "worktree", "add", "-q", "-b", "me", me, "main")
	writeT(t, lane, "x.go", strings.Replace(x.String(), "line 2\n", "LANE 2\n", 1))
	// A pure insertion in Beta: with lane's directory gone, only its copy at
	// the commit (not its deletions, it has none) places it in a section.
	writeT(t, lane, "PLAN.md", strings.Replace(plan, "b1\n", "b1\nLnew\n", 1))
	gitT(t, lane, "commit", "-qam", "lane")
	gitT(t, root, "worktree", "lock", lane)

	check := func(dir, path string) (int, string) {
		t.Chdir(dir)
		var code int
		out, errOut := captureOutputForTest(t, func() { code = cmdCheck([]string{path}) })
		return code, out + errOut
	}
	// The Claude edit hook (block mode) on me's PLAN.md: deny iff the edit
	// shares lane's section (Beta), as wt check grades it once made.
	t.Setenv("WT_CLAUDE_HOOK_BLOCK", "1")
	hookDenies := func(oldLine, newLine string) bool {
		payload, _ := json.Marshal(map[string]any{"cwd": me, "tool_name": "Edit", "tool_input": map[string]string{
			"file_path": filepath.Join(me, "PLAN.md"), "old_string": oldLine, "new_string": newLine}})
		t.Chdir(me)
		return strings.Contains(runHookCapture(t, hookClaudeEdit, string(payload)), `"permissionDecision":"deny"`)
	}
	type grades struct {
		x2, x8, beta, alpha int
		hookBeta, hookAlpha bool
	}
	measure := func() grades {
		var g grades
		g.hookBeta, g.hookAlpha = hookDenies("b2\n", "Mb2\n"), hookDenies("a1\n", "Ma1\n")
		writeT(t, me, "x.go", strings.Replace(x.String(), "line 2\n", "ME 2\n", 1))
		g.x2, _ = check(me, "x.go")
		writeT(t, me, "x.go", strings.Replace(x.String(), "line 8\n", "ME 8\n", 1))
		g.x8, _ = check(me, "x.go")
		writeT(t, me, "x.go", x.String())
		writeT(t, me, "PLAN.md", strings.Replace(plan, "b2\n", "Mb2\n", 1))
		g.beta, _ = check(me, "PLAN.md")
		writeT(t, me, "PLAN.md", strings.Replace(plan, "a1\n", "Ma1\n", 1))
		g.alpha, _ = check(me, "PLAN.md")
		writeT(t, me, "PLAN.md", plan)
		return g
	}
	want := grades{x2: 3, x8: 0, beta: 3, alpha: 0, hookBeta: true, hookAlpha: false}
	if got := measure(); got != want {
		t.Fatalf("precondition, lane in place: %+v, want %+v", got, want)
	}
	// pre-push in place: me commits an overlap and pushes; a disjoint section
	// of the structured doc does not block.
	push := func(file, content string) bool {
		writeT(t, me, file, content)
		gitT(t, me, "commit", "-qam", "me")
		var blocked bool
		captureOutputForTest(t, func() { blocked = prePushRefs(t, me, "me") }) // its report goes to stderr
		gitT(t, me, "reset", "-q", "--hard", "main")
		return blocked
	}
	pushOverlap := func() bool {
		return push("x.go", strings.Replace(x.String(), "line 2\n", "ME 2\n", 1)) &&
			!push("x.go", strings.Replace(x.String(), "line 8\n", "ME 8\n", 1)) &&
			push("PLAN.md", strings.Replace(plan, "b2\n", "Mb2\n", 1)) &&
			!push("PLAN.md", strings.Replace(plan, "a1\n", "Ma1\n", 1))
	}
	if !pushOverlap() {
		t.Fatal("precondition: pre-push blocks on lane's line and section (not on another line or section) while lane is in place")
	}

	if err := os.Rename(lane, lane+"-unmounted"); err != nil {
		t.Fatal(err)
	}
	if got := measure(); got != want {
		t.Errorf("lane's directory gone (locked): %+v, want %+v, as in place", got, want)
	}
	if !pushOverlap() {
		t.Error("lane's directory gone: pre-push did not grade as in place (line and section block, another line or section doesn't)")
	}
	writeT(t, me, "x.go", strings.Replace(x.String(), "line 2\n", "ME 2\n", 1))
	if code, out := check(me, "x.go"); code != 3 || !strings.Contains(out, "window lane could not be read") || !strings.Contains(out, "its uncommitted edits are not compared") {
		t.Errorf("wt check x.go: exit %d, want 3 with lane named, its uncommitted edits not compared:\n%s", code, out)
	}
}

// #256 with #255: a nested worktree without its .git is graded at its
// commit, and never through git run in its directory, which reads the
// checkout around it. Not under an ignored directory, its committed file is
// untracked in the main checkout, and the untracked downgrade (#113) read the
// main checkout's status as the window's: a real add/add read as advisory.
func TestNestedWindowWithoutGit_gradedAtItsCommit(t *testing.T) {
	offlineGH(t)
	t.Setenv("HOME", t.TempDir())
	t.Setenv("WT_SKIP_COLLISION", "")
	root, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	gitT(t, root, "init", "-q", "-b", "main")
	gitT(t, root, "config", "user.email", "t@t.test")
	gitT(t, root, "config", "user.name", "t")
	writeT(t, root, "a.txt", "a\n")
	gitT(t, root, "add", "-A")
	gitT(t, root, "commit", "-qm", "base")
	n := filepath.Join(root, ".wt", "n")
	gitT(t, root, "worktree", "add", "-q", "-b", "n", n, "main")
	writeT(t, n, "y.go", "package n\n")
	gitT(t, n, "add", "y.go")
	gitT(t, n, "commit", "-qm", "n adds y.go")
	writeT(t, root, "y.go", "package main\n") // main creates the same file, untracked here
	check := func() (int, string) {
		t.Chdir(root)
		var code int
		out, errOut := captureOutputForTest(t, func() { code = cmdCheck([]string{"y.go"}) })
		return code, out + errOut
	}
	if code, out := check(); code != 3 {
		t.Fatalf("precondition, n in place: wt check y.go exit %d, want 3 (add/add):\n%s", code, out)
	}
	if err := os.Remove(filepath.Join(n, ".git")); err != nil {
		t.Fatal(err)
	}
	if code, out := check(); code != 3 || !strings.Contains(out, "window n could not be read") {
		t.Errorf("n without its .git: wt check y.go exit %d, want 3 with n named:\n%s", code, out)
	}
	// Pre-push from another worktree adding y.go: blocked (add/add with n's
	// commit), never downgraded by the main checkout's untracked y.go.
	me := filepath.Join(filepath.Dir(root), filepath.Base(root)+"-me")
	gitT(t, root, "worktree", "add", "-q", "-b", "me", me, "main")
	writeT(t, me, "y.go", "package me\n")
	gitT(t, me, "add", "y.go")
	gitT(t, me, "commit", "-qm", "me adds y.go")
	var blocked bool
	captureOutputForTest(t, func() { blocked = prePushRefs(t, me, "me") })
	if !blocked {
		t.Error("pre-push of y.go from me: not blocked, want the add/add with n's commit")
	}
}
