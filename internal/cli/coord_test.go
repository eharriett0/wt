package cli

import (
	"encoding/json"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/eharriett0/wt/internal/config"
	"github.com/eharriett0/wt/internal/coord"
)

// #163: an entry from another session in THIS checkout carries the reader's own
// window id, so rendering the raw id reads as "you". It must be named as what it is.
func TestPostedBy(t *testing.T) {
	self := coord.Self{Window: "/work/repo", Session: "A"}
	cases := []struct {
		rec  coord.Record
		want string
	}{
		{coord.Record{Window: "/work/repo", Session: "24288609-c37a-43a9-8a82-80e93779324e"}, "same checkout, another session (session 24288609…)"},
		{coord.Record{Window: "/work/repo", Session: coord.SessionNone}, "same checkout, another session (no session token)"},
		// Your own record — or a pre-#163 one, which is yours by the wildcard — must
		// never be labelled as another session's (review finding: cmdAck did).
		{coord.Record{Window: "/work/repo", Session: "A"}, "this window (yours)"},
		{coord.Record{Window: "/work/repo"}, "this window (yours)"},
		{coord.Record{Window: "/work/repo-worktrees/roll", Session: "R"}, "/work/repo-worktrees/roll"},
	}
	for _, tc := range cases {
		if got := postedBy(tc.rec, self); got != tc.want {
			t.Errorf("postedBy(%+v) = %q, want %q", tc.rec, got, tc.want)
		}
	}
}

func TestHoldersPhrase(t *testing.T) {
	self := coord.Self{Window: "/w", Session: "A"}
	same := coord.Record{Window: "/w", Session: "B"}
	other := coord.Record{Window: "/other", Session: "C"}
	cases := []struct {
		holds []coord.Record
		want  string
	}{
		{[]coord.Record{other}, "another window"},
		{[]coord.Record{same}, "another session in this checkout"},
		{[]coord.Record{other, same}, "another window AND another session in this checkout"},
	}
	for _, tc := range cases {
		if got := holdersPhrase(tc.holds, self); got != tc.want {
			t.Errorf("holdersPhrase(%v) = %q, want %q", tc.holds, got, tc.want)
		}
	}
	// The gate's "may be your own from before a /clear" note fires only for a
	// same-checkout holder.
	if !sameCheckoutHolder([]coord.Record{other, same}, self) || sameCheckoutHolder([]coord.Record{other}, self) {
		t.Error("sameCheckoutHolder must be true iff a holder shares this checkout")
	}
}

