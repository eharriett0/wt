package coord

import (
	"testing"
	"time"
)

func TestMirrorRoundTrip(t *testing.T) {
	r := Record{ID: "abc", TS: "2026-07-30T12:00:00Z", Window: "winA", Repo: "wt",
		Kind: KindAnnounce, Message: "rolling flux\nsecond line", Hold: []string{"merge-main"}, Issue: 42}
	body := "📣 **wt announce** — window `winA`\n\n" + MirrorJSONBlock(r)
	got := ParseMirroredRecords([]string{body})
	if len(got) != 1 {
		t.Fatalf("round-trip parsed %d records, want 1", len(got))
	}
	g := got[0]
	if g.ID != r.ID || g.Kind != r.Kind || g.Message != r.Message || len(g.Hold) != 1 || g.Hold[0] != "merge-main" || g.Issue != 42 {
		t.Fatalf("round-trip mismatch: %+v", g)
	}
}

func TestParseMirroredRecords_SkipsNonRecords(t *testing.T) {
	bodies := []string{
		"just a human comment, no block",
		"```wt-record\n{bad json\n```",       // malformed → skipped
		"```wt-record\n{\"kind\":\"x\"}\n```", // no ID → skipped
		"prefix\n```wt-record\n{\"id\":\"z\",\"kind\":\"ack\",\"ack_of\":\"abc\"}\n```\nsuffix",
	}
	got := ParseMirroredRecords(bodies)
	if len(got) != 1 || got[0].ID != "z" || got[0].AckOf != "abc" {
		t.Fatalf("want one valid record z, got %+v", got)
	}
}

func TestMergeByID(t *testing.T) {
	local := []Record{{ID: "a", Kind: KindAnnounce}, {ID: "b", Kind: KindAck}}
	remote := []Record{{ID: "b", Kind: KindAck}, {ID: "c", Kind: KindAllClear}} // b dup, c new
	got := MergeByID(local, remote)
	if len(got) != 3 {
		t.Fatalf("merge len %d, want 3 (a,b,c)", len(got))
	}
	if got[0].ID != "a" || got[1].ID != "b" || got[2].ID != "c" {
		t.Fatalf("order = %s,%s,%s; want a,b,c", got[0].ID, got[1].ID, got[2].ID)
	}
}

// Cross-machine: a remote all-clear (parsed from the mirror) clears a locally
// unseen hold once merged — the whole point of #36.
func TestMergeByID_RemoteAllClearClearsHold(t *testing.T) {
	local := []Record{{ID: "h1", Kind: KindAnnounce, Window: "remoteWin", Hold: []string{"merge-main"}}}
	remote := []Record{{ID: "c1", Kind: KindAllClear, AckOf: "h1", Window: "remoteWin"}}
	merged := MergeByID(local, remote)
	fresh, stale := ActiveHoldsAt(merged, "myWin", "merge-main", time.Time{}, 0)
	if len(fresh) != 0 || len(stale) != 0 {
		t.Fatalf("remote all-clear should clear the hold: fresh=%d stale=%d", len(fresh), len(stale))
	}
}
