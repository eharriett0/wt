package cli

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/eharriett0/wt/internal/config"
	"github.com/eharriett0/wt/internal/coord"
)

// sessionTestRepo makes a one-commit repo with an isolated HOME and no session
// variables set, and chdirs into it.
func sessionTestRepo(t *testing.T) string {
	t.Helper()
	repo, home := t.TempDir(), t.TempDir()
	gitInitForTest(t, repo)
	t.Setenv("HOME", home)
	for _, k := range []string{"WT_SESSION", "CLAUDE_CODE_SESSION_ID", "CODEX_SESSION_ID", "WT_WINDOW", "WT_COORD_ISSUE"} {
		t.Setenv(k, "")
	}
	t.Chdir(repo)
	return repo
}

// TestCmdBlockID_ownedBySession is #197 end to end: two sessions in ONE
// checkout. Block reservations were keyed by window, so session B listed A's
// reservation as its own and could --abandon it, freeing the id A was about to
// write. Now only A can mark it; B is told whose it is, and sees it as an
// imminent prepend, not as its own hold.
func TestCmdBlockID_ownedBySession(t *testing.T) {
	repo := sessionTestRepo(t)
	logFile := filepath.Join(repo, "log.md")
	if err := os.WriteFile(logFile, []byte("## NEWEST-3\nolder\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	t.Setenv("WT_SESSION", "session-A")
	var code int
	out, errOut := captureT(t, func() { code = cmdBlockID([]string{"log.md"}) })
	if code != 0 || strings.TrimSpace(out) != "4" {
		t.Fatalf("A reserves: code %d, stdout %q (want 4)\nstderr: %s", code, out, errOut)
	}

	t.Setenv("WT_SESSION", "session-B")
	out, errOut = captureT(t, func() { code = cmdBlockID([]string{"log.md", "--abandon", "4"}) })
	if code != 1 || !strings.Contains(errOut, "another session in this window (session ") || !strings.Contains(errOut, "not by you") {
		t.Errorf("B abandons A's reservation: code %d (want 1)\nstdout: %s\nstderr: %s", code, out, errOut)
	}
	if err := os.WriteFile(logFile, []byte("## NEWEST-4\nnew\n## NEWEST-3\nolder\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	out, errOut = captureT(t, func() { code = cmdBlockID([]string{"log.md", "--written", "4"}) })
	if code != 1 || !strings.Contains(errOut, "not by you") {
		t.Errorf("B marks A's reservation written: code %d (want 1)\nstdout: %s\nstderr: %s", code, out, errOut)
	}
	out, errOut = captureT(t, func() { code = cmdHolds(nil) })
	if strings.Contains(out+errOut, "your block-id reservations") || !strings.Contains(out+errOut, "no block reservations of yours") {
		t.Errorf("B's holds list A's reservation as B's:\nstdout: %s\nstderr: %s", out, errOut)
	}
	c, err := config.Load()
	if err != nil {
		t.Fatal(err)
	}
	_, errOut = captureT(t, func() { blockReservationBanner(c) })
	if !strings.Contains(errOut, "reserved block 4") || !strings.Contains(errOut, "this window") {
		t.Errorf("B's status banner doesn't show A's imminent prepend:\n%s", errOut)
	}

	t.Setenv("WT_SESSION", "session-A")
	out, errOut = captureT(t, func() { code = cmdHolds(nil) })
	if !strings.Contains(out+errOut, "your block-id reservations") {
		t.Errorf("A's holds don't list its own reservation:\nstdout: %s\nstderr: %s", out, errOut)
	}
	out, errOut = captureT(t, func() { code = cmdBlockID([]string{"log.md", "--written", "4"}) })
	if code != 0 {
		t.Errorf("A marks its own reservation written: code %d (want 0)\nstdout: %s\nstderr: %s", code, out, errOut)
	}
}

// TestStatusJSON_otherSessions is #197's item 3: `wt status --json` and MCP's
// wt_status carry the shared-checkout signal `wt doctor --json` already did
// (session + other_sessions), read the same way as the status banner.
func TestStatusJSON_otherSessions(t *testing.T) {
	sessionTestRepo(t)
	c, err := config.Load()
	if err != nil {
		t.Fatal(err)
	}
	logPath, self := coordCtx(c)
	r := coord.Record{ID: "b1", Kind: coord.KindAnnounce, Window: self.Window, Session: "session-B",
		Message: "rolling the thing", TS: time.Now().Add(-time.Minute).UTC().Format(time.RFC3339)}
	if err := coord.Append(logPath, r); err != nil {
		t.Fatal(err)
	}
	read := func(name, text string) StatusPayload {
		t.Helper()
		var p StatusPayload
		if err := json.Unmarshal([]byte(text), &p); err != nil {
			t.Fatalf("%s: not a status payload: %v\n%s", name, err, text)
		}
		return p
	}
	for _, tc := range []struct {
		session string
		others  []string
	}{
		{"session-A", []string{"session-B"}},
		{"session-B", nil}, // its own record is not another session's
	} {
		t.Setenv("WT_SESSION", tc.session)
		out, _ := captureT(t, func() { cmdStatus([]string{"--json"}) })
		text, isErr := mcpStatus(c, false)
		if isErr {
			t.Fatalf("mcpStatus failed: %s", text)
		}
		for name, p := range map[string]StatusPayload{"status --json": read("status --json", out), "wt_status": read("wt_status", text)} {
			var got []string
			for _, s := range p.OtherSessions {
				got = append(got, s.Session)
			}
			if p.Session != tc.session || strings.Join(got, ",") != strings.Join(tc.others, ",") {
				t.Errorf("%s as %s: session %q, other_sessions %v; want %q, %v", name, tc.session, p.Session, got, tc.session, tc.others)
			}
		}
	}
}

// #197 review: a reservation is the session's from any checkout (a subagent in
// its own worktree shares its parent's id), and the token-less directory
// fallback, which wt can't prove, needs --not-mine to abandon, as all-clear
// does. A reservation no session can be proven for says no one can mark it.
func TestCmdBlockID_reviewCases(t *testing.T) {
	repo := sessionTestRepo(t)
	logFile := filepath.Join(repo, "log.md")
	if err := os.WriteFile(logFile, []byte("## NEWEST-3\nolder\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	run := func(args ...string) (int, string) {
		var code int
		out, errOut := captureT(t, func() { code = cmdBlockID(args) })
		return code, out + errOut
	}

	// Own session, another checkout (WT_WINDOW stands in for the subagent's worktree).
	t.Setenv("WT_SESSION", "parent")
	t.Setenv("WT_WINDOW", "/wt/subagent")
	if code, out := run("log.md"); code != 0 || !strings.HasPrefix(strings.TrimSpace(out), "4") {
		t.Fatalf("reserve from the subagent's checkout: %d %s", code, out)
	}
	t.Setenv("WT_WINDOW", "")
	if code, out := run("log.md", "--abandon", "4"); code != 0 {
		t.Errorf("the same session, from its own checkout, abandoning its reservation: code %d\n%s", code, out)
	}

	// Token-less fallback: two token-less shells in one checkout are one party
	// for listing, but abandoning frees the id, so it needs --not-mine.
	t.Setenv("WT_SESSION", "")
	if code, out := run("log.md"); code != 0 || !strings.HasPrefix(strings.TrimSpace(out), "4") {
		t.Fatalf("token-less reserve: %d %s", code, out)
	}
	code, out := run("log.md", "--abandon", "4")
	if code != 1 || !strings.Contains(out, "--not-mine") || !strings.Contains(out, "can't prove it") {
		t.Errorf("token-less abandon without --not-mine: code %d (want 1)\n%s", code, out)
	}
	if code, out := run("log.md", "--abandon", "4", "--not-mine"); code != 0 {
		t.Errorf("token-less abandon with --not-mine: code %d (want 0)\n%s", code, out)
	}

	// A reservation with no session at all (an older wt): nobody can mark it.
	c, err := config.Load()
	if err != nil {
		t.Fatal(err)
	}
	_, self := coordCtx(c)
	home, _ := os.UserHomeDir()
	ledger := coord.BlockLedgerPath(home, canonicalFilePath("log.md"))
	old := coord.Record{ID: "legacy9", Kind: coord.KindBlockReserve, Window: self.Window, File: canonicalFilePath("log.md"), Block: 9,
		TS: time.Now().Add(-time.Minute).UTC().Format(time.RFC3339)}
	if err := coord.Append(ledger, old); err != nil {
		t.Fatal(err)
	}
	t.Setenv("WT_SESSION", "someone")
	code, out = run("log.md", "--abandon", "9")
	if code != 1 || !strings.Contains(out, "a wt without session support, so no session can mark it") {
		t.Errorf("legacy reservation: code %d\n%s", code, out)
	}
}

// #197 review: an unreadable coordination log is reported as such in the
// status payload, never as "no other session".
func TestStatusJSON_unreadableLog(t *testing.T) {
	if os.Geteuid() == 0 {
		t.Skip("root reads a mode-0 file")
	}
	sessionTestRepo(t)
	c, err := config.Load()
	if err != nil {
		t.Fatal(err)
	}
	logPath, self := coordCtx(c)
	if err := coord.Append(logPath, coord.Record{ID: "x", Kind: coord.KindAnnounce, Window: self.Window, Session: "B",
		TS: time.Now().UTC().Format(time.RFC3339)}); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(logPath, 0); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.Chmod(logPath, 0o644) })
	t.Setenv("WT_SESSION", "A")
	text, _ := mcpStatus(c, false)
	var p StatusPayload
	if err := json.Unmarshal([]byte(text), &p); err != nil {
		t.Fatal(err)
	}
	if p.SessionsUnread == "" || len(p.OtherSessions) != 0 {
		t.Errorf("unreadable log: sessions_unread %q, other_sessions %v; want the read error and no sessions", p.SessionsUnread, p.OtherSessions)
	}
}

// #197 re-review: a retried --written succeeds ("already marked"), --written
// after your own abandon is refused for that reason, --not-mine goes only
// with --abandon, the /clear hint only fits two tagged sessions, and an
// expired own reservation is flagged in wt holds.
func TestCmdBlockID_reReviewCases(t *testing.T) {
	repo := sessionTestRepo(t)
	logFile := filepath.Join(repo, "log.md")
	write := func(text string) {
		t.Helper()
		if err := os.WriteFile(logFile, []byte(text), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	run := func(args ...string) (int, string) {
		var code int
		out, errOut := captureT(t, func() { code = cmdBlockID(args) })
		return code, out + errOut
	}
	write("## NEWEST-3\n")
	t.Setenv("WT_SESSION", "A")
	if code, _ := run("log.md"); code != 0 {
		t.Fatal("reserve")
	}
	write("## NEWEST-4\n## NEWEST-3\n")
	if code, out := run("log.md", "--written", "4"); code != 0 {
		t.Fatalf("first --written: %d\n%s", code, out)
	}
	if code, out := run("log.md", "--written", "4"); code != 0 || !strings.Contains(out, "already marked written") || strings.Contains(out, "to reserve your own") {
		t.Errorf("a retried --written: code %d (want 0, already marked)\n%s", code, out)
	}
	if code, _ := run("log.md", "--not-mine"); code != 64 {
		t.Errorf("--not-mine without --abandon: code %d, want 64", code)
	}
	if code, _ := run("log.md", "--written", "4", "--not-mine"); code != 64 {
		t.Errorf("--not-mine with --written: code %d, want 64", code)
	}

	// A reserves 5 and abandons it; --written 5 is then refused for that reason.
	if code, _ := run("log.md"); code != 0 {
		t.Fatal("reserve 5")
	}
	if code, _ := run("log.md", "--abandon", "5"); code != 0 {
		t.Fatal("abandon 5")
	}
	write("## NEWEST-5\n## NEWEST-4\n## NEWEST-3\n")
	if code, out := run("log.md", "--written", "5"); code != 1 || !strings.Contains(out, "you abandoned your reservation") {
		t.Errorf("--written after your own abandon: code %d\n%s", code, out)
	}

	// The /clear hint: only between two tagged sessions.
	if code, _ := run("log.md"); code != 0 { // A reserves 6
		t.Fatal("reserve 6")
	}
	t.Setenv("WT_SESSION", "B")
	if _, out := run("log.md", "--abandon", "6"); !strings.Contains(out, "/clear") {
		t.Errorf("tagged vs tagged: no /clear hint\n%s", out)
	}
	t.Setenv("WT_SESSION", "")
	if _, out := run("log.md", "--abandon", "6"); strings.Contains(out, "/clear") || !strings.Contains(out, "WT_SESSION") {
		t.Errorf("a token-less shell vs a tagged reservation: want the WT_SESSION hint, not /clear\n%s", out)
	}
	if code, _ := run("log.md"); code != 0 { // a token-less reservation of 7
		t.Fatal("reserve 7")
	}
	t.Setenv("WT_SESSION", "B")
	if _, out := run("log.md", "--abandon", "7"); strings.Contains(out, "/clear") || !strings.Contains(out, "a shell or agent with no session token") {
		t.Errorf("a tagged session vs a token-less reservation: wrong wording\n%s", out)
	}

	// An own reservation past the TTL is flagged as expired in wt holds.
	c, err := config.Load()
	if err != nil {
		t.Fatal(err)
	}
	_, self := coordCtx(c)
	home, _ := os.UserHomeDir()
	ledger := coord.BlockLedgerPath(home, canonicalFilePath("log.md"))
	if err := coord.Append(ledger, coord.Record{ID: "old9", Kind: coord.KindBlockReserve, Window: self.Window, Session: "B",
		File: canonicalFilePath("log.md"), Block: 9, TS: time.Now().Add(-45 * time.Minute).UTC().Format(time.RFC3339)}); err != nil {
		t.Fatal(err)
	}
	out, errOut := captureT(t, func() { cmdHolds(nil) })
	if !strings.Contains(out+errOut, "expired: its id may have been handed out again") {
		t.Errorf("an expired own reservation isn't flagged in wt holds:\n%s%s", out, errOut)
	}
}

// #197 item 2, step one: a session whose only records are block-id
// reservations (they live in the per-file ledgers, not the coordination log)
// was invisible to the shared-checkout signal. Now `wt status --json`,
// wt_status, `wt doctor --json` and the status banner all see it, before any
// coordination log exists; another window's reservation is not this
// checkout's.
func TestOtherSessions_countBlockReservations(t *testing.T) {
	repo := sessionTestRepo(t)
	if err := os.WriteFile(filepath.Join(repo, "log.md"), []byte("## NEWEST-1\nolder\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	t.Setenv("WT_SESSION", "session-A")
	var code int
	if out, errOut := captureT(t, func() { code = cmdBlockID([]string{"log.md"}) }); code != 0 {
		t.Fatalf("A reserves: code %d\n%s%s", code, out, errOut)
	}
	c, err := config.Load()
	if err != nil {
		t.Fatal(err)
	}
	if logPath, _ := coordCtx(c); fileExists(logPath) {
		t.Fatalf("precondition: a reservation wrote the coordination log %s", logPath)
	}
	// Another window's reservation by a third session must not count here.
	home, _ := os.UserHomeDir()
	other := coord.Record{ID: "x1", Kind: coord.KindBlockReserve, Window: filepath.Join(t.TempDir(), "elsewhere"),
		Session: "session-C", File: filepath.Join(repo, "log.md"), Block: 7, TS: time.Now().UTC().Format(time.RFC3339)}
	if err := coord.Append(coord.BlockLedgerPath(home, filepath.Join(repo, "other.md")), other); err != nil {
		t.Fatal(err)
	}

	t.Setenv("WT_SESSION", "session-B")
	out, _ := captureT(t, func() { cmdStatus([]string{"--json"}) })
	var p StatusPayload
	if err := json.Unmarshal([]byte(out), &p); err != nil {
		t.Fatalf("status --json: %v\n%s", err, out)
	}
	if len(p.OtherSessions) != 1 || p.OtherSessions[0].Session != "session-A" {
		t.Errorf("status --json other_sessions = %+v, want session-A only", p.OtherSessions)
	}
	text, _ := mcpStatus(c, false)
	if !strings.Contains(text, `"session": "session-A"`) || strings.Contains(text, "session-C") {
		t.Errorf("wt_status: want session-A as another session, not session-C:\n%s", text)
	}
	out, _ = captureT(t, func() { cmdDoctor([]string{"--json"}) })
	var d struct {
		Coord struct {
			OtherSessions []coord.SessionActivity `json:"other_sessions"`
		} `json:"coord"`
	}
	if err := json.Unmarshal([]byte(out), &d); err != nil {
		t.Fatalf("doctor --json: %v\n%s", err, out)
	}
	if len(d.Coord.OtherSessions) != 1 || d.Coord.OtherSessions[0].Session != "session-A" {
		t.Errorf("doctor --json other_sessions = %+v, want session-A only", d.Coord.OtherSessions)
	}
	_, errOut := captureT(t, func() { sharedCheckoutBanner(c) })
	if !strings.Contains(errOut, "checkout shared") || !strings.Contains(errOut, "session-A") {
		t.Errorf("status banner doesn't warn of session-A:\n%s", errOut)
	}
	// Once the log exists (B posts), doctor reads both.
	logPath, self := coordCtx(c)
	if err := coord.Append(logPath, coord.Record{ID: "b1", Kind: coord.KindAnnounce, Window: self.Window, Session: self.Session,
		Message: "B's note", TS: time.Now().UTC().Format(time.RFC3339)}); err != nil {
		t.Fatal(err)
	}
	out, _ = captureT(t, func() { cmdDoctor([]string{"--json"}) })
	d.Coord.OtherSessions = nil
	if err := json.Unmarshal([]byte(out), &d); err != nil {
		t.Fatalf("doctor --json: %v\n%s", err, out)
	}
	if len(d.Coord.OtherSessions) != 1 || d.Coord.OtherSessions[0].Session != "session-A" {
		t.Errorf("doctor --json with a log = %+v, want session-A only", d.Coord.OtherSessions)
	}
	t.Setenv("WT_SESSION", "session-A")
	if _, errOut := captureT(t, func() { sharedCheckoutBanner(c) }); strings.Contains(errOut, "checkout shared") {
		t.Errorf("A's own reservation reads as another session's:\n%s", errOut)
	}
}

func fileExists(p string) bool {
	_, err := os.Stat(p)
	return err == nil
}
