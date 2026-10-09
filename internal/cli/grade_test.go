package cli

import (
	"fmt"
	"reflect"
	"slices"
	"strings"
	"sync"
	"testing"

	"github.com/eharriett0/wt/internal/collide"
	"github.com/eharriett0/wt/internal/config"
	"github.com/eharriett0/wt/internal/gitx"
)

// fakeFacts is a table-driven gradeFacts: every observation is looked up by
// (worktree, path), and calls are counted so the tests can pin the decision's
// laziness (an early verdict must not pay for a diff).
type fakeFacts struct {
	merged, untracked, subsumed map[factKey]bool
	ranges                      map[factKey][]gitx.LineRange
	headings                    map[factKey][]string // structured-doc sections a worktree edits

	mu    sync.Mutex // overlaps grade concurrently; the maps above are read-only
	calls map[string]int
}

func newFakeFacts() *fakeFacts {
	return &fakeFacts{
		merged: map[factKey]bool{}, untracked: map[factKey]bool{}, subsumed: map[factKey]bool{},
		ranges: map[factKey][]gitx.LineRange{}, headings: map[factKey][]string{}, calls: map[string]int{},
	}
}

// count records a call; the only write the fake does, so the only locked one.
func (f *fakeFacts) count(name string) {
	f.mu.Lock()
	f.calls[name]++
	f.mu.Unlock()
}

// factory hands the shared fake to every overlap the concurrent grader starts.
func (f *fakeFacts) factory() func() gradeFacts { return func() gradeFacts { return f } }

func (f *fakeFacts) AlreadyMerged(wt, p string) bool {
	f.count("merged")
	return f.merged[factKey{wt, p}]
}

func (f *fakeFacts) Untracked(wt, p string) bool {
	f.count("untracked")
	return f.untracked[factKey{wt, p}]
}

func (f *fakeFacts) Subsumed(wt, p string) bool {
	f.count("subsumed")
	return f.subsumed[factKey{wt, p}]
}

func (f *fakeFacts) Ranges(wt, p string) []gitx.LineRange {
	f.count("ranges")
	return f.ranges[factKey{wt, p}]
}

// SharedSections mirrors collide.SharedSectionsAcross over the fake headings: a
// heading edited by ≥2 of the worktrees is shared; graded=false when no worktree
// has the doc.
func (f *fakeFacts) SharedSections(wts []string, p, _ string) ([]string, bool) {
	f.count("sections")
	count := map[string]int{}
	var order []string
	found := false
	for _, wt := range wts {
		hs, ok := f.headings[factKey{wt, p}]
		if !ok {
			continue
		}
		found = true
		for _, h := range hs {
			if count[h] == 0 {
				order = append(order, h)
			}
			count[h]++
		}
	}
	if !found {
		return nil, false
	}
	var shared []string
	for _, h := range order {
		if count[h] >= 2 {
			shared = append(shared, h)
		}
	}
	return shared, true
}

func rng(s, e int) gitx.LineRange { return gitx.LineRange{Start: s, End: e} }

func gradeTestConfig() *config.Config {
	return &config.Config{
		Base:            "main",
		SharedDocs:      []string{"CLAUDE.md", "PLAN.md"},
		AppendOnlyPaths: []string{"CHANGELOG.md"},
		StructuredDocs:  map[string]string{"PLAN.md": "^## "},
	}
}

