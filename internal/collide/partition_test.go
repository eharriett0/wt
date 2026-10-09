package collide

import (
	"reflect"
	"testing"
)

// The suppressed windows of an ACTIVE overlap must not ride along into its
// listing and grade (#182): a merged branch whose file equals base has no ranges
// (graded "indeterminate" → HIGH) and a dormant or closed branch's old hunk
// overlaps, so both turned a file `wt check` grades low into a HIGH in
// `wt status`, the MCP wt_status and the per-turn agent banner.
func TestPartitionOverlaps_ActiveListsOnlyLiveWindows(t *testing.T) {
	live := map[string]WindowLiveness{
		"cur":     {Level: LiveDirty},
		"merged":  {Level: LiveStale, MergedPR: "11"},
		"closed":  {Level: LiveClosedPR, ClosedPR: "12"},
		"dormant": {Level: LiveDormant},
		"open":    {Level: LiveOpenPR, PR: "14"},
		"unmrgd":  {Level: LiveUnmerged},
		"unknown": {Level: LiveUnknown},
		// a dirty worktree is NEVER suppressed, even with its commits merged
		// under another name (#168) or far behind base (#87)
		"dirtytip": {Level: LiveDirty, MergedPR: "9", BehindBase: 40},
	}
	ov := []Overlap{
		{File: "doc.md", Windows: []string{"cur", "merged", "open"}},
		{File: "main.tf", Windows: []string{"closed", "cur", "dormant", "unmrgd"}},
		{File: "x.go", Windows: []string{"cur", "dirtytip", "merged"}},
		{File: "y.go", Windows: []string{"ghost", "merged", "unknown"}}, // ghost: no liveness → live
		{File: "z.go", Windows: []string{"cur", "dormant", "merged"}},   // one live → benign
	}
	active, benign := PartitionOverlaps(ov, live)
	want := []Overlap{
		{File: "doc.md", Windows: []string{"cur", "open"}},
		{File: "main.tf", Windows: []string{"cur", "unmrgd"}},
		{File: "x.go", Windows: []string{"cur", "dirtytip"}},
		{File: "y.go", Windows: []string{"ghost", "unknown"}},
	}
	if !reflect.DeepEqual(active, want) {
		t.Errorf("active =\n  %v\nwant\n  %v", active, want)
	}
	// benign overlaps are only counted — returned untouched
	if len(benign) != 1 || !reflect.DeepEqual(benign[0], ov[4]) {
		t.Errorf("benign = %v, want [%v]", benign, ov[4])
	}
	// the input must not be mutated (callers reuse ov)
	if got := ov[0].Windows; !reflect.DeepEqual(got, []string{"cur", "merged", "open"}) {
		t.Errorf("input overlap mutated: %v", got)
	}
}

func TestPartitionOverlapsFor(t *testing.T) {
	live := map[string]WindowLiveness{
		"merged":  {Level: LiveStale, MergedPR: "11"},
		"closed":  {Level: LiveClosedPR, ClosedPR: "12"},
		"dormant": {Level: LiveDormant},
		"open":    {Level: LiveOpenPR, PR: "14"},
		"dirty":   {Level: LiveDirty},
		"unmrgd":  {Level: LiveUnmerged},
	}
	cur := Self{Label: "cur"} // hand-built overlaps carry no worktrees: matched by label
	cases := []struct {
		name   string
		self   Self
		live   map[string]WindowLiveness
		ov     Overlap
		active []string // nil → benign
	}{
		{"participant + one live other → listed", cur, live,
			Overlap{File: "f", Windows: []string{"cur", "open"}}, []string{"cur", "open"}},
		{"participant + only suppressed others → nothing to say", cur, live,
			Overlap{File: "f", Windows: []string{"closed", "cur", "dormant", "merged"}}, nil},
		{"participant: suppressed others dropped, live kept", cur, live,
			Overlap{File: "f", Windows: []string{"cur", "merged", "open", "dormant"}}, []string{"cur", "open"}},
		{"participant: a dirty other is never suppressed", cur, live,
			Overlap{File: "f", Windows: []string{"cur", "dirty", "merged"}}, []string{"cur", "dirty"}},
		{"participant: missing liveness counts as live", cur, live,
			Overlap{File: "f", Windows: []string{"cur", "ghost"}}, []string{"cur", "ghost"}},
		// `wt check` never consults the CURRENT window's liveness: a window still
		// editing after its own PR merged must still see who it collides with.
		// PartitionOverlaps counted it and dropped the overlap (1 live).
		{"participant: self's own suppressed liveness is ignored", cur,
			map[string]WindowLiveness{"cur": {Level: LiveStale, MergedPR: "3"}, "open": {Level: LiveOpenPR}},
			Overlap{File: "f", Windows: []string{"cur", "open"}}, []string{"cur", "open"}},
		{"non-participant: two live others → heads-up", cur, live,
			Overlap{File: "f", Windows: []string{"open", "unmrgd"}}, []string{"open", "unmrgd"}},
		{"non-participant: one live + suppressed → nothing", cur, live,
			Overlap{File: "f", Windows: []string{"dormant", "open"}}, nil},
		{"non-participant: suppressed dropped from the listing", cur, live,
			Overlap{File: "f", Windows: []string{"merged", "open", "unmrgd"}}, []string{"open", "unmrgd"}},
		{"self unknown → exactly PartitionOverlaps", Self{}, live,
			Overlap{File: "f", Windows: []string{"cur", "merged", "open"}}, []string{"cur", "open"}},
		{"self unknown, one live → benign", Self{}, live,
			Overlap{File: "f", Windows: []string{"merged", "open"}}, nil},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			active, benign := PartitionOverlapsFor([]Overlap{c.ov}, c.live, c.self)
			if c.active == nil {
				if len(active) != 0 || len(benign) != 1 {
					t.Fatalf("want benign, got active=%v benign=%v", active, benign)
				}
				return
			}
			if len(active) != 1 || len(benign) != 0 {
				t.Fatalf("want active, got active=%v benign=%v", active, benign)
			}
			if active[0].File != c.ov.File || !reflect.DeepEqual(active[0].Windows, c.active) {
				t.Errorf("active = %v, want windows %v", active[0], c.active)
			}
		})
	}
}

