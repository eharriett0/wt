package worktree

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/eharriett0/wt/internal/config"
)

func TestShippedVerdict(t *testing.T) {
	// merged PR → shipped regardless of cherry (the #37 core)
	if !ShippedVerdict(5, false, true) {
		t.Error("merged PR should be shipped even with unshipped cherry commits")
	}
	if !ShippedVerdict(3, true, true) {
		t.Error("merged PR should be shipped even when cherry FAILED")
	}
	// no merged PR: cherry decides
	if !ShippedVerdict(0, false, false) {
		t.Error("cherry==0, no PR → shipped")
	}
	if ShippedVerdict(2, false, false) {
		t.Error("cherry!=0, no PR → not shipped")
	}
	if ShippedVerdict(0, true, false) {
		t.Error("cherry failed + no PR → can't tell → not shipped")
	}
}

func TestIsAbandonedBranch(t *testing.T) {
	wip := []string{"WIP: claim #42 — foo"}
	real := []string{"WIP: claim #42 — foo", "feat: real work"}
	cases := []struct {
		name             string
		subjects         []string
		prOpen, prMerged bool
		want             bool
	}{
		{"empty, no PR", nil, false, false, true},
		{"wip-only, no PR", wip, false, false, true},
		{"wip-only but open PR", wip, true, false, false},
		{"wip-only but merged PR", wip, false, true, false},
		{"has real commit", real, false, false, false},
	}
	for _, tc := range cases {
		if got := IsAbandonedBranch(tc.subjects, tc.prOpen, tc.prMerged); got != tc.want {
			t.Errorf("%s: IsAbandonedBranch = %v, want %v", tc.name, got, tc.want)
		}
	}
}

func TestReapVerdict(t *testing.T) {
	cases := []struct {
		name                                       string
		unshipped                                  int
		cherryFailed, prMerged, hasUpstream, grace bool
		want                                       bool
	}{
		// The #61 data-loss cases — must all be KEEP (false):
		{"fresh commitless wt new (no upstream)", 0, false, false, false, false, false},
		{"within grace window", 0, false, false, true, true, false},
		{"merged PR but still in grace", 0, false, true, true, true, false},
		{"pushed, unshipped commits", 2, false, false, true, false, false},
		{"no upstream even if cherry says 0", 0, false, false, false, false, false},
		// Legit reaps (true):
		{"merged PR, past grace", 0, false, true, true, false, true},
		{"pushed + patch-equivalent on base", 0, false, false, true, false, true},
		// cherry failed, no merged PR, has upstream → can't prove shipped → keep
		{"cherry failed, no PR", 0, true, false, true, false, false},
	}
	for _, tc := range cases {
		got := ReapVerdict(tc.unshipped, tc.cherryFailed, tc.prMerged, tc.hasUpstream, tc.grace)
		if got != tc.want {
			t.Errorf("%s: ReapVerdict = %v, want %v", tc.name, got, tc.want)
		}
	}
}

func TestStaleIndexReportable(t *testing.T) {
	cases := []struct {
		name                                 string
		prState                              string
		prOK, dirty, staleIndex, grace, want bool
	}{
		// Fires (REPORTS — never auto-removes): MERGED PR + dirty index + flag + past grace.
		{"merged + dirty index + flag → report", "MERGED", true, true, true, false, true},
		// Must NOT fire — the gates:
		{"closed PR kept on purpose → NOT reaped (even with flag)", "CLOSED", true, true, true, false, false},
		{"flag off → never (even merged+dirty)", "MERGED", true, true, false, false, false},
		{"clean index → nothing to reclaim", "MERGED", true, false, true, false, false},
		{"within grace → protected", "MERGED", true, true, true, true, false},
		{"open PR → active, keep", "OPEN", true, true, true, false, false},
		{"no PR resolved → not shippable, keep", "", false, true, true, false, false},
		{"unknown state → keep", "SOMETHING", true, true, true, false, false},
	}
	for _, tc := range cases {
		got := StaleIndexReportable(tc.prState, tc.prOK, tc.dirty, tc.staleIndex, tc.grace)
		if got != tc.want {
			t.Errorf("%s: StaleIndexReportable(%q,%v,%v,%v,%v) = %v, want %v",
				tc.name, tc.prState, tc.prOK, tc.dirty, tc.staleIndex, tc.grace, got, tc.want)
		}
	}
}