// TestGradeEntry pins the per-entry `wt check` decision that every surface now
// single-sources (#182): `wt check`, wt_check, both edit hooks, `wt status`,
// wt_status and the per-turn banner.
func TestGradeEntry(t *testing.T) {
	const cur, oth = "/wt/cur", "/wt/oth"
	type facts struct {
		merged, untracked, subsumed bool
		cur, other                  []gitx.LineRange
		curHeads, otherHeads        []string
	}
	cases := []struct {
		name     string
		path     string // requested path (cf.Path)
		matched  string // cf.MatchedFile ("" → path)
		wl       collide.WindowLiveness
		f        facts
		cat      Category
		sev      string
		spans    []gitx.LineRange
		sections []string
		flag     string // AlreadyMerged | Untracked | Subsumed
	}{
		{name: "merged branch is stale (hidden by default)", path: "f.go", wl: collide.WindowLiveness{Level: collide.LiveStale, MergedPR: "11"},
			f: facts{cur: []gitx.LineRange{rng(1, 5)}, other: []gitx.LineRange{rng(2, 3)}}, cat: CatStale, sev: "low"},
		{name: "dormant branch is stale", path: "f.go", wl: collide.WindowLiveness{Level: collide.LiveDormant},
			f: facts{cur: []gitx.LineRange{rng(1, 5)}, other: []gitx.LineRange{rng(2, 3)}}, cat: CatStale, sev: "low"},
		{name: "closed-PR branch is stale", path: "f.go", wl: collide.WindowLiveness{Level: collide.LiveClosedPR, ClosedPR: "12"},
			f: facts{cur: []gitx.LineRange{rng(1, 5)}, other: []gitx.LineRange{rng(2, 3)}}, cat: CatStale, sev: "low"},
		{name: "a dirty window is never suppressed", path: "f.go", wl: collide.WindowLiveness{Level: collide.LiveDirty, MergedPR: "9", BehindBase: 40},
			f: facts{cur: []gitx.LineRange{rng(1, 5)}, other: []gitx.LineRange{rng(2, 3)}}, cat: CatBlocking, sev: "HIGH", spans: []gitx.LineRange{rng(2, 3)}},
		{name: "#109 already-merged content", path: "f.go", wl: collide.WindowLiveness{Level: collide.LiveDirty},
			f: facts{merged: true, cur: []gitx.LineRange{rng(1, 5)}, other: []gitx.LineRange{rng(2, 3)}}, cat: CatFYI, sev: "low", flag: "AlreadyMerged"},
		{name: "#113 untracked in the other window", path: "f.go", wl: collide.WindowLiveness{Level: collide.LiveDirty},
			f: facts{untracked: true, cur: []gitx.LineRange{rng(1, 5)}}, cat: CatFYI, sev: "low", flag: "Untracked"},
		{name: "shared doc is advisory, even overlapping", path: "CLAUDE.md", wl: collide.WindowLiveness{Level: collide.LiveOpenPR},
			f: facts{cur: []gitx.LineRange{rng(1, 5)}, other: []gitx.LineRange{rng(2, 3)}}, cat: CatAdvisory, sev: "low"},
		{name: "#22 structured doc, same section → HIGH", path: "PLAN.md", wl: collide.WindowLiveness{Level: collide.LiveOpenPR},
			f: facts{curHeads: []string{"## A", "## B"}, otherHeads: []string{"## B"}}, cat: CatBlocking, sev: "HIGH", sections: []string{"## B"}},
		{name: "#22 structured doc, disjoint sections → advisory", path: "PLAN.md", wl: collide.WindowLiveness{Level: collide.LiveOpenPR},
			f: facts{curHeads: []string{"## A"}, otherHeads: []string{"## B"}}, cat: CatAdvisory, sev: "low"},
		{name: "append-only is FYI, even overlapping", path: "CHANGELOG.md", wl: collide.WindowLiveness{Level: collide.LiveOpenPR},
			f: facts{cur: []gitx.LineRange{rng(1, 5)}, other: []gitx.LineRange{rng(2, 3)}}, cat: CatFYI, sev: "low"},
		{name: "disjoint hunks are FYI", path: "f.go", wl: collide.WindowLiveness{Level: collide.LiveOpenPR},
			f: facts{cur: []gitx.LineRange{rng(1, 5)}, other: []gitx.LineRange{rng(40, 41)}}, cat: CatFYI, sev: "low"},
		{name: "overlapping hunks are HIGH", path: "f.go", wl: collide.WindowLiveness{Level: collide.LiveUnmerged},
			f: facts{cur: []gitx.LineRange{rng(10, 12)}, other: []gitx.LineRange{rng(11, 13)}}, cat: CatBlocking, sev: "HIGH", spans: []gitx.LineRange{rng(11, 12)}},
		{name: "#122 overlap that is base's own change → FYI", path: "f.go", wl: collide.WindowLiveness{Level: collide.LiveUnmerged},
			f: facts{subsumed: true, cur: []gitx.LineRange{rng(10, 12)}, other: []gitx.LineRange{rng(11, 13)}}, cat: CatFYI, sev: "low", spans: []gitx.LineRange{rng(11, 12)}, flag: "Subsumed"},
		{name: "this window has no ranges yet → indeterminate HIGH (pre-edit heads-up)", path: "f.go", wl: collide.WindowLiveness{Level: collide.LiveOpenPR},
			f: facts{other: []gitx.LineRange{rng(11, 13)}}, cat: CatBlocking, sev: "HIGH"},
		{name: "other has no ranges (binary) → indeterminate HIGH", path: "f.go", wl: collide.WindowLiveness{Level: collide.LiveOpenPR},
			f: facts{cur: []gitx.LineRange{rng(1, 2)}}, cat: CatBlocking, sev: "HIGH"},
		{name: "unknown liveness is surfaced, never hidden", path: "f.go", wl: collide.WindowLiveness{},
			f: facts{cur: []gitx.LineRange{rng(10, 12)}, other: []gitx.LineRange{rng(12, 12)}}, cat: CatBlocking, sev: "HIGH", spans: []gitx.LineRange{rng(12, 12)}},
		// a basename query: globs match the requested path, git lookups the resolved file
		{name: "basename query grades the resolved file", path: "f.go", matched: "pkg/f.go", wl: collide.WindowLiveness{Level: collide.LiveOpenPR},
			f: facts{cur: []gitx.LineRange{rng(10, 12)}, other: []gitx.LineRange{rng(11, 11)}}, cat: CatBlocking, sev: "HIGH", spans: []gitx.LineRange{rng(11, 11)}},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			resolved := c.matched
			if resolved == "" {
				resolved = c.path
			}
			ff := newFakeFacts()
			ff.merged[factKey{oth, resolved}] = c.f.merged
			ff.untracked[factKey{oth, resolved}] = c.f.untracked
			ff.subsumed[factKey{oth, resolved}] = c.f.subsumed
			ff.ranges[factKey{cur, resolved}] = c.f.cur
			ff.ranges[factKey{oth, resolved}] = c.f.other
			if c.f.curHeads != nil {
				ff.headings[factKey{cur, resolved}] = c.f.curHeads
			}
			if c.f.otherHeads != nil {
				ff.headings[factKey{oth, resolved}] = c.f.otherHeads
			}
			e := gradeEntry(gradeTestConfig(), ff, cur, oth, collide.Conflict{Path: c.path, Window: "oth", MatchedFile: c.matched}, c.wl)
			if e.Category != c.cat || e.Severity != c.sev {
				t.Fatalf("got %s/%s, want %s/%s (%+v)", e.Category, e.Severity, c.cat, c.sev, e)
			}
			if !reflect.DeepEqual(e.OverlapSpans, c.spans) && !(len(e.OverlapSpans) == 0 && len(c.spans) == 0) {
				t.Errorf("spans = %v, want %v", e.OverlapSpans, c.spans)
			}
			if !reflect.DeepEqual(e.SharedSections, c.sections) {
				t.Errorf("sections = %v, want %v", e.SharedSections, c.sections)
			}
			flags := map[string]bool{"AlreadyMerged": e.AlreadyMerged, "Untracked": e.Untracked, "Subsumed": e.Subsumed}
			for name, set := range flags {
				if set != (name == c.flag) {
					t.Errorf("%s = %v, want %v", name, set, name == c.flag)
				}
			}
			if e.Path != c.path || e.Window != "oth" || e.Liveness != c.wl.Label() {
				t.Errorf("identity fields = %q/%q/%q", e.Path, e.Window, e.Liveness)
			}
		})
	}
}

