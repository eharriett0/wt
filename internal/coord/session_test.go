package coord

import (
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// #163 / #211: two agent sessions started in ONE checkout resolve to the same
// window id (path-based, deliberately kept — #18/#156), so the window alone can't
// tell them apart. Self.Attribute is the ONE ownership decision: the same real
// token (or its mirror hash) is provably yours; a token-less record read by a
// token-less reader is yours only by the directory fallback (#18 — wt can't prove
// it); a different session is another party; and a record with no session at all
// is UNATTRIBUTABLE, never yours (#211: a month of pre-#163 records was listed as
// the current session's own).
func TestAttribute(t *testing.T) {
	const w, other = "/x/repo", "/x/other"
	cases := []struct {
		name string
		self Self
		rec  Record
		want Attribution
	}{
		{"same window, same session", Self{w, "A"}, Record{Window: w, Session: "A"}, AttrMine},
		{"same window, DIFFERENT session — another party in this checkout", Self{w, "A"}, Record{Window: w, Session: "B"}, AttrAnotherSession},
		{"pre-#163 record (no session) is unattributable, never a wildcard (#211)", Self{w, "A"}, Record{Window: w}, AttrUnattributable},
		{"zero-session self proves nothing", Self{Window: w}, Record{Window: w, Session: "B"}, AttrUnattributable},
		{"neither side has a session", Self{Window: w}, Record{Window: w}, AttrUnattributable},
		{"token-less self vs token-less record: the directory fallback, not proof (#18)", Self{w, SessionNone}, Record{Window: w, Session: SessionNone}, AttrMineFallback},
		{"token-less self vs pre-#163 record", Self{w, SessionNone}, Record{Window: w}, AttrUnattributable},
		{"token-less self vs tagged record → another party", Self{w, SessionNone}, Record{Window: w, Session: "B"}, AttrAnotherSession},
		{"tagged self vs token-less record → another party", Self{w, "A"}, Record{Window: w, Session: SessionNone}, AttrAnotherSession},
		{"other window, same session: still another window for the gate", Self{w, "A"}, Record{Window: other, Session: "A"}, AttrOtherWindow},
		{"other window, no session", Self{w, "A"}, Record{Window: other}, AttrOtherWindow},
		{"other window, zero self", Self{Window: w}, Record{Window: other}, AttrOtherWindow},
		{"other window, both token-less: no fallback across checkouts", Self{w, SessionNone}, Record{Window: other, Session: SessionNone}, AttrOtherWindow},
		// The GitHub mirror carries MirrorSession(token): self still owns its own
		// record read back from there (#18 across clones that share only the mirror).
		{"own record read back from the mirror (hashed) → mine", Self{w, "A"}, Record{Window: w, Session: MirrorSession("A")}, AttrMine},
		{"another session's mirrored record → another party", Self{w, "A"}, Record{Window: w, Session: MirrorSession("B")}, AttrAnotherSession},
		{"token-less self never claims a tagged session's mirrored record", Self{w, SessionNone}, Record{Window: w, Session: MirrorSession("A")}, AttrAnotherSession},
		{"own mirrored record from another window", Self{w, "A"}, Record{Window: other, Session: MirrorSession("A")}, AttrOtherWindow},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := tc.self.Attribute(tc.rec)
			if got != tc.want {
				t.Errorf("%+v.Attribute(%+v) = %v, want %v", tc.self, tc.rec, got, tc.want)
			}
			// The predicates are views of Attribute and can never disagree.
			if tc.self.Owns(tc.rec) != (got == AttrMine || got == AttrMineFallback) || tc.self.SharesCheckout(tc.rec) != (got == AttrAnotherSession) ||
				tc.self.Unattributed(tc.rec) != (got == AttrUnattributable) {
				t.Errorf("Owns/SharesCheckout/Unattributed disagree with Attribute=%v for %+v", got, tc.rec)
			}
			if got == AttrMine && !tc.self.ProvablyWrote(tc.rec) {
				t.Errorf("a provably-mine record must be ProvablyWrote: %+v", tc.rec)
			}
			if got == AttrMineFallback && tc.self.ProvablyWrote(tc.rec) {
				t.Errorf("the directory fallback proves nothing: %+v", tc.rec)
			}
		})
	}
}

// ProvablyWrote (#211 review F5) is authorship by token, in ANY window: what lets
// `wt all-clear` release a record without asking. Only a real token proves it;
// "" and SessionNone never do, whatever the window.
func TestProvablyWrote(t *testing.T) {
	cases := []struct {
		self Self
		rec  Record
		want bool
	}{
		{Self{"/w", "A"}, Record{Window: "/w", Session: "A"}, true},
		{Self{"/w", "A"}, Record{Window: "/elsewhere", Session: "A"}, true},
		{Self{"/w", "A"}, Record{Window: "/elsewhere", Session: MirrorSession("A")}, true},
		{Self{"/w", "A"}, Record{Window: "/w", Session: "B"}, false},
		{Self{"/w", "A"}, Record{Window: "/w"}, false},
		{Self{"/w", SessionNone}, Record{Window: "/w", Session: SessionNone}, false},
		{Self{"/w", SessionNone}, Record{Window: "/elsewhere", Session: SessionNone}, false},
		{Self{Window: "/w"}, Record{Window: "/w"}, false},
		{Self{Window: "/w"}, Record{Window: "/w", Session: MirrorSession("")}, false},
	}
	for _, tc := range cases {
		if got := tc.self.ProvablyWrote(tc.rec); got != tc.want {
			t.Errorf("%+v.ProvablyWrote(%+v) = %v, want %v", tc.self, tc.rec, got, tc.want)
		}
	}
}

