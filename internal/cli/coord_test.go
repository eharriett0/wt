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
		// Your own record must never be labelled as another session's (review
		// finding: cmdAck did) — and a pre-#163 one is NOT yours (#211): it is named
		// as one wt can't attribute.
		{coord.Record{Window: "/work/repo", Session: "A"}, "this window (yours)"},
		{coord.Record{Window: "/work/repo"}, "same checkout, can't tell whose (written before wt recorded sessions)"},
		{coord.Record{Window: "/work/repo-worktrees/roll", Session: "R"}, "/work/repo-worktrees/roll"},
	}
	for _, tc := range cases {
		if got := postedBy(tc.rec, self); got != tc.want {
			t.Errorf("postedBy(%+v) = %q, want %q", tc.rec, got, tc.want)
		}
	}
	// A token-less shell's own token-less record is its own (the directory
	// fallback); a pre-#163 one is unattributable to it too.
	tokenless := coord.Self{Window: "/work/repo", Session: coord.SessionNone}
	if got := postedBy(coord.Record{Window: "/work/repo", Session: coord.SessionNone}, tokenless); got != "this window (yours)" {
		t.Errorf("token-less pair label = %q", got)
	}
	if got := postedBy(coord.Record{Window: "/work/repo"}, tokenless); got != "same checkout, can't tell whose (written before wt recorded sessions)" {
		t.Errorf("token-less reader, pre-#163 record label = %q", got)
	}
}

// #211: who a record belongs to, as the all-clear guard names it.
func TestWhoseRecord(t *testing.T) {
	self := coord.Self{Window: "/w", Session: "aaaaaaaa-1111-2222-3333-444444444444"}
	cases := []struct {
		rec  coord.Record
		want string
	}{
		{coord.Record{Window: "/w", Session: self.Session}, "yours"},
		{coord.Record{Window: "/w", Session: "bbbbbbbb-5555-6666-7777-888888888888"}, "another session's, in this checkout (session bbbbbbbb…)"},
		{coord.Record{Window: "/w"}, "unattributable (written before wt recorded sessions)"},
		{coord.Record{Window: "/elsewhere", Session: "C"}, "another window's (/elsewhere)"},
	}
	for _, tc := range cases {
		if got := whoseRecord(tc.rec, self); got != tc.want {
			t.Errorf("whoseRecord(%+v) = %q, want %q", tc.rec, got, tc.want)
		}
	}
}

// The all-clear guard (#211): an all-clear releases a hold for EVERY window, so
// only a record this session provably wrote is released without a word. Anything
// else is asked about on a terminal, refused otherwise, and released only with
// --not-mine. Before, `wt all-clear` released any id silently — #211's operator
// nearly released another live session's hold that `wt holds` had listed as theirs.
func TestAllClearVerdict(t *testing.T) {
	self := coord.Self{Window: "/w", Session: "A"}
	mine := coord.Record{Window: "/w", Session: "A"}
	mirroredMine := coord.Record{Window: "/w", Session: coord.MirrorSession("A")}
	another := coord.Record{Window: "/w", Session: "B"}
	legacy := coord.Record{Window: "/w"}
	otherWin := coord.Record{Window: "/elsewhere", Session: "C"}
	tokenless := coord.Record{Window: "/w", Session: coord.SessionNone}
	cases := []struct {
		name         string
		self         coord.Self
		rec          coord.Record
		notMine, tty bool
		want         clearVerdict
	}{
		{"mine → post", self, mine, false, false, clearPost},
		{"mine read back from the mirror → post", self, mirroredMine, false, false, clearPost},
		{"another session's, no terminal → refuse", self, another, false, false, clearRefuse},
		{"another session's, terminal → ask", self, another, false, true, clearAsk},
		{"another session's, --not-mine → post", self, another, true, false, clearPost},
		{"pre-#163 (unattributable), no terminal → refuse", self, legacy, false, false, clearRefuse},
		{"pre-#163, terminal → ask", self, legacy, false, true, clearAsk},
		{"another window's, no terminal → refuse", self, otherWin, false, false, clearRefuse},
		{"token-less shell, its own token-less record (directory fallback) → post", coord.Self{Window: "/w", Session: coord.SessionNone}, tokenless, false, false, clearPost},
		{"token-less shell, a pre-#163 record → refuse", coord.Self{Window: "/w", Session: coord.SessionNone}, legacy, false, false, clearRefuse},
		{"token-less shell, a tagged session's record → refuse", coord.Self{Window: "/w", Session: coord.SessionNone}, another, false, false, clearRefuse},
	}
	for _, tc := range cases {
		if got := allClearVerdict(tc.self, tc.rec, tc.notMine, tc.tty); got != tc.want {
			t.Errorf("%s: allClearVerdict = %v, want %v", tc.name, got, tc.want)
		}
	}
}

