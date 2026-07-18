package coord

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func writeLines(t *testing.T, path string, lines ...string) {
	t.Helper()
	if err := os.WriteFile(path, []byte(strings.Join(lines, "\n")+"\n"), 0o644); err != nil {
		t.Fatalf("write: %v", err)
	}
}

func ids(recs []Record) []string {
	out := make([]string, len(recs))
	for i, r := range recs {
		out[i] = r.ID
	}
	return out
}

func rec(kind, window, id, ackOf string, hold ...string) Record {
	return Record{ID: id, TS: "2026-07-18T12:00:00Z", Window: window, Repo: "eagle-valley", Kind: kind, AckOf: ackOf, Hold: hold}
}

func TestAppendLoadRoundTrip(t *testing.T) {
	path := filepath.Join(t.TempDir(), "sub", "eagle-valley.jsonl")
	in := []Record{
		{ID: "a1", TS: "2026-07-18T12:00:00Z", Window: "roll-nodes", Repo: "eagle-valley", Kind: KindAnnounce, Message: "AMI roll", Hold: []string{"merge-main"}, Issue: 1072},
		{ID: "k2", TS: "2026-07-18T12:01:00Z", Window: "dep-sweep", Repo: "eagle-valley", Kind: KindAck, AckOf: "a1", State: "read-only + PR #1073 held"},
	}
	for _, r := range in {
		if err := Append(path, r); err != nil {
			t.Fatalf("append: %v", err)
		}
	}
	got, err := Load(path)
	if err != nil {
		t.Fatalf("load: %v", err)
	}
	if len(got) != 2 || got[0].ID != "a1" || got[1].AckOf != "a1" || got[0].Issue != 1072 {
		t.Fatalf("round-trip mismatch: %+v", got)
	}
	if len(got[0].Hold) != 1 || got[0].Hold[0] != "merge-main" {
		t.Fatalf("hold not preserved: %+v", got[0].Hold)
	}
}

func TestLoadMissingFileIsEmpty(t *testing.T) {
	got, err := Load(filepath.Join(t.TempDir(), "nope.jsonl"))
	if err != nil || got != nil {
		t.Fatalf("missing file should be empty/no-error, got %v %v", got, err)
	}
}

func TestLoadSkipsMalformedLines(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "eagle-valley.jsonl")
	// One good line, one garbage line, one good line.
	writeLines(t, path,
		`{"id":"a1","kind":"announce","window":"w1"}`,
		`not json at all`,
		`{"id":"a2","kind":"announce","window":"w2"}`,
		`{"kind":"announce"}`, // no id → skipped
	)
	got, err := Load(path)
	if err != nil {
		t.Fatalf("load: %v", err)
	}
	if len(got) != 2 || got[0].ID != "a1" || got[1].ID != "a2" {
		t.Fatalf("malformed-skip mismatch: %+v", got)
	}
}

func TestInbox(t *testing.T) {
	recs := []Record{
		rec(KindAnnounce, "roll-nodes", "a1", ""),   // from other → should show
		rec(KindAnnounce, "dep-sweep", "self1", ""), // our own → hidden
		rec(KindAnnounce, "other", "a2", ""),        // from other, will be acked → hidden
		rec(KindAck, "dep-sweep", "x", "a2"),        // self acked a2
		rec(KindAnnounce, "other", "a3", ""),        // from other, will be all-cleared → hidden
		rec(KindAllClear, "other", "y", "a3"),
	}
	got := Inbox(recs, "dep-sweep")
	if len(got) != 1 || got[0].ID != "a1" {
		t.Fatalf("inbox should be [a1], got %+v", ids(got))
	}
}

func TestActiveHoldsBlocksUntilAckedOrCleared(t *testing.T) {
	base := []Record{
		{ID: "a1", Kind: KindAnnounce, Window: "roll-nodes", Hold: []string{"merge-main", "flux-reconcile"}},
	}
	// Un-acked hold covering merge-main → blocks.
	if h := ActiveHolds(base, "dep-sweep", "merge-main"); len(h) != 1 {
		t.Fatalf("expected 1 active hold, got %d", len(h))
	}
	// Op not covered → no block.
	if h := ActiveHolds(base, "dep-sweep", "kubectl-mutate:loki"); len(h) != 0 {
		t.Fatalf("uncovered op should not block, got %d", len(h))
	}
	// After self acks → cleared for self.
	acked := append(append([]Record{}, base...), Record{ID: "k", Kind: KindAck, Window: "dep-sweep", AckOf: "a1"})
	if h := ActiveHolds(acked, "dep-sweep", "merge-main"); len(h) != 0 {
		t.Fatalf("acked hold should not block self, got %d", len(h))
	}
	// After all-clear → cleared for everyone.
	clr := append(append([]Record{}, base...), Record{ID: "z", Kind: KindAllClear, Window: "roll-nodes", AckOf: "a1"})
	if h := ActiveHolds(clr, "dep-sweep", "merge-main"); len(h) != 0 {
		t.Fatalf("all-cleared hold should not block, got %d", len(h))
	}
	// A window never blocks on its OWN hold.
	if h := ActiveHolds(base, "roll-nodes", "merge-main"); len(h) != 0 {
		t.Fatalf("self hold should not block self, got %d", len(h))
	}
}

func TestPendingHolds(t *testing.T) {
	recs := []Record{
		rec(KindAnnounce, "roll-nodes", "a1", "", "merge-main"), // other + hold → pending
		rec(KindAnnounce, "docs", "a2", ""),                     // other, no hold → not pending
		rec(KindAnnounce, "self", "s1", "", "merge-main"),       // self → excluded
		rec(KindAnnounce, "other", "a3", "", "flux-reconcile"),  // will be acked → excluded
		rec(KindAck, "self", "k", "a3"),
	}
	got := PendingHolds(recs, "self")
	if len(got) != 1 || got[0].ID != "a1" {
		t.Fatalf("PendingHolds should be [a1], got %+v", ids(got))
	}
}

func TestHoldCovers(t *testing.T) {
	cases := []struct {
		hold []string
		op   string
		want bool
	}{
		{[]string{"merge-main"}, "merge-main", true},
		{[]string{"merge-main"}, "flux-reconcile", false},
		{[]string{"kubectl-mutate"}, "kubectl-mutate:harbor", true}, // family scope
		{[]string{"kubectl-mutate:harbor"}, "kubectl-mutate:loki", false},
		{[]string{"*"}, "anything", true},
		{nil, "merge-main", false},
	}
	for _, c := range cases {
		if got := HoldCovers(c.hold, c.op); got != c.want {
			t.Errorf("HoldCovers(%v, %q) = %v, want %v", c.hold, c.op, got, c.want)
		}
	}
}

func TestNewIDMonotonic(t *testing.T) {
	a := NewID(time.Unix(0, 1))
	b := NewID(time.Unix(0, 2))
	if a == b || len(a) == 0 {
		t.Fatalf("ids should differ and be non-empty: %q %q", a, b)
	}
}

func TestLogPathSlug(t *testing.T) {
	p := LogPath("/home/u", "timoniersystems/eagle-valley")
	if filepath.Base(p) != "timoniersystems-eagle-valley.jsonl" {
		t.Fatalf("unexpected slug path: %s", p)
	}
}
