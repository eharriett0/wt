package cli

import (
	"encoding/json"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"

	"github.com/eharriett0/wt/internal/gitx"
)

// #200 end to end, against real worktrees: a collision on a file whose name git
// would C-quote. git printed `café.md` as `"caf\303\251.md"`, wt stored that
// as the window's touched path, and `wt check café.md` said "clear" while
// another window edited the same line. The gitx tests pin each lister; these
// pin what the operator sees, from both sides of each read (an UNCOMMITTED edit
// is read from `git status`, a COMMITTED one from `git diff`), and the two edit
// hooks.

// oddCheckNames are names git C-quotes, a line read splits, or a trim mangles.
// The gitx tests run more shapes (a tab, a backslash, a leading space) through
// each lister; these cover what the layers above them do with a name: a
// non-ASCII directory, a space inside a name or at its end (an argument and a
// hook payload path used to be trimmed), a quote, a newline.
var oddCheckNames = []string{
	"café.md", "dír é/naïve.md", "a b.md", `q"uote.md`, "new\nline.md", "trail.md ",
}

// oddLine is line n of the i-th odd file. The content never repeats the name,
// which may itself hold a newline.
func oddLine(i, n int) string { return fmt.Sprintf("f%d l%02d\n", i, n) }

// oddNamesRepo commits every odd name on main (40 lines each) and adds the
// worktrees wb, which COMMITS an edit of line 5 of every file, and wd, which
// edits nothing; with uncommitted, also wa, which makes wb's edit and leaves it
// UNCOMMITTED.
func oddNamesRepo(t *testing.T, uncommitted bool) (wa, wb, wd string) {
	t.Helper()
	if runtime.GOOS == "windows" {
		t.Skip("quotes, tabs and newlines are not legal in Windows file names")
	}
	hermeticGitT(t)
	offlineGH(t)
	t.Setenv("HOME", t.TempDir())
	t.Setenv("WT_SKIP_COLLISION", "")
	t.Setenv("HOOK_DISABLE_MULTIWINDOW_CHECK", "")
	t.Setenv("WT_CLAUDE_HOOK_BLOCK", "0")
	t.Setenv("WT_CODEX_HOOK_BLOCK", "0")
	t.Chdir(t.TempDir()) // the commands and hooks chdir; restored after

	root := t.TempDir()
	gitT(t, root, "init", "-q", "-b", "main")
	gitT(t, root, "config", "user.email", "t@t.test")
	gitT(t, root, "config", "user.name", "t")
	for i, f := range oddCheckNames {
		var b strings.Builder
		for n := 1; n <= 40; n++ {
			b.WriteString(oddLine(i, n))
		}
		writeNested(t, root, f, b.String())
	}
	gitT(t, root, "add", "-A")
	gitT(t, root, "commit", "-qm", "base")
	base := t.TempDir()
	windows := []string{"wb", "wd"}
	if uncommitted {
		windows = append(windows, "wa")
	}
	for _, w := range windows {
		gitT(t, root, "branch", w)
		gitT(t, root, "worktree", "add", "-q", filepath.Join(base, w), w)
		if w == "wd" {
			continue
		}
		for i, f := range oddCheckNames {
			writeT(t, filepath.Join(base, w), f, strings.Replace(readT(t, root, f), oddLine(i, 5), "EDITED "+oddLine(i, 5), 1))
		}
	}
	gitT(t, filepath.Join(base, "wb"), "commit", "-qam", "wb edits line 5 of every file")
	if uncommitted {
		wa = filepath.Join(base, "wa")
	}
	return wa, filepath.Join(base, "wb"), filepath.Join(base, "wd")
}

// checkPayloadIn runs `wt check --json args...` in cwd and returns its exit
// code and payload.
func checkPayloadIn(t *testing.T, cwd string, args ...string) (int, CheckPayload) {
	t.Helper()
	t.Chdir(cwd)
	r, w, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	stdout := os.Stdout
	os.Stdout = w
	code := cmdCheck(append([]string{"--json"}, args...))
	os.Stdout = stdout
	w.Close()
	out, _ := io.ReadAll(r)
	var p CheckPayload
	if err := json.Unmarshal(out, &p); err != nil {
		t.Fatalf("not a check payload: %v\n%s", err, out)
	}
	return code, p
}

