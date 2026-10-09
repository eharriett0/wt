package cli

import (
	"fmt"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"sync"
	"testing"

	"github.com/eharriett0/wt/internal/collide"
	"github.com/eharriett0/wt/internal/config"
	"github.com/eharriett0/wt/internal/gitx"
)

// bannerLines runs the banner's pure pipeline and returns its overlap lines.
func bannerLines(t *testing.T, ff *fakeFacts, ws []collide.Window, live map[string]collide.WindowLiveness, self collide.Self) []string {
	t.Helper()
	graded := agentOverlaps(gradeTestConfig(), ff.factory(), ws, collide.Overlaps(ws), live, self)
	msg, has := codexContextMessage(graded, self.Label)
	if !has {
		return nil
	}
	lines := strings.Split(msg, "\n")
	return lines[1 : len(lines)-1]
}

// statusGrade runs `wt status`'s pure pipeline and returns file → severity.
func statusGrade(ff *fakeFacts, ws []collide.Window, live map[string]collide.WindowLiveness) map[string]string {
	active, _ := collide.PartitionOverlaps(collide.Overlaps(ws), live)
	out := map[string]string{}
	for _, o := range gradeOverlaps(gradeTestConfig(), ff.factory(), ws, active, nil, collide.Self{}) {
		out[o.File] = o.Severity
	}
	return out
}

// A window whose own copy of a file is already on base (#109), or whose change
// to it already landed (#122), holds nothing that can collide, so its banner
// reads "same file" there even though `wt check` from it still blocks (its
// pre-edit heads-up). HIGH ⇒ check blocks, not ⟺ (#182). A window still editing
// after its PR merged, and one holding an untracked copy (#113, an add/add it is
// about to commit), stay HIGH: both have something to collide with.
func TestAgentOverlaps_SelfContestsNothing(t *testing.T) {
	const file = "x.go"
	cases := []struct {
		name     string
		selfLive *collide.WindowLiveness // the current window's own liveness (never consulted)
		set      func(ff *fakeFacts, me, other factKey)
		want     string
	}{
		{"own PR merged, clean: copy already on base (#109)", &collide.WindowLiveness{Level: collide.LiveStale, MergedPR: "31"},
			func(ff *fakeFacts, me, other factKey) {
				ff.merged[me] = true // no ranges either: identical to base
				ff.ranges[other] = []gitx.LineRange{rng(11, 11)}
			}, "same file"},
		{"change landed under another PR (#122)", nil,
			func(ff *fakeFacts, me, other factKey) {
				ff.subsumed[me] = true
				ff.ranges[me] = []gitx.LineRange{rng(10, 12)} // phantom: base's own change
				ff.ranges[other] = []gitx.LineRange{rng(11, 11)}
			}, "same file"},
		{"own PR merged, still editing (dirty)", &collide.WindowLiveness{Level: collide.LiveStale, MergedPR: "21", Dirty: true},
			func(ff *fakeFacts, me, other factKey) {
				ff.ranges[me] = []gitx.LineRange{rng(20, 22)}
				ff.ranges[other] = []gitx.LineRange{rng(21, 21)}
			}, "overlapping hunks — HIGH"},
		{"untracked copy of a file another window added (#113)", nil,
			func(ff *fakeFacts, me, other factKey) {
				ff.untracked[me] = true
				ff.ranges[other] = []gitx.LineRange{rng(0, 1)}
			}, "overlapping hunks — HIGH"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			ws := []collide.Window{
				{Worktree: "/wt/cur", Branch: "cur", Touched: []string{file}},
				{Worktree: "/wt/y-live", Branch: "y-live", Touched: []string{file}},
			}
			live := map[string]collide.WindowLiveness{"y-live": {Level: collide.LiveUnmerged}}
			if tc.selfLive != nil {
				live["cur"] = *tc.selfLive
			}
			ff := newFakeFacts()
			tc.set(ff, factKey{"/wt/cur", file}, factKey{"/wt/y-live", file})

			// `wt check x.go` from cur blocks in every one of these cases
			checkBlocks := false
			for _, e := range checkEntries(gradeTestConfig(), ff, ws, "/wt/cur", collide.CheckPaths(ws, "/wt/cur", collide.ExactQueries([]string{file})), live) {
				checkBlocks = checkBlocks || e.Category == CatBlocking
			}
			if !checkBlocks {
				t.Fatalf("precondition: wt check from cur should block")
			}
			got := bannerLines(t, ff, ws, live, curSelf)
			want := []string{"  x.go — also being edited by y-live (" + tc.want + ")"}
			if !reflect.DeepEqual(got, want) {
				t.Errorf("banner = %q, want %q", got, want)
			}
		})
	}
}