// What the merge gate and `wt holds` tell a reader about holds that gate it
// (#163). `wt ack` (waives the hold for you ONLY) always leads. `wt all-clear`
// releases a hold for EVERY window, so it is offered only for a hold that looks
// orphaned, and always says "EVERY window": the gate used to hand any
// same-checkout reader — a plain terminal next to a live session — a global
// release. A same-checkout holder adds the /clear explanation.
func TestHoldAdvice(t *testing.T) {
	now := time.Date(2026, 10, 8, 12, 0, 0, 0, time.UTC)
	at := func(d time.Duration) string { return now.Add(-d).Format(time.RFC3339) }
	self := coord.Self{Window: "/w", Session: "A"}
	liveSame := coord.Record{ID: "live", Window: "/w", Session: "B", TS: at(5 * time.Minute), Hold: []string{"merge-main"}}
	liveOther := coord.Record{ID: "other", Window: "/elsewhere", Session: "C", TS: at(5 * time.Minute), Hold: []string{"merge-main"}}
	silent := coord.Record{ID: "silent", Window: "/w", Session: "D", TS: at(5 * time.Hour), Hold: []string{"merge-main"}}
	old := coord.Record{ID: "old", Window: "/elsewhere", Session: "E", TS: at(13 * time.Hour), Hold: []string{"merge-main"}}
	cases := []struct {
		name      string
		holds     []coord.Record
		wantClear []string // ids offered `wt all-clear`
		wantClrNt bool     // the /clear note
	}{
		{"a live same-checkout hold: ack + /clear note, NO all-clear", []coord.Record{liveSame}, nil, true},
		{"a live hold from another window: ack only", []coord.Record{liveOther}, nil, false},
		{"its session silent for hours: all-clear offered", []coord.Record{silent}, []string{"silent"}, true},
		{"placed 13h ago: all-clear offered", []coord.Record{old}, []string{"old"}, false},
		{"mixed: only the orphaned one is offered", []coord.Record{liveSame, old}, []string{"old"}, true},
	}
	for _, tc := range cases {
		lines := holdAdvice(tc.holds, tc.holds, self, now)
		if len(lines) == 0 || !strings.HasPrefix(lines[0], "ack it first: wt ack <id>") || !strings.Contains(lines[0], "for YOU only") {
			t.Errorf("%s: advice must lead with `wt ack` (waives for you only), got %q", tc.name, lines)
			continue
		}
		all := strings.Join(lines, "\n")
		var cleared []string
		for _, l := range lines {
			if strings.Contains(l, "wt all-clear") {
				if !strings.Contains(l, "releases it for EVERY window") {
					t.Errorf("%s: an all-clear suggestion must say it releases the hold for EVERY window: %q", tc.name, l)
				}
				cleared = append(cleared, strings.Fields(l)[0])
			}
		}
		if strings.Join(cleared, ",") != strings.Join(tc.wantClear, ",") {
			t.Errorf("%s: all-clear offered for %v, want %v:\n%s", tc.name, cleared, tc.wantClear, all)
		}
		if got := strings.Contains(all, "/clear"); got != tc.wantClrNt {
			t.Errorf("%s: /clear note = %v, want %v:\n%s", tc.name, got, tc.wantClrNt, all)
		}
	}
}

// Every record wt writes carries the writer's window AND session (#163). A
// record without the session is a pre-#163 wildcard that every session in the
// checkout owns — dropping the stamp silently re-merges sessions.
func TestStampRecord(t *testing.T) {
	self := coord.Self{Window: "/w", Session: "A"}
	at := time.Date(2026, 10, 8, 12, 0, 0, 0, time.UTC)
	r := stampRecord(self, "repo", coord.KindAnnounce, at)
	if r.Window != "/w" || r.Session != "A" || r.Repo != "repo" || r.Kind != coord.KindAnnounce ||
		r.ID != coord.NewID(at) || r.TS != "2026-10-08T12:00:00Z" {
		t.Errorf("stampRecord = %+v", r)
	}
	if tl := stampRecord(coord.Self{Window: "/w", Session: coord.SessionNone}, "repo", coord.KindAck, at); tl.Session != coord.SessionNone {
		t.Errorf("a token-less writer must stamp SessionNone, never \"\": %+v", tl)
	}
}

// `wt ack --all`'s acks are this session's (#163), one per note, with distinct
// increasing ids so MergeByID can never collapse two of them.
func TestBulkAckRecords(t *testing.T) {
	self := coord.Self{Window: "/w", Session: "A"}
	base := time.Date(2026, 10, 8, 12, 0, 0, 0, time.UTC)
	notes := []coord.Record{{ID: "n1"}, {ID: "n2"}, {ID: "n3"}}
	got := bulkAckRecords(notes, self, "repo", base)
	if len(got) != 3 {
		t.Fatalf("bulkAckRecords = %+v, want 3 acks", got)
	}
	seen := map[string]bool{}
	for i, r := range got {
		if r.Kind != coord.KindAck || r.AckOf != notes[i].ID || r.Window != "/w" || r.Session != "A" || r.Repo != "repo" {
			t.Errorf("ack %d = %+v", i, r)
		}
		if seen[r.ID] {
			t.Errorf("duplicate ack id %q", r.ID)
		}
		seen[r.ID] = true
	}
	// An ack by this session clears the note for it and for no other session.
	recs := append([]coord.Record{{ID: "n1", Kind: coord.KindAnnounce, Window: "/x", Session: "X"}}, got[0])
	if box := coord.Inbox(recs, self); len(box) != 0 {
		t.Errorf("A's own bulk ack must clear n1 for A: %v", box)
	}
	if box := coord.Inbox(recs, coord.Self{Window: "/w", Session: "B"}); len(box) != 1 {
		t.Errorf("A's bulk ack must not clear n1 for B: %v", box)
	}
}