// The grade asks for facts in decision order, so an early verdict never pays for
// a diff and the costly subsumption merge runs only for a would-be HIGH.
func TestGradeEntry_Lazy(t *testing.T) {
	const cur, oth = "/wt/cur", "/wt/oth"
	c := gradeTestConfig()
	cf := func(p string) collide.Conflict { return collide.Conflict{Path: p, Window: "oth", MatchedFile: p} }
	live := collide.WindowLiveness{Level: collide.LiveOpenPR}

	ff := newFakeFacts()
	gradeEntry(c, ff, cur, oth, cf("f.go"), collide.WindowLiveness{Level: collide.LiveStale})
	if len(ff.calls) != 0 {
		t.Errorf("a suppressed window must cost nothing, got calls %v", ff.calls)
	}

	ff = newFakeFacts()
	ff.merged[factKey{oth, "f.go"}] = true
	gradeEntry(c, ff, cur, oth, cf("f.go"), live)
	if ff.calls["untracked"]+ff.calls["ranges"]+ff.calls["subsumed"] != 0 {
		t.Errorf("already-merged must stop the grade, got calls %v", ff.calls)
	}

	ff = newFakeFacts()
	gradeEntry(c, ff, cur, oth, cf("CLAUDE.md"), live)
	gradeEntry(c, ff, cur, oth, cf("CHANGELOG.md"), live)
	if ff.calls["ranges"]+ff.calls["subsumed"]+ff.calls["sections"] != 0 {
		t.Errorf("a shared / append-only file must not diff, got calls %v", ff.calls)
	}

	ff = newFakeFacts()
	ff.ranges[factKey{cur, "f.go"}] = []gitx.LineRange{rng(1, 2)}
	ff.ranges[factKey{oth, "f.go"}] = []gitx.LineRange{rng(9, 9)}
	gradeEntry(c, ff, cur, oth, cf("f.go"), live)
	if ff.calls["subsumed"] != 0 {
		t.Errorf("disjoint hunks must not run the subsumption merge, got calls %v", ff.calls)
	}
	ff.ranges[factKey{oth, "f.go"}] = []gitx.LineRange{rng(2, 2)}
	gradeEntry(c, ff, cur, oth, cf("f.go"), live)
	if ff.calls["subsumed"] != 1 {
		t.Errorf("a would-be HIGH runs the subsumption merge once, got calls %v", ff.calls)
	}

	// an unresolvable other worktree: no ranges are asked of "" (git would run
	// in the cwd and read THIS window's diff as the other's)
	ff = newFakeFacts()
	ff.ranges[factKey{cur, "f.go"}] = []gitx.LineRange{rng(1, 2)}
	e := gradeEntry(c, ff, cur, "", cf("f.go"), live)
	if ff.calls["ranges"] != 1 || e.Category != CatBlocking {
		t.Errorf("no other worktree: want 1 ranges call + indeterminate HIGH, got %v / %s", ff.calls, e.Category)
	}
}

