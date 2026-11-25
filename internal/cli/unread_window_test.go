package cli

import (
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/eharriett0/wt/internal/collide"
	"github.com/eharriett0/wt/internal/config"
	"github.com/eharriett0/wt/internal/gitx"
	"github.com/eharriett0/wt/internal/hooks"
)

// withUnreadNote adds the unread note to an edit hook's decision without ever
// denying on its own (#253).
func TestWithUnreadNote(t *testing.T) {
	const note = "window wb could not be read (git status: fatal: bad object HEAD)"
	decode := func(t *testing.T, s string) preToolUseOutput {
		t.Helper()
		var d preToolUseOutput
		if err := json.Unmarshal([]byte(s), &d); err != nil {
			t.Fatalf("not hook JSON: %v\n%s", err, s)
		}
		return d
	}
	if out, has := withUnreadNote(`{"x":1}`, true, ""); out != `{"x":1}` || !has {
		t.Errorf("no note changed the decision: %s %v", out, has)
	}
	if out, has := withUnreadNote("", false, ""); out != "" || has {
		t.Errorf("no note, no decision: %q %v, want none", out, has)
	}
	out, has := withUnreadNote("", false, note)
	if d := decode(t, out).HookSpecificOutput; !has || d.HookEventName != "PreToolUse" || d.AdditionalContext != "wt: "+note || d.PermissionDecision != "" {
		t.Errorf("note alone: %s, want context only, no decision", out)
	}
	advisory, _ := claudeDecision("notes.txt", []pendingGrade{{entry: CheckEntry{Window: "wc", Liveness: "active"}}}, false)
	out, has = withUnreadNote(advisory, true, note)
	if d := decode(t, out).HookSpecificOutput; !has || !strings.HasPrefix(d.AdditionalContext, decode(t, advisory).HookSpecificOutput.AdditionalContext+"\n") || !strings.HasSuffix(d.AdditionalContext, note) || d.PermissionDecision != "" {
		t.Errorf("advisory: %s, want its context then the note, no decision", out)
	}
	deny, _ := claudeDecision("notes.txt", []pendingGrade{{entry: CheckEntry{Window: "wc", Liveness: "active"}, confirmed: true}}, true)
	out, has = withUnreadNote(deny, true, note)
	if d := decode(t, out).HookSpecificOutput; !has || d.PermissionDecision != "deny" || !strings.HasSuffix(d.PermissionDecisionReason, "\nwt: "+note) || d.AdditionalContext != "" {
		t.Errorf("deny: %s, want still deny, the note after its reason", out)
	}
	if out, has := withUnreadNote("not json", true, note); out != "not json" || !has {
		t.Errorf("unparseable decision: %q %v, want it as it was", out, has)
	}
}

