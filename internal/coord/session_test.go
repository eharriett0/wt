package coord

import (
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// #163: two agent sessions started in ONE checkout resolve to the same window id
// (path-based, deliberately kept — #18/#156), so the window alone can't tell them
// apart. Self.Owns is the ONE ownership predicate that adds the session.
func TestSelfOwns(t *testing.T) {
	const w, other = "/x/repo", "/x/other"
	cases := []struct {
		name string
		self Self
		rec  Record
		want bool
	}{
		{"same window, same session", Self{w, "A"}, Record{Window: w, Session: "A"}, true},
		{"same window, DIFFERENT session — another party in this checkout", Self{w, "A"}, Record{Window: w, Session: "B"}, false},
		{"pre-#163 record (no session) is a wildcard — exactly today's behaviour", Self{w, "A"}, Record{Window: w}, true},
		{"zero-session self (no session known) is a wildcard", Self{Window: w}, Record{Window: w, Session: "B"}, true},
		{"neither side has a session", Self{Window: w}, Record{Window: w}, true},
		{"token-less self vs token-less record: indistinguishable → same", Self{w, SessionNone}, Record{Window: w, Session: SessionNone}, true},
		{"token-less self vs tagged record → another party", Self{w, SessionNone}, Record{Window: w, Session: "B"}, false},
		{"tagged self vs token-less record → another party", Self{w, "A"}, Record{Window: w, Session: SessionNone}, false},
		{"other window, same session → not own", Self{w, "A"}, Record{Window: other, Session: "A"}, false},
		{"other window, no session → not own", Self{w, "A"}, Record{Window: other}, false},
		{"other window, zero self → not own", Self{Window: w}, Record{Window: other}, false},
		// The GitHub mirror carries MirrorSession(token): self still owns its own
		// record read back from there (#18 across clones that share only the mirror).
		{"own record read back from the mirror (hashed) → own", Self{w, "A"}, Record{Window: w, Session: MirrorSession("A")}, true},
		{"another session's mirrored record → another party", Self{w, "A"}, Record{Window: w, Session: MirrorSession("B")}, false},
		{"token-less self never claims a tagged session's mirrored record", Self{w, SessionNone}, Record{Window: w, Session: MirrorSession("A")}, false},
		{"own mirrored record from another window → not own", Self{w, "A"}, Record{Window: other, Session: MirrorSession("A")}, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := tc.self.Owns(tc.rec); got != tc.want {
				t.Errorf("%+v.Owns(%+v) = %v, want %v", tc.self, tc.rec, got, tc.want)
			}
		})
	}
}