// Two windows can share a label (#182): two worktrees that claimed one issue are
// both "#77", and two detached worktrees are both named by their directory.
// Grading by label compared one of them with itself and dropped the pair, so a
// real collision read "same file" in `wt status`, `--blocking` and every other
// window's banner while `wt check` blocked. Windows are told apart by worktree.
func TestGradeOverlaps_SharedLabels(t *testing.T) {
	const file = "fL.go"
	claimed := func(wt, branch string) collide.Window { // label "#77"
		return collide.Window{Worktree: wt, Branch: branch, Issue: "77", Touched: []string{file}}
	}
	detached := func(wt string) collide.Window { // label = directory name
		return collide.Window{Worktree: wt, Branch: "HEAD", Touched: []string{file}}
	}
	observer := collide.Window{Worktree: "/wt/obs", Branch: "obs", Touched: []string{"other.go"}}
	obs := collide.Self{Label: "obs", Worktree: "/wt/obs"}

	cases := []struct {
		name       string
		pair       [2]collide.Window
		ranges     [2][]gitx.LineRange
		status     string
		thirdParty string // the banner line in a window editing neither
		fromFirst  string // the banner line in pair[0]
	}{
		{"claimed twice (r3), overlapping",
			[2]collide.Window{claimed("/wt/fix-77-a", "fix-77-a"), claimed("/wt/fix-77-b", "fix-77-b")},
			[2][]gitx.LineRange{{rng(10, 12)}, {rng(11, 13)}}, "HIGH",
			"  fL.go — being edited by #77 (fix-77-a), #77 (fix-77-b) (overlapping hunks — HIGH)",
			"  fL.go — also being edited by #77 (fix-77-b) (overlapping hunks — HIGH)"},
		{"claimed twice, disjoint",
			[2]collide.Window{claimed("/wt/fix-77-a", "fix-77-a"), claimed("/wt/fix-77-b", "fix-77-b")},
			[2][]gitx.LineRange{{rng(10, 12)}, {rng(40, 41)}}, "low",
			"  fL.go — being edited by #77 (fix-77-a), #77 (fix-77-b) (same file)",
			"  fL.go — also being edited by #77 (fix-77-b) (same file)"},
		{"detached, same directory name (r2), overlapping",
			[2]collide.Window{detached("/dupa/x"), detached("/dupb/x")},
			[2][]gitx.LineRange{{rng(10, 12)}, {rng(11, 13)}}, "HIGH",
			"  fL.go — being edited by x (dupa/x), x (dupb/x) (overlapping hunks — HIGH)",
			"  fL.go — also being edited by x (dupb/x) (overlapping hunks — HIGH)"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			ws := []collide.Window{tc.pair[0], tc.pair[1], observer}
			ff := newFakeFacts()
			for i, w := range tc.pair {
				ff.ranges[factKey{w.Worktree, file}] = tc.ranges[i]
			}
			live := map[string]collide.WindowLiveness{} // both live (missing)
			if got := statusGrade(ff, ws, live)[file]; got != tc.status {
				t.Errorf("status = %q, want %q", got, tc.status)
			}
			if got := bannerLines(t, ff, ws, live, obs); !reflect.DeepEqual(got, []string{tc.thirdParty}) {
				t.Errorf("third window's banner = %q, want %q", got, tc.thirdParty)
			}
			first := collide.Self{Label: tc.pair[0].Label(), Worktree: tc.pair[0].Worktree}
			if got := bannerLines(t, ff, ws, live, first); !reflect.DeepEqual(got, []string{tc.fromFirst}) {
				t.Errorf("first window's banner = %q, want %q", got, tc.fromFirst)
			}
		})
	}

	// A namesake editing a file THIS window doesn't touch is another window: the
	// line must neither vanish nor say "also" as if this window were editing it.
	t.Run("namesake of self on a file self isn't editing", func(t *testing.T) {
		a := collide.Window{Worktree: "/wt/fix-77-a", Branch: "fix-77-a", Issue: "77", Touched: []string{"mine.go"}}
		b := claimed("/wt/fix-77-b", "fix-77-b")
		c := collide.Window{Worktree: "/wt/c", Branch: "c", Touched: []string{file}}
		ws := []collide.Window{a, b, c}
		ff := newFakeFacts()
		ff.ranges[factKey{b.Worktree, file}] = []gitx.LineRange{rng(1, 2)}
		ff.ranges[factKey{c.Worktree, file}] = []gitx.LineRange{rng(2, 3)}
		got := bannerLines(t, ff, ws, map[string]collide.WindowLiveness{}, collide.Self{Label: "#77", Worktree: a.Worktree})
		want := []string{"  fL.go — being edited by #77 (fix-77-b), c (overlapping hunks — HIGH)"}
		if !reflect.DeepEqual(got, want) {
			t.Errorf("banner = %q, want %q", got, want)
		}
	})
}

