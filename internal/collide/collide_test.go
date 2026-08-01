package collide

import (
	"reflect"
	"testing"
	"time"

	"github.com/eharriett0/wt/internal/gitx"
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
	want := []Conflict{{Path: "internal/foo.go", Window: "#1", MatchedFile: "internal/foo.go"}}
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
	day := 24 * time.Hour
	cases := []struct {
		name   string
		in     LiveFacts
		maxAge time.Duration
		want   Liveness
	}{
		{"open PR wins over everything", LiveFacts{HasOpenPR: true, Dirty: true, Unshipped: 5}, 0, LiveOpenPR},
		{"dirty worktree, no PR", LiveFacts{Dirty: true, Unshipped: 0}, 0, LiveDirty},
		{"unmerged commits, clean, no PR", LiveFacts{Unshipped: 3}, 0, LiveUnmerged},
		{"fully merged, clean, no PR → stale", LiveFacts{Unshipped: 0}, 0, LiveStale},
		{"uncomputable unshipped, no other signal → unknown", LiveFacts{Unshipped: -1}, 0, LiveUnknown},
		{"dirty beats uncomputable", LiveFacts{Dirty: true, Unshipped: -1}, 0, LiveDirty},
		{"unmerged + old + maxAge set + PR-checked → dormant", LiveFacts{Unshipped: 3, Age: 5 * day, PRChecked: true}, 3 * day, LiveDormant},
		{"unmerged + old + maxAge off → unmerged", LiveFacts{Unshipped: 3, Age: 5 * day, PRChecked: true}, 0, LiveUnmerged},
		{"unmerged + recent → unmerged despite maxAge", LiveFacts{Unshipped: 3, Age: 1 * day, PRChecked: true}, 3 * day, LiveUnmerged},
		{"unmerged + old but gh NOT checked → unmerged (never suppress on unknown PR)", LiveFacts{Unshipped: 3, Age: 5 * day, PRChecked: false}, 3 * day, LiveUnmerged},
		{"dirty is never dormant", LiveFacts{Dirty: true, Unshipped: 3, Age: 30 * day, PRChecked: true}, day, LiveDirty},
		{"open PR is never dormant", LiveFacts{HasOpenPR: true, Unshipped: 3, Age: 30 * day, PRChecked: true}, day, LiveOpenPR},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if got := ClassifyFacts(c.in, c.maxAge); got != c.want {
				t.Errorf("ClassifyFacts(%+v, %v) = %v, want %v", c.in, c.maxAge, got, c.want)
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

func TestIsSharedDoc(t *testing.T) {
	shared := []string{"CLAUDE.md", "MEMORY.md"}
	cases := []struct {
		path string
		want bool
	}{
		{"CLAUDE.md", true},
		{"infrastructure/CLAUDE.md", true}, // basename match through a path
		{"MEMORY.md", true},
		{".claude/memory/MEMORY.md", true},
		{"internal/cli/cli.go", false},
		{"claude.md", false}, // case-sensitive basename
		{"README.md", false},
		{"  CLAUDE.md  ", true}, // trimmed
	}
	for _, c := range cases {
		if got := IsSharedDoc(c.path, shared); got != c.want {
			t.Errorf("IsSharedDoc(%q) = %v, want %v", c.path, got, c.want)
		}
	}
	// Empty shared list → soft-list disabled → always false.
	if IsSharedDoc("CLAUDE.md", nil) {
		t.Error("IsSharedDoc with nil shared list should be false (soft-list off)")
	}
}

func lr(s, e int) gitx.LineRange { return gitx.LineRange{Start: s, End: e} }

func TestOverlappingSpans(t *testing.T) {
	a := []gitx.LineRange{lr(10, 20), lr(50, 55)}
	b := []gitx.LineRange{lr(18, 30), lr(100, 100)}
	got := OverlappingSpans(a, b)
	if len(got) != 1 || got[0] != lr(18, 20) {
		t.Errorf("OverlappingSpans = %+v, want [{18 20}]", got)
	}
	if s := OverlappingSpans([]gitx.LineRange{lr(1, 5)}, []gitx.LineRange{lr(6, 9)}); len(s) != 0 {
		t.Errorf("disjoint ranges should not overlap, got %+v", s)
	}
}

func TestConflictSeverity(t *testing.T) {
	over := []gitx.LineRange{lr(10, 20)}
	near := []gitx.LineRange{lr(15, 25)}
	far := []gitx.LineRange{lr(90, 95)}
	if ConflictSeverity(over, far, false) != SevFYI {
		t.Error("disjoint ranges should be SevFYI")
	}
	if ConflictSeverity(over, near, false) != SevHigh {
		t.Error("overlapping ranges should be SevHigh")
	}
	if ConflictSeverity(nil, far, false) != SevHigh {
		t.Error("indeterminate (no current ranges) should be SevHigh (safe)")
	}
	if ConflictSeverity(over, near, true) != SevFYI {
		t.Error("append-only should force SevFYI even when ranges overlap")
	}
}

func TestOverlapSeverity(t *testing.T) {
	disjoint := [][]gitx.LineRange{{lr(1, 5)}, {lr(10, 15)}, {lr(20, 25)}}
	if OverlapSeverity(disjoint, false) != SevFYI {
		t.Error("all-disjoint windows should be SevFYI")
	}
	overlapping := [][]gitx.LineRange{{lr(1, 5)}, {lr(4, 9)}}
	if OverlapSeverity(overlapping, false) != SevHigh {
		t.Error("overlapping windows should be SevHigh")
	}
	if OverlapSeverity(overlapping, true) != SevFYI {
		t.Error("append-only forces SevFYI")
	}
	// Indeterminate: a participating window with no computable ranges (untracked
	// / binary / diff error) can't be proven disjoint → SevHigh (fail-safe).
	indeterminate := [][]gitx.LineRange{{lr(1, 5)}, {}}
	if OverlapSeverity(indeterminate, false) != SevHigh {
		t.Error("a window with empty ranges should force SevHigh (indeterminate)")
	}
	if OverlapSeverity(indeterminate, true) != SevFYI {
		t.Error("append-only still forces SevFYI even when indeterminate")
	}
}

func TestIsAppendOnly(t *testing.T) {
	globs := []string{"*.log", "envs/*/inventory.yaml", "CHANGELOG.md"}
	cases := map[string]bool{
		"build.log":                true,  // *.log basename
		"deep/nested/build.log":    true,  // *.log basename match
		"envs/prod/inventory.yaml": true,  // path glob
		"envs/inventory.yaml":      false, // one level short of envs/*/…
		"CHANGELOG.md":             true,
		"internal/cli/cli.go":      false,
	}
	for p, want := range cases {
		if got := IsAppendOnly(p, globs); got != want {
			t.Errorf("IsAppendOnly(%q) = %v, want %v", p, got, want)
		}
	}
	if IsAppendOnly("anything", nil) {
		t.Error("empty glob list → never append-only")
	}
}

func TestHumanAge(t *testing.T) {
	cases := map[time.Duration]string{
		3 * 24 * time.Hour: "3d",
		5 * time.Hour:      "5h",
		12 * time.Minute:   "12m",
		30 * time.Second:   "just now",
	}
	for d, want := range cases {
		if got := HumanAge(d); got != want {
			t.Errorf("HumanAge(%v) = %q, want %q", d, got, want)
		}
	}
}

func TestScanWorkers(t *testing.T) {
	// Bounded worker count for the concurrent worktree scan (#22): enough to
	// hide git latency at ~30 windows, capped so we don't spawn dozens of git
	// processes at once.
	if n := scanWorkers(); n < 4 || n > 16 {
		t.Errorf("scanWorkers() = %d, want within [4,16]", n)
	}
}

func TestMatchDoubleStar(t *testing.T) {
	yes := [][2]string{
		{"docs/**/*.md", "docs/sub/a.md"},     // ** spans one dir
		{"docs/**/*.md", "docs/x/y/a.md"},     // ** spans multiple dirs
		{"**/*.md", "a.md"},                   // ** matches zero segments
		{"**/*.md", "deep/nested/a.md"},       // ** matches many
		{"docs/*.md", "docs/a.md"},            // no ** → single-level (unchanged)
		{"cluster/**", "cluster/apps/x.yaml"}, // trailing ** absorbs the rest
		{"*.md", "a.md"},                      // basename glob
	}
	for _, c := range yes {
		if !MatchDoubleStar(c[0], c[1]) {
			t.Errorf("MatchDoubleStar(%q,%q) = false, want true", c[0], c[1])
		}
	}
	no := [][2]string{
		{"docs/*.md", "docs/sub/a.md"}, // single * does NOT cross '/'
		{"docs/**/*.md", "src/a.md"},   // prefix must still match
		{"*.md", "a.go"},               // wrong ext
		{"docs/**/*.md", "docs/a.txt"}, // ** ok but final seg mismatches
	}
	for _, c := range no {
		if MatchDoubleStar(c[0], c[1]) {
			t.Errorf("MatchDoubleStar(%q,%q) = true, want false", c[0], c[1])
		}
	}
}

func TestIsAppendOnly_DoubleStar(t *testing.T) {
	globs := []string{"docs/**/*.md", "CHANGELOG.md"}
	if !IsAppendOnly("docs/gotchas/trading.md", globs) {
		t.Error("nested doc under docs/**/*.md should be append-only")
	}
	if !IsAppendOnly("CHANGELOG.md", globs) {
		t.Error("basename glob should still match")
	}
	if IsAppendOnly("src/main.go", globs) {
		t.Error("unrelated path must not match")
	}
}
