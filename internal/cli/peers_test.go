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

	"github.com/eharriett0/wt/internal/config"
	"github.com/eharriett0/wt/internal/coord"
	"github.com/eharriett0/wt/internal/peers"
)

var apiWorker = peers.Peer{SessionID: "B", Name: "api-worker", Status: "idle", PID: 7}

// #250: a hold whose session is live on this machine is never called
// orphaned, so wt never suggests releasing it for every window; it says to
// ask that session. A listing that doesn't show it changes nothing.
func TestHoldAdvice_liveHolderIsAskedNotCleared(t *testing.T) {
	now := time.Now()
	self := coord.Self{Window: "/w", Session: "A"}
	at := func(d time.Duration) string { return now.Add(-d).UTC().Format(time.RFC3339) }
	silent := coord.Record{ID: "h1", Kind: coord.KindAnnounce, Window: "/w", Session: "B", Hold: []string{"merge-main"}, TS: at(5 * time.Hour)}
	recs := []coord.Record{silent}
	v := coord.ViewOf(recs, self)
	live := func(s string) (peers.Peer, bool) {
		if s == "B" {
			return apiWorker, true
		}
		return peers.Peer{}, false
	}

	all := strings.Join(holdAdvice(recs, recs, v, now, live), "\n")
	if !strings.Contains(all, "h1: its session is live as `api-worker`, idle: ask it with SendMessage to release it") {
		t.Errorf("a live holder's hold: advice doesn't say to ask it:\n%s", all)
	}
	if strings.Contains(all, "wt all-clear h1") || strings.Contains(all, "orphaned") {
		t.Errorf("a live holder's hold is offered an all-clear:\n%s", all)
	}
	// Control: not listed (or no listing), the 5h-silent hold looks orphaned.
	for _, lookup := range []peerLookup{nil, func(string) (peers.Peer, bool) { return peers.Peer{}, false }} {
		all := strings.Join(holdAdvice(recs, recs, v, now, lookup), "\n")
		if !strings.Contains(all, "h1 looks orphaned") || !strings.Contains(all, "wt all-clear h1") {
			t.Errorf("control: a silent hold of a session not listed live:\n%s", all)
		}
	}
	// #250 review: a live session with no name can't be addressed: it is
	// called not orphaned, nothing more; and an undated hold says why it
	// gates first.
	unnamed := func(s string) (peers.Peer, bool) { return peers.Peer{Status: "busy"}, s == "B" }
	all = strings.Join(holdAdvice(recs, recs, v, now, unnamed), "\n")
	if !strings.Contains(all, "h1: its session is still running on this machine, so it is not orphaned") || strings.Contains(all, "wt all-clear h1") {
		t.Errorf("an unnamed live holder:\n%s", all)
	}
	undated := coord.Record{ID: "h3", Kind: coord.KindAnnounce, Window: "/w", Session: "B", Hold: []string{"merge-main"}, TS: "?"}
	all = strings.Join(holdAdvice([]coord.Record{undated}, []coord.Record{undated}, coord.ViewOf([]coord.Record{undated}, self), now, live), "\n")
	if !strings.Contains(all, "h3 ") || strings.Contains(all, "h3: its session is live") {
		t.Errorf("an undated hold of a live holder skipped its note:\n%s", all)
	}
	// This session's own token from another checkout is never "another session".
	own := coord.Record{ID: "h2", Kind: coord.KindAnnounce, Window: "/elsewhere", Session: "A", Hold: []string{"merge-main"}, TS: at(time.Minute)}
	mine := func(string) (peers.Peer, bool) { return peers.Peer{Name: "me", Status: "busy"}, true }
	all = strings.Join(holdAdvice([]coord.Record{own}, []coord.Record{own}, coord.ViewOf([]coord.Record{own}, self), now, mine), "\n")
	if strings.Contains(all, "SendMessage") || !strings.Contains(all, "carries this session's own token") {
		t.Errorf("this session's own hold from another checkout:\n%s", all)
	}
}