func TestSessionToken(t *testing.T) {
	cases := []struct {
		name             string
		env              map[string]string
		wantTok, wantSrc string
	}{
		{"nothing set", nil, "", ""},
		{"WT_SESSION", map[string]string{"WT_SESSION": "agent-2"}, "agent-2", "WT_SESSION"},
		{"Claude Code", map[string]string{"CLAUDE_CODE_SESSION_ID": "c1"}, "c1", "CLAUDE_CODE_SESSION_ID"},
		// Codex exports CODEX_SESSION_ID to every shell command (openai/codex#37848).
		{"Codex", map[string]string{"CODEX_SESSION_ID": "x1", "CODEX_THREAD_ID": "t1"}, "x1", "CODEX_SESSION_ID"},
		{"Claude Code's id outranks an inherited Codex one", map[string]string{"CLAUDE_CODE_SESSION_ID": "c1", "CODEX_SESSION_ID": "x1"}, "c1", "CLAUDE_CODE_SESSION_ID"},
		{"WT_SESSION overrides the harness id", map[string]string{"WT_SESSION": "pinned", "CLAUDE_CODE_SESSION_ID": "c1"}, "pinned", "WT_SESSION"},
		{"WT_SESSION overrides Codex too", map[string]string{"WT_SESSION": "pinned", "CODEX_SESSION_ID": "x1"}, "pinned", "WT_SESSION"},
		{"blank WT_SESSION is unset", map[string]string{"WT_SESSION": "   ", "CLAUDE_CODE_SESSION_ID": "c1"}, "c1", "CLAUDE_CODE_SESSION_ID"},
		{"value is trimmed", map[string]string{"WT_SESSION": "  a  "}, "a", "WT_SESSION"},
		// Terminal ids are deliberately NOT a session: inherited by every tmux pane
		// / editor started in the tab (false confidence), and per-tab for a human
		// (a hold in one tab would block its owner in another; #18 WT_WINDOW pinning
		// across terminals would stop exempting its own hold).
		{"terminal ids are ignored", map[string]string{"TERM_SESSION_ID": "t1", "ITERM_SESSION_ID": "w0t1p0:i1"}, "", ""},
		{"a Claude session in a terminal tab is the Claude session", map[string]string{"CLAUDE_CODE_SESSION_ID": "c1", "TERM_SESSION_ID": "t1"}, "c1", "CLAUDE_CODE_SESSION_ID"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			tok, src := SessionToken(func(k string) string { return tc.env[k] })
			if tok != tc.wantTok || src != tc.wantSrc {
				t.Errorf("SessionToken = (%q, %q), want (%q, %q)", tok, src, tc.wantTok, tc.wantSrc)
			}
		})
	}
}

func TestCurrentSelf(t *testing.T) {
	env := func(m map[string]string) func(string) string { return func(k string) string { return m[k] } }
	// No token anywhere → the explicit SessionNone marker, never "" (which would
	// make this session's records wildcards that match every other session).
	if s := CurrentSelf(env(nil), "/x/repo", "main"); s.Session != SessionNone || s.Window != "/x/repo" {
		t.Errorf("token-less CurrentSelf = %+v, want {/x/repo none}", s)
	}
	// Two terminal tabs of one human are one (token-less) session, as before:
	// tab ids are not sessions, and a token-less record is a token-less reader's
	// own (the directory fallback).
	t1 := CurrentSelf(env(map[string]string{"TERM_SESSION_ID": "t1"}), "/x/repo", "main")
	t2 := CurrentSelf(env(map[string]string{"TERM_SESSION_ID": "t2"}), "/x/repo", "main")
	if t1 != t2 || !t2.Owns(Record{Window: "/x/repo", Session: t1.Session}) {
		t.Errorf("two terminal tabs must stay one session: %+v vs %+v", t1, t2)
	}
	s := CurrentSelf(env(map[string]string{"CLAUDE_CODE_SESSION_ID": "c1", "WT_WINDOW": "pinned"}), "/x/repo", "main")
	if s.Session != "c1" || s.Window != "pinned" {
		t.Errorf("CurrentSelf = %+v, want {pinned c1} (window id itself is unchanged: WT_WINDOW wins)", s)
	}
	// The window id is exactly WindowID's — the session never leaks into it (#156:
	// claims key on the window and must survive a restart, i.e. a new session).
	if s.Window != WindowID("pinned", "/x/repo", "main") {
		t.Errorf("window drifted from WindowID: %q", s.Window)
	}
}

// TestTwoSessionsShareCheckout is the #163 acceptance: session B announces (one
// with a merge-main hold) from the SAME checkout as session A. Before the fix A
// saw "inbox clear", `wt holds` listed B's announcements as A's own, and the
// merge-pr gate exempted B's hold. B itself must keep the own-hold exemption.
func TestTwoSessionsShareCheckout(t *testing.T) {
	const w = "/work/repo" // the primary checkout, no worktrees
	a, b := Self{w, "A"}, Self{w, "B"}
	now := time.Date(2026, 10, 8, 12, 0, 0, 0, time.UTC)
	recs := []Record{
		{ID: "n1", Kind: KindAnnounce, Window: w, Session: "B", Message: "moving prod tags"},
		{ID: "h1", Kind: KindAnnounce, Window: w, Session: "B", Message: "pushing rebuilds", Hold: []string{"merge-main"}},
	}
	if got := ids(Inbox(recs, a, now)); strings.Join(got, ",") != "n1,h1" {
		t.Errorf("A's inbox = %v, want [n1 h1] — another session's announcements must reach A", got)
	}
	if got := ids(PendingHolds(recs, a, now)); strings.Join(got, ",") != "h1" {
		t.Errorf("A's pending holds = %v, want [h1]", got)
	}
	if got := ActiveHolds(recs, a, "merge-main", now); len(got) != 1 || got[0].ID != "h1" {
		t.Errorf("A's merge gate holds = %v, want [h1] — B's hold must NOT be exempted for A", ids(got))
	}
	if got := OwnOpenAnnouncements(recs, a); len(got) != 0 {
		t.Errorf("A's own open = %v, want none — B's announcements are not A's", ids(got))
	}
	if got := ids(SameCheckoutOpen(recs, a)); strings.Join(got, ",") != "n1,h1" {
		t.Errorf("A's same-checkout others = %v, want [n1 h1]", got)
	}
	// B, the announcer: unchanged — its own inbox is empty, it is exempt from its
	// own hold (#18), and its holds are its own.
	if got := Inbox(recs, b, now); len(got) != 0 {
		t.Errorf("B's inbox = %v, want empty (you don't ack yourself)", ids(got))
	}
	if got := ActiveHolds(recs, b, "merge-main", now); len(got) != 0 {
		t.Errorf("B self-blocked on its OWN hold: %v", ids(got))
	}
	if got := ids(OwnOpenAnnouncements(recs, b)); strings.Join(got, ",") != "n1,h1" {
		t.Errorf("B's own open = %v, want [n1 h1]", got)
	}
	if got := SameCheckoutOpen(recs, b); len(got) != 0 {
		t.Errorf("B's same-checkout others = %v, want none", ids(got))
	}
	// A acks the plain note → it leaves A's inbox; the hold still gates A.
	recs = append(recs, Record{ID: "k1", Kind: KindAck, Window: w, Session: "A", AckOf: "n1"})
	if got := ids(Inbox(recs, a, now)); strings.Join(got, ",") != "h1" {
		t.Errorf("after A acks n1, A's inbox = %v, want [h1]", got)
	}
}

