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

// fuzzy builds a MatchFuzzy query: a `wt check` argument that names no path in
// the repo (QueryFor decides that in production).
func fuzzy(p string) []Query { return []Query{{Path: p, Mode: MatchFuzzy}} }

func TestCheckPaths(t *testing.T) {
	ws := []Window{
		{Issue: "1", Worktree: "/w/1", Touched: []string{"internal/foo.go", "main.go"}},
		{Issue: "2", Worktree: "/w/2", Touched: []string{"internal/bar.go"}},
	}
	// From window 2, about to edit internal/foo.go + README.md.
	got := CheckPaths(ws, "/w/2", ExactQueries([]string{"internal/foo.go", "README.md"}))
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
	got := CheckPaths(ws, "/w/2", fuzzy("foo.go"))
	if len(got) != 1 || got[0].Window != "#1" {
		t.Errorf("a fuzzy basename query should match a touched path, got %v", got)
	}
}

// ---- #181: a repo-root file collided with a same-named nested file ----------
//
// Branch A edits the root README.md; branch B edits only pkg/svc/README.md.
// "README.md" is a suffix of "pkg/svc/README.md" — a root path has no "/" to
// anchor on — so `wt check README.md` reported HIGH against B and the pre-push
// hook blocked A's push, for a file B never touched. The suffix/basename tiers
// are now fuzzy-only, and only a query that names no real path is fuzzy.

func TestCheckPaths_ExactMatchTable(t *testing.T) {
	cases := []struct {
		name    string
		touched []string // the OTHER window's touched set
		q       Query
		want    []string // matched files, sorted; nil = no collision
	}{
		{"root file vs nested namesake (THE #181 bug)",
			[]string{"pkg/svc/README.md"}, Query{Path: "README.md"}, nil},
		{"nested vs a different nested namesake",
			[]string{"pkg/svc/README.md"}, Query{Path: "docs/adr/README.md"}, nil},
		{"a path with a directory component is not a suffix search",
			[]string{"pkg/svc/README.md"}, Query{Path: "svc/README.md"}, nil},
		{"root Makefile vs nested Makefile",
			[]string{"tools/Makefile"}, Query{Path: "Makefile"}, nil},
		{"root directory vs a nested namesake directory",
			[]string{"site/docs/x.md"}, Query{Path: "docs"}, nil},
		{"positive control: the same root file",
			[]string{"README.md", "pkg/svc/README.md"}, Query{Path: "README.md"}, []string{"README.md"}},
		{"positive control: the same nested file",
			[]string{"pkg/svc/README.md"}, Query{Path: "pkg/svc/README.md"}, []string{"pkg/svc/README.md"}},
		{"positive control: a root directory still expands",
			[]string{"docs/x.md", "site/docs/y.md"}, Query{Path: "docs/"}, []string{"docs/x.md"}},
		{"fuzzy: a bare name for a nested file still matches",
			[]string{"internal/foo.go"}, Query{Path: "foo.go", Mode: MatchFuzzy}, []string{"internal/foo.go"}},
		{"fuzzy: a partial path still suffix-matches on a segment boundary",
			[]string{"pkg/svc/README.md"}, Query{Path: "svc/README.md", Mode: MatchFuzzy}, []string{"pkg/svc/README.md"}},
		{"fuzzy: a bare directory name still reaches a nested directory",
			[]string{"site/docs/x.md"}, Query{Path: "docs", Mode: MatchFuzzy}, []string{"site/docs/x.md"}},
		{"fuzzy: still on a segment boundary",
			[]string{"pkg/svc/NOTREADME.md"}, Query{Path: "README.md", Mode: MatchFuzzy}, nil},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			ws := []Window{
				{Branch: "feat-b", Worktree: "/w/b", Touched: tc.touched},
				{Branch: "feat-a", Worktree: "/w/a"},
			}
			var got []string
			for _, cf := range CheckPaths(ws, "/w/a", []Query{tc.q}) {
				got = append(got, cf.MatchedFile)
			}
			if !reflect.DeepEqual(got, tc.want) {
				t.Errorf("CheckPaths(%+v) matched %v, want %v", tc.q, got, tc.want)
			}
		})
	}
}

