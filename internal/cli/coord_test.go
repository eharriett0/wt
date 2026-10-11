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
// window id, so rendering the raw id reads as "you". It must be named as what it
// is — and (#211 review) a record with no session says whether it is pre-upgrade
// history or a still-running wt without session support, and this session's own
// token from another checkout reads as yours, placed from there.
func TestPostedBy(t *testing.T) {
	now := time.Date(2026, 10, 10, 12, 0, 0, 0, time.UTC)
	at := func(d time.Duration) string { return now.Add(-d).Format(time.RFC3339) }
	self := coord.Self{Window: "/work/repo", Session: "A"}
	// The log first carried a session 10h ago: a record with no session from
	// before then is history, one from after it a still-running older wt's.
	v := coord.ViewOf([]coord.Record{{ID: "first", Window: "/work/other", Session: "Z", TS: at(10 * time.Hour)}}, self)
	cases := []struct {
		rec  coord.Record
		want string
	}{
		{coord.Record{Window: "/work/repo", Session: "24288609-c37a-43a9-8a82-80e93779324e"}, "same checkout, another session (session 24288609…)"},
		{coord.Record{Window: "/work/repo", Session: coord.SessionNone}, "same checkout, another session (no session token)"},
		// Your own record must never be labelled as another session's (review
		// finding: cmdAck did) — and a record with no session is NOT yours (#211).
		{coord.Record{Window: "/work/repo", Session: "A"}, "this window (yours)"},
		{coord.Record{Window: "/work/repo", TS: at(20 * time.Hour)}, "same checkout, can't tell whose (written before wt recorded sessions)"},
		{coord.Record{Window: "/work/repo", TS: at(time.Hour)}, "same checkout, can't tell whose (written by a wt without session support)"},
		{coord.Record{Window: "/work/repo", TS: "garbage"}, "same checkout, can't tell whose (written by a wt without session support)"},
		{coord.Record{Window: "/work/repo-worktrees/roll", Session: "R"}, "/work/repo-worktrees/roll"},
		{coord.Record{Window: "/work/repo-worktrees/roll", Session: "A"}, "yours (this session's token), placed from checkout /work/repo-worktrees/roll"},
		{coord.Record{Window: "/work/repo-worktrees/roll", Session: coord.MirrorSession("A")}, "yours (this session's token), placed from checkout /work/repo-worktrees/roll"},
	}
	for _, tc := range cases {
		if got := postedBy(tc.rec, v); got != tc.want {
			t.Errorf("postedBy(%+v) = %q, want %q", tc.rec, got, tc.want)
		}
	}
	// A token-less shell's token-less record is its own only by the directory
	// fallback, and says so; another checkout's token-less record proves nothing.
	tokenless := coord.ViewOf(nil, coord.Self{Window: "/work/repo", Session: coord.SessionNone})
	if got := postedBy(coord.Record{Window: "/work/repo", Session: coord.SessionNone}, tokenless); got != "this window (yours by the directory fallback: no session token, so wt can't prove it)" {
		t.Errorf("token-less pair label = %q", got)
	}
	if got := postedBy(coord.Record{Window: "/work/repo-worktrees/roll", Session: coord.SessionNone}, tokenless); got != "/work/repo-worktrees/roll" {
		t.Errorf("token-less reader, another checkout's token-less record = %q", got)
	}
	// A View not built by ViewOf calls nothing history: the safe wording.
	if got := postedBy(coord.Record{Window: "/work/repo", TS: at(20 * time.Hour)}, coord.View{Self: self}); got != "same checkout, can't tell whose (written by a wt without session support)" {
		t.Errorf("zero-epoch view label = %q", got)
	}
}