// Another session's ack is not yours (#163): it read the announcement, you did
// not. Before the fix an ack from ANY session in the checkout cleared it for all.
func TestAckedByIsPerSession(t *testing.T) {
	const w = "/work/repo"
	a, b := Self{w, "A"}, Self{w, "B"}
	now := time.Date(2026, 10, 8, 12, 0, 0, 0, time.UTC)
	recs := []Record{
		{ID: "x", Kind: KindAnnounce, Window: "/work/repo-worktrees/roll", Session: "R", Hold: []string{"merge-main"}},
		{ID: "k", Kind: KindAck, Window: w, Session: "B", AckOf: "x"}, // B acked it
	}
	if got := Inbox(recs, b, now); len(got) != 0 {
		t.Errorf("B acked x, B's inbox = %v, want empty", ids(got))
	}
	if got := ids(Inbox(recs, a, now)); strings.Join(got, ",") != "x" {
		t.Errorf("A's inbox = %v, want [x] — B's ack must not clear it for A", got)
	}
	if got := ActiveHolds(recs, a, "merge-main", now); len(got) != 1 {
		t.Errorf("A's gate = %v, want [x] — B's ack must not waive the hold for A", ids(got))
	}
	if got := ActiveHolds(recs, b, "merge-main", now); len(got) != 0 {
		t.Errorf("B acked the hold, B's gate = %v, want none", ids(got))
	}
}

// #211: a record written before sessions existed (no "session" field — every
// pre-#163 log line, every older wt binary) proves nothing about who wrote it.
// It used to be a wildcard every session in its window owned, so `wt holds`
// listed a month of them as the current session's own (with all-clear lines),
// the merge gate exempted their holds for everyone, and inbox hid them. Now it is
// unattributable: never yours, surfaced while it could be live
// (UnattributedHorizon), and a young unattributable hold gates.
func TestPreSessionRecordsAreUnattributable(t *testing.T) {
	const w = "/work/repo"
	now := time.Date(2026, 10, 10, 12, 0, 0, 0, time.UTC)
	at := func(d time.Duration) string { return now.Add(-d).Format(time.RFC3339) }
	legacy := []Record{
		{ID: "young", Kind: KindAnnounce, Window: w, TS: at(2 * time.Hour), Hold: []string{"merge-main"}},
		{ID: "old", Kind: KindAnnounce, Window: w, TS: at(30 * 24 * time.Hour), Hold: []string{"merge-main"}},
		{ID: "oldnote", Kind: KindAnnounce, Window: w, TS: at(20 * 24 * time.Hour), Message: "fyi"},
	}
	for _, self := range []Self{{w, "A"}, {w, SessionNone}, {Window: w}} {
		if got := OwnOpenAnnouncements(legacy, self); len(got) != 0 {
			t.Errorf("%+v: own open = %v, want none — a pre-#163 record is nobody's to all-clear", self, ids(got))
		}
		if got := ids(UnattributedOpen(legacy, self)); strings.Join(got, ",") != "young,old,oldnote" {
			t.Errorf("%+v: unattributed open = %v, want all three", self, got)
		}
		if got := ids(Inbox(legacy, self, now)); strings.Join(got, ",") != "young" {
			t.Errorf("%+v: inbox = %v, want [young] — surfaced while live, the month-old ones only counted", self, got)
		}
		if n, h := UnattributedBacklog(legacy, self, now); n != 1 || h != 1 {
			t.Errorf("%+v: backlog = %d notes, %d holds; want 1, 1", self, n, h)
		}
		fresh, stale := ActiveHoldsAt(legacy, self, "merge-main", now, 0) // hold_max_age disabled
		if strings.Join(ids(fresh), ",") != "young" || strings.Join(ids(stale), ",") != "old" {
			t.Errorf("%+v: gate fresh=%v stale=%v; want the young hold to BLOCK and the old one stale even with expiry off", self, ids(fresh), ids(stale))
		}
		if got := OtherSessions(legacy, self, now, 0); len(got) != 0 {
			t.Errorf("%+v: OtherSessions = %+v, want none (a legacy record names no party)", self, got)
		}
		if InboxClearAmbiguous(legacy, self, now) {
			t.Errorf("%+v: a legacy-only log must not hedge the inbox", self)
		}
		// An undatable one can't be shown to be old: surfaced, never hidden.
		undated := []Record{{ID: "undated", Kind: KindAnnounce, Window: w, TS: "garbage"}}
		if got := ids(Inbox(undated, self, now)); strings.Join(got, ",") != "undated" {
			t.Errorf("%+v: undatable unattributable record = %v, want it in the inbox", self, got)
		}
	}
}

// #211's second symptom: an ack waives a signal ONLY for the session that made
// it. Tagged sessions: per session (#163). An ack with no session counts only
// when it predates the log's first session-stamped record (pre-upgrade history,
// #211 review F1) AND its signal is older than UnattributedHorizon: a young hold
// it acked gates every session again (the safe direction), while the month of
// pre-upgrade acks every upgraded log carries doesn't flood every inbox.
func TestAcksCountOnlyForTheirSession(t *testing.T) {
	const w, other = "/work/repo", "/work/repo-worktrees/roll"
	now := time.Date(2026, 10, 10, 12, 0, 0, 0, time.UTC)
	at := func(d time.Duration) string { return now.Add(-d).Format(time.RFC3339) }
	recs := []Record{
		// pre-upgrade history: another window's note, acked here with no session
		{ID: "old", Kind: KindAnnounce, Window: other, TS: at(10 * 24 * time.Hour), Hold: []string{"merge-main"}},
		{ID: "k2", Kind: KindAck, Window: w, TS: at(9 * 24 * time.Hour), AckOf: "old"},
		// sessions begin here 5 days ago
		{ID: "first", Kind: KindAnnounce, Window: other, Session: "R", TS: at(5 * 24 * time.Hour)},
		{ID: "young", Kind: KindAnnounce, Window: other, Session: "R", TS: at(time.Hour), Hold: []string{"merge-main"}},
		{ID: "k1", Kind: KindAck, Window: w, TS: at(50 * time.Minute), AckOf: "young"},               // a still-running older wt's ack
		{ID: "k3", Kind: KindAck, Window: w, Session: "B", TS: at(40 * time.Minute), AckOf: "young"}, // session B's ack
	}
	cases := []struct {
		name  string
		self  Self
		inbox string // ids still in the inbox
	}{
		{"session A: neither B's ack nor the older wt's ack waives the young hold", Self{w, "A"}, "first,young"},
		{"session B: its own ack waives it", Self{w, "B"}, "first"},
		{"a token-less shell: same as A", Self{w, SessionNone}, "first,young"},
	}
	for _, tc := range cases {
		if got := strings.Join(ids(Inbox(recs, tc.self, now)), ","); got != tc.inbox {
			t.Errorf("%s: inbox = [%s], want [%s]", tc.name, got, tc.inbox)
		}
		fresh, _ := ActiveHoldsAt(recs, tc.self, "merge-main", now, 24*time.Hour)
		if got := strings.Join(ids(fresh), ","); got != strings.TrimPrefix(strings.TrimPrefix(tc.inbox, "first"), ",") {
			t.Errorf("%s: gate = [%s]", tc.name, got)
		}
	}
}

