package cli

import (
	"encoding/json"
	"fmt"
	"os"
	"runtime"
	"strings"
	"testing"
	"time"

	"github.com/eharriett0/wt/internal/config"
	"github.com/eharriett0/wt/internal/coord"
	"github.com/eharriett0/wt/internal/lock"
)

// #197 item 2 review: a prompting session writes a presence mark an hour, so
// some expire every day, and doctor warned "prunable" daily in every repo with
// agent sessions. It lists them instead, and warns only about the rest.
func TestDoctor_listsExpiredPresenceWithoutWarning(t *testing.T) {
	repo, home := t.TempDir(), t.TempDir()
	gitInitForTest(t, repo)
	t.Setenv("HOME", home)
	for _, k := range []string{"WT_SESSION", "CLAUDE_CODE_SESSION_ID", "CODEX_SESSION_ID", "WT_WINDOW"} {
		t.Setenv(k, "")
	}
	t.Setenv("WT_SESSION", "session-B")
	t.Chdir(repo)
	c, err := config.Load()
	if err != nil {
		t.Fatal(err)
	}
	logPath, self := coordCtx(c)
	at := func(d time.Duration) string { return time.Now().Add(-d).UTC().Format(time.RFC3339) }
	add := func(r coord.Record) {
		t.Helper()
		r.Window = self.Window
		if err := coord.Append(logPath, r); err != nil {
			t.Fatal(err)
		}
	}
	doctor := func() (text string, prunable, expired int) {
		t.Helper()
		out, errOut := captureT(t, func() { cmdDoctor(nil) })
		js, _ := captureT(t, func() { cmdDoctor([]string{"--json"}) })
		var d struct {
			Coord struct {
				Prunable        int `json:"prunable"`
				PresenceExpired int `json:"presence_expired"`
			} `json:"coord"`
		}
		if err := json.Unmarshal([]byte(js), &d); err != nil {
			t.Fatalf("doctor --json: %v\n%s", err, js)
		}
		return out + errOut, d.Coord.Prunable, d.Coord.PresenceExpired
	}

	add(coord.Record{ID: "p", Kind: coord.KindPresence, Session: "session-A", TS: at(25 * time.Hour)})
	text, prunable, expired := doctor()
	if prunable != 0 || expired != 1 || strings.Contains(text, "prunable") || !strings.Contains(text, "1 presence mark(s) older than a day") {
		t.Errorf("only an expired presence mark: prunable=%d presence_expired=%d, want 0 and 1, listed without a warning:\n%s", prunable, expired, text)
	}
	// Control: an all-cleared announcement is still warned about.
	add(coord.Record{ID: "a", Kind: coord.KindAnnounce, Session: "session-B", Message: "done", TS: at(time.Hour)})
	add(coord.Record{ID: "c", Kind: coord.KindAllClear, AckOf: "a", Session: "session-B", TS: at(time.Hour)})
	text, prunable, expired = doctor()
	if prunable != 2 || expired != 1 || !strings.Contains(text, "2 resolved/expired record(s) prunable") {
		t.Errorf("with an all-cleared announcement: prunable=%d presence_expired=%d, want 2 and 1, warned:\n%s", prunable, expired, text)
	}
}

// #197 item 2 review: the presence mark gives far more repositories a log,
// and the per-turn hook read it with Load, which waits 5s for a writer, so a
// prune stopped mid-rewrite stalled every prompt there. The hook reads with
// coord.LoadForHook (a second at most), and its mark never waits: a turn
// under a held log returns promptly and writes nothing.
func TestHookPresence_neverStallsOnAHeldLog(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("the advisory lock is a no-op off unix")
	}
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
	turn := func(session string) time.Duration {
		t.Helper()
		t.Setenv("CLAUDE_CODE_SESSION_ID", session)
		payload := fmt.Sprintf(`{"cwd":%q,"hook_event_name":"UserPromptSubmit"}`, repo)
		start := time.Now()
		captureStdoutForTest(t, func() { withStdinForTest(t, payload, func() { runHook([]string{"claude-context"}) }) })
		return time.Since(start)
	}
	turn("session-A") // makes the log
	holder, err := os.OpenFile(logPath, os.O_RDWR, 0)
	if err != nil {
		t.Fatal(err)
	}
	defer holder.Close()
	if err := lock.Exclusive(holder); err != nil {
		t.Fatal(err)
	}
	took := turn("session-C")
	lock.Release(holder)
	if took > 3*time.Second {
		t.Errorf("a turn under a held log took %s, want at most about a second", took)
	}
	recs, err := coord.Load(logPath)
	if err != nil {
		t.Fatal(err)
	}
	for _, r := range recs {
		if r.Session == "session-C" {
			t.Errorf("a turn under a held log wrote %+v", r)
		}
	}
	if turn("session-C"); !strings.Contains(func() string {
		recs, _ := coord.Load(logPath)
		b, _ := json.Marshal(recs)
		return string(b)
	}(), `"session-C"`) {
		t.Errorf("control: once the log is free, C's turn writes no mark")
	}
}

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