// Codex exports CODEX_SESSION_ID to its shell commands but not to its hook
// processes; the hook payload's session_id comes from the same
// Session::session_id(). So codex-context falls back to the payload id when its
// env has no token. claude-context never does: Claude Code sets
// CLAUDE_CODE_SESSION_ID in hooks itself, and an old one that sets neither would
// be split in two by a payload fallback.
func TestAgentHookSession(t *testing.T) {
	cases := []struct {
		name    string
		env     string
		codex   bool
		payload string
		want    string
	}{
		{"codex, token-less env → the payload id", coord.SessionNone, true, "019a-x", "019a-x"},
		{"codex, env has a token (WT_SESSION) → env wins", "pinned", true, "019a-x", "pinned"},
		{"codex, no payload id → stays token-less", coord.SessionNone, true, "  ", coord.SessionNone},
		{"claude, token-less env → payload ignored", coord.SessionNone, false, "c1", coord.SessionNone},
		{"claude, env token", "c1", false, "c1", "c1"},
	}
	for _, tc := range cases {
		if got := agentHookSession(tc.env, tc.codex, tc.payload); got != tc.want {
			t.Errorf("%s: agentHookSession = %q, want %q", tc.name, got, tc.want)
		}
	}
	if got := parseHookSessionID([]byte(`{"cwd":"/x","session_id":" 019a-x "}`)); got != "019a-x" {
		t.Errorf("parseHookSessionID = %q", got)
	}
	if got := parseHookSessionID([]byte(`not json`)); got != "" {
		t.Errorf("parseHookSessionID(garbage) = %q, want empty", got)
	}
}

// #163: `wt inbox` must never print a bare "inbox clear" when this session has no
// session token while records under its checkout carry one.
func TestInboxClearMessage(t *testing.T) {
	const w = "/work/repo"
	now := time.Date(2026, 10, 8, 12, 0, 0, 0, time.UTC)
	tagged := []coord.Record{{ID: "x", Kind: coord.KindAnnounce, Window: w, Session: "B", TS: now.Add(-time.Hour).Format(time.RFC3339)}}

	msg, hedged := inboxClearMessage(tagged, coord.Self{Window: w, Session: "A"}, now)
	if hedged || !strings.HasPrefix(msg, "inbox clear") {
		t.Errorf("a tagged session can vouch for its own inbox: (%q, %v)", msg, hedged)
	}
	msg, hedged = inboxClearMessage(nil, coord.Self{Window: w, Session: coord.SessionNone}, now)
	if hedged || !strings.HasPrefix(msg, "inbox clear") {
		t.Errorf("token-less with no tagged records here: plain clear expected, got (%q, %v)", msg, hedged)
	}
	msg, hedged = inboxClearMessage(tagged, coord.Self{Window: w, Session: coord.SessionNone}, now)
	if !hedged || strings.HasPrefix(msg, "inbox clear") {
		t.Fatalf("token-less self + a tagged session posting here must hedge, got (%q, %v)", msg, hedged)
	}
	for _, want := range []string{"NOT a confirmed clear", "WT_SESSION", "wt new"} {
		if !strings.Contains(msg, want) {
			t.Errorf("hedge missing %q: %q", want, msg)
		}
	}
}