// #211: who a record belongs to, as the all-clear guard names it.
func TestWhoseRecord(t *testing.T) {
	now := time.Date(2026, 10, 10, 12, 0, 0, 0, time.UTC)
	at := func(d time.Duration) string { return now.Add(-d).Format(time.RFC3339) }
	self := coord.Self{Window: "/w", Session: "aaaaaaaa-1111-2222-3333-444444444444"}
	v := coord.ViewOf([]coord.Record{{Window: "/w", Session: self.Session, TS: at(10 * time.Hour)}}, self)
	cases := []struct {
		rec  coord.Record
		want string
	}{
		{coord.Record{Window: "/w", Session: self.Session}, "yours"},
		{coord.Record{Window: "/w", Session: "bbbbbbbb-5555-6666-7777-888888888888"}, "another session's, in this checkout (session bbbbbbbb…)"},
		{coord.Record{Window: "/w", TS: at(20 * time.Hour)}, "unattributable (written before wt recorded sessions)"},
		{coord.Record{Window: "/w", TS: at(time.Hour)}, "unattributable (written by a wt without session support)"},
		{coord.Record{Window: "/elsewhere", Session: "C"}, "another window's (/elsewhere)"},
		{coord.Record{Window: "/elsewhere", Session: self.Session}, "yours, placed from checkout /elsewhere"},
	}
	for _, tc := range cases {
		if got := whoseRecord(tc.rec, v); got != tc.want {
			t.Errorf("whoseRecord(%+v) = %q, want %q", tc.rec, got, tc.want)
		}
	}
	tokenless := coord.ViewOf(nil, coord.Self{Window: "/w", Session: coord.SessionNone})
	if got := whoseRecord(coord.Record{Window: "/w", Session: coord.SessionNone}, tokenless); got != "yours only by the directory fallback (no session token, like this shell)" {
		t.Errorf("directory fallback = %q", got)
	}
}

// The all-clear guard (#211): an all-clear releases a hold for EVERY window, so
// only a record carrying this session's own token (or its mirror hash) is
// released without a word — from any checkout. Anything else, the token-less
// directory fallback included (wt counts it as yours but can't prove it), is
// asked about on a terminal, refused otherwise, and released only with
// --not-mine. Before, `wt all-clear` released any id silently — #211's operator
// nearly released another live session's hold that `wt holds` had listed as theirs.
func TestAllClearVerdict(t *testing.T) {
	self := coord.Self{Window: "/w", Session: "A"}
	tokenlessSelf := coord.Self{Window: "/w", Session: coord.SessionNone}
	mine := coord.Record{Window: "/w", Session: "A"}
	mirroredMine := coord.Record{Window: "/w", Session: coord.MirrorSession("A")}
	mineElsewhere := coord.Record{Window: "/elsewhere", Session: "A"}
	mirroredElsewhere := coord.Record{Window: "/elsewhere", Session: coord.MirrorSession("A")}
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
		{"mine, placed from another checkout → post (F5)", self, mineElsewhere, false, false, clearPost},
		{"mine, mirrored, from another checkout → post", self, mirroredElsewhere, false, false, clearPost},
		{"another session's, no terminal → refuse", self, another, false, false, clearRefuse},
		{"another session's, terminal → ask", self, another, false, true, clearAsk},
		{"another session's, --not-mine → post", self, another, true, false, clearPost},
		{"pre-#163 (unattributable), no terminal → refuse", self, legacy, false, false, clearRefuse},
		{"pre-#163, terminal → ask", self, legacy, false, true, clearAsk},
		{"another window's, no terminal → refuse", self, otherWin, false, false, clearRefuse},
		{"token-less shell, its directory-fallback record, no terminal → refuse (can't prove it)", tokenlessSelf, tokenless, false, false, clearRefuse},
		{"token-less shell, its directory-fallback record, terminal → ask", tokenlessSelf, tokenless, false, true, clearAsk},
		{"token-less shell, its directory-fallback record, --not-mine → post", tokenlessSelf, tokenless, true, false, clearPost},
		{"token-less shell, another checkout's token-less record → refuse", tokenlessSelf, coord.Record{Window: "/elsewhere", Session: coord.SessionNone}, false, false, clearRefuse},
		{"token-less shell, a pre-#163 record → refuse", tokenlessSelf, legacy, false, false, clearRefuse},
		{"token-less shell, a tagged session's record → refuse", tokenlessSelf, another, false, false, clearRefuse},
	}
	for _, tc := range cases {
		if got := allClearVerdict(tc.self, tc.rec, tc.notMine, tc.tty); got != tc.want {
			t.Errorf("%s: allClearVerdict = %v, want %v", tc.name, got, tc.want)
		}
	}
}