// `wt holds` lists, of the announcements in this checkout wt can't attribute
// (#211): the newest holds (capped) and the plain ones still inside the horizon;
// older ones are only counted — a month of them is what buried the operator's
// real holds.
func TestUnattributedView(t *testing.T) {
	now := time.Date(2026, 10, 10, 12, 0, 0, 0, time.UTC)
	at := func(d time.Duration) string { return now.Add(-d).Format(time.RFC3339) }
	var open []coord.Record
	for i := 0; i < unattributedShown+3; i++ { // oldest first, as the log is
		open = append(open, coord.Record{ID: fmt.Sprintf("h%02d", i), TS: at(time.Duration(40-i) * 24 * time.Hour), Hold: []string{"merge-main"}})
	}
	open = append(open,
		coord.Record{ID: "oldnote", TS: at(5 * 24 * time.Hour)},
		coord.Record{ID: "freshnote", TS: at(time.Hour)},
	)
	show, olderHolds, olderNotes := unattributedView(open, now)
	got := recIDs(show)
	if len(got) != unattributedShown+1 || got[0] != "freshnote" || got[1] != fmt.Sprintf("h%02d", unattributedShown+2) || olderHolds != 3 || olderNotes != 1 {
		t.Errorf("unattributedView = %v (+%d holds, +%d notes); want freshnote then the %d newest holds, +3 holds, +1 note", got, olderHolds, olderNotes, unattributedShown)
	}
	for _, id := range got {
		if id == "oldnote" {
			t.Errorf("a plain announcement past the horizon must be counted, not listed: %v", got)
		}
	}
	// stale = no longer blocks merge-pr: past hold_max_age or the horizon, whichever is first
	h := coord.Record{TS: at(10 * time.Hour)}
	if unattributedHoldStale(h, now, 24*time.Hour) || !unattributedHoldStale(h, now, 2*time.Hour) || unattributedHoldStale(h, now, 0) {
		t.Error("a 10h-old unattributable hold: fresh under 24h/0 hold_max_age, stale under 2h")
	}
	if !unattributedHoldStale(coord.Record{TS: at(30 * time.Hour)}, now, 0) {
		t.Error("an unattributable hold past the 24h horizon is stale even with hold_max_age off")
	}
	if backlogLine(0, 0) != "" || !strings.Contains(backlogLine(4, 1), "+ 5 older announcement(s) in this checkout (1 with a hold)") {
		t.Errorf("backlogLine = %q", backlogLine(4, 1))
	}
	shown, more := newestHolds(open[:4], 2)
	if strings.Join(recIDs(shown), ",") != "h02,h03" || more != 2 {
		t.Errorf("newestHolds = %v +%d, want [h02 h03] +2", recIDs(shown), more)
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
		{[]coord.Record{{Window: "/w"}}, "an unattributable writer in this checkout"},
		{[]coord.Record{other, {Window: "/w"}}, "another window AND an unattributable writer in this checkout"},
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
	if !unattributedHolder([]coord.Record{other, {Window: "/w"}}, self) || unattributedHolder([]coord.Record{other, same}, self) {
		t.Error("unattributedHolder must be true iff a holder is unattributable in this checkout")
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
		{"a young pre-#163 hold here: ack + can't-tell-whose note, NO all-clear", []coord.Record{{ID: "legacy", Window: "/w", TS: at(time.Hour), Hold: []string{"merge-main"}}}, nil, false},
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
	} // #211: an unattributable holder gets the can't-tell-whose note (it may be
	// yours: ack it); a holder wt can attribute does not.
	unattr := coord.Record{ID: "legacy", Window: "/w", TS: at(time.Hour), Hold: []string{"merge-main"}}
	if all := strings.Join(holdAdvice([]coord.Record{unattr}, nil, self, now), "\n"); !strings.Contains(all, "can't tell whose") {
		t.Errorf("an unattributable holder must get the can't-tell-whose note:\n%s", all)
	}
	if all := strings.Join(holdAdvice([]coord.Record{liveSame, liveOther}, nil, self, now), "\n"); strings.Contains(all, "can't tell whose") {
		t.Errorf("attributable holders must not get the can't-tell-whose note:\n%s", all)
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
	if box := coord.Inbox(recs, self, base); len(box) != 0 {
		t.Errorf("A's own bulk ack must clear n1 for A: %v", box)
	}
	if box := coord.Inbox(recs, coord.Self{Window: "/w", Session: "B"}, base); len(box) != 1 {
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
		{ID: "legacy", Window: "/work/repo"},                                       // can't tell whose (#211)
	}
	got := hookInbox(box, self, false)
	if strings.Join(recIDs(got), ",") != "same,held,pinned,legacy" {
		t.Fatalf("single worktree: hookInbox = %v, want [same held pinned legacy] (other windows' plain notes stay out, their holds never; this checkout's unattributable ones in)", recIDs(got))
	}
	if got[0].Window != "same checkout, another session (session B)" || got[1].Window != "/work/repo-worktrees/removed-long-ago" {
		t.Errorf("labels = %q, %q", got[0].Window, got[1].Window)
	}
	if got := hookInbox(box, self, true); len(got) != 5 {
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
	if !got.SameCheckout || got.Session != "B" || got.Attribution != "another_session" {
		t.Errorf("same-checkout MCP record = %+v", got)
	}
	// #211: a same-checkout record wt can't attribute is flagged, and says so.
	if legacy := toMCPCoord(coord.Record{ID: "l", Window: "/w"}, self); !legacy.SameCheckout || legacy.Attribution != "unattributable" {
		t.Errorf("unattributable MCP record = %+v", legacy)
	}
	if other := toMCPCoord(coord.Record{ID: "y", Window: "/other", Session: "C"}, self); other.SameCheckout || other.Attribution != "" {
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

// TestUnattributedWiring drives the real commands over #211's shape: one
// checkout whose log carries pre-#163 (session-less) records next to two live
// sessions' holds. It pins what the pure tests can't: `wt holds` lists only this
// session's records as yours (with all-clear lines), the all-clear guard refuses
// anything else without a terminal or --not-mine (and asks on one), `announce
// --clear` is guarded the same way, and a young pre-#163 hold gates merge-pr.
func TestUnattributedWiring(t *testing.T) {
	repo, home := t.TempDir(), t.TempDir()
	gitInitForTest(t, repo)
	t.Setenv("HOME", home)
	for _, k := range []string{"WT_SESSION", "CODEX_SESSION_ID", "WT_WINDOW", "WT_COORD_ISSUE"} {
		t.Setenv(k, "")
	}
	t.Setenv("CLAUDE_CODE_SESSION_ID", "aaaaaaaa-1111-2222-3333-444444444444")
	t.Chdir(repo)
	c, err := config.Load()
	if err != nil {
		t.Fatal(err)
	}
	logPath, self := coordCtx(c)
	now := time.Now().UTC()
	at := func(d time.Duration) string { return now.Add(-d).Format(time.RFC3339) }
	recs := []coord.Record{
		{ID: "legacyhold", Window: self.Window, TS: at(time.Hour), Hold: []string{"merge-main"}, Message: "pre-#163 hold"},
		{ID: "legacynote", Window: self.Window, TS: at(5 * 24 * time.Hour), Message: "pre-#163 note"},
		{ID: "minehold", Window: self.Window, Session: self.Session, TS: at(time.Minute), Hold: []string{"merge-main"}, Message: "mine"},
		{ID: "bhold", Window: self.Window, Session: "bbbbbbbb-5555-6666-7777-888888888888", TS: at(time.Minute), Hold: []string{"merge-main"}, Message: "B's"},
	}
	for _, r := range recs {
		r.Kind, r.Repo = coord.KindAnnounce, "repo"
		if err := coord.Append(logPath, r); err != nil {
			t.Fatal(err)
		}
	}
	clears := func() []string {
		all, _ := coord.Load(logPath)
		var out []string
		for _, r := range all {
			if r.Kind == coord.KindAllClear {
				out = append(out, r.AckOf)
			}
		}
		return out
	}

	out, _ := captureOutputForTest(t, func() { cmdHolds(nil) })
	if !strings.Contains(out, "all-clear: wt all-clear minehold") {
		t.Errorf("`wt holds` must list this session's own hold with its all-clear line:\n%s", out)
	}
	for _, id := range []string{"legacyhold", "legacynote", "bhold"} {
		if strings.Contains(out, "wt all-clear "+id) {
			t.Errorf("`wt holds` offered an all-clear for %s, which is not this session's:\n%s", id, out)
		}
	}
	if !strings.Contains(out, "can't tell whose") || !strings.Contains(out, "legacyhold") || strings.Contains(out, "legacynote") {
		t.Errorf("`wt holds` must list the young pre-#163 hold apart, and only count the 5-day-old note:\n%s", out)
	}

	// inbox surfaces the young pre-#163 hold (labelled) and counts the old note.
	inbox, _ := captureOutputForTest(t, func() { cmdInbox(nil) })
	if !strings.Contains(inbox, "legacyhold  same checkout, can't tell whose") || strings.Contains(inbox, "legacynote") ||
		!strings.Contains(inbox, "+ 1 older announcement(s) in this checkout (0 with a hold)") {
		t.Errorf("`wt inbox` must show the young pre-#163 hold and count, not list, the 5-day-old note:\n%s", inbox)
	}

	interactive := false
	defer func(old func() bool) { confirmInteractive = old }(confirmInteractive)
	confirmInteractive = func() bool { return interactive }
	for _, id := range []string{"legacyhold", "bhold"} {
		if code := cmdAllClear([]string{id}); code != 1 {
			t.Errorf("non-TTY all-clear of %s (not this session's) = exit %d, want 1 (refused)", id, code)
		}
	}
	if code := cmdAnnounce([]string{"--clear", "bhold"}); code != 1 {
		t.Errorf("non-TTY `announce --clear` of another session's hold = exit %d, want 1", code)
	}
	if got := clears(); len(got) != 0 {
		t.Fatalf("a refused all-clear wrote %v", got)
	}
	// The pre-#163 hold is an hour old: it is not exempt for this session. Seven
	// more, days old, only warn — and the warning lists the newest few, not all.
	for i := 0; i < staleHoldsShown+2; i++ {
		old := coord.Record{ID: fmt.Sprintf("oldhold%d", i), Kind: coord.KindAnnounce, Repo: "repo", Window: self.Window,
			TS: at(time.Duration(48+i) * time.Hour), Hold: []string{"merge-main"}}
		if err := coord.Append(logPath, old); err != nil {
			t.Fatal(err)
		}
	}
	var gateErr string
	var code int
	_, gateErr = captureOutputForTest(t, func() { code = mergeCoordGate(c) })
	if code != 1 || !strings.Contains(gateErr, "an unattributable writer in this checkout") {
		t.Errorf("merge gate = %d, want 1: a young pre-#163 hold must gate, not exempt whoever reads it:\n%s", code, gateErr)
	}
	if !strings.Contains(gateErr, fmt.Sprintf("%d stale merge-main hold(s)", staleHoldsShown+2)) || !strings.Contains(gateErr, "…and 2 older") {
		t.Errorf("the gate must count every stale hold but list only the newest %d:\n%s", staleHoldsShown, gateErr)
	}
	interactive = true
	if code := withStdinCode(t, "n\n", func() int { return cmdAllClear([]string{"bhold"}) }); code != 1 {
		t.Errorf("answering n = exit %d, want 1", code)
	}
	if code := withStdinCode(t, "y\n", func() int { return cmdAllClear([]string{"bhold"}) }); code != 0 {
		t.Errorf("answering y = exit %d, want 0", code)
	}
	interactive = false
	if code := cmdAllClear([]string{"legacyhold", "--not-mine"}); code != 0 {
		t.Errorf("--not-mine = exit %d, want 0", code)
	}
	if code := cmdAllClear([]string{"minehold"}); code != 0 {
		t.Errorf("all-clear of this session's own hold = exit %d, want 0 (no prompt, no flag)", code)
	}
	if got := strings.Join(clears(), ","); got != "bhold,legacyhold,minehold" {
		t.Errorf("all-clears written = %s, want bhold (asked, y), legacyhold (--not-mine), minehold (own)", got)
	}
}

// withStdinCode runs f with os.Stdin reading payload and its output captured.
func withStdinCode(t *testing.T, payload string, f func() int) (code int) {
	t.Helper()
	captureOutputForTest(t, func() { withStdinForTest(t, payload, func() { code = f() }) })
	return code
}

// captureOutputForTest returns what f prints to os.Stdout and os.Stderr.
func captureOutputForTest(t *testing.T, f func()) (stdout, stderr string) {
	t.Helper()
	er, ew, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	old := os.Stderr
	os.Stderr = ew
	done := make(chan string)
	go func() { b, _ := io.ReadAll(er); done <- string(b) }()
	stdout = captureStdoutForTest(t, f)
	os.Stderr = old
	_ = ew.Close()
	return stdout, <-done
}
