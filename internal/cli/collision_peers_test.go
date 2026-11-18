package cli

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"

	"github.com/eharriett0/wt/internal/collide"
	"github.com/eharriett0/wt/internal/peers"
)

// #250 (reshaped): a session belongs to the deepest worktree holding its
// working directory, never to a parent of that worktree, never to a sibling
// that shares a name prefix; this session is never named.
func TestAssignPeers(t *testing.T) {
	wts := []string{"/r", "/r/.wt/feat", "/other"}
	live := map[string]peers.Peer{
		"nested":  {SessionID: "nested", Name: "feat-worker", Cwd: "/r/.wt/feat/sub", PID: 1},
		"main":    {SessionID: "main", Name: "main-worker", Cwd: "/r/src", PID: 2},
		"sibling": {SessionID: "sibling", Name: "rx-worker", Cwd: "/rx", PID: 3},
		"self":    {SessionID: "self", Name: "me", Cwd: "/other", PID: 4},
		"nocwd":   {SessionID: "nocwd", Name: "nowhere", PID: 5},
		"o1":      {SessionID: "o1", Name: "one", Cwd: "/other", PID: 6},
		"o2":      {SessionID: "o2", Name: "two", Cwd: "/other/x", PID: 7},
	}
	by := assignPeers(wts, live, "self")
	names := func(wt string) string {
		var n []string
		for _, p := range by[wt] {
			n = append(n, p.Name)
		}
		return strings.Join(n, ",")
	}
	for wt, want := range map[string]string{"/r": "main-worker", "/r/.wt/feat": "feat-worker", "/other": "one,two"} {
		if got := names(wt); got != want {
			t.Errorf("sessions in %s = %q, want %q", wt, got, want)
		}
	}
	if len(by) != 3 {
		t.Errorf("assigned %v, want only the three worktrees (no /rx, no self, no session without a cwd)", by)
	}
}

func TestCollisionNote(t *testing.T) {
	if got := collisionNote(nil); got != "" {
		t.Errorf("no sessions: %q", got)
	}
	got := collisionNote([]peers.Peer{{Name: "api-worker", Status: "idle"}, {Status: "busy"}})
	if want := " (live: `api-worker`, idle; an unnamed session, busy; coordinate with SendMessage)"; got != want {
		t.Errorf("collisionNote = %q, want %q", got, want)
	}
}

// The per-turn banner names windows by label: only labels it will show are
// looked up, and a label two worktrees share gets both their notes.
func TestLabelSessionNotes(t *testing.T) {
	ws := []collide.Window{
		{Worktree: "/r", Branch: "main"},
		{Worktree: "/w/b", Branch: "feat-b"},
		{Worktree: "/w/c", Branch: "feat-c"},
	}
	asked := map[string]int{}
	note := func(wt string) string {
		asked[wt]++
		if wt == "/w/b" {
			return " (live: `b`)"
		}
		return ""
	}
	ov := []StatusOverlap{{File: "x", Windows: []string{ws[0].Label(), ws[1].Label()}}}
	got := labelSessionNotes(ws, ws[0].Label(), ov, note)
	if got[ws[1].Label()] != " (live: `b`)" || len(got) != 1 {
		t.Errorf("labelSessionNotes = %v, want feat-b's note only", got)
	}
	if asked["/w/c"] != 0 || asked["/r"] != 0 {
		t.Errorf("looked up windows the banner won't show: %v", asked)
	}
	asked = map[string]int{}
	if got := labelSessionNotes(ws, ws[0].Label(), nil, note); got != nil || len(asked) != 0 {
		t.Errorf("no overlaps: %v (asked %v), want nil and no lookup", got, asked)
	}
	msg, _ := codexContextMessage(ov, ws[0].Label(), map[string]string{ws[1].Label(): " (live: `b`)"})
	if !strings.Contains(msg, ws[1].Label()+" (live: `b`)") {
		t.Errorf("per-turn message doesn't carry the note:\n%s", msg)
	}
}