// The prompt's default is no (#211 review, M22): Enter, EOF and anything but
// y/yes leave the hold standing.
func TestConfirmAllClearDefaultsToNo(t *testing.T) {
	for in, want := range map[string]bool{"\n": false, "": false, "n\n": false, "nope\n": false, " \n": false,
		"y\n": true, "Y\n": true, "yes\n": true, " YES \n": true, "y": true} {
		var got bool
		captureOutputForTest(t, func() { got = confirmAllClear("h1", "is another session's", strings.NewReader(in)) })
		if got != want {
			t.Errorf("confirmAllClear(%q) = %v, want %v", in, got, want)
		}
	}
}

// `wt holds` lists, of the announcements in this checkout wt can't attribute
// (#211): EVERY hold that still gates (never folded behind the count, #211
// review F4), the newest holds that no longer gate (capped), and the plain ones
// still a live signal; older ones are only counted, and the fold line says how
// many holds still gate.
func TestUnattributedView(t *testing.T) {
	now := time.Date(2026, 10, 10, 12, 0, 0, 0, time.UTC)
	at := func(d time.Duration) string { return now.Add(-d).Format(time.RFC3339) }
	self := coord.Self{Window: "/w", Session: "A"}
	// sessions began here 3 days ago
	v := coord.ViewOf([]coord.Record{{Window: "/w", Session: "A", TS: at(72 * time.Hour)}}, self)
	open := []coord.Record{{ID: "g0", Window: "/w", TS: at(50 * 24 * time.Hour), Hold: []string{"merge-main"}}} // gates (say hold_max_age is off for it)
	for i := 0; i < unattributedShown+3; i++ {                                                                  // oldest first, as the log is
		open = append(open, coord.Record{ID: fmt.Sprintf("h%02d", i), Window: "/w", TS: at(time.Duration(40-i) * 24 * time.Hour), Hold: []string{"merge-main"}})
	}
	open = append(open,
		coord.Record{ID: "oldnote", Window: "/w", TS: at(5 * 24 * time.Hour)},
		coord.Record{ID: "livenote", Window: "/w", TS: at(48 * time.Hour)}, // after sessions began: a still-running older wt's
		coord.Record{ID: "freshnote", Window: "/w", TS: at(time.Hour)},
	)
	gating := map[string]bool{"g0": true}
	show, olderHolds, olderNotes, live := unattributedView(open, gating, v, now)
	got := strings.Join(recIDs(show), ",")
	want := "freshnote,livenote,h12,h11,h10,h09,h08,h07,h06,h05,h04,h03,g0"
	if got != want || olderHolds != 3 || olderNotes != 1 || live != 1 {
		t.Errorf("unattributedView = [%s] (+%d holds, +%d notes, %d live); want [%s] +3 holds, +1 note, 1 live", got, olderHolds, olderNotes, live, want)
	}
	// What the section prints for a hold says why it gates or not, and the fold
	// line counts the ones that still gate.
	out, _ := captureOutputForTest(t, func() { unattributedHoldsSection(open, gating, v, now, 7*24*time.Hour) })
	for _, s := range []string{"g0", "(still blocks merge-pr)", "stale: past 24h, the limit for a hold wt can't attribute",
		"+ 3 older hold(s) that no longer gate anything and 1 plain announcement(s) older than 24h, not listed — 1 hold(s) here still gate, all listed above",
		"written by a wt without session support"} {
		if !strings.Contains(out, s) {
			t.Errorf("section missing %q:\n%s", s, out)
		}
	}
	if strings.Contains(out, "oldnote") || strings.Contains(out, "all-clear: wt all-clear") {
		t.Errorf("a plain announcement past the horizon is counted, and no all-clear line is offered:\n%s", out)
	}
	// holdStatus: the limit that expired a hold is named; an acked one says so.
	h10h := coord.Record{ID: "x", Window: "/w", TS: at(10 * time.Hour), Hold: []string{"merge-main"}}
	if s := holdStatus(h10h, nil, v, now, 2*time.Hour); s != "stale: past hold_max_age (2h); no longer gates" {
		t.Errorf("past hold_max_age: %q", s)
	}
	if s := holdStatus(h10h, nil, v, now, 24*time.Hour); s != "acked by you; no longer gates you" {
		t.Errorf("not gating, within its limit: %q", s)
	}
	if s := holdStatus(coord.Record{ID: "f", Window: "/w", Hold: []string{"flux"}}, map[string]bool{"f": true}, v, now, 0); s != "still live" {
		t.Errorf("a gating hold on another op: %q", s)
	}
	if backlogLine(0, 0, 0, 0) != "" || !strings.Contains(backlogLine(4, 1, 0, 0), "+ 5 older announcement(s) in this checkout (1 with a hold)") ||
		!strings.Contains(backlogLine(4, 1, 7, 7), "`wt holds` lists them") || !strings.Contains(backlogLine(4, 1, 9, 7), "`wt holds --issue 9` lists them") {
		t.Errorf("backlogLine = %q / %q", backlogLine(4, 1, 7, 7), backlogLine(4, 1, 9, 7))
	}
	shown, more := newestHolds(open[:4], 2)
	if strings.Join(recIDs(shown), ",") != "h01,h02" || more != 2 {
		t.Errorf("newestHolds = %v +%d, want [h01 h02] +2", recIDs(shown), more)
	}
	for d, want := range map[time.Duration]string{0: "off", 24 * time.Hour: "24h", 48 * time.Hour: "2d", 7 * 24 * time.Hour: "7d", 6 * time.Hour: "6h", 90 * time.Minute: "90m", 30 * time.Second: "30s"} {
		if got := fmtLimit(d); got != want {
			t.Errorf("fmtLimit(%v) = %q, want %q", d, got, want)
		}
	}
}