// scenarioWindows / scenarioFacts reproduce the #182 report and its smoke: the
// current window "cur" edits five files that merged, closed-PR, dormant, disjoint,
// overlapping, dirty and append-only windows also touch.
// curSelf is the scenario's current window, as the banner identifies it.
var curSelf = collide.Self{Label: "cur", Worktree: "/wt/cur"}

func scenarioWindows() []collide.Window {
	w := func(name string, files ...string) collide.Window {
		return collide.Window{Worktree: "/wt/" + name, Branch: name, Touched: files}
	}
	return []collide.Window{
		w("cur", "CHANGELOG.md", "CLAUDE.md", "api.go", "code.go", "doc.md", "infra/main.tf", "util.go"),
		w("a-merged", "CHANGELOG.md", "CLAUDE.md", "doc.md"),
		w("b-closed", "code.go"),
		w("c-dormant", "infra/main.tf"),
		w("d-disjoint", "doc.md", "infra/main.tf"),
		w("e-append", "CHANGELOG.md", "CLAUDE.md"),
		w("f-live", "code.go"),
		w("g-dirty", "api.go"),
		w("h1", "other.go", "util.go"),
		w("h2", "other.go", "util.go"),
	}
}

func scenarioLive() map[string]collide.WindowLiveness {
	return map[string]collide.WindowLiveness{
		"a-merged":   {Level: collide.LiveStale, MergedPR: "11"},
		"b-closed":   {Level: collide.LiveClosedPR, ClosedPR: "12"},
		"c-dormant":  {Level: collide.LiveDormant},
		"d-disjoint": {Level: collide.LiveOpenPR, PR: "14"},
		"e-append":   {Level: collide.LiveOpenPR, PR: "15"},
		"f-live":     {Level: collide.LiveUnmerged},
		"g-dirty":    {Level: collide.LiveDirty},
		"h1":         {Level: collide.LiveUnmerged},
		"h2":         {Level: collide.LiveUnmerged},
	}
}

func scenarioFacts() *fakeFacts {
	ff := newFakeFacts()
	set := func(wt, p string, rs ...gitx.LineRange) { ff.ranges[factKey{"/wt/" + wt, p}] = rs }
	set("cur", "api.go", rng(5, 5))
	set("cur", "code.go", rng(10, 12))
	set("cur", "doc.md", rng(5, 5))
	set("cur", "infra/main.tf", rng(20, 20))
	set("cur", "util.go", rng(50, 50))
	// a-merged's doc.md is byte-for-byte base now (its squash landed): no ranges,
	// which the old all-windows grade read as "indeterminate" → HIGH
	set("b-closed", "code.go", rng(10, 12))
	set("c-dormant", "infra/main.tf", rng(20, 21))
	set("d-disjoint", "doc.md", rng(50, 50))
	set("d-disjoint", "infra/main.tf", rng(50, 50))
	set("f-live", "code.go", rng(11, 13))
	set("g-dirty", "api.go", rng(5, 5))
	set("h1", "util.go", rng(10, 12))
	set("h1", "other.go", rng(1, 2))
	set("h2", "util.go", rng(11, 13))
	set("h2", "other.go", rng(2, 3))
	return ff
}