// #253: a window whose `git status` fails (here its branch holds an object
// the repo lacks) is named by every collision surface, never read as "no
// changes" or a bare clear, and never blocks. Control: the same windows with
// wb's branch intact, where wb's edit of line 5 is HIGH and the hooks deny. A
// worktree whose directory is gone is not unread in either.
func TestUnreadWindow_namedEverywhere(t *testing.T) {
	offlineGH(t)
	t.Setenv("HOME", t.TempDir())
	t.Setenv("WT_SKIP_COLLISION", "")
	t.Setenv("HOOK_DISABLE_MULTIWINDOW_CHECK", "")
	t.Setenv("WT_CLAUDE_HOOK_BLOCK", "1")
	t.Setenv("WT_CODEX_HOOK_BLOCK", "1")
	t.Setenv("GIT_DEFAULT_REF_FORMAT", "files") // wb's branch is broken by writing its ref file
	t.Chdir(t.TempDir())
	root := t.TempDir()
	gitT(t, root, "init", "-q", "-b", "main")
	gitT(t, root, "config", "user.email", "t@t.test")
	gitT(t, root, "config", "user.name", "t")
	var notes strings.Builder
	for i := 1; i <= 9; i++ {
		fmt.Fprintf(&notes, "line %d\n", i)
	}
	writeT(t, root, "notes.txt", notes.String())
	writeT(t, root, "other.txt", "alone\n") // no other window touches it
	gitT(t, root, "add", "-A")
	gitT(t, root, "commit", "-qm", "base")
	dirs := t.TempDir()
	wa, wb, wc := filepath.Join(dirs, "wa"), filepath.Join(dirs, "wb"), filepath.Join(dirs, "wc")
	gitT(t, root, "worktree", "add", "-q", "-b", "wa", wa, "main")
	gitT(t, root, "worktree", "add", "-q", "-b", "wb", wb, "main")
	gitT(t, root, "worktree", "add", "-q", "-b", "wc", wc, "main")
	// wd's directory is deleted (git lists it as prunable): it holds no
	// uncommitted edits, so it is never unread, in either state.
	wd := filepath.Join(dirs, "wd")
	gitT(t, root, "worktree", "add", "-q", "-b", "wd", wd, "main")
	if err := os.RemoveAll(wd); err != nil {
		t.Fatal(err)
	}
	writeT(t, wa, "notes.txt", strings.Replace(notes.String(), "line 5\n", "A line 5\n", 1))
	writeT(t, wb, "notes.txt", strings.Replace(notes.String(), "line 5\n", "B line 5\n", 1))
	// wc edits the same file far from line 5: an entry the hooks re-grade and
	// drop, so the note also rides on a run that had entries.
	writeT(t, wc, "notes.txt", strings.Replace(notes.String(), "line 9\n", "C line 9\n", 1))
	claude, _ := json.Marshal(map[string]any{"cwd": wa, "tool_name": "Edit", "tool_input": map[string]string{
		"file_path": filepath.Join(wa, "notes.txt"), "old_string": "A line 5\n", "new_string": "AA line 5\n"}})
	codex, _ := json.Marshal(map[string]any{"cwd": wa, "tool_name": "apply_patch", "tool_input": map[string]string{
		"command": "*** Begin Patch\n*** Update File: notes.txt\n@@\n-A line 5\n+AA line 5\n*** End Patch\n"}})
	alone, _ := json.Marshal(map[string]any{"cwd": wa, "tool_name": "Edit", "tool_input": map[string]string{
		"file_path": filepath.Join(wa, "other.txt"), "old_string": "alone\n", "new_string": "still alone\n"}})
	turn := fmt.Sprintf(`{"cwd":%q,"hook_event_name":"UserPromptSubmit"}`, wa)

	type seen struct {
		status, statusJSON, statusBlocking, check, checkBlocking, checkJSON, mcp string
		claude, codex, turn, prePush, preCommit                                  string
		claudeAlone, checkAlone                                                  string
		checkUnknown, mcpUnknown, codexTurn, mcpStatus                           string
		checkUnknownCode                                                         int
		checkCode, checkBlockingCode, statusBlockingCode                         int
		prePushBlocked                                                           bool
	}
	observe := func(t *testing.T) seen {
		t.Helper()
		t.Chdir(wa)
		var s seen
		out, errOut := captureOutputForTest(t, func() { cmdStatus(nil) })
		s.status = out + errOut
		s.statusJSON, _ = captureOutputForTest(t, func() { cmdStatus([]string{"--json"}) })
		out, errOut = captureOutputForTest(t, func() { s.statusBlockingCode = cmdStatus([]string{"--blocking"}) })
		s.statusBlocking = out + errOut
		out, errOut = captureOutputForTest(t, func() { s.checkCode = cmdCheck([]string{"notes.txt"}) })
		s.check = out + errOut
		out, errOut = captureOutputForTest(t, func() { s.checkBlockingCode = cmdCheck([]string{"--blocking", "notes.txt"}) })
		s.checkBlocking = out + errOut
		s.checkJSON, _ = captureOutputForTest(t, func() { cmdCheck([]string{"--json", "notes.txt"}) })
		out, errOut = captureOutputForTest(t, func() { cmdCheck([]string{"other.txt"}) })
		s.checkAlone = out + errOut
		out, errOut = captureOutputForTest(t, func() { s.checkUnknownCode = cmdCheck([]string{"pkg/new.go"}) })
		s.checkUnknown = out + errOut
		c, err := config.Load()
		if err != nil {
			t.Fatal(err)
		}
		s.mcp, _ = mcpCheck(c, []string{"notes.txt"}, false, false)
		s.mcpUnknown, _ = mcpCheck(c, []string{"pkg/new.go"}, false, false)
		s.mcpStatus, _ = mcpStatus(c, false)
		s.claude = runHookCapture(t, hookClaudeEdit, string(claude))
		s.codex = runHookCapture(t, hookCodexEdit, string(codex))
		s.claudeAlone = runHookCapture(t, hookClaudeEdit, string(alone))
		s.turn = captureStdoutForTest(t, func() { withStdinForTest(t, turn, func() { runHook([]string{"claude-context"}) }) })
		s.codexTurn = captureStdoutForTest(t, func() { withStdinForTest(t, turn, func() { runHook([]string{"codex-context"}) }) })
		_, s.prePush = captureOutputForTest(t, func() { s.prePushBlocked = prePushRefs(t, wa, "wa", "wa-again") })
		t.Chdir(wa)
		_, s.preCommit = captureOutputForTest(t, func() { hooks.HookPreCommit(c) })
		return s
	}

	const named = "window wb could not be read"
	healthy := observe(t)
	for name, out := range map[string]string{"status": healthy.status, "status --json": healthy.statusJSON, "status --blocking": healthy.statusBlocking,
		"check": healthy.check, "check --blocking": healthy.checkBlocking, "check --json": healthy.checkJSON, "MCP wt_check": healthy.mcp, "claude-edit": healthy.claude, "codex-edit": healthy.codex,
		"check other.txt": healthy.checkAlone, "claude-edit other.txt": healthy.claudeAlone,
		"check pkg/new.go": healthy.checkUnknown, "MCP wt_check pkg/new.go": healthy.mcpUnknown, "codex per-turn": healthy.codexTurn, "MCP wt_status": healthy.mcpStatus,
		"per-turn": healthy.turn, "pre-push": healthy.prePush, "pre-commit": healthy.preCommit} {
		if strings.Contains(out, "could not be read") || strings.Contains(out, `"unread`) {
			t.Errorf("control, %s names an unread window:\n%s", name, out)
		}
	}
	if healthy.checkCode != 3 || healthy.checkBlockingCode != 3 || healthy.statusBlockingCode != 3 ||
		!strings.Contains(healthy.claude, `"permissionDecision":"deny"`) || !strings.Contains(healthy.codex, `"permissionDecision":"deny"`) {
		t.Fatalf("precondition: with wb intact, check exit %d, --blocking %d, status --blocking %d, and the hooks deny?\n%s\n%s",
			healthy.checkCode, healthy.checkBlockingCode, healthy.statusBlockingCode, healthy.claude, healthy.codex)
	}

	ref := filepath.Join(root, ".git", "refs", "heads", "wb")
	if _, err := os.Stat(ref); err != nil {
		t.Fatalf("precondition: wb's ref is a file: %v", err)
	}
	writeT(t, filepath.Dir(ref), "wb", strings.Repeat("1", 40)+"\n")
	broken := observe(t)

	// wt status: wb is UNREAD, never "(no changes)", and the verdict is
	// qualified, not "all clear".
	lines := strings.Split(broken.status, "\n")
	found := false
	for i, ln := range lines {
		if !strings.HasSuffix(strings.TrimSpace(ln), string(filepath.Separator)+"wb") || i+1 >= len(lines) {
			continue
		}
		found = true
		// HEAD is the missing object, so its commits weren't read either.
		if next := lines[i+1]; !strings.Contains(next, "UNREAD") || !strings.Contains(next, "bad object HEAD") || !strings.Contains(next, "none of its edits are compared") {
			t.Errorf("wt status, the line under wb's worktree: %q, want UNREAD with git's reason and none of its edits compared", next)
		}
		for _, l := range lines[i+1:] { // wb's block runs to the blank line
			if strings.TrimSpace(l) == "" {
				break
			}
			if strings.Contains(l, "(no changes)") {
				t.Errorf("wt status still says wb has no changes:\n%s", broken.status)
			}
		}
	}
	if !found {
		t.Fatalf("wt status lists no wb worktree:\n%s", broken.status)
	}
	if broken.statusBlockingCode != 0 || !strings.Contains(broken.statusBlocking, named) || !strings.Contains(broken.statusBlocking, "wt could read") {
		t.Errorf("wt status --blocking: exit %d, want 0 with wb named and the verdict qualified:\n%s", broken.statusBlockingCode, broken.statusBlocking)
	}
	if !strings.Contains(broken.status, named) || strings.Contains(broken.status, "all clear") || !strings.Contains(broken.status, "wt could read") {
		t.Errorf("wt status doesn't name wb, or still says all clear:\n%s", broken.status)
	}
	var st StatusPayload
	if err := json.Unmarshal([]byte(broken.statusJSON), &st); err != nil {
		t.Fatalf("status --json: %v\n%s", err, broken.statusJSON)
	}
	var wbUnread, wbNotCompared string
	for _, w := range st.Windows {
		if w.Label == "wb" {
			wbUnread, wbNotCompared = w.Unread, w.NotCompared
		}
	}
	if !strings.Contains(wbUnread, "bad object HEAD") || wbNotCompared != "all" {
		t.Errorf("status --json: wb's unread = %q, not_compared %q; want git's reason, all\n%s", wbUnread, wbNotCompared, broken.statusJSON)
	}

	// wt check: named, the clear qualified, and no block (exit 0).
	if broken.checkCode != 0 || !strings.Contains(broken.check, named) || !strings.Contains(broken.check, "1 could not be read") {
		t.Errorf("wt check: exit %d, want 0 with wb named and the clear qualified:\n%s", broken.checkCode, broken.check)
	}
	if broken.checkBlockingCode != 0 || !strings.Contains(broken.checkBlocking, named) || !strings.Contains(broken.checkBlocking, "1 could not be read") {
		t.Errorf("wt check --blocking: exit %d, want 0 with wb named and the clear qualified:\n%s", broken.checkBlockingCode, broken.checkBlocking)
	}
	for name, out := range map[string]string{"check --json": broken.checkJSON, "MCP wt_check": broken.mcp} {
		var p CheckPayload
		if err := json.Unmarshal([]byte(out), &p); err != nil {
			t.Fatalf("%s: %v\n%s", name, err, out)
		}
		if len(p.UnreadWindows) != 1 || p.UnreadWindows[0].Window != "wb" || !strings.Contains(p.UnreadWindows[0].Error, "bad object HEAD") || p.UnreadWindows[0].NotCompared != "all" || p.Blocking {
			t.Errorf("%s: unread_windows = %+v, blocking %v; want wb with git's reason, not blocking", name, p.UnreadWindows, p.Blocking)
		}
	}

	if !strings.Contains(broken.checkAlone, named) || !strings.Contains(broken.checkAlone, "no other window is touching other.txt (of the windows wt could read; 1 could not be read)") {
		t.Errorf("wt check of a file no window touches: want wb named and the clear qualified:\n%s", broken.checkAlone)
	}

	// Both edit hooks (block mode on): the note, never a deny, also for a file
	// no other window touches (the Claude hook's no-entries return).
	if strings.TrimSpace(healthy.claudeAlone) != "" {
		t.Errorf("control, claude-edit of a file no window touches printed: %s", healthy.claudeAlone)
	}
	for name, out := range map[string]string{"claude-edit": broken.claude, "codex-edit": broken.codex, "claude-edit other.txt": broken.claudeAlone} {
		if !strings.Contains(out, named) || strings.Contains(out, `"permissionDecision"`) {
			t.Errorf("%s: want the note as context, no decision:\n%s", name, out)
		}
	}
	var ctx preToolUseOutput
	_ = json.Unmarshal([]byte(broken.turn), &ctx)
	if !strings.Contains(ctx.HookSpecificOutput.AdditionalContext, named) {
		t.Errorf("per-turn context doesn't name wb:\n%s", broken.turn)
	}

	// A path no window wt could read touches is still refused (#93), and the
	// refusal names wb: it may be the window creating that path.
	if healthy.checkUnknownCode != 64 || broken.checkUnknownCode != 64 || !strings.Contains(broken.checkUnknown, named) || !strings.Contains(broken.checkUnknown, "no window wt could read is touching") {
		t.Errorf("wt check pkg/new.go: exit %d then %d, want 64 both, the second naming wb:\n%s", healthy.checkUnknownCode, broken.checkUnknownCode, broken.checkUnknown)
	}
	if !strings.Contains(broken.mcpUnknown, named) {
		t.Errorf("MCP wt_check pkg/new.go doesn't name wb:\n%s", broken.mcpUnknown)
	}
	if !strings.Contains(broken.codexTurn, named) {
		t.Errorf("codex per-turn context doesn't name wb:\n%s", broken.codexTurn)
	}
	var mst StatusPayload
	if err := json.Unmarshal([]byte(broken.mcpStatus), &mst); err != nil {
		t.Fatalf("MCP wt_status: %v\n%s", err, broken.mcpStatus)
	}
	mcpUnread := ""
	for _, w := range mst.Windows {
		if w.Label == "wb" {
			mcpUnread = w.Unread
		}
	}
	if !strings.Contains(mcpUnread, "bad object HEAD") {
		t.Errorf("MCP wt_status: wb's unread = %q, want git's reason", mcpUnread)
	}

	// Pre-push: once per push, never blocking; pre-commit: named.
	if n := strings.Count(broken.prePush, named); n != 1 || broken.prePushBlocked {
		t.Errorf("pre-push of two refs: named %d time(s), blocked %v; want once, not blocked:\n%s", n, broken.prePushBlocked, broken.prePush)
	}
	if !strings.Contains(broken.preCommit, named) {
		t.Errorf("pre-commit doesn't name wb:\n%s", broken.preCommit)
	}

	// With no overlap left, wt status reaches its no-collisions verdict: still
	// about the windows wt could read, never "all clear" (#254 review: the
	// states above always had an overlap).
	writeT(t, wa, "notes.txt", notes.String())
	writeT(t, wc, "notes.txt", notes.String())
	t.Chdir(wa)
	out, errOut := captureOutputForTest(t, func() { cmdStatus(nil) })
	if quiet := out + errOut; !strings.Contains(quiet, "no file collisions among the") || strings.Contains(quiet, "all clear") || !strings.Contains(quiet, named) {
		t.Errorf("wt status with no overlap: want wb named and the verdict about the windows wt could read:\n%s", quiet)
	}
}