func TestDistinctTail(t *testing.T) {
	for _, tc := range []struct {
		wt     string
		others []string
		want   string
	}{
		{"/wt/fix-77-b", []string{"/wt/fix-77-a"}, "fix-77-b"},
		{"/dupb/x", []string{"/dupa/x"}, "dupb/x"},
		{"/a/b/x", []string{"/c/b/x"}, "a/b/x"},
		{"/wt/x", []string{"/wt/x"}, "/wt/x"}, // nothing shorter is distinct
		{"/wt/x", nil, "x"},
	} {
		if got := distinctTail(tc.wt, tc.others); got != tc.want {
			t.Errorf("distinctTail(%q, %q) = %q, want %q", tc.wt, tc.others, got, tc.want)
		}
	}
}

// gitGradeFacts' memo is plain maps, so the concurrent overlap grading must take
// a fresh instance for every overlap. A shared one is a data race on every
// `wt status` and per-turn banner (a fatal "concurrent map writes" outside -race).
func TestGitFactsFor_FreshInstancePerCall(t *testing.T) {
	newFacts := gitFactsFor(&config.Config{Base: "main"})
	if a, b := newFacts(), newFacts(); a == b {
		t.Fatal("gitFactsFor handed out the same gitGradeFacts twice; its memo must not be shared across goroutines")
	}
}

// oneFileFacts records every path it is asked about in an unsynchronized map, so
// sharing one instance between overlaps races (under -race) and shows up as an
// instance that saw more than one file.
type oneFileFacts struct{ files map[string]bool }

func (f *oneFileFacts) see(p string) { f.files[p] = true }
func (f *oneFileFacts) AlreadyMerged(_, p string) bool {
	f.see(p)
	return false
}
func (f *oneFileFacts) Untracked(_, p string) bool {
	f.see(p)
	return false
}
func (f *oneFileFacts) Subsumed(_, p string) bool {
	f.see(p)
	return false
}
func (f *oneFileFacts) Ranges(_, p string) []gitx.LineRange {
	f.see(p)
	return []gitx.LineRange{rng(1, 2)}
}
func (f *oneFileFacts) SharedSections(_ []string, p, _ string) ([]string, bool) {
	f.see(p)
	return nil, false
}

func TestGradeOverlaps_FreshFactsPerOverlap(t *testing.T) {
	var mu sync.Mutex
	var made []*oneFileFacts
	newFacts := func() gradeFacts {
		f := &oneFileFacts{files: map[string]bool{}}
		mu.Lock()
		made = append(made, f)
		mu.Unlock()
		return f
	}
	ws := []collide.Window{{Worktree: "/wt/a", Branch: "a"}, {Worktree: "/wt/b", Branch: "b"}}
	var active []collide.Overlap
	for i := 0; i < 3*gradeWorkers; i++ {
		active = append(active, collide.Overlap{File: fmt.Sprintf("f%02d.go", i), Windows: []string{"a", "b"}, Worktrees: []string{"/wt/a", "/wt/b"}})
	}
	graded := gradeOverlaps(gradeTestConfig(), newFacts, ws, active, nil, collide.Self{})
	if len(made) != len(active) {
		t.Fatalf("%d facts instances for %d overlaps; want one each", len(made), len(active))
	}
	for i, f := range made {
		if len(f.files) != 1 {
			t.Errorf("instance %d was asked about %d files; each overlap must grade with its own", i, len(f.files))
		}
	}
	for i, o := range graded { // the output keeps the input order
		if o.File != active[i].File || o.Severity != "HIGH" {
			t.Errorf("graded[%d] = %s %s, want %s HIGH", i, o.File, o.Severity, active[i].File)
		}
	}
}