// TestManagedByClean is the #101 decision: which worktrees `wt clean` will even
// look at. The bug it fixes is a scope MISMATCH — the collision engine scans
// every worktree git knows about, so a worktree outside worktree_root could
// hard-block a push while clean refused to manage it, with no in-tool way to
// clear it. A repo whose worktree_root ever changed accumulates these forever.
func TestManagedByClean(t *testing.T) {
	dir := t.TempDir()
	root := filepath.Join(dir, "repo-worktrees")   // configured worktree_root
	legacy := filepath.Join(dir, "repo.worktrees") // an older convention, still registered with git
	inRoot := filepath.Join(root, "feat-a")
	outOfRoot := filepath.Join(legacy, "feat-b")
	for _, d := range []string{inRoot, outOfRoot} {
		if err := os.MkdirAll(d, 0o755); err != nil {
			t.Fatal(err)
		}
	}

	cases := []struct {
		name     string
		path     string
		allRoots bool
		want     bool
	}{
		{"in-root is managed by default", inRoot, false, true},
		{"in-root is managed with --all-roots too", inRoot, true, true},
		{"out-of-root is SKIPPED by default (the pre-#101 behaviour, preserved)", outOfRoot, false, false},
		{"out-of-root is managed with --all-roots (the fix)", outOfRoot, true, true},

		// A sibling directory whose name merely PREFIXES the root must not be
		// treated as inside it — "…/repo-worktrees-old" is not under
		// "…/repo-worktrees". This is what a naive strings.HasPrefix would get
		// wrong, and getting it wrong here means removing the wrong worktree.
		{"prefix-sibling is not inside the root", filepath.Join(dir, "repo-worktrees-old", "x"), false, false},

		// The root itself is not a worktree to reap; Clean skips index 0 anyway,
		// but the predicate should not claim a parent is "inside" its child.
		{"a parent of the root is not inside it", dir, false, false},
	}
	for _, tc := range cases {
		if got := ManagedByClean(tc.path, root, tc.allRoots); got != tc.want {
			t.Errorf("%s: ManagedByClean(%q, root, allRoots=%v) = %v, want %v",
				tc.name, tc.path, tc.allRoots, got, tc.want)
		}
	}
}

// TestReapableBranch guards the footgun the #101 e2e exposed: a worktree checked
// out on the BASE branch is trivially "patch-equivalent on base", so every
// downstream verdict says shipped and clean prints `git branch -D main`. It had
// been hidden only because such worktrees happened to sit outside worktree_root
// and were never evaluated — an accident, not a guard.
func TestReapableBranch(t *testing.T) {
	cases := []struct {
		branch, base string
		want         bool
	}{
		{"feat-x", "main", true},
		{"main", "main", false},    // the base branch itself — NEVER reap
		{"trunk", "trunk", false},  // base is configurable; the rule follows it
		{"main", "trunk", true},    // "main" is only special when it IS the base
		{"HEAD", "main", false},    // detached
		{"", "main", false},        // unresolvable
		{"mainline", "main", true}, // a name that merely CONTAINS the base is fine
	}
	for _, tc := range cases {
		if got := ReapableBranch(tc.branch, tc.base); got != tc.want {
			t.Errorf("ReapableBranch(%q, base=%q) = %v, want %v", tc.branch, tc.base, got, tc.want)
		}
	}
}

// Remove must keep refusing an out-of-root path — the relaxation is reachable
// only through the unexported remove() that `wt clean --all-roots` calls, so an
// unrelated caller can never widen its own blast radius by accident.
func TestRemoveStillRefusesOutOfRoot(t *testing.T) {
	dir := t.TempDir()
	root := filepath.Join(dir, "wts")
	outside := filepath.Join(dir, "elsewhere", "feat-x")
	if err := os.MkdirAll(outside, 0o755); err != nil {
		t.Fatal(err)
	}
	err := Remove(&config.Config{WorktreeRoot: root}, outside, "feat-x", false)
	if err == nil {
		t.Fatal("Remove must refuse a path outside worktree_root")
	}
	if !strings.Contains(err.Error(), "not under worktree root") {
		t.Errorf("unexpected error: %v", err)
	}
}