// An unread window that COMMITTED a change to the path is still graded (#254
// review): its working copy can't be read, so the grade is indeterminate and
// HIGH (the fail-safe), and it is tagged "working tree unread", not
// "uncommitted edits". Control: the same commits with wh's index intact are
// disjoint (low, exit 0).
func TestUnreadWindow_committedPathStillGraded(t *testing.T) {
	offlineGH(t)
	t.Setenv("HOME", t.TempDir())
	t.Setenv("WT_SKIP_COLLISION", "")
	t.Chdir(t.TempDir())
	root := t.TempDir()
	gitT(t, root, "init", "-q", "-b", "main")
	gitT(t, root, "config", "user.email", "t@t.test")
	gitT(t, root, "config", "user.name", "t")
	var notes strings.Builder
	for i := 1; i <= 9; i++ {
		fmt.Fprintf(&notes, "line %d\n", i)
	}
	writeT(t, root, "notes.txt", notes.String())
	gitT(t, root, "add", "-A")
	gitT(t, root, "commit", "-qm", "base")
	dirs := t.TempDir()
	wa, wh := filepath.Join(dirs, "wa"), filepath.Join(dirs, "wh")
	gitT(t, root, "worktree", "add", "-q", "-b", "wa", wa, "main")
	gitT(t, root, "worktree", "add", "-q", "-b", "wh", wh, "main")
	writeT(t, wa, "notes.txt", strings.Replace(notes.String(), "line 5\n", "A line 5\n", 1))
	gitT(t, wa, "commit", "-qam", "wa")
	writeT(t, wh, "notes.txt", strings.Replace(notes.String(), "line 1\n", "H line 1\n", 1))
	gitT(t, wh, "commit", "-qam", "wh")
	check := func() (int, string) {
		t.Chdir(wa)
		var code int
		out, errOut := captureOutputForTest(t, func() { code = cmdCheck([]string{"notes.txt"}) })
		return code, out + errOut
	}
	if code, out := check(); code != 0 || strings.Contains(out, "could not be read") {
		t.Fatalf("precondition: with wh's index intact, exit %d (want 0) and no unread window:\n%s", code, out)
	}
	index := filepath.Join(root, ".git", "worktrees", "wh", "index")
	if _, err := os.Stat(index); err != nil {
		t.Fatalf("precondition: wh's index is at %s: %v", index, err)
	}
	writeT(t, filepath.Dir(index), "index", "not an index\n")
	code, out := check()
	if code != 3 || !strings.Contains(out, "window wh could not be read") || !strings.Contains(out, "working tree unread") || strings.Contains(out, "uncommitted edits]") {
		t.Errorf("wh's index unreadable: exit %d, want 3 (indeterminate), wh named and tagged \"working tree unread\":\n%s", code, out)
	}
	// wt status says why the overlap is indeterminate, not "e.g. binary".
	sout, serr := captureOutputForTest(t, func() { cmdStatus(nil) })
	if !strings.Contains(sout+serr, "wh's working tree could not be read") || strings.Contains(sout+serr, "e.g. binary") {
		t.Errorf("wt status: want the indeterminate overlap explained by wh's unread working tree:\n%s", sout+serr)
	}
	bout, berr := captureOutputForTest(t, func() { cmdStatus([]string{"--blocking"}) })
	if !strings.Contains(bout+berr, "wh's working tree could not be read") {
		t.Errorf("wt status --blocking: want the indeterminate overlap explained by wh's unread working tree:\n%s", bout+berr)
	}
}