// The per-turn hook shows each entry's Window as its poster; a same-checkout
// entry is relabelled (and the caller's slice is not mutated).
func TestLabelForContext(t *testing.T) {
	self := coord.Self{Window: "/work/repo", Session: "A"}
	box := []coord.Record{
		{ID: "h", Window: "/work/repo", Session: "B", Message: "pushing rebuilds", Hold: []string{"merge-main"}},
		{ID: "n", Window: "/work/repo-worktrees/roll", Session: "R", Message: "rolling nodes"},
	}
	got := labelForContext(box, self)
	if got[0].Window != "same checkout, another session (session B)" || got[1].Window != "/work/repo-worktrees/roll" {
		t.Errorf("labels = %q, %q", got[0].Window, got[1].Window)
	}
	if box[0].Window != "/work/repo" {
		t.Errorf("labelForContext mutated its input: %q", box[0].Window)
	}
	msg, has := coordContextMessage(got, 0, time.Now())
	if !has || !strings.Contains(msg, "HOLD same checkout, another session (session B) [merge-main]") {
		t.Errorf("per-turn context must name the same-checkout hold, got:\n%s", msg)
	}
}

// In a single-worktree repo the hook speaks for another session in this checkout
// (#163) AND for every hold from another window — the merge gate enforces those
// whether or not that window's worktree still exists, so the hook must never hide
// one. Plain announcements from removed worktrees / a same-named clone stay out
// of every turn, as before. With other worktrees it is the full inbox.
func TestHookInbox(t *testing.T) {
	self := coord.Self{Window: "/work/repo", Session: "A"}
	box := []coord.Record{
		{ID: "same", Window: "/work/repo", Session: "B"},
		{ID: "gone", Window: "/work/repo-worktrees/removed-long-ago", Session: "R"},
		{ID: "held", Window: "/work/repo-worktrees/removed-long-ago", Session: "R", Hold: []string{"merge-main"}},
		{ID: "pinned", Window: "awin", Session: "P", Hold: []string{"merge-main"}}, // a WT_WINDOW-pinned session here
	}
	got := hookInbox(box, self, false)
	if strings.Join(recIDs(got), ",") != "same,held,pinned" {
		t.Fatalf("single worktree: hookInbox = %v, want [same held pinned] (other windows' plain notes stay out, their holds never)", recIDs(got))
	}
	if got[0].Window != "same checkout, another session (session B)" || got[1].Window != "/work/repo-worktrees/removed-long-ago" {
		t.Errorf("labels = %q, %q", got[0].Window, got[1].Window)
	}
	if got := hookInbox(box, self, true); len(got) != 4 {
		t.Errorf("other worktrees present: hookInbox = %+v, want the whole inbox", got)
	}
}

func recIDs(recs []coord.Record) []string {
	out := make([]string, 0, len(recs))
	for _, r := range recs {
		out = append(out, r.ID)
	}
	return out
}

// #163: the hook used to return before looking whenever the repo had ≤1
// worktree — the issue's exact setup (two sessions in one primary checkout), so
// the other session's announcements never reached the per-turn context.
func TestHookPlan(t *testing.T) {
	cases := []struct {
		name               string
		worktrees          int
		coordLog           bool
		wantRun, wantScan  bool
		wantCoordLogLooked bool
	}{
		{"multi-worktree: scan overlaps + coordination", 2, false, true, true, false},
		{"single checkout WITH a coord log: coordination only", 1, true, true, false, true},
		{"single checkout, no coord log: stay silent and cheap", 1, false, false, false, true},
	}
	for _, tc := range cases {
		looked := false
		run, scan := hookPlan(tc.worktrees, func() bool { looked = true; return tc.coordLog })
		if run != tc.wantRun || scan != tc.wantScan || looked != tc.wantCoordLogLooked {
			t.Errorf("%s: hookPlan = (run %v, scan %v, looked %v), want (%v, %v, %v)",
				tc.name, run, scan, looked, tc.wantRun, tc.wantScan, tc.wantCoordLogLooked)
		}
	}
}