// #211 review F1, the shape that held: session C's hold in another window, 30h
// old, under hold_max_age=7d; a minute ago an older wt in A's checkout (another
// party: maybe another session there) acked it with no session. That ack must
// waive nothing for A, which never acked — not in the gate, not in the inbox,
// and not when every record comes back through the GitHub mirror.
func TestFreshSessionlessAckWaivesNothing(t *testing.T) {
	const w, other = "/work/repo", "/work/repo-worktrees/roll"
	now := time.Date(2026, 10, 10, 12, 0, 0, 0, time.UTC)
	at := func(d time.Duration) string { return now.Add(-d).Format(time.RFC3339) }
	local := []Record{
		{ID: "h", Kind: KindAnnounce, Window: other, Session: "C", TS: at(30 * time.Hour), Hold: []string{"merge-main"}},
		{ID: "k", Kind: KindAck, Window: w, TS: at(time.Minute), AckOf: "h"},
	}
	var bodies []string
	for _, r := range local {
		bodies = append(bodies, "mirrored\n\n"+MirrorJSONBlock(r))
	}
	viaMirror := MergeByID(nil, ParseMirroredRecords(bodies))
	for name, recs := range map[string][]Record{"local log": local, "mirror only": viaMirror} {
		for _, self := range []Self{{w, "A"}, {w, SessionNone}} {
			fresh, stale := ActiveHoldsAt(recs, self, "merge-main", now, 7*24*time.Hour)
			if strings.Join(ids(fresh), ",") != "h" || len(stale) != 0 {
				t.Errorf("%s, %+v: gate fresh=%v stale=%v — want h to block: nobody in this session acked it", name, self, ids(fresh), ids(stale))
			}
			if got := strings.Join(ids(Inbox(recs, self, now)), ","); got != "h" {
				t.Errorf("%s, %+v: inbox = [%s], want [h]", name, self, got)
			}
		}
	}
	// An ack is written after its announcement, so a session-less ack of a
	// session-stamped hold can never be history. Only a pre-upgrade ack of a
	// pre-upgrade signal still counts.
	hist := []Record{
		{ID: "n", Kind: KindAnnounce, Window: other, TS: at(9 * 24 * time.Hour), Hold: []string{"merge-main"}},
		{ID: "k", Kind: KindAck, Window: w, TS: at(8 * 24 * time.Hour), AckOf: "n"},
		{ID: "s", Kind: KindAnnounce, Window: other, Session: "C", TS: at(7 * 24 * time.Hour)},
	}
	if fresh, stale := ActiveHoldsAt(hist, Self{w, "A"}, "merge-main", now, 0); len(fresh)+len(stale) != 0 {
		t.Errorf("a pre-upgrade ack of a pre-upgrade hold still waives it (M17): fresh=%v stale=%v", ids(fresh), ids(stale))
	}
	if got := Inbox(hist, Self{w, "A"}, now); strings.Join(ids(got), ",") != "s" {
		t.Errorf("inbox = %v, want [s] — the pre-upgrade ack still keeps its old signal out", ids(got))
	}
}

// #211 review F2: the 24h cap is for pre-upgrade history only. A record with no
// session written AFTER the log first carried one is a still-running older wt's
// — maybe another party's live hold — and gets hold_max_age like another
// window's. Pins hold_max_age > 24h and off (M05/M28), and another session's
// hold never takes the cap (M45).
func TestHoldLimitCapsOnlyPreSessionHistory(t *testing.T) {
	const w = "/work/repo"
	now := time.Date(2026, 10, 10, 12, 0, 0, 0, time.UTC)
	at := func(d time.Duration) string { return now.Add(-d).Format(time.RFC3339) }
	recs := []Record{
		{ID: "hist", Kind: KindAnnounce, Window: w, TS: at(30 * time.Hour), Hold: []string{"merge-main"}},
		{ID: "first", Kind: KindAnnounce, Window: "/work/x", Session: "Z", TS: at(28 * time.Hour)},
		{ID: "live", Kind: KindAnnounce, Window: w, TS: at(25 * time.Hour), Hold: []string{"merge-main"}},
		{ID: "other", Kind: KindAnnounce, Window: w, Session: "B", TS: at(25 * time.Hour), Hold: []string{"merge-main"}},
		{ID: "elsewhere", Kind: KindAnnounce, Window: "/work/y", TS: at(25 * time.Hour), Hold: []string{"merge-main"}},
	}
	self := Self{w, "A"}
	cases := []struct {
		maxAge       time.Duration
		fresh, stale string
	}{
		{7 * 24 * time.Hour, "live,other,elsewhere", "hist"}, // only history is capped at 24h
		{0, "live,other,elsewhere", "hist"},                  // hold_max_age off: history still capped, the rest never expire
		{24 * time.Hour, "", "hist,live,other,elsewhere"},    // the default: everyone stale past 24h
		{2 * time.Hour, "", "hist,live,other,elsewhere"},
	}
	for _, tc := range cases {
		fresh, stale := ActiveHoldsAt(recs, self, "merge-main", now, tc.maxAge)
		if strings.Join(ids(fresh), ",") != tc.fresh || strings.Join(ids(stale), ",") != tc.stale {
			t.Errorf("hold_max_age=%v: fresh=%v stale=%v; want fresh=[%s] stale=[%s]", tc.maxAge, ids(fresh), ids(stale), tc.fresh, tc.stale)
		}
	}
	v := ViewOf(recs, self)
	for id, want := range map[string]bool{"hist": true, "live": false, "other": false, "elsewhere": false} {
		r := recs[map[string]int{"hist": 0, "live": 2, "other": 3, "elsewhere": 4}[id]]
		if _, capped := v.HoldLimit(r, 7*24*time.Hour); capped != want {
			t.Errorf("HoldLimit(%s).capped = %v, want %v", id, capped, want)
		}
	}
	// The inbox: history drops out past 24h, a still-running older wt's record
	// and another session's never do (M43).
	if got := strings.Join(ids(Inbox(recs, self, now)), ","); got != "first,live,other,elsewhere" {
		t.Errorf("inbox = [%s], want [first live other elsewhere]", got)
	}
	// A log with no session-stamped record at all (the upgrade, before any
	// session-aware write): every datable record with no session is history, so
	// a month of it neither floods the inbox nor gates past 24h.
	upgrade := []Record{recs[0], recs[2]}
	if fresh, stale := ActiveHoldsAt(upgrade, self, "merge-main", now, 0); len(fresh) != 0 || strings.Join(ids(stale), ",") != "hist,live" {
		t.Errorf("no epoch: fresh=%v stale=%v, want both stale", ids(fresh), ids(stale))
	}
	if got := Inbox(upgrade, self, now); len(got) != 0 {
		t.Errorf("no epoch: inbox = %v, want empty (both past the horizon)", ids(got))
	}
	// An undatable session-stamped record proves nothing about when sessions began.
	if at, ok := SessionEpoch([]Record{{Session: "Z", TS: "garbage"}, {Session: "Y", TS: "2026-10-01T00:00:00Z"}}); !ok || at.Format(time.RFC3339) != "2026-10-01T00:00:00Z" {
		t.Errorf("SessionEpoch = %v %v", at, ok)
	}
	if _, ok := SessionEpoch([]Record{{TS: "2026-10-01T00:00:00Z"}}); ok {
		t.Errorf("a log with no session has no epoch")
	}
}

