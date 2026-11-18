package cli

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"

	"github.com/eharriett0/wt/internal/collide"
	"github.com/eharriett0/wt/internal/coord"
	"github.com/eharriett0/wt/internal/peers"
)

// #250 (reshaped): a live session is credited to the window of its latest
// record in this repository's log from the last day, not to the listing's
// cwd (where it was started). Self, a session the listing doesn't show, a
// token-less record and a stale one are never credited.
func TestSessionsByWindow(t *testing.T) {
	now := time.Now()
	at := func(d time.Duration) string { return now.Add(-d).UTC().Format(time.RFC3339) }
	live := map[string]peers.Peer{
		"moved":           {SessionID: "moved", Name: "mover", Cwd: "/parent", PID: 1},
		"one":             {SessionID: "one", Name: "one", Cwd: "/parent", PID: 2},
		"two":             {SessionID: "two", Name: "two", Cwd: "/r/b", PID: 3},
		"self":            {SessionID: "self", Name: "me", PID: 4},
		"stale":           {SessionID: "stale", Name: "old", PID: 5},
		"started-in-main": {SessionID: "started-in-main", Name: "path-worker", PID: 6},
		"went-back":       {SessionID: "went-back", Name: "returner", PID: 7},
		"by-path":         {SessionID: "by-path", Name: "reacher", PID: 8},
	}
	recs := []coord.Record{
		{Kind: coord.KindPresence, Window: "/r/a", Session: "moved", TS: at(3 * time.Hour)},
		{Kind: coord.KindPresence, Window: "/r/b", Session: "moved", TS: at(10 * time.Minute)},
		{Kind: coord.KindAnnounce, Window: "/r/b", Session: "one", TS: at(time.Hour)},
		{Kind: coord.KindPresence, Window: "/r/b", Session: "two", TS: at(2 * time.Hour)},
		{Kind: coord.KindPresence, Window: "/r/b", Session: "self", TS: at(time.Minute)},
		{Kind: coord.KindPresence, Window: "/r/a", Session: "stale", TS: at(25 * time.Hour)},
		{Kind: coord.KindPresence, Window: "/r/a", Session: "gone", TS: at(time.Minute)},
		{Kind: coord.KindPresence, Window: "/r/a", Session: coord.SessionNone, TS: at(time.Minute)},
		{Kind: coord.KindPresence, Window: "/r/a", Session: "one", TS: "undated"},
		// #251 round 2: started in /r/main (its turns mark presence there),
		// working in /r/b by path (its wt commands run there).
		{Kind: coord.KindAnnounce, Window: "/r/b", Session: "started-in-main", TS: at(2 * time.Hour)},
		{Kind: coord.KindPresence, Window: "/r/main", Session: "started-in-main", TS: at(5 * time.Minute)},
		// #251 round 3: its presence followed it into /r/a, where it
		// announced, then into /r/b: it works in /r/b now.
		{Kind: coord.KindPresence, Window: "/r/a", Session: "went-back", TS: at(4 * time.Hour)},
		{Kind: coord.KindAnnounce, Window: "/r/a", Session: "went-back", TS: at(3 * time.Hour)},
		{Kind: coord.KindPresence, Window: "/r/b", Session: "went-back", TS: at(20 * time.Minute)},
		// Left /r/b for /r/main, then ran a wt command in /r/b by path: the
		// command is newer than its last mark, so it still works in /r/b.
		{Kind: coord.KindPresence, Window: "/r/b", Session: "by-path", TS: at(5 * time.Hour)},
		{Kind: coord.KindPresence, Window: "/r/main", Session: "by-path", TS: at(4 * time.Hour)},
		{Kind: coord.KindAnnounce, Window: "/r/b", Session: "by-path", TS: at(time.Hour)},
	}
	by := sessionsByWindow(recs, live, map[string]bool{"self": true, "my-shell": true}, now)
	var b []string
	for _, p := range by["/r/b"] {
		b = append(b, p.Name)
	}
	if strings.Join(b, ",") != "reacher,mover,one,path-worker,two,returner" || len(by["/r/a"]) != 0 || len(by["/r/main"]) != 0 || len(by) != 1 {
		t.Errorf("sessionsByWindow = %v, want, by session id, reacher (its command is newer), mover (moved to /r/b), one, path-worker (works there, marks /r/main), two and returner (its marks followed it to /r/b) in /r/b", by)
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
	writeT(t, root, "fyi.txt", b.String())
	gitT(t, root, "add", "-A")
	gitT(t, root, "commit", "-qm", "base")
	wb, wd := filepath.Join(base, "wb"), filepath.Join(base, "wd")
	for _, w := range []string{"wb", "wd"} {
		gitT(t, root, "branch", w)
		gitT(t, root, "worktree", "add", "-q", filepath.Join(base, w), w)
	}
	writeT(t, wb, "notes.txt", strings.Replace(b.String(), "line 5\n", "EDITED line 5\n", 1))
	writeT(t, wb, "fyi.txt", strings.Replace(b.String(), "line 5\n", "EDITED line 5\n", 1))
	gitT(t, wb, "commit", "-qam", "wb edits line 5")
	writeT(t, wd, "notes.txt", strings.Replace(b.String(), "line 6\n", "EDITED line 6\n", 1))
	writeT(t, wd, "fyi.txt", strings.Replace(b.String(), "line 30\n", "EDITED line 30\n", 1)) // disjoint: FYI

	bin := t.TempDir()
	runs := filepath.Join(bin, "runs")
	// Every session was started in a parent folder (the listing's cwd is
	// where a session started); wt's records say where each works now: s-b
	// in wb, s-self here, s-out (live) in no window of this repository.
	listing, _ := json.Marshal([]map[string]any{
		{"sessionId": "s-b", "name": "api-worker", "status": "idle", "pid": 11, "cwd": base},
		{"sessionId": "s-out", "name": "elsewhere", "status": "busy", "pid": 12, "cwd": wb},
		{"sessionId": "s-self", "name": "me", "status": "busy", "pid": 13, "cwd": base},
		{"sessionId": "s-c", "name": "ledger-worker", "status": "idle", "pid": 14, "cwd": base},
	})
	now := time.Now().UTC().Format(time.RFC3339)
	for _, r := range []coord.Record{
		{ID: "p1", Kind: coord.KindPresence, Window: realPath(wb), Session: "s-b", Repo: "repo", TS: now},
		{ID: "p2", Kind: coord.KindPresence, Window: realPath(wd), Session: "s-self", Repo: "repo", TS: now},
	} {
		if err := coord.Append(filepath.Join(os.Getenv("HOME"), ".wt", "coordination", filepath.Base(root)+".jsonl"), r); err != nil {
			t.Fatal(err)
		}
	}
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
	// #250 review: this session's own latest record sits in wb too, and its wt
	// token is WT_SESSION, not the Claude id the listing keys it by: still
	// never named.
	if err := coord.Append(filepath.Join(os.Getenv("HOME"), ".wt", "coordination", filepath.Base(root)+".jsonl"),
		coord.Record{ID: "p3", Kind: coord.KindPresence, Window: realPath(wb), Session: "s-self", Repo: "repo", TS: time.Now().UTC().Format(time.RFC3339)}); err != nil {
		t.Fatal(err)
	}
	t.Setenv("WT_SESSION", "my-shell")
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
	// #251 round 2: the --blocking and the FYI (low) lines carry it too.
	for _, args := range [][]string{{"--blocking", "notes.txt"}, {"fyi.txt"}} {
		peers.Reset()
		out, errOut := captureOutputForTest(t, func() { cmdCheck(args) })
		if !strings.Contains(out+errOut, named) {
			t.Errorf("wt check %v doesn't name wb's live session:\n%s%s", args, out, errOut)
		}
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

	// A session whose only record is a block-id reservation (in its file's
	// ledger, not the log) is credited to that window too.
	if strings.Contains(out+errOut, "ledger-worker") {
		t.Fatalf("a session with no record was named:\n%s%s", out, errOut)
	}
	if err := coord.Append(coord.BlockLedgerPath(os.Getenv("HOME"), filepath.Join(wb, "notes.txt")), coord.Record{
		ID: "b1", Kind: coord.KindBlockReserve, Window: realPath(wb), Session: "s-c", Repo: filepath.Base(root),
		TS: time.Now().UTC().Format(time.RFC3339)}); err != nil {
		t.Fatal(err)
	}
	peers.Reset()
	out, errOut = captureOutputForTest(t, func() { cmdCheck([]string{"notes.txt"}) })
	if !strings.Contains(out+errOut, "`ledger-worker`, idle") {
		t.Errorf("a session whose record is a block reservation in wb wasn't named:\n%s%s", out, errOut)
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