func TestRepoNameFrom(t *testing.T) {
	if got := repoNameFrom("/Users/e/engineering/me/.git", "/Users/e/engineering/me-worktrees/feat-1"); got != "me" {
		t.Errorf("linked worktree must name the MAIN repo, got %q", got)
	}
	if got := repoNameFrom("/srv/repos/me.git", "/srv/checkouts/me-co"); got != "me-co" {
		t.Errorf("non-.git common dir falls back to the toplevel, got %q", got)
	}
}

// `wt inbox --json` keeps its array-of-records shape and adds same_checkout, the
// one thing a consumer can't derive (the window id equals its own).
func TestInboxEntryJSON(t *testing.T) {
	b, err := json.Marshal([]inboxEntry{
		{Record: coord.Record{ID: "x", Window: "/w", Session: "B", Kind: coord.KindAnnounce}, SameCheckout: true},
		{Record: coord.Record{ID: "y", Window: "/other", Kind: coord.KindAnnounce}},
	})
	if err != nil {
		t.Fatal(err)
	}
	var back []map[string]any
	if err := json.Unmarshal(b, &back); err != nil {
		t.Fatal(err)
	}
	if back[0]["id"] != "x" || back[0]["window"] != "/w" || back[0]["session"] != "B" || back[0]["same_checkout"] != true {
		t.Errorf("same-checkout entry = %v", back[0])
	}
	if _, ok := back[1]["same_checkout"]; ok {
		t.Errorf("another window's entry must omit same_checkout: %v", back[1])
	}
}

func TestToMCPCoord(t *testing.T) {
	self := coord.Self{Window: "/w", Session: "A"}
	got := toMCPCoord(coord.Record{ID: "x", Window: "/w", Session: "B"}, self)
	if !got.SameCheckout || got.Session != "B" {
		t.Errorf("same-checkout MCP record = %+v", got)
	}
	if other := toMCPCoord(coord.Record{ID: "y", Window: "/other", Session: "C"}, self); other.SameCheckout {
		t.Errorf("another window flagged same-checkout: %+v", other)
	}
}