// postedByLive names another party's live session, and only theirs.
func TestPostedByLive(t *testing.T) {
	orig := livePeers
	t.Cleanup(func() { livePeers = orig })
	listing, listed := map[string]peers.Peer{"B": apiWorker, "A": {SessionID: "A", Name: "me", PID: 1}, "C": {SessionID: "C", Name: "other-window", Status: "busy", PID: 2},
		coord.SessionNone: {SessionID: coord.SessionNone, Name: "never", PID: 3}}, true
	livePeers = func() (map[string]peers.Peer, bool) { return listing, listed }
	self := coord.Self{Window: "/w", Session: "A"}
	recs := []coord.Record{
		{ID: "b", Window: "/w", Session: "B"},
		{ID: "a", Window: "/w", Session: "A"},
		{ID: "c", Window: "/other", Session: "C"},
		{ID: "d", Window: "/other", Session: "D"},
		{ID: "n", Window: "/w", Session: coord.SessionNone},
	}
	v := coord.ViewOf(recs, self)
	for _, tc := range []struct{ id, want string }{
		{"b", "same checkout, another session (session B…) (live as `api-worker`, idle: ask it with SendMessage)"},
		{"a", "this window (yours)"},
		{"c", "/other (live as `other-window`, busy: ask it with SendMessage)"},
		{"d", "/other"},
	} {
		for _, r := range recs {
			if r.ID == tc.id {
				if got := postedByLive(r, v); got != tc.want && !strings.HasPrefix(tc.want, "same checkout") {
					t.Errorf("%s: %q, want %q", tc.id, got, tc.want)
				} else if strings.HasPrefix(tc.want, "same checkout") && !strings.HasSuffix(got, "(live as `api-worker`, idle: ask it with SendMessage)") {
					t.Errorf("%s: %q, want another session named live", tc.id, got)
				}
			}
		}
	}
	if got := postedByLive(recs[4], v); strings.Contains(got, "live") {
		t.Errorf("a token-less record was looked up: %q", got)
	}
	// This session's own token, placed from another checkout, is never
	// "another party", live as it is.
	own := coord.Record{ID: "e", Window: "/elsewhere", Session: "A"}
	if got := postedByLive(own, coord.ViewOf(append(recs, own), self)); got != "yours (this session's token), placed from checkout /elsewhere" {
		t.Errorf("this session's own record from another checkout: %q", got)
	}
	// #250 review: under WT_SESSION, a record this session wrote with its
	// Claude id reads as another session's; the listing keys this session by
	// that id, and it is never named.
	t.Setenv("CLAUDE_CODE_SESSION_ID", "A-claude")
	listing["A-claude"] = peers.Peer{SessionID: "A-claude", Name: "me-too", Status: "busy", PID: 9}
	mineByClaudeID := coord.Record{ID: "m", Window: "/w", Session: "A-claude"}
	if got := postedByLive(mineByClaudeID, coord.ViewOf(append(recs, mineByClaudeID), self)); strings.Contains(got, "me-too") {
		t.Errorf("this session, by its Claude id, was named: %q", got)
	}
	// A listing wt couldn't read names nobody.
	listed = false
	if got := postedByLive(recs[0], v); strings.Contains(got, "live") {
		t.Errorf("an unread listing named a session: %q", got)
	}
}