// The horizon and the cap are both "older than 24h", exactly (M35/M36): at 24h a
// pre-upgrade record still shows and its hold still gates; a second later it
// doesn't.
func TestUnattributedHorizonBoundary(t *testing.T) {
	const w = "/work/repo"
	now := time.Date(2026, 10, 10, 12, 0, 0, 0, time.UTC)
	mk := func(age time.Duration) []Record {
		return []Record{{ID: "h", Kind: KindAnnounce, Window: w, TS: now.Add(-age).Format(time.RFC3339), Hold: []string{"merge-main"}}}
	}
	self := Self{w, "A"}
	if got := Inbox(mk(UnattributedHorizon), self, now); len(got) != 1 {
		t.Errorf("at exactly 24h the record still shows: %v", ids(got))
	}
	if fresh, _ := ActiveHoldsAt(mk(UnattributedHorizon), self, "merge-main", now, 0); len(fresh) != 1 {
		t.Errorf("at exactly 24h the hold still gates")
	}
	if got := Inbox(mk(UnattributedHorizon+time.Second), self, now); len(got) != 0 {
		t.Errorf("a second past 24h it no longer shows: %v", ids(got))
	}
	if fresh, stale := ActiveHoldsAt(mk(UnattributedHorizon+time.Second), self, "merge-main", now, 0); len(fresh) != 0 || len(stale) != 1 {
		t.Errorf("a second past 24h it is stale")
	}
	if n, h := UnattributedBacklog(mk(UnattributedHorizon+time.Second), self, now); n != 0 || h != 1 {
		t.Errorf("backlog = %d/%d, want 0/1", n, h)
	}
}

// What `wt inbox` counts and `wt holds` lists of pre-upgrade records leaves out
// the ones already resolved: all-cleared (M12b, M13b) or waived by a pre-upgrade
// ack (M11b).
func TestUnattributedCountsSkipResolved(t *testing.T) {
	const w = "/work/repo"
	now := time.Date(2026, 10, 10, 12, 0, 0, 0, time.UTC)
	at := func(d time.Duration) string { return now.Add(-d).Format(time.RFC3339) }
	recs := []Record{
		{ID: "open", Kind: KindAnnounce, Window: w, TS: at(5 * 24 * time.Hour)},
		{ID: "cleared", Kind: KindAnnounce, Window: w, TS: at(5 * 24 * time.Hour), Hold: []string{"merge-main"}},
		{ID: "c", Kind: KindAllClear, Window: w, TS: at(4 * 24 * time.Hour), AckOf: "cleared"},
		{ID: "acked", Kind: KindAnnounce, Window: w, TS: at(5 * 24 * time.Hour), Hold: []string{"merge-main"}},
		{ID: "k", Kind: KindAck, Window: w, TS: at(4 * 24 * time.Hour), AckOf: "acked"},
		{ID: "first", Kind: KindAnnounce, Window: "/work/x", Session: "Z", TS: at(3 * 24 * time.Hour)},
	}
	self := Self{w, "A"}
	if n, h := UnattributedBacklog(recs, self, now); n != 1 || h != 0 {
		t.Errorf("backlog = %d notes, %d holds; want 1, 0 (cleared and acked ones are resolved)", n, h)
	}
	if got := strings.Join(ids(UnattributedOpen(recs, self)), ","); got != "open,acked" {
		t.Errorf("UnattributedOpen = [%s], want [open acked] (an all-cleared one is gone)", got)
	}
	if got := strings.Join(ids(NoSessionOpen(recs)), ","); got != "open,acked" {
		t.Errorf("NoSessionOpen = [%s]", got)
	}
	// GatingHolds agrees with the gate for merge-main.
	recs = append(recs, Record{ID: "g", Kind: KindAnnounce, Window: w, Session: "B", TS: at(time.Hour), Hold: []string{"merge-main"}},
		Record{ID: "f", Kind: KindAnnounce, Window: w, Session: "B", TS: at(time.Hour), Hold: []string{"flux"}})
	fresh, _ := ActiveHoldsAt(recs, self, "merge-main", now, 24*time.Hour)
	gating := GatingHolds(recs, self, now, 24*time.Hour)
	if strings.Join(ids(fresh), ",") != "g" || len(gating) != 2 || !gating["g"] || !gating["f"] {
		t.Errorf("GatingHolds = %v, gate fresh = %v", gating, ids(fresh))
	}
}