// TestHookAgentContextSingleCheckoutWiring drives the real per-turn hook in a
// ONE-worktree repo that has a coordination log: #163's exact setup (two sessions
// in one primary checkout). It pins the wiring the pure tests can't reach: the
// hook (through runHook's dispatch) must get to hookPlan rather than return early
// on ≤1 worktree, it must surface another window's hold there, and codex-context
// must read the log as its payload's session when its env carries no token
// (Codex hooks get no CODEX_SESSION_ID) while claude-context never does.
func TestHookAgentContextSingleCheckoutWiring(t *testing.T) {
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
	logPath, self := coordCtx(c) // the window exactly as the hook resolves it
	at := time.Now().Add(-time.Minute).UTC().Format(time.RFC3339)
	for _, r := range []coord.Record{
		{ID: "ownclaude", Window: self.Window, Session: "claude-A", Hold: []string{"merge-main"}},
		{ID: "otherclaude", Window: self.Window, Session: "claude-B", Hold: []string{"merge-main"}},
		{ID: "owncodex", Window: self.Window, Session: "codex-X", Hold: []string{"merge-main"}},
		{ID: "removedhold", Window: "/gone/worktree", Session: "R", Hold: []string{"merge-main"}},
		{ID: "removednote", Window: "/gone/worktree", Session: "R", Message: "old note"},
	} {
		r.TS, r.Kind = at, coord.KindAnnounce
		if err := coord.Append(logPath, r); err != nil {
			t.Fatal(err)
		}
	}
	cases := []struct {
		hook           string
		envSession     string // CLAUDE_CODE_SESSION_ID in the hook's env
		payloadSession string
		want, wantNot  []string
	}{
		{"claude-context", "claude-A", "claude-A", // reads as its env session
			[]string{"otherclaude", "owncodex", "removedhold"}, []string{"ownclaude", "removednote"}},
		{"codex-context", "", "codex-X", // no token in a Codex hook's env: the payload session
			[]string{"ownclaude", "otherclaude", "removedhold"}, []string{"owncodex", "removednote"}},
		{"claude-context", "", "codex-X", // never takes the payload session
			[]string{"owncodex", "ownclaude", "otherclaude"}, nil},
	}
	for _, tc := range cases {
		t.Setenv("CLAUDE_CODE_SESSION_ID", tc.envSession)
		payload := fmt.Sprintf(`{"cwd":%q,"session_id":%q,"hook_event_name":"UserPromptSubmit"}`, repo, tc.payloadSession)
		out := captureStdoutForTest(t, func() { withStdinForTest(t, payload, func() { runHook([]string{tc.hook}) }) })
		name := tc.hook + " env=" + tc.envSession + " payload=" + tc.payloadSession
		var got struct {
			HookSpecificOutput struct {
				AdditionalContext string `json:"additionalContext"`
			} `json:"hookSpecificOutput"`
		}
		if err := json.Unmarshal([]byte(out), &got); err != nil {
			t.Fatalf("%s: no per-turn context in a single-worktree repo with a coord log (did the hook return before hookPlan?): %q", name, out)
		}
		ctx := got.HookSpecificOutput.AdditionalContext
		for _, id := range tc.want {
			if !strings.Contains(ctx, "wt ack "+id+")") {
				t.Errorf("%s: context is missing %s:\n%s", name, id, ctx)
			}
		}
		for _, id := range tc.wantNot {
			if strings.Contains(ctx, id) {
				t.Errorf("%s: context must not show %s:\n%s", name, id, ctx)
			}
		}
	}
}

// gitInitForTest makes dir a git repo with one empty commit, isolated from the
// caller's git environment (a hook's GIT_DIR) and global config.
func gitInitForTest(t *testing.T, dir string) {
	t.Helper()
	var env []string
	for _, kv := range os.Environ() {
		if !strings.HasPrefix(kv, "GIT_") {
			env = append(env, kv)
		}
	}
	env = append(env, "GIT_AUTHOR_NAME=t", "GIT_AUTHOR_EMAIL=t@example.invalid", "GIT_COMMITTER_NAME=t",
		"GIT_COMMITTER_EMAIL=t@example.invalid", "GIT_CONFIG_NOSYSTEM=1", "GIT_CONFIG_GLOBAL="+os.DevNull)
	for _, args := range [][]string{{"init", "-q", "-b", "main"}, {"commit", "-q", "--allow-empty", "-m", "init"}} {
		cmd := exec.Command("git", args...)
		cmd.Dir, cmd.Env = dir, env
		if out, err := cmd.CombinedOutput(); err != nil {
			t.Fatalf("git %v: %v\n%s", args, err, out)
		}
	}
}

// withStdinForTest runs f with os.Stdin reading payload (runHook reads the hook
// payload from os.Stdin).
func withStdinForTest(t *testing.T, payload string, f func()) {
	t.Helper()
	p := filepath.Join(t.TempDir(), "payload.json")
	if err := os.WriteFile(p, []byte(payload), 0o644); err != nil {
		t.Fatal(err)
	}
	in, err := os.Open(p)
	if err != nil {
		t.Fatal(err)
	}
	defer in.Close()
	old := os.Stdin
	os.Stdin = in
	defer func() { os.Stdin = old }()
	f()
}

// captureStdoutForTest returns what f prints to os.Stdout.
func captureStdoutForTest(t *testing.T, f func()) string {
	t.Helper()
	r, w, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	old := os.Stdout
	os.Stdout = w
	defer func() { os.Stdout = old }()
	f()
	_ = w.Close()
	b, _ := io.ReadAll(r)
	return string(b)
}