func coversLine(rs []gitx.LineRange, n int) bool {
	for _, r := range rs {
		if r.Start <= n && n <= r.End {
			return true
		}
	}
	return false
}

// wa and wb both edited line 5 of every odd name: each window's `wt check`
// sees the other's edit overlap its own, read from git status (wa's,
// uncommitted) and from git diff (wb's, committed), with both windows' ranges
// read through the same names.
func TestCheckCommand_OddNamesCollide(t *testing.T) {
	wa, wb, _ := oddNamesRepo(t, true)
	for _, side := range []struct{ cwd, other string }{{wa, "wb"}, {wb, "wa"}} {
		code, p := checkPayloadIn(t, side.cwd, oddCheckNames...) // one Scan for every name
		got := map[string]CheckEntry{}
		for _, e := range p.Entries {
			got[e.Path+"←"+e.Window] = e
		}
		if code != 3 || len(got) != len(oddCheckNames) {
			t.Errorf("wt check <every odd name> in %s = exit %d, %d entries; want exit 3 and %d",
				filepath.Base(side.cwd), code, len(got), len(oddCheckNames))
		}
		for _, f := range oddCheckNames {
			if e, ok := got[f+"←"+side.other]; !ok || e.Severity != "HIGH" || !coversLine(e.OverlapSpans, 5) {
				t.Errorf("wt check %q in %s: %s entry %+v (listed %v), want HIGH overlapping line 5",
					f, filepath.Base(side.cwd), side.other, e, ok)
			}
		}
	}
}

// Both edit hooks grade an agent's pending edit of an odd name the way `wt
// check` grades the edit once made (#92): line 5 overlaps wb's edit. The first
// name also pins the controls: line 30 is clear of it, and the block switch
// turns the overlap into a deny.
func TestEditHooks_OddNamesCollide(t *testing.T) {
	_, _, wd := oddNamesRepo(t, false)
	claude := func(i, line int) string {
		p, _ := json.Marshal(map[string]any{"cwd": wd, "tool_name": "Edit", "tool_input": map[string]string{
			"file_path": filepath.Join(wd, oddCheckNames[i]), "old_string": oddLine(i, line), "new_string": "X " + oddLine(i, line)}})
		return string(p)
	}
	codex := func(i, line int) string {
		l := strings.TrimSuffix(oddLine(i, line), "\n")
		p, _ := json.Marshal(map[string]any{"cwd": wd, "tool_name": "apply_patch", "tool_input": map[string]string{
			"command": "*** Begin Patch\n*** Update File: " + oddCheckNames[i] + "\n@@\n-" + l + "\n+X " + l + "\n*** End Patch\n"}})
		return string(p)
	}
	type editHook struct {
		name, blockEnv string
		hook           func(io.Reader) int
		payload        func(i, line int) string
	}
	for i, f := range oddCheckNames {
		hooks := []editHook{{"claude-edit", "WT_CLAUDE_HOOK_BLOCK", hookClaudeEdit, claude}}
		// apply_patch is line-based and codex trims a path's surrounding space:
		// it can't name these files at all
		if !strings.Contains(f, "\n") && strings.TrimSpace(f) == f {
			hooks = append(hooks, editHook{"codex-edit", "WT_CODEX_HOOK_BLOCK", hookCodexEdit, codex})
		}
		for _, h := range hooks {
			if got := hookVerdict(t, runHookCapture(t, h.hook, h.payload(i, 5))); got != "overlap" {
				t.Errorf("%s of line 5 of %q: %s, want overlap", h.name, f, got)
			}
			if i > 0 {
				continue
			}
			if got := hookVerdict(t, runHookCapture(t, h.hook, h.payload(i, 30))); got != "silent" {
				t.Errorf("%s of line 30 of %q: %s, want silent", h.name, f, got)
			}
			t.Setenv(h.blockEnv, "1")
			if got := hookVerdict(t, runHookCapture(t, h.hook, h.payload(i, 5))); got != "deny" {
				t.Errorf("%s of line 5 of %q with %s=1: %s, want deny", h.name, f, h.blockEnv, got)
			}
			t.Setenv(h.blockEnv, "0")
		}
	}
}