// `wt prune-coord --legacy` (#211 review): drops only pre-upgrade records that
// gate or show nothing for ANY reader — a plain one past 24h, a hold past both
// the 24h cap and hold_max_age (never with hold_max_age off: other windows still
// gate on it) — with their acks, and nothing it didn't list.
func TestLegacyPrunable(t *testing.T) {
	const w = "/work/repo"
	now := time.Date(2026, 10, 10, 12, 0, 0, 0, time.UTC)
	at := func(d time.Duration) string { return now.Add(-d).Format(time.RFC3339) }
	recs := []Record{
		{ID: "oldnote", Kind: KindAnnounce, Window: w, TS: at(20 * 24 * time.Hour)},
		{ID: "oldhold", Kind: KindAnnounce, Window: w, TS: at(10 * 24 * time.Hour), Hold: []string{"merge-main"}},
		{ID: "k", Kind: KindAck, Window: "/work/y", TS: at(9 * 24 * time.Hour), AckOf: "oldhold"}, // pre-upgrade too (a session-stamped one would move the epoch here)
		{ID: "midhold", Kind: KindAnnounce, Window: w, TS: at(3 * 24 * time.Hour), Hold: []string{"merge-main"}},
		{ID: "youngnote", Kind: KindAnnounce, Window: w, TS: at(20 * time.Hour)},
		{ID: "undated", Kind: KindAnnounce, Window: w, TS: "garbage"},
		{ID: "cleared", Kind: KindAnnounce, Window: w, TS: at(20 * 24 * time.Hour)},
		{ID: "c", Kind: KindAllClear, Window: w, TS: at(19 * 24 * time.Hour), AckOf: "cleared"},
		{ID: "first", Kind: KindAnnounce, Window: "/work/x", Session: "Z", TS: at(15 * time.Hour)},
		{ID: "livelegacy", Kind: KindAnnounce, Window: w, TS: at(14 * time.Hour)},           // after sessions began: maybe live
		{ID: "tagged", Kind: KindAnnounce, Window: w, Session: "B", TS: at(10 * time.Hour)}, // a session's: never legacy
	}
	cases := []struct {
		maxAge time.Duration
		want   string
	}{
		{24 * time.Hour, "oldnote,oldhold,midhold"},
		{7 * 24 * time.Hour, "oldnote,oldhold"}, // midhold still gates other windows for 4 more days
		{0, "oldnote"},                          // hold_max_age off: no hold ever expires for other windows
	}
	for _, tc := range cases {
		if got := strings.Join(ids(LegacyPrunable(recs, now, tc.maxAge)), ","); got != tc.want {
			t.Errorf("hold_max_age=%v: LegacyPrunable = [%s], want [%s]", tc.maxAge, got, tc.want)
		}
	}
	path := filepath.Join(t.TempDir(), "repo.jsonl")
	for _, r := range recs {
		if err := Append(path, r); err != nil {
			t.Fatal(err)
		}
	}
	dry, n, err := PruneLegacy(path, now, 7*24*time.Hour, false)
	if after, _ := Load(path); err != nil || n != 0 || len(dry) != 2 || len(after) != len(recs) {
		t.Fatalf("dry run: cands=%v dropped=%d err=%v, log %d → %d; want 2 candidates and nothing dropped", ids(dry), n, err, len(recs), len(after))
	}
	cands, n, err := PruneLegacy(path, now, 7*24*time.Hour, true)
	after, _ := Load(path)
	if err != nil || strings.Join(ids(cands), ",") != "oldnote,oldhold" || n != 3 || len(after) != len(recs)-3 {
		t.Fatalf("apply: cands=%v dropped=%d err=%v, log now %v", ids(cands), n, err, ids(after))
	}
	for _, r := range after {
		if r.ID == "oldnote" || r.ID == "oldhold" || r.ID == "k" {
			t.Errorf("%s survived the drop", r.ID)
		}
	}
	if again, n, _ := PruneLegacy(path, now, 7*24*time.Hour, true); len(again) != 0 || n != 0 {
		t.Errorf("a second run drops nothing: %v %d", ids(again), n)
	}
}

// A token-less shell owns the token-less records of its own window — the
// directory fallback #211 itself suggests, and #18: a terminal's own hold never
// blocks it, and WT_WINDOW pinning across checkouts works for terminals. The
// accepted cost: two token-less shells in one checkout are one party (WT_SESSION
// per shell separates them). A tagged session still sees a token-less writer as
// another party, and a pre-#163 record (no session at all) stays
// unattributable to the token-less shell too.
func TestTokenlessShellsAreOneParty(t *testing.T) {
	const w = "/work/repo"
	now := time.Date(2026, 10, 10, 12, 0, 0, 0, time.UTC)
	ts := now.Add(-time.Hour).Format(time.RFC3339)
	self := Self{w, SessionNone}
	recs := []Record{
		{ID: "h", Kind: KindAnnounce, Window: w, Session: SessionNone, TS: ts, Hold: []string{"merge-main"}},
		{ID: "legacy", Kind: KindAnnounce, Window: w, TS: ts, Hold: []string{"merge-main"}},
	}
	if got := ids(ActiveHolds(recs, self, "merge-main", now)); strings.Join(got, ",") != "legacy" {
		t.Errorf("token-less self's gate = %v, want only the pre-#163 hold (its own token-less hold is exempt, #18)", got)
	}
	if got := ids(Inbox(recs, self, now)); strings.Join(got, ",") != "legacy" {
		t.Errorf("token-less inbox = %v, want only the pre-#163 hold", got)
	}
	if got := ids(OwnOpenAnnouncements(recs, self)); strings.Join(got, ",") != "h" {
		t.Errorf("token-less own open = %v, want [h]", got)
	}
	// #18 across checkouts: the same token-less shell pinned with WT_WINDOW.
	if got := ActiveHolds([]Record{{ID: "p", Kind: KindAnnounce, Window: "pin", Session: SessionNone, Hold: []string{"merge-main"}}}, Self{"pin", SessionNone}, "merge-main", now); len(got) != 0 {
		t.Errorf("a WT_WINDOW-pinned terminal is blocked by its own hold: %v", ids(got))
	}
	// A tagged session in the same checkout is another party to it, and is gated.
	if got := ids(ActiveHolds(recs, Self{w, "A"}, "merge-main", now)); strings.Join(got, ",") != "h,legacy" {
		t.Errorf("tagged session's gate = %v, want both holds", got)
	}
	// Its acks are its own: they waive for it, and for no tagged session.
	acked := append(recs, Record{ID: "k", Kind: KindAck, Window: w, Session: SessionNone, AckOf: "legacy"})
	if got := ActiveHolds(acked, self, "merge-main", now); len(got) != 0 {
		t.Errorf("after its own ack: gate = %v, want waived", ids(got))
	}
	if got := ids(ActiveHolds(acked, Self{w, "A"}, "merge-main", now)); strings.Join(got, ",") != "h,legacy" {
		t.Errorf("a token-less ack must not waive anything for a tagged session: %v", got)
	}
}

func TestOtherSessions(t *testing.T) {
	now := time.Date(2026, 10, 8, 12, 0, 0, 0, time.UTC)
	at := func(d time.Duration) string { return now.Add(-d).Format(time.RFC3339) }
	const w = "/work/repo"
	recs := []Record{
		{ID: "1", Window: w, Session: "A", TS: at(time.Minute)},             // self → skipped
		{ID: "2", Window: w, TS: at(time.Minute)},                           // legacy → skipped
		{ID: "3", Window: "/work/other", Session: "B", TS: at(time.Minute)}, // other window → skipped
		{ID: "4", Window: w, Session: "B", TS: at(time.Hour)},               // B
		{ID: "5", Window: w, Session: "B", TS: at(5 * time.Minute)},         // B, newer
		{ID: "6", Window: w, Session: "C", TS: at(30 * time.Hour)},          // C, too old for 24h
		{ID: "7", Window: w, Session: "D", TS: "not-a-time"},                // undatable → skipped
		{ID: "8", Window: w, Session: SessionNone, TS: at(2 * time.Hour)},   // a token-less session
	}
	got := OtherSessions(recs, Self{w, "A"}, now, SharedCheckoutWindow)
	if len(got) != 2 || got[0].Session != "B" || got[0].Records != 2 || !got[0].Last.Equal(now.Add(-5*time.Minute)) || got[1].Session != SessionNone {
		t.Fatalf("OtherSessions(24h) = %+v, want [B×2 last 5m, none×1]", got)
	}
	if all := OtherSessions(recs, Self{w, "A"}, now, 0); len(all) != 3 || all[2].Session != "C" {
		t.Errorf("OtherSessions(no limit) = %+v, want B, none, C (newest first)", all)
	}
	if z := OtherSessions(recs, Self{Window: w}, now, 0); z != nil {
		t.Errorf("zero-session self = %+v, want nil (no session to differ from)", z)
	}
}

