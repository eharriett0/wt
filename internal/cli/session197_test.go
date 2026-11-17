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
	if code != 1 || !strings.Contains(errOut, "another session in this checkout (session ") || !strings.Contains(errOut, "not by you") {
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
	if !strings.Contains(errOut, "reserved block 4") || !strings.Contains(errOut, "this checkout") {
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
