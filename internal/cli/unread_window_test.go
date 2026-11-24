package cli

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/eharriett0/wt/internal/config"
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
// wb's branch intact, where wb's edit of line 5 is HIGH and the hooks deny.
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
	gitT(t, root, "add", "-A")
	gitT(t, root, "commit", "-qm", "base")
	dirs := t.TempDir()
	wa, wb, wc := filepath.Join(dirs, "wa"), filepath.Join(dirs, "wb"), filepath.Join(dirs, "wc")
	gitT(t, root, "worktree", "add", "-q", "-b", "wa", wa, "main")
	gitT(t, root, "worktree", "add", "-q", "-b", "wb", wb, "main")
	gitT(t, root, "worktree", "add", "-q", "-b", "wc", wc, "main")
	writeT(t, wa, "notes.txt", strings.Replace(notes.String(), "line 5\n", "A line 5\n", 1))
	writeT(t, wb, "notes.txt", strings.Replace(notes.String(), "line 5\n", "B line 5\n", 1))
	// wc edits the same file far from line 5: an entry the hooks re-grade and
	// drop, so the note also rides on a run that had entries.
	writeT(t, wc, "notes.txt", strings.Replace(notes.String(), "line 9\n", "C line 9\n", 1))
	claude, _ := json.Marshal(map[string]any{"cwd": wa, "tool_name": "Edit", "tool_input": map[string]string{
		"file_path": filepath.Join(wa, "notes.txt"), "old_string": "A line 5\n", "new_string": "AA line 5\n"}})
	codex, _ := json.Marshal(map[string]any{"cwd": wa, "tool_name": "apply_patch", "tool_input": map[string]string{
		"command": "*** Begin Patch\n*** Update File: notes.txt\n@@\n-A line 5\n+AA line 5\n*** End Patch\n"}})
	turn := fmt.Sprintf(`{"cwd":%q,"hook_event_name":"UserPromptSubmit"}`, wa)

	type seen struct {
		status, statusJSON, statusBlocking, check, checkBlocking, checkJSON, mcp string
		claude, codex, turn, prePush, preCommit                                  string
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
		c, err := config.Load()
		if err != nil {
			t.Fatal(err)
		}
		s.mcp, _ = mcpCheck(c, []string{"notes.txt"}, false, false)
		s.claude = runHookCapture(t, hookClaudeEdit, string(claude))
		s.codex = runHookCapture(t, hookCodexEdit, string(codex))
		s.turn = captureStdoutForTest(t, func() { withStdinForTest(t, turn, func() { runHook([]string{"claude-context"}) }) })
		_, s.prePush = captureOutputForTest(t, func() { s.prePushBlocked = prePushRefs(t, wa, "wa", "wa-again") })
		t.Chdir(wa)
		_, s.preCommit = captureOutputForTest(t, func() { hooks.HookPreCommit(c) })
		return s
	}

	const named = "window wb could not be read"
	healthy := observe(t)
	for name, out := range map[string]string{"status": healthy.status, "status --json": healthy.statusJSON, "status --blocking": healthy.statusBlocking,
		"check": healthy.check, "check --blocking": healthy.checkBlocking, "check --json": healthy.checkJSON, "MCP wt_check": healthy.mcp, "claude-edit": healthy.claude, "codex-edit": healthy.codex,
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
		if next := lines[i+1]; !strings.Contains(next, "UNREAD") || !strings.Contains(next, "bad object HEAD") {
			t.Errorf("wt status, the line under wb's worktree: %q, want UNREAD with git's reason", next)
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
	var wbUnread string
	for _, w := range st.Windows {
		if w.Label == "wb" {
			wbUnread = w.Unread
		}
	}
	if !strings.Contains(wbUnread, "bad object HEAD") {
		t.Errorf("status --json: wb's unread = %q, want git's reason\n%s", wbUnread, broken.statusJSON)
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
		if len(p.UnreadWindows) != 1 || p.UnreadWindows[0].Window != "wb" || !strings.Contains(p.UnreadWindows[0].Error, "bad object HEAD") || p.Blocking {
			t.Errorf("%s: unread_windows = %+v, blocking %v; want wb with git's reason, not blocking", name, p.UnreadWindows, p.Blocking)
		}
	}

	// Both edit hooks (block mode on): the note, never a deny.
	for name, out := range map[string]string{"claude-edit": broken.claude, "codex-edit": broken.codex} {
		if !strings.Contains(out, named) || strings.Contains(out, `"permissionDecision"`) {
			t.Errorf("%s: want the note as context, no decision:\n%s", name, out)
		}
	}
	var ctx preToolUseOutput
	_ = json.Unmarshal([]byte(broken.turn), &ctx)
	if !strings.Contains(ctx.HookSpecificOutput.AdditionalContext, named) {
		t.Errorf("per-turn context doesn't name wb:\n%s", broken.turn)
	}

	// Pre-push: once per push, never blocking; pre-commit: named.
	if n := strings.Count(broken.prePush, named); n != 1 || broken.prePushBlocked {
		t.Errorf("pre-push of two refs: named %d time(s), blocked %v; want once, not blocked:\n%s", n, broken.prePushBlocked, broken.prePush)
	}
	if !strings.Contains(broken.preCommit, named) {
		t.Errorf("pre-commit doesn't name wb:\n%s", broken.preCommit)
	}
}