// Scan reads each worktree as git lists it (#253 review): a removed worktree
// git would prune is not unread, a locked one whose directory is gone (an
// unmounted volume) is, one git can't enter is (git calls it prunable too, but
// its directory is there), a path ending in a space is read whole, and the main
// work tree of a --separate-git-dir repository reads itself from inside it
// and is named unread from another window.
func TestScan_worktreeStates(t *testing.T) {
	offlineGH(t)
	t.Setenv("HOME", t.TempDir())
	real := func(t *testing.T, p string) string {
		t.Helper()
		r, err := filepath.EvalSymlinks(p)
		if err != nil {
			t.Fatal(err)
		}
		return r
	}
	scan := func(t *testing.T, dir string) []collide.Window {
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
		return ws
	}
	at := func(ws []collide.Window, worktree string) (collide.Window, bool) {
		for _, w := range ws {
			if w.Worktree == worktree {
				return w, true
			}
		}
		return collide.Window{}, false
	}
	has := func(files []string, f string) bool {
		for _, x := range files {
			if x == f {
				return true
			}
		}
		return false
	}

	root := real(t, t.TempDir())
	gitT(t, root, "init", "-q", "-b", "main")
	gitT(t, root, "config", "user.email", "t@t.test")
	gitT(t, root, "config", "user.name", "t")
	writeT(t, root, "notes.txt", "a\n")
	gitT(t, root, "add", "-A")
	gitT(t, root, "commit", "-qm", "base")
	dirs := real(t, t.TempDir())
	gone, locked, spaced := filepath.Join(dirs, "gone"), filepath.Join(dirs, "locked"), filepath.Join(dirs, "spaced ")
	for br, p := range map[string]string{"gone": gone, "locked": locked, "spaced": spaced} {
		gitT(t, root, "worktree", "add", "-q", "-b", br, p, "main")
	}
	gitT(t, root, "worktree", "lock", locked)
	for _, p := range []string{gone, locked} {
		if err := os.RemoveAll(p); err != nil {
			t.Fatal(err)
		}
	}
	writeT(t, spaced, "notes.txt", "spaced edit\n")
	// Nested worktrees (#255), in an ignored directory: one healthy (the
	// control), one whose .git file is gone, one left as an empty directory
	// (an unmounted volume's mount point). git lists the last two as
	// prunable, and git status there SUCCEEDS on the main checkout.
	writeT(t, root, ".gitignore", ".wt/\n")
	nest := map[string]string{}
	for _, n := range []string{"ok", "nogit", "empty"} {
		nest[n] = filepath.Join(root, ".wt", n)
		gitT(t, root, "worktree", "add", "-q", "-b", "n-"+n, nest[n], "main")
		writeT(t, nest[n], "n-"+n+".txt", "nested edit\n")
	}
	if err := os.Remove(filepath.Join(nest["nogit"], ".git")); err != nil {
		t.Fatal(err)
	}
	if err := os.Rename(nest["empty"], filepath.Join(dirs, "empty-unmounted")); err != nil {
		t.Fatal(err)
	}
	if err := os.Mkdir(nest["empty"], 0o755); err != nil {
		t.Fatal(err)
	}

	// A worktree git reads, except for a directory it can't list: partly
	// unread, the rest kept (#254 review).
	partial := ""
	if os.Geteuid() != 0 {
		partial = filepath.Join(dirs, "partial")
		gitT(t, root, "worktree", "add", "-q", "-b", "partial", partial, "main")
		if err := os.MkdirAll(filepath.Join(partial, "u"), 0o755); err != nil {
			t.Fatal(err)
		}
		writeT(t, partial, "u/new.txt", "n\n")
		writeT(t, partial, "p.txt", "partial's own file\n")
		if err := os.Chmod(filepath.Join(partial, "u"), 0); err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { _ = os.Chmod(filepath.Join(partial, "u"), 0o755) })
	}
	shut := ""
	if os.Geteuid() != 0 { // root enters a mode-000 directory
		shut = filepath.Join(dirs, "shut")
		gitT(t, root, "worktree", "add", "-q", "-b", "shut", shut, "main")
		if err := os.Chmod(shut, 0); err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { _ = os.Chmod(shut, 0o755) })
	}
	ws := scan(t, root)
	if w, ok := at(ws, nest["ok"]); !ok || w.Unread != "" || !has(w.Touched, "n-ok.txt") {
		t.Errorf("healthy nested worktree: %+v (listed %v), want read, with its edit", w, ok)
	}
	for _, n := range []string{"nogit", "empty"} {
		w, ok := at(ws, nest[n])
		if !ok || w.Unread != collide.ErrNoGitLink || len(w.Touched) != 0 || w.Branch != "n-"+n || collide.NotCompared(w) != "all" {
			t.Errorf("nested worktree %s: %+v (listed %v), want unread (%s), nothing attributed, its own branch", n, w, ok, collide.ErrNoGitLink)
		}
	}
	if partial != "" {
		if w, ok := at(ws, partial); !ok || !w.UnreadSome || !strings.Contains(w.Unread, "could not open directory 'u/'") || !has(w.Touched, "p.txt") || collide.NotCompared(w) != "some_uncommitted" {
			t.Errorf("worktree with a directory git can't list: %+v (listed %v), want partly unread, its readable edit kept", w, ok)
		}
		if err := os.Chmod(filepath.Join(partial, "u"), 0o755); err != nil {
			t.Fatal(err)
		}
	}
	if shut != "" {
		if w, ok := at(ws, shut); !ok || !strings.Contains(w.Unread, "permission denied") || w.Branch != "shut" {
			t.Errorf("worktree git can't enter: %+v (listed %v), want unread: permission denied", w, ok)
		}
		if err := os.Chmod(shut, 0o755); err != nil {
			t.Fatal(err)
		}
	}
	if w, ok := at(ws, gone); !ok || w.Unread != "" {
		t.Errorf("removed (prunable) worktree: %+v (listed %v), want listed, not unread", w, ok)
	}
	if w, ok := at(ws, locked); !ok || !strings.Contains(w.Unread, "no such file or directory") || w.Branch != "locked" {
		t.Errorf("locked worktree, directory gone: %+v (listed %v), want unread with its reason", w, ok)
	}
	// git never ran there, so its commits weren't read either: wt status's
	// UNREAD line says so.
	sout, serr := captureOutputForTest(t, func() { cmdStatus(nil) })
	line := ""
	for _, ln := range strings.Split(sout+serr, "\n") {
		if strings.Contains(ln, "UNREAD: "+gitx.ErrNoDir.Error()) {
			line = ln
		}
	}
	if !strings.Contains(line, "none of its edits are compared") {
		t.Errorf("wt status, the locked worktree's UNREAD line: %q, want \"none of its edits are compared\":\n%s", line, sout+serr)
	}
	if w, ok := at(ws, spaced); !ok || w.Unread != "" || !has(w.Touched, "notes.txt") {
		t.Errorf("worktree at %q: %+v (listed %v), want read, with its edit", spaced, w, ok)
	}
	// From inside it, it is itself: its own edit is no collision (#254 review).
	t.Chdir(spaced)
	var code int
	out, errOut := captureOutputForTest(t, func() { code = cmdCheck([]string{"notes.txt"}) })
	if code != 0 || strings.Contains(out+errOut, "← spaced") {
		t.Errorf("wt check notes.txt inside %q: exit %d, want 0 and no entry for itself:\n%s", spaced, code, out+errOut)
	}

	// A worktree moved without `git worktree repair`: git lists it at its old
	// path, so from inside it no entry is this work tree, and none may be
	// taken for it (the main checkout stays the main checkout).
	moved := filepath.Join(dirs, "moved")
	gitT(t, root, "worktree", "add", "-q", "-b", "moved", filepath.Join(dirs, "before"), "main")
	if err := os.Rename(filepath.Join(dirs, "before"), moved); err != nil {
		t.Fatal(err)
	}
	ws = scan(t, moved)
	if _, ok := at(ws, root); !ok {
		t.Errorf("from a moved worktree, the main checkout is no longer listed at %s: %+v", root, ws)
	}

	// A bare layout: the bare entry is not unread; a worktree nested in it
	// that lost its .git file is (git status walks up into the bare repo).
	bareRepo := filepath.Join(real(t, t.TempDir()), "r.git")
	gitT(t, root, "clone", "-q", "--bare", root, bareRepo)
	bw, nested := filepath.Join(dirs, "bw"), filepath.Join(bareRepo, "nested")
	gitT(t, bareRepo, "worktree", "add", "-q", "-b", "bw", bw, "main")
	gitT(t, bareRepo, "worktree", "add", "-q", "-b", "nested", nested, "main")
	if err := os.Remove(filepath.Join(nested, ".git")); err != nil {
		t.Fatal(err)
	}
	writeT(t, nested, "notes.txt", "nested edit\n")
	ws = scan(t, bw)
	if w, ok := at(ws, bareRepo); !ok || w.Unread != "" {
		t.Errorf("bare repository entry: %+v (listed %v), want listed, not unread", w, ok)
	}
	if w, ok := at(ws, nested); !ok || w.Unread == "" {
		t.Errorf("worktree nested in the bare repository without its .git: %+v (listed %v), want unread", w, ok)
	}

	// A submodule with a linked worktree: git lists the submodule's checkout by
	// its git directory (.git/modules/sub), and git status succeeds there
	// through core.worktree. From the checkout, that entry is itself.
	subSrc, super := real(t, t.TempDir()), real(t, t.TempDir())
	for _, d := range []string{subSrc, super} {
		gitT(t, d, "init", "-q", "-b", "main")
		gitT(t, d, "config", "user.email", "t@t.test")
		gitT(t, d, "config", "user.name", "t")
	}
	writeT(t, subSrc, "s.txt", "a\n")
	gitT(t, subSrc, "add", "-A")
	gitT(t, subSrc, "commit", "-qm", "sub")
	gitT(t, super, "commit", "-q", "--allow-empty", "-m", "super")
	gitT(t, super, "-c", "protocol.file.allow=always", "submodule", "add", "-q", subSrc, "sub")
	gitT(t, super, "commit", "-qm", "add sub")
	sub, swb := filepath.Join(super, "sub"), filepath.Join(real(t, t.TempDir()), "swb")
	gitT(t, sub, "worktree", "add", "-q", "-b", "swb", swb)
	modules := filepath.Join(super, ".git", "modules", "sub")
	listed := exec.Command("git", "worktree", "list", "--porcelain")
	listed.Dir = sub
	if lo, err := listed.Output(); err != nil || !strings.Contains(string(lo), "worktree "+modules+"\n") {
		t.Fatalf("precondition: git lists the submodule's checkout by %s: %v\n%s", modules, err, lo)
	}
	writeT(t, sub, "s.txt", "sub edit\n")
	ws = scan(t, sub)
	if _, ok := at(ws, modules); ok {
		t.Errorf("from the submodule's checkout, a window is still listed by its git directory: %+v", ws)
	}
	if w, ok := at(ws, sub); !ok || w.Unread != "" || !has(w.Touched, "s.txt") {
		t.Errorf("from the submodule's checkout, its own window: %+v (listed %v), want read, with its edit", w, ok)
	}
	out, errOut = captureOutputForTest(t, func() { code = cmdCheck([]string{"s.txt"}) })
	if code != 0 {
		t.Errorf("wt check s.txt in the submodule's checkout: exit %d, want 0 (its own edit):\n%s", code, out+errOut)
	}
	// The submodule's git dir has a work tree (core.worktree): a real failure
	// there keeps git's reason, from its linked worktree too (#254 review).
	writeT(t, modules, "index", "not an index\n")
	ws = scan(t, swb)
	if w, ok := at(ws, modules); !ok || !strings.Contains(w.Unread, "index") || w.Unread == gitx.ErrGitDirOnly.Error() {
		t.Errorf("from swb, the submodule checkout with a corrupt index: %+v (listed %v), want unread with git's reason", w, ok)
	}

	sepBase := real(t, t.TempDir())
	sep, work, wb := filepath.Join(sepBase, "sep.git"), filepath.Join(sepBase, "main"), filepath.Join(sepBase, "wb")
	gitT(t, sepBase, "init", "-q", "-b", "main", "--separate-git-dir", sep, work)
	gitT(t, work, "config", "user.email", "t@t.test")
	gitT(t, work, "config", "user.name", "t")
	writeT(t, work, "notes.txt", "a\n")
	gitT(t, work, "add", "-A")
	gitT(t, work, "commit", "-qm", "base")
	gitT(t, work, "worktree", "add", "-q", "-b", "wb", wb, "main")
	writeT(t, work, "notes.txt", "main edit\n")
	fromWB := scan(t, wb)
	if w, ok := at(fromWB, sep); !ok || w.Unread != gitx.ErrGitDirOnly.Error() {
		t.Errorf("from wb, the git directory's window: %+v (listed %v), want unread: %v", w, ok, gitx.ErrGitDirOnly)
	}
	fromMain := scan(t, work)
	if _, ok := at(fromMain, sep); ok {
		t.Errorf("from the main work tree, a window is still listed by its git directory: %+v", fromMain)
	}
	if w, ok := at(fromMain, work); !ok || w.Unread != "" || !has(w.Touched, "notes.txt") {
		t.Errorf("from the main work tree, its own window: %+v (listed %v), want read by its work tree, with its edit", w, ok)
	}
}