// End to end through runHook and merge-pr's gate, with a fake `claude` on
// PATH answering `agents --json`: the per-turn context and the gate's refusal
// name the live holder, the listing is read once a command, and a turn with
// nothing of another party's to show never runs claude.
func TestLiveHolderNamed_hookAndGate(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("the fake claude is a shell script")
	}
	repo, home, bin := t.TempDir(), t.TempDir(), t.TempDir()
	gitInitForTest(t, repo)
	t.Setenv("HOME", home)
	for _, k := range []string{"WT_SESSION", "CLAUDE_CODE_SESSION_ID", "CODEX_SESSION_ID", "WT_WINDOW",
		"WT_SKIP_COLLISION", "HOOK_DISABLE_MULTIWINDOW_CHECK", "CLAUDE_CODE_EXECPATH", "WT_PEERS"} {
		t.Setenv(k, "")
	}
	runs := filepath.Join(bin, "runs")
	listing := `[{"sessionId":"claude-B","name":"api-worker","status":"idle","pid":4242,"kind":"interactive","cwd":"/r"}]`
	script := "#!/bin/sh\necho \"$*\" >> '" + runs + "'\necho '" + listing + "'\n"
	if err := os.WriteFile(filepath.Join(bin, "claude"), []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", bin+string(os.PathListSeparator)+os.Getenv("PATH"))
	// A fresh script's first exec can take seconds on a loaded macOS (it is
	// scanned): a listing expected to answer gets room, not the 2s.
	origTimeout := peers.Timeout
	peers.Timeout = 30 * time.Second
	t.Cleanup(func() { peers.Timeout = origTimeout })
	ran := func() int {
		b, _ := os.ReadFile(runs)
		return strings.Count(string(b), "\n")
	}
	t.Chdir(repo)
	c, err := config.Load()
	if err != nil {
		t.Fatal(err)
	}
	logPath, self := coordCtx(c)
	turn := func(session string) string {
		t.Helper()
		peers.Reset()
		t.Setenv("CLAUDE_CODE_SESSION_ID", session)
		payload := fmt.Sprintf(`{"cwd":%q,"hook_event_name":"UserPromptSubmit"}`, repo)
		out := captureStdoutForTest(t, func() { withStdinForTest(t, payload, func() { runHook([]string{"claude-context"}) }) })
		var got struct {
			HookSpecificOutput struct {
				AdditionalContext string `json:"additionalContext"`
			} `json:"hookSpecificOutput"`
		}
		_ = json.Unmarshal([]byte(out), &got)
		return got.HookSpecificOutput.AdditionalContext
	}
	at := time.Now().Add(-time.Minute).UTC().Format(time.RFC3339)
	add := func(r coord.Record) {
		t.Helper()
		r.TS, r.Kind, r.Window = at, coord.KindAnnounce, self.Window
		if err := coord.Append(logPath, r); err != nil {
			t.Fatal(err)
		}
	}

	add(coord.Record{ID: "ownhold", Session: "claude-A", Hold: []string{"merge-main"}})
	turn("claude-A")
	if n := ran(); n != 0 {
		t.Errorf("a turn with only its own records ran claude %d time(s), want none", n)
	}

	add(coord.Record{ID: "bhold", Session: "claude-B", Hold: []string{"merge-main"}, Message: "migrating"})
	add(coord.Record{ID: "bnote", Session: "claude-B", Message: "fyi"})
	ctx := turn("claude-A")
	if !strings.Contains(ctx, "live as `api-worker`, idle: ask it with SendMessage") {
		t.Errorf("per-turn context doesn't name the live holder:\n%s", ctx)
	}
	if n := ran(); n != 1 {
		t.Errorf("a turn showing two of B's records ran claude %d time(s), want once", n)
	}

	peers.Reset()
	t.Setenv("CLAUDE_CODE_SESSION_ID", "claude-A")
	var code int
	out, errOut := captureOutputForTest(t, func() { code = mergeCoordGate(c) })
	errOut = out + errOut
	if code == 0 || !strings.Contains(listingLine(errOut, "bhold"), "live as `api-worker`, idle") ||
		!strings.Contains(errOut, "bhold: its session is live as `api-worker`, idle: ask it with SendMessage to release it") {
		t.Errorf("merge gate (code %d) doesn't name the live holder in its listing and advice:\n%s", code, errOut)
	}
	// status --json, MCP wt_status and doctor --json carry its name and status
	// in other_sessions.
	peers.Reset()
	stOut, _ := captureT(t, func() { cmdStatus([]string{"--json"}) })
	var st StatusPayload
	if err := json.Unmarshal([]byte(stOut), &st); err != nil {
		t.Fatalf("status --json: %v\n%s", err, stOut)
	}
	if len(st.OtherSessions) != 1 || st.OtherSessions[0].Name != "api-worker" || st.OtherSessions[0].Status != "idle" {
		t.Errorf("status --json other_sessions = %+v, want claude-B named api-worker, idle", st.OtherSessions)
	}
	peers.Reset()
	if text, _ := mcpStatus(c, false); !strings.Contains(text, `"name": "api-worker"`) {
		t.Errorf("wt_status doesn't name the live session:\n%s", text)
	}
	peers.Reset()
	dOut, _ := captureT(t, func() { cmdDoctor([]string{"--json"}) })
	var d struct {
		Coord struct {
			OtherSessions []coord.SessionActivity `json:"other_sessions"`
		} `json:"coord"`
	}
	if err := json.Unmarshal([]byte(dOut), &d); err != nil {
		t.Fatalf("doctor --json: %v\n%s", err, dOut)
	}
	if len(d.Coord.OtherSessions) != 1 || d.Coord.OtherSessions[0].Name != "api-worker" {
		t.Errorf("doctor --json other_sessions = %+v, want claude-B named api-worker", d.Coord.OtherSessions)
	}
	// `wt inbox` and the status banner list it the same way.
	for name, show := range map[string]func(){
		"inbox":  func() { cmdInbox(nil) },
		"banner": func() { peerHoldBanner(c) },
	} {
		peers.Reset()
		out, errOut := captureOutputForTest(t, show)
		if !strings.Contains(listingLine(out+errOut, "bhold"), "live as `api-worker`, idle") {
			t.Errorf("%s doesn't name the live holder:\n%s%s", name, out, errOut)
		}
	}
	// So does the status banner's checkout-shared warning (#251 round 4).
	peers.Reset()
	if _, errOut := captureT(t, func() { sharedCheckoutBanner(c) }); !strings.Contains(errOut, "latest: live `api-worker`") {
		t.Errorf("the checkout-shared banner doesn't name the live session:\n%s", errOut)
	}
}