func TestCheckPaths_NamesTheFileThatActuallyCollides(t *testing.T) {
	// The pre-push hook sends BOTH outgoing files. Fuzzy matching resolved
	// README.md to pkg/svc/README.md first, then deduped the real entry away, so
	// the block message blamed the root README.md and never named the file that
	// actually collides.
	ws := []Window{
		{Branch: "feat-b", Worktree: "/w/b", Touched: []string{"pkg/svc/README.md"}},
		{Branch: "feat-a", Worktree: "/w/a"},
	}
	got := CheckPaths(ws, "/w/a", ExactQueries([]string{"README.md", "pkg/svc/README.md"}))
	want := []Conflict{{Path: "pkg/svc/README.md", Window: "feat-b", MatchedFile: "pkg/svc/README.md"}}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("CheckPaths =\n%#v\nwant\n%#v", got, want)
	}
}

func TestQueryFor(t *testing.T) {
	ws := []Window{
		{Branch: "feat-b", Worktree: "/w/b", Touched: []string{
			"pkg/svc/README.md", "internal/foo.go", "NEW.md", "envs/app/netpol.yaml",
		}},
	}
	cases := []struct {
		name   string
		arg    string
		rel    string // the arg read relative to the cwd; "" = names no location in the repo
		onDisk bool
		want   Query
	}{
		{"root file that exists, typed at the root",
			"README.md", "README.md", true, Query{"README.md", MatchExact}},
		{"same name typed in pkg/svc/ names pkg/svc/README.md",
			"README.md", "pkg/svc/README.md", true, Query{"pkg/svc/README.md", MatchExact}},
		{"subdirectory-relative path typed in pkg/",
			"svc/README.md", "pkg/svc/README.md", true, Query{"pkg/svc/README.md", MatchExact}},
		{"../ out of a subdirectory names the root file",
			"../README.md", "README.md", true, Query{"README.md", MatchExact}},
		{"a path deleted here but still tracked is real",
			"gone.go", "gone.go", true, Query{"gone.go", MatchExact}},
		{"a file that exists only on another branch, at that exact path",
			"NEW.md", "NEW.md", false, Query{"NEW.md", MatchExact}},
		{"a directory that exists only on another branch",
			"envs/app/", "envs/app", false, Query{"envs/app", MatchExact}},
		{"bare name that is no path here stays a fuzzy search",
			"foo.go", "foo.go", false, Query{"foo.go", MatchFuzzy}},
		{"a fuzzy search keeps the argument as typed, not the cwd-joined path",
			"foo.go", "pkg/foo.go", false, Query{"foo.go", MatchFuzzy}},
		{"partial path that names nothing here stays a fuzzy search (#154)",
			"configs/kiali", "configs/kiali", false, Query{"configs/kiali", MatchFuzzy}},
		{"a path that leaves the repo can't be exact",
			"../../etc/hosts", "", true, Query{"../../etc/hosts", MatchFuzzy}},
		{"surrounding whitespace is trimmed",
			"  foo.go ", "", false, Query{"foo.go", MatchFuzzy}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := QueryFor(tc.arg, tc.rel, tc.onDisk, ws); got != tc.want {
				t.Errorf("QueryFor(%q, %q, %v) = %+v, want %+v", tc.arg, tc.rel, tc.onDisk, got, tc.want)
			}
		})
	}
}

func TestQueryFor_IssueScenarioEndToEnd(t *testing.T) {
	// #181 as reported, at the pure layer: A edits the root README.md, B (open
	// PR) edits only pkg/svc/README.md.
	ws := []Window{
		{Branch: "feat-a", Worktree: "/w/a", Touched: []string{"README.md"}},
		{Branch: "feat-b", Worktree: "/w/b", Touched: []string{"pkg/svc/README.md", "internal/foo.go"}},
	}
	check := func(arg, rel string, onDisk bool) int {
		return len(CheckPaths(ws, "/w/a", []Query{QueryFor(arg, rel, onDisk, ws)}))
	}
	if n := check("README.md", "README.md", true); n != 0 {
		t.Errorf("wt check README.md (root, exists) = %d collision(s), want 0", n)
	}
	if n := check("pkg/svc/README.md", "pkg/svc/README.md", true); n != 1 {
		t.Errorf("control: wt check pkg/svc/README.md = %d, want 1", n)
	}
	if n := check("README.md", "pkg/svc/README.md", true); n != 1 {
		t.Errorf("control: wt check README.md from pkg/svc/ = %d, want 1", n)
	}
	if n := check("foo.go", "foo.go", false); n != 1 {
		t.Errorf("control: bare foo.go (no root foo.go) must stay fuzzy = %d, want 1", n)
	}
}