// End to end: window wb committed an edit of line 5, and this window (wd)
// has an uncommitted edit of line 6, which git conflicts with it. A session
// is live in wb, another in a directory outside both, and this one in wd.
// wt check, both edit hooks (advisory and deny) and the per-turn hook name
// wb's session, the listing runs once a command, and a command with no
// collision to show never runs claude.
func TestCollisions_nameTheLiveSessionInTheOtherWindow(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("the fake claude is a shell script")
	}
	hermeticGitT(t)
	offlineGH(t)
	t.Setenv("HOME", t.TempDir())
	for _, k := range []string{"WT_SKIP_COLLISION", "HOOK_DISABLE_MULTIWINDOW_CHECK", "WT_SESSION", "CODEX_SESSION_ID", "WT_WINDOW", "CLAUDE_CODE_EXECPATH", "WT_PEERS"} {
		t.Setenv(k, "")
	}
	t.Setenv("WT_CLAUDE_HOOK_BLOCK", "0")
	t.Setenv("WT_CODEX_HOOK_BLOCK", "0")
	t.Setenv("CLAUDE_CODE_SESSION_ID", "s-self")

	root, base := t.TempDir(), t.TempDir()
	gitT(t, root, "init", "-q", "-b", "main")
	gitT(t, root, "config", "user.email", "t@t.test")
	gitT(t, root, "config", "user.name", "t")
	var b strings.Builder
	for n := 1; n <= 40; n++ {
		fmt.Fprintf(&b, "line %d\n", n)
	}
	writeT(t, root, "notes.txt", b.String())
	writeT(t, root, "quiet.txt", "nobody edits this\n")
	gitT(t, root, "add", "-A")
	gitT(t, root, "commit", "-qm", "base")
	wb, wd := filepath.Join(base, "wb"), filepath.Join(base, "wd")
	for _, w := range []string{"wb", "wd"} {
		gitT(t, root, "branch", w)
		gitT(t, root, "worktree", "add", "-q", filepath.Join(base, w), w)
	}
	writeT(t, wb, "notes.txt", strings.Replace(b.String(), "line 5\n", "EDITED line 5\n", 1))
	gitT(t, wb, "commit", "-qam", "wb edits line 5")
	writeT(t, wd, "notes.txt", strings.Replace(b.String(), "line 6\n", "EDITED line 6\n", 1))

	bin := t.TempDir()
	runs := filepath.Join(bin, "runs")
	listing, _ := json.Marshal([]map[string]any{
		{"sessionId": "s-b", "name": "api-worker", "status": "idle", "pid": 11, "cwd": filepath.Join(wb, "sub")},
		{"sessionId": "s-out", "name": "elsewhere", "status": "busy", "pid": 12, "cwd": t.TempDir()},
		{"sessionId": "s-self", "name": "me", "status": "busy", "pid": 13, "cwd": wd},
	})
	script := "#!/bin/sh\necho \"$*\" >> '" + runs + "'\ncat <<'EOF'\n" + string(listing) + "\nEOF\n"
	if err := os.WriteFile(filepath.Join(bin, "claude"), []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", bin+string(os.PathListSeparator)+os.Getenv("PATH"))
	ran := func() int {
		b, _ := os.ReadFile(runs)
		return strings.Count(string(b), "\n")
	}
	const named = "(live: `api-worker`, idle; coordinate with SendMessage)"
	t.Chdir(wd)

	// wt check (text).
	peers.Reset()
	before := ran()
	out, errOut := captureOutputForTest(t, func() { cmdCheck([]string{"notes.txt"}) })
	if !strings.Contains(out+errOut, "wb [") || !strings.Contains(out+errOut, named) {
		t.Errorf("wt check doesn't name wb's live session:\n%s%s", out, errOut)
	}
	if strings.Contains(out+errOut, "`me`") || strings.Contains(out+errOut, "elsewhere") {
		t.Errorf("wt check names a session that isn't wb's:\n%s%s", out, errOut)
	}
	if n := ran() - before; n != 1 {
		t.Errorf("wt check ran claude %d times, want once", n)
	}
	peers.Reset()
	before = ran()
	captureOutputForTest(t, func() { cmdCheck([]string{"quiet.txt"}) })
	if n := ran() - before; n != 0 {
		t.Errorf("wt check of a file no window contests ran claude %d time(s)", n)
	}

	// Both edit hooks, advisory and deny.
	claude, _ := json.Marshal(map[string]any{"cwd": wd, "tool_name": "Edit", "tool_input": map[string]string{
		"file_path": filepath.Join(wd, "notes.txt"), "old_string": "line 5\n", "new_string": "X line 5\n"}})
	codex, _ := json.Marshal(map[string]any{"cwd": wd, "tool_name": "apply_patch", "tool_input": map[string]string{
		"command": "*** Begin Patch\n*** Update File: notes.txt\n@@\n-line 5\n+X line 5\n*** End Patch\n"}})
	for _, tc := range []struct {
		name, blockEnv string
		payload        []byte
		claude         bool
	}{{"claude-edit", "WT_CLAUDE_HOOK_BLOCK", claude, true}, {"codex-edit", "WT_CODEX_HOOK_BLOCK", codex, false}} {
		for _, block := range []string{"0", "1"} {
			t.Setenv(tc.blockEnv, block)
			peers.Reset()
			hook := hookCodexEdit
			if tc.claude {
				hook = hookClaudeEdit
			}
			got := runHookCapture(t, hook, string(tc.payload))
			if !strings.Contains(got, named) {
				t.Errorf("%s (block=%s) doesn't name wb's live session:\n%s", tc.name, block, got)
			}
			if block == "1" && !strings.Contains(got, `"permissionDecision":"deny"`) {
				t.Errorf("%s with block=1 didn't deny:\n%s", tc.name, got)
			}
		}
		t.Setenv(tc.blockEnv, "0")
	}

	// The per-turn hook.
	peers.Reset()
	before = ran()
	payload := fmt.Sprintf(`{"cwd":%q,"hook_event_name":"UserPromptSubmit"}`, wd)
	turn := captureStdoutForTest(t, func() { withStdinForTest(t, payload, func() { runHook([]string{"claude-context"}) }) })
	var ctx struct {
		HookSpecificOutput struct {
			AdditionalContext string `json:"additionalContext"`
		} `json:"hookSpecificOutput"`
	}
	_ = json.Unmarshal([]byte(turn), &ctx)
	if !strings.Contains(ctx.HookSpecificOutput.AdditionalContext, named) {
		t.Errorf("per-turn context doesn't name wb's live session:\n%s", turn)
	}
	if n := ran() - before; n != 1 {
		t.Errorf("a turn ran claude %d times, want once", n)
	}

	// WT_PEERS=0: nothing named, claude never run.
	t.Setenv("WT_PEERS", "0")
	peers.Reset()
	before = ran()
	out, errOut = captureOutputForTest(t, func() { cmdCheck([]string{"notes.txt"}) })
	if strings.Contains(out+errOut, "live:") || ran() != before {
		t.Errorf("WT_PEERS=0: named a session or ran claude:\n%s%s", out, errOut)
	}
}