// listingLine is the line of out that lists record id (`<id>  <who> …`),
// not an advice line about it.
func listingLine(out, id string) string {
	for _, l := range strings.Split(out, "\n") {
		if strings.HasPrefix(strings.TrimSpace(l), id+"  ") {
			return l
		}
	}
	return ""
}

// #250: the stale-hold warning offers `wt all-clear` per hold; for a holder
// that is live it says to ask instead.
func TestWarnStaleHolds_liveHolderIsAsked(t *testing.T) {
	orig := livePeers
	t.Cleanup(func() { livePeers = orig })
	now := time.Now()
	self := coord.Self{Window: "/w", Session: "A"}
	h := coord.Record{ID: "h1", Kind: coord.KindAnnounce, Window: "/w", Session: "B", Hold: []string{"merge-main"},
		TS: now.Add(-30 * time.Hour).UTC().Format(time.RFC3339)}
	v := coord.ViewOf([]coord.Record{h}, self)
	for _, tc := range []struct {
		live      bool
		want, not string
	}{
		{true, "(its session is live: ask it before any all-clear)", "wt all-clear h1"},
		{false, "(all-clear: wt all-clear h1)", "its session is live"},
	} {
		listed := map[string]peers.Peer{}
		if tc.live {
			listed["B"] = apiWorker
		}
		livePeers = func() (map[string]peers.Peer, bool) { return listed, true }
		out, errOut := captureOutputForTest(t, func() { warnStaleHolds([]coord.Record{h}, v, now, 24*time.Hour) })
		if all := out + errOut; !strings.Contains(all, tc.want) || strings.Contains(all, tc.not) {
			t.Errorf("live=%v: stale-hold warning:\n%s\nwant %q, not %q", tc.live, all, tc.want, tc.not)
		}
	}
}