func TestQueryFor_RealPathIsWhatTheHooksAsk(t *testing.T) {
	// The hook block predicate MUST equal `wt check` (#92). The hooks match
	// git's repo-relative paths exactly, so `wt check` must ask the same
	// question for a path that exists, or the two disagree and one gets
	// bypassed.
	for _, rel := range []string{"README.md", "pkg/svc/README.md", "go.mod"} {
		if got, hook := QueryFor(rel, rel, true, nil), ExactQueries([]string{rel})[0]; got != hook {
			t.Errorf("wt check %s = %+v, hooks ask %+v", rel, got, hook)
		}
	}
}

func TestMatchMode_ZeroValueIsExact(t *testing.T) {
	// A query whose mode was never decided must not be able to invent a
	// collision: the zero value is the strict mode.
	if (Query{}).Mode != MatchExact {
		t.Fatal("the zero MatchMode must be MatchExact")
	}
	ws := []Window{{Worktree: "/w/b", Touched: []string{"pkg/svc/README.md"}}}
	if got := CheckPaths(ws, "/w/a", []Query{{Path: "README.md"}}); len(got) != 0 {
		t.Errorf("an undecided query matched by suffix: %v", got)
	}
}

// ---- #154: a DIRECTORY argument used to match nothing and report "clear" ----
//
// The bug was reported to me as "wt check cannot see uncommitted work". It could;
// the control that isolated the real cause was re-running the SAME check with the
// other window's work COMMITTED, which still reported clear. Commit state was
// never the variable — the requested path being a directory was. These tests are
// written at the matching layer, where that distinction lives, so they cannot be
// satisfied by anything about staged vs committed.

func TestCheckPaths_DirectoryExpandsToFilesBeneathIt(t *testing.T) {
	ws := []Window{
		{Issue: "1", Worktree: "/w/1", Touched: []string{
			"envs/app/netpol.yaml", "envs/app/kustomization.yaml", "docs/x.md",
		}},
		{Issue: "2", Worktree: "/w/2", Touched: nil},
	}
	got := CheckPaths(ws, "/w/2", ExactQueries([]string{"envs/app/"}))
	want := []Conflict{
		{Path: "envs/app/kustomization.yaml", Window: "#1", MatchedFile: "envs/app/kustomization.yaml"},
		{Path: "envs/app/netpol.yaml", Window: "#1", MatchedFile: "envs/app/netpol.yaml"},
	}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("directory must expand to the files beneath it =\n%#v\nwant\n%#v", got, want)
	}
}

func TestCheckPaths_DirectoryWithoutTrailingSlash(t *testing.T) {
	// How anyone actually types it. Reporting differently from the slashed form
	// would be its own small trap.
	ws := []Window{
		{Issue: "1", Worktree: "/w/1", Touched: []string{"envs/app/netpol.yaml"}},
		{Issue: "2", Worktree: "/w/2", Touched: nil},
	}
	if got := CheckPaths(ws, "/w/2", ExactQueries([]string{"envs/app"})); len(got) != 1 {
		t.Errorf("unslashed directory should match, got %v", got)
	}
}

func TestCheckPaths_DirectoryMatchesOnSegmentBoundaryOnly(t *testing.T) {
	// THE FALSE-POSITIVE CONTROL. A bare prefix test would make "envs/app" match
	// "envs/application/x.yaml" — a collision invented in a sibling directory. A
	// false HIGH on a safety tool is how people learn to pass --bypass.
	ws := []Window{
		{Issue: "1", Worktree: "/w/1", Touched: []string{"envs/application/x.yaml", "envs/app-2/y.yaml"}},
		{Issue: "2", Worktree: "/w/2", Touched: nil},
	}
	for _, mode := range []MatchMode{MatchExact, MatchFuzzy} {
		if got := CheckPaths(ws, "/w/2", []Query{{Path: "envs/app", Mode: mode}}); len(got) != 0 {
			t.Errorf("mode %d: sibling directories must not match, got %v", mode, got)
		}
	}
}