// TestAgentOverlaps_Issue182 is the issue's repro, end to end through the pure
// banner pipeline: the per-turn banner must not list merged / closed / dormant
// windows, and a file this window edits is HIGH only where `wt check` blocks.
func TestAgentOverlaps_Issue182(t *testing.T) {
	c := gradeTestConfig()
	ws := scenarioWindows()
	graded := agentOverlaps(c, scenarioFacts().factory(), ws, collide.Overlaps(ws), scenarioLive(), curSelf)
	msg, has := codexContextMessage(graded, "cur")
	if !has {
		t.Fatal("expected a banner")
	}
	want := []string{
		"  CHANGELOG.md — also being edited by e-append (same file)",
		"  CLAUDE.md — also being edited by e-append (same file)",
		"  api.go — also being edited by g-dirty (overlapping hunks — HIGH)", // dirty: never suppressed
		"  code.go — also being edited by f-live (overlapping hunks — HIGH)", // a real live overlap stays HIGH
		"  doc.md — also being edited by d-disjoint (same file)",             // was HIGH via a-merged
		"  infra/main.tf — also being edited by d-disjoint (same file)",      // was HIGH via c-dormant
		"  other.go — being edited by h1, h2 (overlapping hunks — HIGH)",     // not ours: h1 and h2 collide
		"  util.go — also being edited by h1, h2 (same file)",                // was HIGH: h1×h2, not us
	}
	got := strings.Split(msg, "\n")
	if len(got) != len(want)+2 || !reflect.DeepEqual(got[1:len(got)-1], want) {
		t.Errorf("banner =\n%s\nwant lines\n%s", msg, strings.Join(want, "\n"))
	}
	for _, hidden := range []string{"a-merged", "b-closed", "c-dormant"} {
		if strings.Contains(msg, hidden) {
			t.Errorf("suppressed window %s listed: %s", hidden, msg)
		}
	}
}

// A window still editing after its OWN PR merged is classified stale (#79: PR
// state outranks a dirty index). The old banner counted that against the
// window itself, so a file it shared with one live window fell below "two
// live" and vanished, a HIGH `wt check` blocks on included. `wt check` never
// consults the current window's liveness; neither does the banner now.
func TestAgentOverlaps_OwnMergedPRStillWarned(t *testing.T) {
	live := scenarioLive()
	live["cur"] = collide.WindowLiveness{Level: collide.LiveStale, MergedPR: "99", Dirty: true}
	ws := scenarioWindows()
	graded := agentOverlaps(gradeTestConfig(), scenarioFacts().factory(), ws, collide.Overlaps(ws), live, curSelf)
	got := map[string]string{}
	for _, o := range graded {
		got[o.File] = strings.Join(windowsExcluding(o.Windows, "cur"), ",") + " " + o.Severity
	}
	for file, want := range map[string]string{
		"api.go":  "g-dirty HIGH",
		"code.go": "f-live HIGH",
		"doc.md":  "d-disjoint low",
	} {
		if got[file] != want {
			t.Errorf("%s = %q, want %q (all: %v)", file, got[file], want, got)
		}
	}
}

