package collide

import (
	"reflect"
	"testing"
)

func TestLabel(t *testing.T) {
	if got := (Window{Issue: "42", Branch: "feat-42-x", Worktree: "/w/x"}).Label(); got != "#42" {
		t.Errorf("issue label = %q, want #42", got)
	}
	if got := (Window{Branch: "feat-42-x", Worktree: "/w/x"}).Label(); got != "feat-42-x" {
		t.Errorf("branch label = %q", got)
	}
	if got := (Window{Branch: "HEAD", Worktree: "/w/detached"}).Label(); got != "detached" {
		t.Errorf("detached label = %q, want basename", got)
	}
}

func TestOverlaps(t *testing.T) {
	ws := []Window{
		{Issue: "1", Branch: "feat-1", Worktree: "/w/1", Touched: []string{"a.go", "b.go"}},
		{Issue: "2", Branch: "feat-2", Worktree: "/w/2", Touched: []string{"b.go", "c.go"}},
		{Branch: "feat-3", Worktree: "/w/3", Touched: []string{"c.go", "d.go"}}, // unclaimed window
	}
	got := Overlaps(ws)
	want := []Overlap{
		{File: "b.go", Windows: []string{"#1", "#2"}},
		{File: "c.go", Windows: []string{"#2", "feat-3"}},
	}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("Overlaps =\n%#v\nwant\n%#v", got, want)
	}
}

func TestOverlaps_NoneWhenDisjoint(t *testing.T) {
	ws := []Window{
		{Issue: "1", Worktree: "/w/1", Touched: []string{"a.go"}},
		{Issue: "2", Worktree: "/w/2", Touched: []string{"b.go"}},
	}
	if got := Overlaps(ws); len(got) != 0 {
		t.Errorf("disjoint windows should have no overlap, got %v", got)
	}
}

func TestOverlaps_DedupWithinWindow(t *testing.T) {
	// A file listed twice within one window must not self-overlap.
	ws := []Window{{Issue: "1", Worktree: "/w/1", Touched: []string{"a.go", "a.go"}}}
	if got := Overlaps(ws); len(got) != 0 {
		t.Errorf("intra-window dup must not count as overlap, got %v", got)
	}
}

func TestCheckPaths(t *testing.T) {
	ws := []Window{
		{Issue: "1", Worktree: "/w/1", Touched: []string{"internal/foo.go", "main.go"}},
		{Issue: "2", Worktree: "/w/2", Touched: []string{"internal/bar.go"}},
	}
	// From window 2, about to edit internal/foo.go + README.md.
	got := CheckPaths(ws, "/w/2", []string{"internal/foo.go", "README.md"})
	want := []Conflict{{Path: "internal/foo.go", Window: "#1"}}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("CheckPaths =\n%#v\nwant\n%#v", got, want)
	}
}

func TestCheckPaths_BasenameMatch(t *testing.T) {
	ws := []Window{
		{Issue: "1", Worktree: "/w/1", Touched: []string{"internal/foo.go"}},
		{Issue: "2", Worktree: "/w/2", Touched: nil},
	}
	got := CheckPaths(ws, "/w/2", []string{"foo.go"})
	if len(got) != 1 || got[0].Window != "#1" {
		t.Errorf("basename should match a touched path, got %v", got)
	}
}

func TestCheckPaths_ExcludesOwnWorktree(t *testing.T) {
	ws := []Window{{Issue: "1", Worktree: "/w/1", Touched: []string{"a.go"}}}
	if got := CheckPaths(ws, "/w/1", []string{"a.go"}); len(got) != 0 {
		t.Errorf("own worktree must be excluded, got %v", got)
	}
}

func TestClassifyFacts(t *testing.T) {
	cases := []struct {
		name string
		in   LiveFacts
		want Liveness
	}{
		{"open PR wins over everything", LiveFacts{HasOpenPR: true, Dirty: true, Unshipped: 5}, LiveOpenPR},
		{"dirty worktree, no PR", LiveFacts{Dirty: true, Unshipped: 0}, LiveDirty},
		{"unmerged commits, clean, no PR", LiveFacts{Unshipped: 3}, LiveUnmerged},
		{"fully merged, clean, no PR → stale", LiveFacts{Unshipped: 0}, LiveStale},
		{"uncomputable unshipped, no other signal → unknown", LiveFacts{Unshipped: -1}, LiveUnknown},
		{"dirty beats uncomputable", LiveFacts{Dirty: true, Unshipped: -1}, LiveDirty},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if got := ClassifyFacts(c.in); got != c.want {
				t.Errorf("ClassifyFacts(%+v) = %v, want %v", c.in, got, c.want)
			}
		})
	}
}

func TestLiveness_IsStale(t *testing.T) {
	// Only LiveStale is suppressed; unknown is surfaced (never hide on ambiguity).
	for l, wantStale := range map[Liveness]bool{
		LiveUnknown:  false,
		LiveStale:    true,
		LiveUnmerged: false,
		LiveDirty:    false,
		LiveOpenPR:   false,
	} {
		if got := l.IsStale(); got != wantStale {
			t.Errorf("%v.IsStale() = %v, want %v", l, got, wantStale)
		}
	}
}

func TestPartitionConflicts(t *testing.T) {
	cs := []Conflict{
		{Path: "a.go", Window: "#1"},   // open PR → active
		{Path: "b.go", Window: "#2"},   // stale → suppressed
		{Path: "c.go", Window: "gone"}, // missing from map → active (ambiguity is not suppressed)
	}
	live := map[string]WindowLiveness{
		"#1": {Level: LiveOpenPR, PR: "747"},
		"#2": {Level: LiveStale},
	}
	active, stale := PartitionConflicts(cs, live)
	if len(active) != 2 || active[0].Window != "#1" || active[1].Window != "gone" {
		t.Errorf("active = %#v, want #1 + gone", active)
	}
	if len(stale) != 1 || stale[0].Window != "#2" {
		t.Errorf("stale = %#v, want #2", stale)
	}
}

func TestPartitionOverlaps(t *testing.T) {
	ov := []Overlap{
		{File: "live.go", Windows: []string{"#1", "#2"}},     // 2 live → active
		{File: "onelive.go", Windows: []string{"#1", "#3"}},  // 1 live + 1 stale → benign
		{File: "allstale.go", Windows: []string{"#3", "#4"}}, // 0 live → benign
		{File: "ambig.go", Windows: []string{"#1", "ghost"}}, // live + unknown(missing) → active
	}
	live := map[string]WindowLiveness{
		"#1": {Level: LiveDirty},
		"#2": {Level: LiveOpenPR},
		"#3": {Level: LiveStale},
		"#4": {Level: LiveStale},
	}
	active, benign := PartitionOverlaps(ov, live)
	gotActive := map[string]bool{}
	for _, o := range active {
		gotActive[o.File] = true
	}
	if !gotActive["live.go"] || !gotActive["ambig.go"] || len(active) != 2 {
		t.Errorf("active overlaps = %#v, want live.go + ambig.go", active)
	}
	if len(benign) != 2 {
		t.Errorf("benign overlaps = %#v, want onelive.go + allstale.go", benign)
	}
}