func TestCheckPaths_DirectorySuffixMatch(t *testing.T) {
	// Mirrors the existing basename tier one level up: `wt check kiali` should
	// reach a nested configs/kiali/ the way `wt check foo.go` reaches
	// internal/foo.go.
	ws := []Window{
		{Issue: "1", Worktree: "/w/1", Touched: []string{"envs/landru/configs/kiali/netpol.yaml"}},
		{Issue: "2", Worktree: "/w/2", Touched: nil},
	}
	if got := CheckPaths(ws, "/w/2", fuzzy("kiali")); len(got) != 1 {
		t.Errorf("directory suffix should match a nested dir, got %v", got)
	}
	// #181: only for a fuzzy query. An exact "kiali" is a ROOT directory.
	if got := CheckPaths(ws, "/w/2", ExactQueries([]string{"kiali"})); len(got) != 0 {
		t.Errorf("an exact root directory must not reach a nested namesake, got %v", got)
	}
}

func TestCheckPaths_DirectoryAndFileUnderItDoNotDoubleReport(t *testing.T) {
	ws := []Window{
		{Issue: "1", Worktree: "/w/1", Touched: []string{"envs/app/netpol.yaml"}},
		{Issue: "2", Worktree: "/w/2", Touched: nil},
	}
	got := CheckPaths(ws, "/w/2", ExactQueries([]string{"envs/app/", "envs/app/netpol.yaml"}))
	if len(got) != 1 {
		t.Errorf("dir + file under it must dedupe to one entry, got %v", got)
	}
}

func TestCheckPaths_DirectoryStillExcludesOwnWorktree(t *testing.T) {
	ws := []Window{{Issue: "1", Worktree: "/w/1", Touched: []string{"envs/app/netpol.yaml"}}}
	if got := CheckPaths(ws, "/w/1", ExactQueries([]string{"envs/app/"})); len(got) != 0 {
		t.Errorf("own worktree must stay excluded for a directory, got %v", got)
	}
}

func TestCheckPaths_ExactFileMatchIsUnchangedByDirectorySupport(t *testing.T) {
	// Regression guard: the directory tier runs only when no file matched, so an
	// exact/basename request must keep reporting the requested path in Path.
	ws := []Window{
		{Issue: "1", Worktree: "/w/1", Touched: []string{"internal/foo.go"}},
		{Issue: "2", Worktree: "/w/2", Touched: nil},
	}
	got := CheckPaths(ws, "/w/2", fuzzy("foo.go"))
	want := []Conflict{{Path: "foo.go", Window: "#1", MatchedFile: "internal/foo.go"}}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("file matching changed =\n%#v\nwant\n%#v", got, want)
	}
}

func TestMatchTouchedDir_RootIsNotAQuestion(t *testing.T) {
	// "/" or "" would expand to the entire repo and report every file in it.
	touched := []string{"a/b.go", "c/d.go"}
	for _, p := range []string{"", "/", "   "} {
		for _, mode := range []MatchMode{MatchExact, MatchFuzzy} {
			if got := matchTouchedDir(Query{Path: p, Mode: mode}, touched); len(got) != 0 {
				t.Errorf("matchTouchedDir(%q, mode %d) = %v, want none", p, mode, got)
			}
		}
	}
}

func TestPathTouchedByAny_KnowsADirectory(t *testing.T) {
	// #93's typo guard must not call a directory that exists only on another
	// window's branch a nonexistent path.
	ws := []Window{{Issue: "1", Worktree: "/w/1", Touched: []string{"envs/app/netpol.yaml"}}}
	if !PathTouchedByAny(Query{Path: "envs/app/"}, ws) {
		t.Error("a touched directory must count as a real path")
	}
	if PathTouchedByAny(Query{Path: "envs/nope/"}, ws) {
		t.Error("an untouched directory must not")
	}
}