// #255, round 6: a nested worktree without its .git is never read as the work
// tree around it, even when that one is itself partly unread, and never takes
// that window's branch or claim.
func TestScan_nestedUnderUnreadClaimedMain(t *testing.T) {
	if os.Geteuid() == 0 {
		t.Skip("root reads a mode-000 directory")
	}
	offlineGH(t)
	t.Setenv("HOME", t.TempDir())
	root, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	gitT(t, root, "init", "-q", "-b", "main")
	gitT(t, root, "config", "user.email", "t@t.test")
	gitT(t, root, "config", "user.name", "t")
	writeT(t, root, ".gitignore", ".wt/\n")
	writeT(t, root, "shared.txt", "a\n")
	gitT(t, root, "add", "-A")
	gitT(t, root, "commit", "-qm", "base")
	gitT(t, root, "checkout", "-q", "-b", "feat-7")
	writeT(t, root, filepath.Join(".git", "wt-active-work.md"), "## #7 seven\n- Branch: `feat-7`\n")
	writeT(t, root, "shared.txt", "main edit\n")
	locked := filepath.Join(root, "locked") // the main checkout is partly unread
	if err := os.Mkdir(locked, 0o755); err != nil {
		t.Fatal(err)
	}
	writeT(t, locked, "f.txt", "f\n")
	if err := os.Chmod(locked, 0); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.Chmod(locked, 0o755) })
	x := filepath.Join(root, ".wt", "x")
	gitT(t, root, "worktree", "add", "-q", "-b", "n-x", x, "feat-7")
	if err := os.Remove(filepath.Join(x, ".git")); err != nil {
		t.Fatal(err)
	}
	// A LOCKED nested worktree on an unmounted volume: an empty directory,
	// and git does not call a locked worktree prunable (round 7).
	ln := filepath.Join(root, ".wt", "ln")
	gitT(t, root, "worktree", "add", "-q", "-b", "l-nested", ln, "feat-7")
	gitT(t, root, "worktree", "lock", ln)
	writeT(t, root, filepath.Join(".git", "wt-active-work.md"), "## #7 seven\n- Branch: `feat-7`\n\n## #8 eight\n- Branch: `l-nested`\n")
	if err := os.Rename(ln, filepath.Join(filepath.Dir(root), filepath.Base(root)+"-ln-unmounted")); err != nil {
		t.Fatal(err)
	}
	if err := os.Mkdir(ln, 0o755); err != nil {
		t.Fatal(err)
	}
	t.Chdir(root)
	c, err := config.Load()
	if err != nil {
		t.Fatal(err)
	}
	ws, err := collide.Scan(c)
	if err != nil {
		t.Fatal(err)
	}
	var mainW, nested, lockedW collide.Window
	feat7 := 0
	for _, w := range ws {
		switch w.Worktree {
		case root:
			mainW = w
		case x:
			nested = w
		case ln:
			lockedW = w
		}
		if w.Branch == "feat-7" {
			feat7++
		}
	}
	if !mainW.UnreadSome || mainW.Issue != "7" {
		t.Errorf("main checkout: %+v, want partly unread (locked/) and claimed #7", mainW)
	}
	if nested.Unread != collide.ErrNoGitLink || len(nested.Touched) != 0 || nested.Branch != "n-x" || nested.Issue != "" {
		t.Errorf("nested worktree without its .git: %+v, want unread (%s), nothing attributed, its own branch, no claim", nested, collide.ErrNoGitLink)
	}
	if lockedW.Unread != collide.ErrNoGitLink || len(lockedW.Touched) != 0 || lockedW.Branch != "l-nested" || lockedW.Issue != "8" {
		t.Errorf("locked nested worktree on an unmounted volume: %+v, want unread (%s), nothing attributed, its own branch and claim #8", lockedW, collide.ErrNoGitLink)
	}
	if feat7 != 1 {
		t.Errorf("%d windows on feat-7, want 1 (the nested one took the main checkout's branch): %+v", feat7, ws)
	}
}