// The memo is keyed by (worktree, path): one report grades the same file for
// several windows, so a key without the worktree would hand one window's diff to
// another.
func TestGitGradeFacts_MemoKeyedByWorktreeAndPath(t *testing.T) {
	calls := map[string]int{}
	ranges := map[string][]gitx.LineRange{"/a f": {rng(1, 1)}, "/b f": {rng(2, 2)}, "/a g": {rng(3, 3)}}
	src := factSource{
		alreadyMerged: func(wt, _, p string) bool { calls["merged "+wt+" "+p]++; return wt == "/a" && p == "f" },
		untracked:     func(wt, p string) bool { calls["untracked "+wt+" "+p]++; return wt == "/b" && p == "f" },
		subsumed:      func(wt, _, p string) bool { calls["subsumed "+wt+" "+p]++; return wt == "/a" && p == "g" },
		ranges:        func(wt, _, p string) []gitx.LineRange { calls["ranges "+wt+" "+p]++; return ranges[wt+" "+p] },
		rangesNew:     func(wt, _, p string) []gitx.LineRange { calls["new "+wt+" "+p]++; return ranges[wt+" "+p] },
	}
	g := newMemoFacts("main", src)
	keys := []factKey{{"/a", "f"}, {"/b", "f"}, {"/a", "g"}, {"/a", "f"}, {"/b", "f"}}
	for _, k := range keys {
		id := k.worktree + " " + k.path
		if got, want := g.AlreadyMerged(k.worktree, k.path), id == "/a f"; got != want {
			t.Errorf("AlreadyMerged(%s) = %v, want %v", id, got, want)
		}
		if got, want := g.Untracked(k.worktree, k.path), id == "/b f"; got != want {
			t.Errorf("Untracked(%s) = %v, want %v", id, got, want)
		}
		if got, want := g.Subsumed(k.worktree, k.path), id == "/a g"; got != want {
			t.Errorf("Subsumed(%s) = %v, want %v", id, got, want)
		}
		if got := g.Ranges(k.worktree, k.path); !reflect.DeepEqual(got, ranges[id]) {
			t.Errorf("Ranges(%s) = %v, want %v", id, got, ranges[id])
		}
	}
	for name, n := range calls {
		if n != 1 {
			t.Errorf("%s computed %d times; the memo should ask once", name, n)
		}
	}

	// SharedSections reads the NEW-frame ranges through the same memo: two
	// windows editing different sections share none, and a memo keyed without
	// the worktree would hand /b's lookup /a's lines (both in "## A").
	dir := t.TempDir()
	doc := "## A\na1\na2\n## B\nb1\nb2\n"
	for _, wt := range []string{"a", "b"} {
		if err := os.MkdirAll(filepath.Join(dir, wt), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(dir, wt, "PLAN.md"), []byte(doc), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	a, b := filepath.Join(dir, "a"), filepath.Join(dir, "b")
	ranges[a+" PLAN.md"] = []gitx.LineRange{rng(2, 2)} // in ## A
	ranges[b+" PLAN.md"] = []gitx.LineRange{rng(5, 5)} // in ## B
	for i := 0; i < 2; i++ {
		if shared, graded := g.SharedSections([]string{a, b}, "PLAN.md", "^## "); !graded || len(shared) != 0 {
			t.Errorf("SharedSections = %v graded=%v, want no shared section", shared, graded)
		}
	}
	if calls["new "+a+" PLAN.md"] != 1 || calls["new "+b+" PLAN.md"] != 1 {
		t.Errorf("new-frame ranges computed %d/%d times, want once each", calls["new "+a+" PLAN.md"], calls["new "+b+" PLAN.md"])
	}
}