// SharesCheckout = same window AND not owned: the one source for every
// "same checkout, another session" label, so a label can't contradict Owns.
func TestSharesCheckout(t *testing.T) {
	const w = "/x/repo"
	cases := []struct {
		name string
		self Self
		rec  Record
		want bool
	}{
		{"another session here", Self{w, "A"}, Record{Window: w, Session: "B"}, true},
		{"token-less session here vs tagged self", Self{w, "A"}, Record{Window: w, Session: SessionNone}, true},
		{"own session", Self{w, "A"}, Record{Window: w, Session: "A"}, false},
		{"pre-#163 record here is owned, not another session", Self{w, "A"}, Record{Window: w}, false},
		{"another window is not 'this checkout'", Self{w, "A"}, Record{Window: "/x/other", Session: "B"}, false},
	}
	for _, tc := range cases {
		if got := tc.self.SharesCheckout(tc.rec); got != tc.want {
			t.Errorf("%s: SharesCheckout = %v, want %v", tc.name, got, tc.want)
		}
		if tc.want && tc.self.Owns(tc.rec) {
			t.Errorf("%s: a record can't be both another session's and owned", tc.name)
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
	// Two terminal tabs of one human are one (token-less) session, as before.
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
	recs := []Record{
		{ID: "n1", Kind: KindAnnounce, Window: w, Session: "B", Message: "moving prod tags"},
		{ID: "h1", Kind: KindAnnounce, Window: w, Session: "B", Message: "pushing rebuilds", Hold: []string{"merge-main"}},
	}
	if got := ids(Inbox(recs, a)); strings.Join(got, ",") != "n1,h1" {
		t.Errorf("A's inbox = %v, want [n1 h1] — another session's announcements must reach A", got)
	}
	if got := ids(PendingHolds(recs, a)); strings.Join(got, ",") != "h1" {
		t.Errorf("A's pending holds = %v, want [h1]", got)
	}
	if got := ActiveHolds(recs, a, "merge-main"); len(got) != 1 || got[0].ID != "h1" {
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
	if got := Inbox(recs, b); len(got) != 0 {
		t.Errorf("B's inbox = %v, want empty (you don't ack yourself)", ids(got))
	}
	if got := ActiveHolds(recs, b, "merge-main"); len(got) != 0 {
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
	if got := ids(Inbox(recs, a)); strings.Join(got, ",") != "h1" {
		t.Errorf("after A acks n1, A's inbox = %v, want [h1]", got)
	}
}

// Another session's ack is not yours (#163): it read the announcement, you did
// not. Before the fix an ack from ANY session in the checkout cleared it for all.
func TestAckedByIsPerSession(t *testing.T) {
	const w = "/work/repo"
	a, b := Self{w, "A"}, Self{w, "B"}
	recs := []Record{
		{ID: "x", Kind: KindAnnounce, Window: "/work/repo-worktrees/roll", Session: "R", Hold: []string{"merge-main"}},
		{ID: "k", Kind: KindAck, Window: w, Session: "B", AckOf: "x"}, // B acked it
	}
	if got := Inbox(recs, b); len(got) != 0 {
		t.Errorf("B acked x, B's inbox = %v, want empty", ids(got))
	}
	if got := ids(Inbox(recs, a)); strings.Join(got, ",") != "x" {
		t.Errorf("A's inbox = %v, want [x] — B's ack must not clear it for A", got)
	}
	if got := ActiveHolds(recs, a, "merge-main"); len(got) != 1 {
		t.Errorf("A's gate = %v, want [x] — B's ack must not waive the hold for A", ids(got))
	}
	if got := ActiveHolds(recs, b, "merge-main"); len(got) != 0 {
		t.Errorf("B acked the hold, B's gate = %v, want none", ids(got))
	}
}

// Back-compat: records written before sessions existed (no "session" field, as
// every pre-#163 log line and every older wt binary writes them) keep EXACTLY
// today's window-only behaviour, whatever session reads them.
func TestPreSessionRecordsKeepWindowBehaviour(t *testing.T) {
	const w = "/work/repo"
	legacy := []Record{
		{ID: "mine", Kind: KindAnnounce, Window: w, Hold: []string{"merge-main"}},
		{ID: "theirs", Kind: KindAnnounce, Window: "/work/other", Hold: []string{"merge-main"}},
		{ID: "ack", Kind: KindAck, Window: w, AckOf: "theirs"},
	}
	for _, self := range []Self{{w, "A"}, {w, SessionNone}, {Window: w}} {
		if got := Inbox(legacy, self); len(got) != 0 {
			t.Errorf("%+v: inbox = %v, want empty (own legacy announce excluded, legacy ack counts)", self, ids(got))
		}
		if got := ActiveHolds(legacy, self, "merge-main"); len(got) != 0 {
			t.Errorf("%+v: gate = %v, want none (#18 own-hold exemption preserved)", self, ids(got))
		}
		if got := ids(OwnOpenAnnouncements(legacy, self)); strings.Join(got, ",") != "mine" {
			t.Errorf("%+v: own open = %v, want [mine]", self, got)
		}
		if got := OtherSessions(legacy, self, time.Now(), 0); len(got) != 0 {
			t.Errorf("%+v: OtherSessions = %+v, want none (a legacy record names no party)", self, got)
		}
		if InboxClearAmbiguous(legacy, self, time.Now()) {
			t.Errorf("%+v: legacy-only log must not hedge the inbox", self)
		}
	}
}

// A session that has no token at all (SessionNone) still keeps the old behaviour
// with OTHER token-less writers — they are indistinguishable, as before.
func TestTokenlessSessionsStayIndistinguishable(t *testing.T) {
	const w = "/work/repo"
	self := Self{w, SessionNone}
	recs := []Record{{ID: "h", Kind: KindAnnounce, Window: w, Session: SessionNone, Hold: []string{"merge-main"}}}
	if got := ActiveHolds(recs, self, "merge-main"); len(got) != 0 {
		t.Errorf("token-less self vs token-less hold = %v, want exempt (unchanged)", ids(got))
	}
	if got := Inbox(recs, self); len(got) != 0 {
		t.Errorf("token-less inbox = %v, want empty (unchanged)", ids(got))
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
// (no "session" key) loads with Session == "" — the back-compat wildcard.
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

	if got := ActiveHolds(recs, Self{pinned, a}, "merge-main"); len(got) != 0 {
		t.Errorf("the hold's creator is blocked by its own hold in the other clone: %v", ids(got))
	}
	if got := ids(OwnOpenAnnouncements(recs, Self{pinned, a})); strings.Join(got, ",") != "h" {
		t.Errorf("`wt holds` in the other clone = %v, want [h] listed as yours", got)
	}
	if got := OtherSessions(recs, Self{pinned, a}, now, SharedCheckoutWindow); len(got) != 0 {
		t.Errorf("a session's own mirrored records must not warn it about itself: %+v", got)
	}
	for _, other := range []Self{{pinned, b}, {pinned, SessionNone}} {
		if got := ActiveHolds(recs, other, "merge-main"); len(got) != 1 {
			t.Errorf("%+v: gate = %v, want the hold — it is another party's", other, ids(got))
		}
	}
	// Clone one reads its local raw copy first (MergeByID: local wins) — unchanged.
	if got := ActiveHolds(MergeByID([]Record{hold}, remote), Self{pinned, a}, "merge-main"); len(got) != 0 {
		t.Errorf("creator blocked in the clone that placed the hold: %v", ids(got))
	}
}