// #211 review F8: a record whose age can't be measured never reads "just now".
func TestRecordAge(t *testing.T) {
	now := time.Date(2026, 10, 10, 12, 0, 0, 0, time.UTC)
	cases := map[string]string{
		"garbage": "undated",
		"":        "undated",
		now.Add(3 * 24 * time.Hour).Format(time.RFC3339): "dated 3d ahead",
		now.Add(30 * time.Second).Format(time.RFC3339):   "just now", // within a minute: clock noise
		now.Add(-2 * time.Hour).Format(time.RFC3339):     "2h ago",
	}
	for ts, want := range cases {
		if got := recordAge(coord.Record{TS: ts}, now); got != want {
			t.Errorf("recordAge(%q) = %q, want %q", ts, got, want)
		}
	}
	if n := undatedNote(coord.Record{ID: "u", TS: "garbage"}, now); !strings.Contains(n, "never ages out (blocking is the safe direction)") || !strings.Contains(n, "wt all-clear u") {
		t.Errorf("undated note = %q", n)
	}
	if n := undatedNote(coord.Record{ID: "f", TS: now.Add(48 * time.Hour).Format(time.RFC3339)}, now); !strings.Contains(n, "is dated 2d ahead of this clock") || !strings.Contains(n, "keeps gating") {
		t.Errorf("future note = %q", n)
	}
	if n := undatedNote(coord.Record{TS: now.Add(-time.Hour).Format(time.RFC3339)}, now); n != "" {
		t.Errorf("a dated hold gets no note: %q", n)
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
// orphaned, one carrying this session's own token from another checkout (F5),
// or one whose age can't be measured (F8), and always says "EVERY window": the
// gate used to hand any same-checkout reader — a plain terminal next to a live
// session — a global release. A same-checkout holder adds the /clear explanation.
func TestHoldAdvice(t *testing.T) {
	now := time.Date(2026, 10, 8, 12, 0, 0, 0, time.UTC)
	at := func(d time.Duration) string { return now.Add(-d).Format(time.RFC3339) }
	self := coord.Self{Window: "/w", Session: "A"}
	liveSame := coord.Record{ID: "live", Window: "/w", Session: "B", TS: at(5 * time.Minute), Hold: []string{"merge-main"}}
	liveOther := coord.Record{ID: "other", Window: "/elsewhere", Session: "C", TS: at(5 * time.Minute), Hold: []string{"merge-main"}}
	silent := coord.Record{ID: "silent", Window: "/w", Session: "D", TS: at(5 * time.Hour), Hold: []string{"merge-main"}}
	old := coord.Record{ID: "old", Window: "/elsewhere", Session: "E", TS: at(13 * time.Hour), Hold: []string{"merge-main"}}
	mineElsewhere := coord.Record{ID: "mine", Window: "/elsewhere", Session: "A", TS: at(5 * time.Minute), Hold: []string{"merge-main"}}
	undated := coord.Record{ID: "undated", Window: "/elsewhere", Session: "F", TS: "garbage", Hold: []string{"merge-main"}}
	future := coord.Record{ID: "future", Window: "/elsewhere", Session: "G", TS: now.Add(48 * time.Hour).Format(time.RFC3339), Hold: []string{"merge-main"}}
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
		{"this session's own hold from another checkout: named, all-clear offered", []coord.Record{mineElsewhere}, []string{"mine"}, false},
		{"an undatable hold: says why it keeps gating, all-clear offered", []coord.Record{undated}, []string{"undated"}, false},
		{"a future-dated hold: says why it keeps gating, no all-clear", []coord.Record{future}, nil, false},
	}
	for _, tc := range cases {
		lines := holdAdvice(tc.holds, tc.holds, coord.ViewOf(tc.holds, self), now)
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
	// #211: an unattributable holder gets the can't-tell-whose note (it may be
	// yours: ack it); a holder wt can attribute does not.
	unattr := coord.Record{ID: "legacy", Window: "/w", TS: at(time.Hour), Hold: []string{"merge-main"}}
	if all := strings.Join(holdAdvice([]coord.Record{unattr}, nil, coord.ViewOf(nil, self), now), "\n"); !strings.Contains(all, "can't tell whose") {
		t.Errorf("an unattributable holder must get the can't-tell-whose note:\n%s", all)
	}
	if all := strings.Join(holdAdvice([]coord.Record{liveSame, liveOther}, nil, coord.ViewOf(nil, self), now), "\n"); strings.Contains(all, "can't tell whose") {
		t.Errorf("attributable holders must not get the can't-tell-whose note:\n%s", all)
	}
	if all := strings.Join(holdAdvice([]coord.Record{mineElsewhere}, nil, coord.ViewOf(nil, self), now), "\n"); !strings.Contains(all, "carries this session's own token, placed from checkout /elsewhere") {
		t.Errorf("own hold from another checkout must be named:\n%s", all)
	}
	if all := strings.Join(holdAdvice([]coord.Record{future}, nil, coord.ViewOf(nil, self), now), "\n"); !strings.Contains(all, "dated 2d ahead of this clock") {
		t.Errorf("future-dated hold note missing:\n%s", all)
	}
}

// Every record wt writes carries the writer's window AND session (#163). A
// record without the session is unattributable (#211): dropping the stamp makes
// a writer's own records unreleasable without --not-mine and its own holds gate
// it.
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
	got := labelForContext(box, coord.ViewOf(box, self))
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
		{ID: "mine-there", Window: "/work/repo-worktrees/x", Session: "A"},         // this session's token, another checkout: a plain note stays out
	}
	v := coord.ViewOf(box, self)
	got := hookInbox(box, v, false)
	if strings.Join(recIDs(got), ",") != "same,held,pinned,legacy" {
		t.Fatalf("single worktree: hookInbox = %v, want [same held pinned legacy] (other windows' plain notes stay out, their holds never; this checkout's unattributable ones in)", recIDs(got))
	}
	if got[0].Window != "same checkout, another session (session B)" || got[1].Window != "/work/repo-worktrees/removed-long-ago" ||
		got[3].Window != "same checkout, can't tell whose (written by a wt without session support)" {
		t.Errorf("labels = %q, %q, %q", got[0].Window, got[1].Window, got[3].Window)
	}
	if got := hookInbox(box, v, true); len(got) != 6 {
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
	if !strings.Contains(gateErr, fmt.Sprintf("%d merge-main hold(s) older than hold_max_age (24h) — NOT blocking", staleHoldsShown+2)) || !strings.Contains(gateErr, "…and 2 older") ||
		strings.Contains(gateErr, "crashed") {
		t.Errorf("the gate must count every stale hold but list only the newest %d, naming the limit and guessing nothing:\n%s", staleHoldsShown, gateErr)
	}
	// hold_max_age 7d: the same pre-upgrade holds stop gating at the 24h cap, and
	// the warning says so — not "past hold_max_age" (#211 review F2).
	c.HoldMaxAge = 7 * 24 * time.Hour
	_, gateErr = captureOutputForTest(t, func() { code = mergeCoordGate(c) })
	if code != 1 || !strings.Contains(gateErr, fmt.Sprintf("%d merge-main hold(s) written before wt recorded sessions, older than 24h — NOT blocking: wt can't tell whose they are, so they stop gating at 24h (hold_max_age: 7d)", staleHoldsShown+2)) ||
		strings.Contains(gateErr, "older than hold_max_age") {
		t.Errorf("hold_max_age=7d: the capped holds must be named as such:\n%s", gateErr)
	}
	c.HoldMaxAge = 24 * time.Hour
	// inbox --json (M26/M27): a record wt can't attribute is flagged same_checkout
	// with attribution "unattributable", another session's "another_session".
	jsonOut, _ := captureOutputForTest(t, func() { cmdInbox([]string{"--json"}) })
	var entries []map[string]any
	if err := json.Unmarshal([]byte(jsonOut), &entries); err != nil {
		t.Fatalf("inbox --json: %v\n%s", err, jsonOut)
	}
	attr := map[string]string{}
	for _, e := range entries {
		if e["same_checkout"] == true {
			attr[e["id"].(string)], _ = e["attribution"].(string)
		}
	}
	if attr["legacyhold"] != "unattributable" || attr["bhold"] != "another_session" {
		t.Errorf("inbox --json attribution = %v, want legacyhold unattributable, bhold another_session", attr)
	}
	// F5: this session's own hold placed from another checkout is released without
	// asking; usage errors (M23) exit 64 and write nothing.
	elsewhere := coord.Record{ID: "elsewhere", Kind: coord.KindAnnounce, Repo: "repo", Window: "/some/other/checkout", Session: self.Session,
		TS: at(time.Minute), Hold: []string{"merge-main"}}
	if err := coord.Append(logPath, elsewhere); err != nil {
		t.Fatal(err)
	}
	if code := cmdAllClear([]string{"bhold", "legacyhold"}); code != 64 {
		t.Errorf("two ids = exit %d, want 64", code)
	}
	if code := cmdAllClear([]string{"elsewhere"}); code != 0 {
		t.Errorf("all-clear of this session's own hold from another checkout = exit %d, want 0", code)
	}
	if got := clears(); strings.Join(got, ",") != "elsewhere" {
		t.Fatalf("all-clears so far = %v, want [elsewhere]", got)
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
	// announce --clear forwards --not-mine (M24b).
	b2 := coord.Record{ID: "bhold2", Kind: coord.KindAnnounce, Repo: "repo", Window: self.Window, Session: "bbbbbbbb-5555-6666-7777-888888888888",
		TS: at(time.Minute), Hold: []string{"merge-main"}}
	if err := coord.Append(logPath, b2); err != nil {
		t.Fatal(err)
	}
	if code := cmdAnnounce([]string{"--clear", "bhold2", "--not-mine"}); code != 0 {
		t.Errorf("non-TTY `announce --clear <id> --not-mine` = exit %d, want 0", code)
	}
	if got := strings.Join(clears(), ","); got != "elsewhere,bhold,legacyhold,minehold,bhold2" {
		t.Errorf("all-clears written = %s, want elsewhere (own, other checkout), bhold (asked, y), legacyhold (--not-mine), minehold (own), bhold2 (announce --clear --not-mine)", got)
	}
}

// coordWiringRepo is a temp repo + HOME for the coordination wiring tests: the
// config, the log path and this process's identity, with the session env set to
// session ("" = a token-less shell).
func coordWiringRepo(t *testing.T, session string) (*config.Config, string, coord.Self) {
	t.Helper()
	repo, home := t.TempDir(), t.TempDir()
	gitInitForTest(t, repo)
	t.Setenv("HOME", home)
	for _, k := range []string{"WT_SESSION", "CODEX_SESSION_ID", "WT_WINDOW", "WT_COORD_ISSUE"} {
		t.Setenv(k, "")
	}
	t.Setenv("CLAUDE_CODE_SESSION_ID", session)
	t.Chdir(repo)
	c, err := config.Load()
	if err != nil {
		t.Fatal(err)
	}
	logPath, self := coordCtx(c)
	return c, logPath, self
}

// `wt inbox` with nothing to show still says the pre-upgrade backlog exists, in
// the plain and the --json (stderr) paths (M30/M31).
func TestInboxBacklogWiring(t *testing.T) {
	_, logPath, self := coordWiringRepo(t, "aaaaaaaa-1111-2222-3333-444444444444")
	now := time.Now().UTC()
	for _, r := range []coord.Record{
		{ID: "oldnote", Window: self.Window, TS: now.Add(-5 * 24 * time.Hour).Format(time.RFC3339), Message: "pre-upgrade note"},
		{ID: "first", Window: self.Window, Session: self.Session, TS: now.Add(-time.Hour).Format(time.RFC3339), Message: "mine"},
	} {
		r.Kind, r.Repo = coord.KindAnnounce, "repo"
		if err := coord.Append(logPath, r); err != nil {
			t.Fatal(err)
		}
	}
	const want = "+ 1 older announcement(s) in this checkout (0 with a hold)"
	out, _ := captureOutputForTest(t, func() { cmdInbox(nil) })
	if !strings.Contains(out, "inbox clear") || !strings.Contains(out, want) {
		t.Errorf("empty inbox must still count the backlog:\n%s", out)
	}
	jsonOut, jsonErr := captureOutputForTest(t, func() { cmdInbox([]string{"--json"}) })
	if strings.TrimSpace(jsonOut) != "[]" || !strings.Contains(jsonErr, want) {
		t.Errorf("--json: stdout %q, stderr must carry the backlog:\n%s", jsonOut, jsonErr)
	}
}

// A token-less shell (#211 review F3): its own token-less hold is "yours" only by
// the directory fallback — exempt from its gate (#18) and listed by `wt holds`
// with a caveat — but `wt all-clear` won't release it without a terminal or
// --not-mine, and says how to make it provable.
func TestDirectoryFallbackWiring(t *testing.T) {
	c, logPath, self := coordWiringRepo(t, "")
	if self.Session != coord.SessionNone {
		t.Fatalf("token-less self = %+v", self)
	}
	h := coord.Record{ID: "tl", Kind: coord.KindAnnounce, Repo: "repo", Window: self.Window, Session: coord.SessionNone,
		TS: time.Now().UTC().Add(-time.Minute).Format(time.RFC3339), Hold: []string{"merge-main"}, Message: "terminal hold"}
	if err := coord.Append(logPath, h); err != nil {
		t.Fatal(err)
	}
	var code int
	_, gateErr := captureOutputForTest(t, func() { code = mergeCoordGate(c) })
	if code != 0 {
		t.Errorf("a terminal is blocked by its own hold (#18):\n%s", gateErr)
	}
	out, errOut := captureOutputForTest(t, func() { cmdHolds(nil) })
	if !strings.Contains(out, "all-clear: wt all-clear tl") || !strings.Contains(out, "(no session token: see below)") ||
		!strings.Contains(errOut, "1 of these carry no session token, like this shell") || !strings.Contains(errOut, "WT_SESSION") {
		t.Errorf("`wt holds` must list the fallback record as yours, with the caveat:\n%s\n%s", out, errOut)
	}
	defer func(old func() bool) { confirmInteractive = old }(confirmInteractive)
	confirmInteractive = func() bool { return false }
	refOut, refErr := captureOutputForTest(t, func() { code = cmdAllClear([]string{"tl"}) })
	refusal := refErr + refOut
	if code != 1 || !strings.Contains(refusal, "can't prove it") || !strings.Contains(refusal, "set WT_SESSION") || !strings.Contains(refusal, "--not-mine") {
		t.Errorf("non-TTY all-clear of a fallback record = %d, want 1 with the why and the fix:\n%s", code, refusal)
	}
	if code := cmdAllClear([]string{"tl", "--not-mine"}); code != 0 {
		t.Errorf("--not-mine = exit %d, want 0", code)
	}
}

// `wt prune-coord --legacy` lists, and drops only with -y (#211 review).
func TestPruneLegacyWiring(t *testing.T) {
	_, logPath, self := coordWiringRepo(t, "aaaaaaaa-1111-2222-3333-444444444444")
	now := time.Now().UTC()
	for _, r := range []coord.Record{
		{ID: "oldhold", Window: self.Window, TS: now.Add(-10 * 24 * time.Hour).Format(time.RFC3339), Hold: []string{"merge-main"}, Message: "pre-upgrade hold"},
		{ID: "younghold", Window: self.Window, TS: now.Add(-2 * time.Hour).Format(time.RFC3339), Hold: []string{"merge-main"}, Message: "pre-upgrade, young"},
		{ID: "first", Window: self.Window, Session: self.Session, TS: now.Add(-time.Hour).Format(time.RFC3339), Message: "mine"},
	} {
		r.Kind, r.Repo = coord.KindAnnounce, "repo"
		if err := coord.Append(logPath, r); err != nil {
			t.Fatal(err)
		}
	}
	if code := cmdPruneCoord([]string{"-y"}); code != 64 {
		t.Errorf("-y without --legacy = exit %d, want 64", code)
	}
	dry, _ := captureOutputForTest(t, func() { cmdPruneCoord([]string{"--legacy"}) })
	if !strings.Contains(dry, "oldhold") || strings.Contains(dry, "younghold") || !strings.Contains(dry, "dry run — nothing dropped") {
		t.Errorf("--legacy must list only what nothing needs, and drop nothing:\n%s", dry)
	}
	if recs, _ := coord.Load(logPath); len(recs) != 3 {
		t.Fatalf("a dry run changed the log: %d records", len(recs))
	}
	if code := cmdPruneCoord([]string{"--legacy", "-y"}); code != 0 {
		t.Fatalf("--legacy -y = exit %d", code)
	}
	recs, _ := coord.Load(logPath)
	if got := strings.Join(recIDs(recs), ","); got != "younghold,first" {
		t.Errorf("after --legacy -y the log = [%s], want [younghold first]", got)
	}
	plain, _ := captureOutputForTest(t, func() { cmdPruneCoord(nil) })
	if strings.Contains(plain, "--legacy") {
		t.Errorf("nothing left to suggest:\n%s", plain)
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