// Filtering an overlap keeps its worktrees aligned with its labels: grading
// tells windows apart by worktree (#182).
func TestPartitionOverlaps_KeepsWorktreesAligned(t *testing.T) {
	live := map[string]WindowLiveness{"merged": {Level: LiveStale}, "a": {Level: LiveDirty}}
	ov := []Overlap{{File: "f", Windows: []string{"a", "merged", "z"}, Worktrees: []string{"/w/a", "/w/m", "/w/z"}}}
	active, _ := PartitionOverlaps(ov, live)
	want := []Overlap{{File: "f", Windows: []string{"a", "z"}, Worktrees: []string{"/w/a", "/w/z"}}}
	if !reflect.DeepEqual(active, want) {
		t.Errorf("active = %#v, want %#v", active, want)
	}
}

// A label is not an identity (#182): two worktrees that claimed one issue are
// both "#77". PartitionOverlapsFor finds self by WORKTREE, so a window that
// only shares self's label is another window, never folded into self.
func TestPartitionOverlapsFor_ByWorktree(t *testing.T) {
	me := Self{Label: "#77", Worktree: "/w/a"}
	cases := []struct {
		name   string
		ov     Overlap
		active []string // worktrees kept; nil → benign
	}{
		// was benign: both "#77" entries counted as self, so 0 others
		{"self + another window sharing its label → that window is an other",
			Overlap{File: "f", Windows: []string{"#77", "#77"}, Worktrees: []string{"/w/a", "/w/b"}},
			[]string{"/w/a", "/w/b"}},
		// was "participant": the #77 there is the OTHER window, not self
		{"self not editing, a namesake + one more are → a heads-up, not ours",
			Overlap{File: "f", Windows: []string{"#77", "c"}, Worktrees: []string{"/w/b", "/w/c"}},
			[]string{"/w/b", "/w/c"}},
		{"self not editing, only a namesake is → nothing",
			Overlap{File: "f", Windows: []string{"#77", "merged"}, Worktrees: []string{"/w/b", "/w/m"}},
			nil},
	}
	live := map[string]WindowLiveness{"merged": {Level: LiveStale}} // "#77" unclassified → live
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			active, benign := PartitionOverlapsFor([]Overlap{c.ov}, live, me)
			if c.active == nil {
				if len(active) != 0 || len(benign) != 1 {
					t.Fatalf("want benign, got active=%v benign=%v", active, benign)
				}
				return
			}
			if len(active) != 1 || !reflect.DeepEqual(active[0].Worktrees, c.active) || len(active[0].Windows) != len(c.active) {
				t.Fatalf("active = %#v, want worktrees %v", active, c.active)
			}
		})
	}
}

func TestSelf(t *testing.T) {
	ws := []Window{
		{Issue: "77", Branch: "fix-77-a", Worktree: "/w/a"},
		{Issue: "77", Branch: "fix-77-b", Worktree: "/w/b"},
	}
	if got := SelfFor(ws, "/w/b"); got != (Self{Label: "#77", Worktree: "/w/b"}) {
		t.Errorf("SelfFor(/w/b) = %+v", got)
	}
	if got := SelfFor(ws, "/w/nope"); !got.IsZero() {
		t.Errorf("SelfFor(unknown) = %+v, want zero", got)
	}
	with := Overlap{File: "f", Windows: []string{"#77", "#77"}, Worktrees: []string{"/w/a", "/w/b"}}
	without := Overlap{File: "f", Windows: []string{"#77", "c"}}
	me := Self{Label: "#77", Worktree: "/w/b"}
	for _, tc := range []struct {
		o    Overlap
		i    int
		self Self
		want bool
	}{
		{with, 0, me, false}, // same label, other worktree
		{with, 1, me, true},
		{without, 0, me, true}, // no worktrees to go by: the label decides
		{without, 1, me, false},
		{with, 1, Self{}, false}, // the zero Self is nobody
	} {
		if got := tc.self.Is(tc.o, tc.i); got != tc.want {
			t.Errorf("%+v.Is(%v, %d) = %v, want %v", tc.self, tc.o.Worktrees, tc.i, got, tc.want)
		}
	}
}

// ClassifyWindows keys liveness by label, and two windows can share one (#182).
// Folding their answers must never let one window's suppression hide the other.
func TestKeepLeastSuppressed(t *testing.T) {
	merged := WindowLiveness{Level: LiveStale, MergedPR: "1"}
	dormant := WindowLiveness{Level: LiveDormant}
	dirty := WindowLiveness{Level: LiveDirty}
	open := WindowLiveness{Level: LiveOpenPR, PR: "2"}
	for _, tc := range []struct {
		name string
		held WindowLiveness
		ok   bool
		next WindowLiveness
		want WindowLiveness
	}{
		{"first answer", WindowLiveness{}, false, merged, merged},
		{"suppressed, then live → live", merged, true, dirty, dirty},
		{"live, then suppressed → live", dirty, true, merged, dirty},
		{"both live → the first stands", open, true, dirty, open},
		{"both suppressed → the first stands", merged, true, dormant, merged},
	} {
		if got := keepLeastSuppressed(tc.held, tc.ok, tc.next); got != tc.want {
			t.Errorf("%s: got %+v, want %+v", tc.name, got, tc.want)
		}
	}
}