// The banner's grade for a file this window edits must match `wt check <file>`
// from this window, for every liveness × fact × hunk shape × file kind × what
// this window's own copy is, with one and with two other windows. Both sides run
// their real pure cores over the same facts: CheckPaths + checkEntries
// (buildCheckReport) vs Overlaps + agentOverlaps (the banner).
//
// The windows listed are the same. The verdict is HIGH ⇒ check blocks, and the
// ONLY way check blocks while the banner reads low is this window's own copy
// being already on base (#109) or its change already landed (#122): check's
// pre-edit heads-up about a window that holds nothing to collide with (#182).
// An untracked copy (#113) is not exempt: check blocks and so does the banner.
func TestAgentOverlaps_MatchesCheck(t *testing.T) {
	type profile struct {
		wl      *collide.WindowLiveness // nil → missing from the liveness map (treated live)
		fact    string                  // "" | merged | untracked | subsumed
		ranges  string                  // overlap | disjoint | none
		display string
	}
	levels := []*collide.WindowLiveness{
		{Level: collide.LiveStale, MergedPR: "1"}, {Level: collide.LiveDormant}, {Level: collide.LiveClosedPR, ClosedPR: "2"},
		{Level: collide.LiveDirty}, {Level: collide.LiveDirty, MergedPR: "3"}, {Level: collide.LiveOpenPR, PR: "4"},
		{Level: collide.LiveUnmerged}, {Level: collide.LiveUnknown}, nil,
	}
	var all []profile
	for _, wl := range levels {
		for _, fact := range []string{"", "merged", "untracked", "subsumed"} {
			for _, r := range []string{"overlap", "disjoint", "none"} {
				name := "missing"
				if wl != nil {
					name = wl.Label()
				}
				all = append(all, profile{wl, fact, r, fmt.Sprintf("%s/%s/%s", name, fact, r)})
			}
		}
	}
	// a smaller cut for the two-other-windows product
	var some []profile
	for _, p := range all {
		if p.fact != "untracked" && (p.wl == nil || p.wl.Level != collide.LiveUnknown) {
			some = append(some, p)
		}
	}
	files := []string{"f.go", "CLAUDE.md", "CHANGELOG.md", "PLAN.md"}
	c := gradeTestConfig()

	check := func(t *testing.T, file string, curRanges []gitx.LineRange, curFact string, others []profile) {
		t.Helper()
		ws := []collide.Window{{Worktree: "/wt/cur", Branch: "cur", Touched: []string{file}}}
		live := map[string]collide.WindowLiveness{}
		ff := newFakeFacts()
		ck := factKey{"/wt/cur", file}
		ff.ranges[ck] = curRanges
		ff.headings[ck] = []string{"## A"}
		ff.merged[ck], ff.untracked[ck], ff.subsumed[ck] = curFact == "merged", curFact == "untracked", curFact == "subsumed"
		for i, p := range others {
			name := fmt.Sprintf("o%d", i)
			wt := "/wt/" + name
			ws = append(ws, collide.Window{Worktree: wt, Branch: name, Touched: []string{file}})
			if p.wl != nil {
				live[name] = *p.wl
			}
			k := factKey{wt, file}
			ff.merged[k], ff.untracked[k], ff.subsumed[k] = p.fact == "merged", p.fact == "untracked", p.fact == "subsumed"
			switch p.ranges {
			case "overlap":
				ff.ranges[k] = []gitx.LineRange{rng(2, 3)}
				ff.headings[k] = []string{"## A"}
			case "disjoint":
				ff.ranges[k] = []gitx.LineRange{rng(40, 41)}
				ff.headings[k] = []string{"## B"}
			}
		}

		// `wt check <file>` from cur: what it lists by default, and whether it blocks
		var checkListed []string
		checkBlocks := false
		for _, e := range checkEntries(c, ff, ws, "/wt/cur", collide.CheckPaths(ws, "/wt/cur", []string{file}), live) {
			if e.Category != CatStale {
				checkListed = append(checkListed, e.Window)
			}
			checkBlocks = checkBlocks || e.Category == CatBlocking
		}
		slices.Sort(checkListed)

		// the banner
		var bannerListed []string
		bannerHigh := false
		for _, o := range agentOverlaps(c, ff.factory(), ws, collide.Overlaps(ws), live, curSelf) {
			if o.File == file {
				bannerListed = windowsExcluding(o.Windows, "cur")
				bannerHigh = o.Severity == "HIGH"
			}
		}
		slices.Sort(bannerListed)

		var desc []string
		for _, p := range others {
			desc = append(desc, p.display)
		}
		wantHigh := checkBlocks && curFact != "merged" && curFact != "subsumed"
		if !slices.Equal(checkListed, bannerListed) || bannerHigh != wantHigh || (bannerHigh && !checkBlocks) {
			t.Errorf("%s cur=%v/%s others=[%s]: check lists %v blocks=%v, banner lists %v HIGH=%v (want %v)",
				file, curRanges, curFact, strings.Join(desc, " "), checkListed, checkBlocks, bannerListed, bannerHigh, wantHigh)
		}
	}

	curShapes := [][]gitx.LineRange{{rng(1, 5)}, nil} // edited lines / no ranges (binary, untracked here)
	for _, file := range files {
		for _, cr := range curShapes {
			for _, cf := range []string{"", "merged", "subsumed", "untracked"} {
				for _, p := range all {
					check(t, file, cr, cf, []profile{p})
				}
			}
			for _, cf := range []string{"", "merged"} {
				for _, p := range some {
					for _, q := range some {
						check(t, file, cr, cf, []profile{p, q})
					}
				}
			}
		}
	}
}