func TestInboxClearAmbiguous(t *testing.T) {
	now := time.Date(2026, 10, 8, 12, 0, 0, 0, time.UTC)
	recent := now.Add(-time.Hour).Format(time.RFC3339)
	old := now.Add(-48 * time.Hour).Format(time.RFC3339)
	const w = "/work/repo"
	tagged := Record{ID: "t", Window: w, Session: "B", TS: recent}
	cases := []struct {
		name string
		self Self
		recs []Record
		want bool
	}{
		{"token-less self, a tagged session posted here recently → hedge", Self{w, SessionNone}, []Record{tagged}, true},
		{"token-less self, only an OLD tagged record → no permanent hedge", Self{w, SessionNone}, []Record{{Window: w, Session: "B", TS: old}}, false},
		{"token-less self, only token-less/legacy here → no hedge", Self{w, SessionNone}, []Record{{Window: w, Session: SessionNone, TS: recent}, {Window: w, TS: recent}}, false},
		{"token-less self, tagged record in ANOTHER window → no hedge", Self{w, SessionNone}, []Record{{Window: "/other", Session: "B", TS: recent}}, false},
		{"tagged self can tell sessions apart → no hedge", Self{w, "A"}, []Record{tagged}, false},
		{"zero self → no hedge", Self{Window: w}, []Record{tagged}, false},
	}
	for _, tc := range cases {
		if got := InboxClearAmbiguous(tc.recs, tc.self, now); got != tc.want {
			t.Errorf("%s: got %v, want %v", tc.name, got, tc.want)
		}
	}
}

// The shared-checkout warning (wt status / wt doctor) is at most two lines: who,
// then the one remedy that separates the TREE (`wt new`). It must not recommend a
// distinct WT_WINDOW (that splits the identity, not the tree, and would hide the
// other session from the single-worktree per-turn hook), nor all-clear (that
// releases a possibly-live session's hold for every window).
func TestSharedCheckoutWarning(t *testing.T) {
	now := time.Date(2026, 10, 8, 12, 0, 0, 0, time.UTC)
	if got := SharedCheckoutWarning(nil, Self{"/w", "A"}, now); got != "" {
		t.Errorf("no other sessions → %q, want empty", got)
	}
	one := []SessionActivity{{Session: "b0c1d2e3-ffff-4444-8888-000000000000", Records: 2, Last: now.Add(-5 * time.Minute)}}
	two := append(one, SessionActivity{Session: SessionNone, Records: 1, Last: now.Add(-2 * time.Hour)})
	cases := []struct {
		name     string
		others   []SessionActivity
		self     Self
		want     []string
		wantNone []string
	}{
		{"one tagged session", one, Self{"/w", "aaaaaaaa-1111-2222-3333-444444444444"},
			[]string{"another session posted", "latest: session b0c1d2e3…, 5m ago", "you: session aaaaaaaa…", "wt new <branch>", "wt inbox"},
			[]string{"WT_SESSION"}},
		{"two sessions, newest named", two, Self{"/w", "A"},
			[]string{"2 other sessions posted", "latest: session b0c1d2e3…"}, nil},
		{"token-less self is told to set WT_SESSION", one, Self{"/w", SessionNone},
			[]string{"you: no session token", "set WT_SESSION"}, nil},
	}
	for _, tc := range cases {
		msg := SharedCheckoutWarning(tc.others, tc.self, now)
		for _, want := range tc.want {
			if !strings.Contains(msg, want) {
				t.Errorf("%s: missing %q:\n%s", tc.name, want, msg)
			}
		}
		for _, bad := range append([]string{"WT_WINDOW", "all-clear"}, tc.wantNone...) {
			if strings.Contains(msg, bad) {
				t.Errorf("%s: must not mention %q:\n%s", tc.name, bad, msg)
			}
		}
		lines := strings.Split(msg, "\n")
		if len(lines) > 2 {
			t.Errorf("%s: %d lines, want at most 2:\n%s", tc.name, len(lines), msg)
		}
		for _, l := range lines {
			if n := len([]rune(l)); n > 160 {
				t.Errorf("%s: a %d-rune line, want ≤160:\n%s", tc.name, n, l)
			}
		}
	}
}

// A hold earns an all-clear suggestion (which releases it for EVERY window) only
// when it looks orphaned: HoldOrphanAge old, or its own session (window AND
// session) silent for HoldOrphanSilent. Anything else is acked, not cleared.
func TestHoldLooksOrphaned(t *testing.T) {
	now := time.Date(2026, 10, 8, 12, 0, 0, 0, time.UTC)
	at := func(d time.Duration) string { return now.Add(-d).Format(time.RFC3339) }
	const w = "/work/repo"
	hold := func(age time.Duration, session string) Record {
		return Record{ID: "h", Kind: KindAnnounce, Window: w, Session: session, TS: at(age), Hold: []string{"merge-main"}}
	}
	cases := []struct {
		name    string
		h       Record
		recs    []Record
		want    bool
		wantWhy string
	}{
		{"fresh hold, nothing since → live", hold(time.Hour, "B"), nil, false, ""},
		{"5h old, its session silent since → orphaned", hold(5*time.Hour, "B"), nil, true, "its session last wrote 5h ago"},
		{"5h old, its session acked something 10m ago → live", hold(5*time.Hour, "B"),
			[]Record{{ID: "k", Kind: KindAck, Window: w, Session: "B", TS: at(10 * time.Minute)}}, false, ""},
		{"5h old, only ANOTHER session posted since → orphaned", hold(5*time.Hour, "B"),
			[]Record{{ID: "k", Kind: KindAck, Window: w, Session: "C", TS: at(10 * time.Minute)}}, true, "its session last wrote 5h ago"},
		{"5h old, its session posted only from ANOTHER window → orphaned", hold(5*time.Hour, "B"),
			[]Record{{ID: "k", Kind: KindAck, Window: "/elsewhere", Session: "B", TS: at(10 * time.Minute)}}, true, "its session last wrote 5h ago"},
		{"13h old even though its session is active → orphaned by age", hold(13*time.Hour, "B"),
			[]Record{{ID: "k", Kind: KindAck, Window: w, Session: "B", TS: at(10 * time.Minute)}}, true, "placed 13h ago"},
		{"pre-#163 hold, a pre-#163 record from its window 10m ago → live", hold(5*time.Hour, ""),
			[]Record{{ID: "k", Kind: KindAck, Window: w, TS: at(10 * time.Minute)}}, false, ""},
		{"undatable hold → never called orphaned", Record{ID: "h", Window: w, Session: "B", TS: "garbage", Hold: []string{"merge-main"}}, nil, false, ""},
	}
	for _, tc := range cases {
		got, why := HoldLooksOrphaned(append(tc.recs, tc.h), tc.h, now)
		if got != tc.want || why != tc.wantWhy {
			t.Errorf("%s: HoldLooksOrphaned = (%v, %q), want (%v, %q)", tc.name, got, why, tc.want, tc.wantWhy)
		}
	}
}