func TestCheckPaths_ExcludesOwnWorktree(t *testing.T) {
	ws := []Window{{Issue: "1", Worktree: "/w/1", Touched: []string{"a.go"}}}
	if got := CheckPaths(ws, "/w/1", ExactQueries([]string{"a.go"})); len(got) != 0 {
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
		// #73: squash-merged-and-deleted branch — Unshipped>0 (ancestry broken) but
		// a MERGED PR exists ⇒ stale (suppressed), not a false HIGH collision.
		{"merged PR + unshipped (squash) → stale", LiveFacts{Merged: true, Unshipped: 3, PRChecked: true}, 0, LiveStale},
		{"merged wins over dormancy age", LiveFacts{Merged: true, Unshipped: 3, Age: 30 * day, PRChecked: true}, day, LiveStale},
		// #79 comment 2 (REVERSES #73's original dirty>merged): PR state outranks a
		// leftover dirty index. A merged/closed branch with staged cruft `wt clean`
		// can't remove must NOT read HIGH — the dirtiness rides the label, not the level.
		{"merged beats dirty (leftover index, not live work)", LiveFacts{Merged: true, Dirty: true, Unshipped: 3}, 0, LiveStale},
		// #168: a MERGED PR found only by the branch's TIP commit. Clean ⇒ shipped,
		// like a name match. Dirty ⇒ still live: a follow-up branch started at a
		// merged PR's head looks identical, and its uncommitted edits are new work.
		{"merged by tip, clean → stale", LiveFacts{Merged: true, ViaTip: true, Unshipped: 3, PRChecked: true}, 0, LiveStale},
		{"merged by tip does NOT beat dirty", LiveFacts{Merged: true, ViaTip: true, Dirty: true, Unshipped: 3, PRChecked: true}, 0, LiveDirty},
		{"merged by tip, dirty, never dormant", LiveFacts{Merged: true, ViaTip: true, Dirty: true, Unshipped: 3, Age: 30 * day, PRChecked: true}, day, LiveDirty},
		{"open PR beats merged (shouldn't co-occur, but PR wins)", LiveFacts{Merged: true, HasOpenPR: true}, 0, LiveOpenPR},
		// #79: CLOSED-unmerged PR ⇒ suppressed (LiveClosedPR), even with unshipped
		// commits + a dirty index (the branch is kept on purpose). Open/merged win above.
		{"closed PR + unshipped → closed (suppressed)", LiveFacts{ClosedPR: true, Unshipped: 3, PRChecked: true}, 0, LiveClosedPR},
		{"closed PR beats dirty (leftover index)", LiveFacts{ClosedPR: true, Dirty: true, Unshipped: 3}, 0, LiveClosedPR},
		{"merged beats closed (defensive; shouldn't co-occur)", LiveFacts{Merged: true, ClosedPR: true}, 0, LiveStale},
		{"open PR beats closed", LiveFacts{ClosedPR: true, HasOpenPR: true}, 0, LiveOpenPR},
		// no PR resolved + dirty ⇒ genuinely live editing, still HIGH.
		{"dirty, no PR → dirty (live work)", LiveFacts{Dirty: true, Unshipped: 3, PRChecked: true}, 0, LiveDirty},
		// #87 (post-review SAFETY invariant): ClassifyFacts has NO base-drift
		// branch. A dirty base checkout — however far behind — stays LiveDirty
		// (HIGH), because its uncommitted edits could be real work and hiding a
		// dirty collision on the default path would violate "never hide a real
		// collision". The behind-count only enriches the label (see Label test) +
		// drives the wt doctor probe; it never suppresses. A resolved merged/closed
		// PR still outranks dirty (that's the #79 leftover-index case, above).
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
	// Only LiveStale is the definitively-merged case; the rest are not.
	for l, wantStale := range map[Liveness]bool{
		LiveUnknown:  false,
		LiveStale:    true,
		LiveUnmerged: false,
		LiveDirty:    false,
		LiveOpenPR:   false,
		LiveClosedPR: false, // closed is suppressed but not "stale/merged"
	} {
		if got := l.IsStale(); got != wantStale {
			t.Errorf("%v.IsStale() = %v, want %v", l, got, wantStale)
		}
	}
}

func TestLiveness_IsSuppressed(t *testing.T) {
	// Suppressed = dropped from HIGH/exit-3: merged (stale), dormant, closed (#79).
	for l, want := range map[Liveness]bool{
		LiveStale:    true,
		LiveDormant:  true,
		LiveClosedPR: true,
		LiveUnmerged: false,
		LiveDirty:    false,
		LiveOpenPR:   false,
		LiveUnknown:  false, // ambiguity is never suppressed
	} {
		if got := l.IsSuppressed(); got != want {
			t.Errorf("%v.IsSuppressed() = %v, want %v", l, got, want)
		}
	}
}

func TestWindowLiveness_Label(t *testing.T) {
	cases := []struct {
		name string
		wl   WindowLiveness
		want string
	}{
		{"merged", WindowLiveness{Level: LiveStale, MergedPR: "84"}, "merged #84"},
		{"closed PR", WindowLiveness{Level: LiveClosedPR, ClosedPR: "1654"}, "PR #1654 closed"},
		// #79 comment 2: a shipped/closed branch with a leftover dirty index says WHY
		// it's still only an FYI, so a reader doesn't re-investigate it as live work.
		{"merged + leftover index", WindowLiveness{Level: LiveStale, MergedPR: "393", Dirty: true},
			"merged #393 · leftover uncommitted edits"},
		{"closed + leftover index", WindowLiveness{Level: LiveClosedPR, ClosedPR: "1654", Dirty: true},
			"PR #1654 closed · leftover uncommitted edits"},
		{"no PR opened (still HIGH)", WindowLiveness{Level: LiveUnmerged}, "commits, no PR opened"},
		// a plain dirty (no PR) window does NOT get the leftover-edits note.
		{"dirty live work", WindowLiveness{Level: LiveDirty, Dirty: true}, "uncommitted edits"},
		// #87: a far-behind dirty base checkout STAYS LiveDirty (HIGH, never
		// hidden) — the behind-count + "likely stale" only enrich the label so a
		// reader dismisses probable rot fast.
		{"dirty base far behind (HIGH + likely stale)", WindowLiveness{Level: LiveDirty, Dirty: true, BehindBase: 144},
			"uncommitted edits · 144 behind base — likely stale"},
		// below the threshold: note the drift but no "likely stale".
		{"dirty base slightly behind", WindowLiveness{Level: LiveDirty, Dirty: true, BehindBase: 3},
			"uncommitted edits · 3 behind base"},
		// #168: dirty, with commits merged under another branch name (found by the
		// tip). Still HIGH; the label names the PR so only the edits need a look.
		{"dirty + commits merged by tip", WindowLiveness{Level: LiveDirty, Dirty: true, MergedPR: "1105"},
			"uncommitted edits · its commits merged in #1105"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if got := c.wl.Label(); got != c.want {
				t.Errorf("Label() = %q, want %q", got, c.want)
			}
		})
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

func TestPathTouchedByAny(t *testing.T) {
	ws := []Window{
		{Worktree: "/w/1", Touched: []string{"internal/foo.go", "docs/x.md"}},
		{Worktree: "/w/2", Touched: []string{"cmd/main.go"}},
	}
	// exact + suffix + basename all match a touched file (#93 real-path signal)...
	for _, p := range []string{"internal/foo.go", "foo.go", "docs/x.md", "cmd/main.go", "main.go"} {
		if !PathTouchedByAny(Query{Path: p, Mode: MatchFuzzy}, ws) {
			t.Errorf("PathTouchedByAny(fuzzy %q) = false, want true", p)
		}
	}
	// ...but suffix and basename only for a fuzzy query (#181).
	for p, want := range map[string]bool{"internal/foo.go": true, "cmd/main.go": true, "foo.go": false, "main.go": false} {
		if got := PathTouchedByAny(Query{Path: p}, ws); got != want {
			t.Errorf("PathTouchedByAny(exact %q) = %v, want %v", p, got, want)
		}
	}
	// a typo / genuinely-untouched path matches nothing.
	for _, p := range []string{"internal/fooo.go", "nope/typo.go", "", "  "} {
		if PathTouchedByAny(Query{Path: p, Mode: MatchFuzzy}, ws) {
			t.Errorf("PathTouchedByAny(%q) = true, want false", p)
		}
	}
}