// A file this window is NOT editing has no side to take, so it grades window-
// neutral: HIGH iff some PAIR of the windows on it collides, i.e. `wt check`
// would block in BOTH of them. A window whose claim is already merged (#109),
// untracked (#113) or already on base (#122) contests nothing.
func TestGradeOverlaps_WindowNeutral(t *testing.T) {
	c := gradeTestConfig()
	ws := func(names ...string) []collide.Window {
		var out []collide.Window
		for _, n := range names {
			out = append(out, collide.Window{Worktree: "/wt/" + n, Branch: n})
		}
		return out
	}
	type wf struct {
		name                        string
		ranges                      []gitx.LineRange
		heads                       []string
		merged, untracked, subsumed bool
	}
	cases := []struct {
		name     string
		file     string
		wins     []wf
		sev      string
		cat      Category
		spans    []gitx.LineRange
		sections []string
		subsumed bool
	}{
		{name: "two windows overlapping → HIGH", file: "f.go",
			wins: []wf{{name: "x", ranges: []gitx.LineRange{rng(10, 12)}}, {name: "y", ranges: []gitx.LineRange{rng(11, 13)}}},
			sev:  "HIGH", cat: CatBlocking, spans: []gitx.LineRange{rng(11, 12)}},
		{name: "a third, disjoint window doesn't dilute it", file: "f.go",
			wins: []wf{{name: "x", ranges: []gitx.LineRange{rng(10, 12)}}, {name: "y", ranges: []gitx.LineRange{rng(11, 13)}}, {name: "z", ranges: []gitx.LineRange{rng(40, 41)}}},
			sev:  "HIGH", cat: CatBlocking, spans: []gitx.LineRange{rng(11, 12)}},
		{name: "three windows on the same lines: one span, not three", file: "f.go",
			wins: []wf{{name: "x", ranges: []gitx.LineRange{rng(10, 12)}}, {name: "y", ranges: []gitx.LineRange{rng(10, 12)}}, {name: "z", ranges: []gitx.LineRange{rng(10, 12)}}},
			sev:  "HIGH", cat: CatBlocking, spans: []gitx.LineRange{rng(10, 12)}},
		{name: "disjoint → FYI", file: "f.go",
			wins: []wf{{name: "x", ranges: []gitx.LineRange{rng(1, 2)}}, {name: "y", ranges: []gitx.LineRange{rng(9, 9)}}},
			sev:  "low", cat: CatFYI},
		{name: "uncomputable (binary) stays surfaced as HIGH", file: "f.bin",
			wins: []wf{{name: "x"}, {name: "y"}}, sev: "HIGH", cat: CatBlocking},
		{name: "#109 a window holding already-merged content contests nothing", file: "f.go",
			wins: []wf{{name: "x", merged: true}, {name: "y", ranges: []gitx.LineRange{rng(3, 4)}}}, sev: "low", cat: CatFYI},
		{name: "#113 an untracked claim contests nothing", file: "f.go",
			wins: []wf{{name: "x", untracked: true}, {name: "y", ranges: []gitx.LineRange{rng(3, 4)}}}, sev: "low", cat: CatFYI},
		// the old grade dropped the subsumed window but never re-checked the
		// remaining two, so x×z's overlap kept the file HIGH via x and y, which
		// are disjoint — rendered as "indeterminate"
		{name: "#122 the subsumed window's overlap doesn't make the rest HIGH", file: "f.go",
			wins: []wf{{name: "x", ranges: []gitx.LineRange{rng(1, 2)}}, {name: "y", ranges: []gitx.LineRange{rng(10, 11)}}, {name: "z", ranges: []gitx.LineRange{rng(1, 2)}, subsumed: true}},
			sev:  "low", cat: CatFYI, spans: []gitx.LineRange{rng(1, 2)}, subsumed: true},
		{name: "append-only → FYI", file: "CHANGELOG.md",
			wins: []wf{{name: "x", ranges: []gitx.LineRange{rng(1, 2)}}, {name: "y", ranges: []gitx.LineRange{rng(1, 2)}}}, sev: "low", cat: CatFYI},
		{name: "shared doc → advisory", file: "CLAUDE.md",
			wins: []wf{{name: "x", ranges: []gitx.LineRange{rng(1, 2)}}, {name: "y", ranges: []gitx.LineRange{rng(1, 2)}}}, sev: "low", cat: CatAdvisory},
		{name: "#22 structured doc: the pair sharing a section → HIGH", file: "PLAN.md",
			wins: []wf{{name: "x", heads: []string{"## A"}}, {name: "y", heads: []string{"## B"}}, {name: "z", heads: []string{"## B", "## C"}}},
			sev:  "HIGH", cat: CatBlocking, sections: []string{"## B"}},
		{name: "#22 structured doc, disjoint sections → advisory", file: "PLAN.md",
			wins: []wf{{name: "x", heads: []string{"## A"}}, {name: "y", heads: []string{"## B"}}}, sev: "low", cat: CatAdvisory},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			ff := newFakeFacts()
			var names []string
			for _, w := range tc.wins {
				names = append(names, w.name)
				k := factKey{"/wt/" + w.name, tc.file}
				ff.ranges[k], ff.merged[k], ff.untracked[k], ff.subsumed[k] = w.ranges, w.merged, w.untracked, w.subsumed
				if w.heads != nil {
					ff.headings[k] = w.heads
				}
			}
			ov := []collide.Overlap{{File: tc.file, Windows: names}}
			for _, self := range []collide.Self{{}, {Label: "not-a-participant", Worktree: "/wt/not-a-participant"}} {
				got := gradeOverlaps(c, ff.factory(), ws(names...), ov, nil, self)
				if len(got) != 1 {
					t.Fatalf("self=%+v: got %d overlaps", self, len(got))
				}
				o := got[0]
				if o.Severity != tc.sev || o.Category != tc.cat {
					t.Errorf("self=%+v: got %s/%s, want %s/%s (%+v)", self, o.Category, o.Severity, tc.cat, tc.sev, o)
				}
				if !reflect.DeepEqual(o.OverlapSpans, tc.spans) && !(len(o.OverlapSpans) == 0 && len(tc.spans) == 0) {
					t.Errorf("self=%+v: spans = %v, want %v", self, o.OverlapSpans, tc.spans)
				}
				if !reflect.DeepEqual(o.SharedSections, tc.sections) {
					t.Errorf("self=%+v: sections = %v, want %v", self, o.SharedSections, tc.sections)
				}
				if o.Subsumed != tc.subsumed {
					t.Errorf("self=%+v: subsumed = %v, want %v", self, o.Subsumed, tc.subsumed)
				}
				if !reflect.DeepEqual(o.Windows, names) {
					t.Errorf("self=%+v: windows = %v, want %v", self, o.Windows, names)
				}
			}
		})
	}
}