func TestShortSession(t *testing.T) {
	cases := map[string]string{
		"":                                     "session unknown",
		SessionNone:                            "no session token",
		"B":                                    "session B",
		"agent-review":                         "session agent-review", // a WT_SESSION label stays readable
		"24288609-c37a-43a9-8a82-80e93779324e": "session 24288609…",
		// A mirrored hash shows its hex, not just "sha256:" (which named no session).
		"sha256:61eef8f68895bbdc": "session 61eef8f6 (mirrored)",
	}
	for in, want := range cases {
		if got := ShortSession(in); got != want {
			t.Errorf("ShortSession(%q) = %q, want %q", in, got, want)
		}
	}
	for in, want := range map[string]string{"c1": "c1", "24288609-c37a-43a9-8a82-80e93779324e": "24288609…", "sha256:61eef8f68895bbdc": "61eef8f6 (mirrored)"} {
		if got := ShortToken(in); got != want {
			t.Errorf("ShortToken(%q) = %q, want %q", in, got, want)
		}
	}
}

// The session survives the log round-trip and the GitHub mirror; a pre-#163 line
// (no "session" key) loads with Session == "" — unattributable (#211).
func TestSessionRoundTrip(t *testing.T) {
	path := filepath.Join(t.TempDir(), "repo.jsonl")
	if err := Append(path, Record{ID: "a", Kind: KindAnnounce, Window: "/w", Session: "B"}); err != nil {
		t.Fatal(err)
	}
	f, err := Load(path)
	if err != nil || len(f) != 1 || f[0].Session != "B" {
		t.Fatalf("round-trip = %+v, %v; want session B", f, err)
	}
	writeLines(t, path, `{"id":"old","kind":"announce","window":"/w"}`)
	if old, _ := Load(path); len(old) != 1 || old[0].Session != "" {
		t.Errorf("pre-#163 line = %+v, want empty session", old)
	}
}

// The GitHub mirror never carries the raw session id (a local agent session id;
// the mirror issue may be public) — a stable hash that still separates sessions
// on another machine, and that only the session it was made from owns.
func TestMirrorSessionIsHashed(t *testing.T) {
	const raw = "24288609-c37a-43a9-8a82-80e93779324e"
	block := MirrorJSONBlock(Record{ID: "m", Window: "/w", Session: raw})
	if strings.Contains(block, raw) {
		t.Fatalf("raw session id leaked into the mirror: %s", block)
	}
	back := ParseMirroredRecords([]string{"body\n\n" + block})
	if len(back) != 1 || !strings.HasPrefix(back[0].Session, "sha256:") || back[0].Session != MirrorSession(raw) {
		t.Fatalf("mirror round-trip = %+v, want the stable hash %q", back, MirrorSession(raw))
	}
	if MirrorSession(raw) == MirrorSession("other") || MirrorSession(MirrorSession(raw)) != MirrorSession(raw) {
		t.Errorf("hash must separate sessions and be idempotent")
	}
	for _, passthrough := range []string{"", SessionNone} {
		if got := MirrorSession(passthrough); got != passthrough {
			t.Errorf("MirrorSession(%q) = %q, want unchanged", passthrough, got)
		}
	}
	// A record read back from the mirror belongs to the session it was made from
	// (#18: its creator must not be blocked by it) and to no other session.
	if !(Self{"/w", raw}).Owns(back[0]) {
		t.Errorf("a session must own its own record read back from the mirror")
	}
	for _, other := range []string{"another-session", SessionNone} {
		if (Self{"/w", other}).Owns(back[0]) {
			t.Errorf("session %q must not own another session's mirrored record", other)
		}
	}
}

// #18 across two separate clones that share ONLY the GitHub mirror: different
// main dir names give them different local logs, so the second clone sees the
// hold only as its mirrored (hashed) copy. The session that placed it, pinned to
// the same WT_WINDOW in both, must not be blocked by its own hold there; any
// other session pinned the same way still is. (Before the fix the hashed
// session never matched, and the creator was blocked.)
func TestMirroredOwnHoldAcrossClones(t *testing.T) {
	const pinned = "me" // WT_WINDOW=me in BOTH clones
	const a, b = "aaaaaaaa-1111-2222-3333-444444444444", "bbbbbbbb-5555-6666-7777-888888888888"
	now := time.Date(2026, 10, 8, 12, 0, 0, 0, time.UTC)
	hold := Record{ID: "h", TS: now.Add(-time.Minute).Format(time.RFC3339), Window: pinned, Repo: "cloneone",
		Kind: KindAnnounce, Message: "rolling", Issue: 7, Hold: []string{"merge-main"}, Session: a}
	remote := ParseMirroredRecords([]string{"📣 **wt announce**\n\n" + MirrorJSONBlock(hold)})
	recs := MergeByID(nil, remote) // clone two's own log is empty

	if got := ActiveHolds(recs, Self{pinned, a}, "merge-main", now); len(got) != 0 {
		t.Errorf("the hold's creator is blocked by its own hold in the other clone: %v", ids(got))
	}
	if got := ids(OwnOpenAnnouncements(recs, Self{pinned, a})); strings.Join(got, ",") != "h" {
		t.Errorf("`wt holds` in the other clone = %v, want [h] listed as yours", got)
	}
	if got := OtherSessions(recs, Self{pinned, a}, now, SharedCheckoutWindow); len(got) != 0 {
		t.Errorf("a session's own mirrored records must not warn it about itself: %+v", got)
	}
	for _, other := range []Self{{pinned, b}, {pinned, SessionNone}} {
		if got := ActiveHolds(recs, other, "merge-main", now); len(got) != 1 {
			t.Errorf("%+v: gate = %v, want the hold — it is another party's", other, ids(got))
		}
	}
	// Clone one reads its local raw copy first (MergeByID: local wins) — unchanged.
	if got := ActiveHolds(MergeByID([]Record{hold}, remote), Self{pinned, a}, "merge-main", now); len(got) != 0 {
		t.Errorf("creator blocked in the clone that placed the hold: %v", ids(got))
	}
}
