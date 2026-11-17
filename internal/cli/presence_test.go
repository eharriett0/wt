package cli

import (
	"encoding/json"
	"fmt"
	"os"
	"strings"
	"testing"

	"github.com/eharriett0/wt/internal/config"
	"github.com/eharriett0/wt/internal/coord"
)

// #197 item 2, step two: a session that never runs a coordination command is
// seen through the per-turn hook's presence mark. Here a single-worktree repo
// with no coordination log (the #163 case: two agents in one checkout): the
// hook used to return at once. Session A's turn writes one presence record
// (creating the log), a second turn within the hour writes none, a token-less
// turn writes nothing; then session B's status --json lists A, and B's inbox
// shows nothing (presence is no announcement).
func TestHookPresence_marksASilentSession(t *testing.T) {
	repo, home := t.TempDir(), t.TempDir()
	gitInitForTest(t, repo)
	t.Setenv("HOME", home)
	for _, k := range []string{"WT_SESSION", "CLAUDE_CODE_SESSION_ID", "CODEX_SESSION_ID", "WT_WINDOW",
		"WT_SKIP_COLLISION", "HOOK_DISABLE_MULTIWINDOW_CHECK"} {
		t.Setenv(k, "")
	}
	t.Chdir(repo)
	c, err := config.Load()
	if err != nil {
		t.Fatal(err)
	}
	logPath, _ := coordCtx(c)
	turn := func(hook, envSession, payloadSession string) {
		t.Helper()
		t.Setenv("CLAUDE_CODE_SESSION_ID", envSession)
		payload := fmt.Sprintf(`{"cwd":%q,"session_id":%q,"hook_event_name":"UserPromptSubmit"}`, repo, payloadSession)
		captureStdoutForTest(t, func() { withStdinForTest(t, payload, func() { runHook([]string{hook}) }) })
	}
	presence := func() []coord.Record {
		t.Helper()
		recs, err := coord.Load(logPath)
		if err != nil {
			t.Fatal(err)
		}
		var out []coord.Record
		for _, r := range recs {
			if r.Kind == coord.KindPresence {
				out = append(out, r)
			}
		}
		return out
	}

	turn("claude-context", "", "") // token-less: nothing to mark, no log made
	if _, err := os.Stat(logPath); err == nil {
		t.Fatalf("a token-less turn created the coordination log")
	}
	turn("claude-context", "session-A", "")
	if p := presence(); len(p) != 1 || p[0].Session != "session-A" || p[0].Repo != mainRepoName(c) {
		t.Fatalf("after A's turn: presence %+v, want one record of session-A", p)
	}
	turn("claude-context", "session-A", "")
	if p := presence(); len(p) != 1 {
		t.Errorf("a second turn within the hour wrote another mark: %d records", len(p))
	}
	turn("codex-context", "", "codex-X") // Codex: the payload's session
	if p := presence(); len(p) != 2 || p[1].Session != "codex-X" {
		t.Errorf("Codex turn: presence %+v, want codex-X's mark too", p)
	}

	t.Setenv("CLAUDE_CODE_SESSION_ID", "")
	t.Setenv("WT_SESSION", "session-B")
	out, _ := captureT(t, func() { cmdStatus([]string{"--json"}) })
	var st StatusPayload
	if err := json.Unmarshal([]byte(out), &st); err != nil {
		t.Fatalf("status --json: %v\n%s", err, out)
	}
	var others []string
	for _, s := range st.OtherSessions {
		others = append(others, s.Session)
	}
	if strings.Join(others, ",") != "codex-X,session-A" && strings.Join(others, ",") != "session-A,codex-X" {
		t.Errorf("B's other_sessions = %v, want session-A and codex-X", others)
	}
	inbox, inboxErr := captureT(t, func() { cmdInbox(nil) })
	if strings.Contains(inbox+inboxErr, "presence") || !strings.Contains(inbox+inboxErr, "inbox clear") {
		t.Errorf("B's inbox shows presence:\n%s%s", inbox, inboxErr)
	}
}