// `wt status` (window-neutral) still reports a collision between two OTHER
// windows on a file the current window also edits; only the banner, which takes
// this window's side, grades it low.
func TestGradeOverlaps_StatusVsBanner(t *testing.T) {
	c := gradeTestConfig()
	ws := scenarioWindows()
	active, _ := collide.PartitionOverlaps(collide.Overlaps(ws), scenarioLive())
	sev := func(graded []StatusOverlap, file string) string {
		for _, o := range graded {
			if o.File == file {
				return o.Severity
			}
		}
		return "absent"
	}
	status := gradeOverlaps(c, scenarioFacts().factory(), ws, active, nil, collide.Self{})
	banner := agentOverlaps(c, scenarioFacts().factory(), ws, collide.Overlaps(ws), scenarioLive(), curSelf)
	for file, want := range map[string][2]string{
		"util.go":       {"HIGH", "low"}, // h1×h2 collide; neither touches cur's lines
		"other.go":      {"HIGH", "HIGH"},
		"code.go":       {"HIGH", "HIGH"},
		"api.go":        {"HIGH", "HIGH"},
		"doc.md":        {"low", "low"}, // was HIGH in both (a-merged)
		"infra/main.tf": {"low", "low"}, // was HIGH in both (c-dormant)
	} {
		if got := [2]string{sev(status, file), sev(banner, file)}; got != want {
			t.Errorf("%s: status/banner = %v, want %v", file, got, want)
		}
	}
	for _, o := range status {
		for _, hidden := range []string{"a-merged", "b-closed", "c-dormant"} {
			if slices.Contains(o.Windows, hidden) {
				t.Errorf("status lists suppressed %s on %s", hidden, o.File)
			}
		}
	}
}

func TestAppendNewSpans(t *testing.T) {
	got := appendNewSpans([]gitx.LineRange{rng(1, 2)}, []gitx.LineRange{rng(1, 2), rng(5, 6), rng(5, 6), rng(9, 9)})
	if want := []gitx.LineRange{rng(1, 2), rng(5, 6), rng(9, 9)}; !reflect.DeepEqual(got, want) {
		t.Errorf("appendNewSpans = %v, want %v", got, want)
	}
}