// TestMatchedName pins `wt clean <name>` selection (#169): a name picks a worktree
// by its directory basename, its branch, or its (real) path, and nothing else.
func TestMatchedName(t *testing.T) {
	wt := "/r/wts/feat-42-login"
	cases := []struct {
		name     string
		names    []string
		resolved []string
		branch   string
		want     string
	}{
		{"by directory", []string{"feat-42-login"}, nil, "feat-42-login", "feat-42-login"},
		{"by branch when it differs from the directory", []string{"fix-x"}, nil, "fix-x", "fix-x"},
		{"by path", []string{"../wts/feat-42-login"}, []string{"/r/wts/feat-42-login"}, "b", "../wts/feat-42-login"},
		{"a branch name with a slash still matches as a branch", []string{"bot/image-x"}, []string{"/cwd/bot/image-x"}, "bot/image-x", "bot/image-x"},
		{"a prefix is not a match", []string{"feat-42"}, nil, "feat-42-login", ""},
		{"another worktree's name", []string{"feat-43-login"}, nil, "feat-43-login-other", ""},
		{"empty names match nothing", []string{""}, nil, "", ""},
		{"first matching name is returned", []string{"nope", "feat-42-login"}, nil, "b", "feat-42-login"},
	}
	for _, c := range cases {
		if got := MatchedName(c.names, c.resolved, wt, c.branch); got != c.want {
			t.Errorf("%s: MatchedName = %q, want %q", c.name, got, c.want)
		}
	}
}

// TestCheckNames pins the name contract (#169): every name must pick out exactly
// one worktree, or the run stops with nothing cleaned. A typo (`wt clean -y
// feat-4` for feat-42) fails loudly instead of reading as "not shipped", and an
// ambiguous name, or a list with one bad name, never removes the rest.
func TestCheckNames(t *testing.T) {
	one := map[string][]string{"a": {"/w/a"}, "b": {"/w/b"}}
	if err := CheckNames([]string{"a", "b", "a", ""}, one); err != nil {
		t.Errorf("each name picks out one worktree (a repeat is not ambiguity) → %v, want nil", err)
	}
	if err := CheckNames(nil, nil); err != nil {
		t.Errorf("no names → %v, want nil", err)
	}
	cases := []struct {
		names []string
		hits  map[string][]string
		want  []string // substrings the error must carry
	}{
		{[]string{"a", "c", "e"}, one, []string{"no worktree matches c, e", "nothing was cleaned"}},
		{[]string{"c", "c"}, one, []string{"no worktree matches c (a name"}}, // named once, not "c, c"
		{[]string{"x"}, nil, []string{"no worktree matches x"}},
		{[]string{"a", "d"}, map[string][]string{"a": {"/w/a"}, "d": {"/w/d", "/w/x"}},
			[]string{"more than one worktree matches d (/w/d, /w/x)", "by its path", "nothing was cleaned"}},
		{[]string{"c", "d"}, map[string][]string{"d": {"/w/d", "/w/x"}},
			[]string{"no worktree matches c", "more than one worktree matches d"}},
	}
	for _, tc := range cases {
		err := CheckNames(tc.names, tc.hits)
		if err == nil {
			t.Errorf("CheckNames(%q) = nil, want an error", tc.names)
			continue
		}
		for _, w := range tc.want {
			if !strings.Contains(err.Error(), w) {
				t.Errorf("CheckNames(%q) = %q, want it to contain %q", tc.names, err, w)
			}
		}
	}
}

// #169: the hint is printed only after something was listed as shipped, and a
// named run echoes only the names that picked one out. The scratch-repo smoke
// found `wt clean nosuch` suggesting `wt clean -y nosuch`, a command that exits 1.
func TestRerunHint(t *testing.T) {
	cases := []struct {
		named  bool
		listed []string
		want   string
	}{
		{false, nil, ""},
		{true, nil, ""},
		{false, []string{""}, "re-run with `wt clean -y` to remove the shipped worktrees listed above"},
		{true, []string{"a", "feat/d"}, "re-run with `wt clean -y a feat/d` to remove the shipped ones listed above"},
	}
	for _, tc := range cases {
		if got := RerunHint(tc.named, tc.listed); got != tc.want {
			t.Errorf("RerunHint(%v, %q) = %q, want %q", tc.named, tc.listed, got, tc.want)
		}
	}
}
