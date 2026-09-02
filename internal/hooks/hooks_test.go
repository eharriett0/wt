package hooks

import (
	"os"
	"path/filepath"
	"reflect"
	"testing"

	"github.com/eharriett0/wt/internal/collide"
	"github.com/eharriett0/wt/internal/config"
	"github.com/eharriett0/wt/internal/gitx"
)

func rng(a, b int) gitx.LineRange { return gitx.LineRange{Start: a, End: b} }

const testRoot = "/wt/self" // the committing/pushing worktree

// stubRanges returns cur for the current worktree (testRoot), other for anyone
// else — so gradeConflicts' hunk-level decision is exercised without a live repo.
func stubRanges(cur, other []gitx.LineRange) rangeFn {
	return func(worktree, base, path string) []gitx.LineRange {
		if worktree == testRoot {
			return cur
		}
		return other
	}
}

func TestGradeConflicts(t *testing.T) {
	ws := []collide.Window{{Branch: "winA", Worktree: "/wt/A"}} // Label() == "winA"
	cases := []struct {
		name       string
		cfg        *config.Config
		conflict   collide.Conflict
		cur, other []gitx.LineRange
		wantHard   bool
	}{
		{
			"overlapping hunks are hard (block)",
			&config.Config{Base: "main"},
			collide.Conflict{Path: "foo.go", Window: "winA"},
			[]gitx.LineRange{rng(1, 5)}, []gitx.LineRange{rng(3, 7)}, true,
		},
		{
			"disjoint hunks in the same file are advisory",
			&config.Config{Base: "main"},
			collide.Conflict{Path: "foo.go", Window: "winA"},
			[]gitx.LineRange{rng(1, 2)}, []gitx.LineRange{rng(10, 11)}, false,
		},
		{
			"shared doc is advisory even when ranges overlap",
			&config.Config{Base: "main", SharedDocs: []string{"CLAUDE.md"}},
			collide.Conflict{Path: "CLAUDE.md", Window: "winA"},
			[]gitx.LineRange{rng(1, 5)}, []gitx.LineRange{rng(1, 5)}, false,
		},
		{
			"append-only path is advisory even when ranges overlap",
			&config.Config{Base: "main", AppendOnlyPaths: []string{"CHANGELOG.md"}},
			collide.Conflict{Path: "CHANGELOG.md", Window: "winA"},
			[]gitx.LineRange{rng(1, 5)}, []gitx.LineRange{rng(1, 5)}, false,
		},
		{
			"empty current side stays hard (fail-safe — can't prove disjoint)",
			&config.Config{Base: "main"},
			collide.Conflict{Path: "new.go", Window: "winA"},
			nil, []gitx.LineRange{rng(1, 2)}, true,
		},
		{
			"empty other side stays hard (fail-safe)",
			&config.Config{Base: "main"},
			collide.Conflict{Path: "foo.go", Window: "winA"},
			[]gitx.LineRange{rng(1, 2)}, nil, true,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			hard, soft := gradeConflicts(tc.cfg, []collide.Conflict{tc.conflict}, testRoot, ws, stubRanges(tc.cur, tc.other), stubRanges(tc.cur, tc.other))
			gotHard := len(hard) == 1 && len(soft) == 0
			gotSoft := len(soft) == 1 && len(hard) == 0
			if tc.wantHard && !gotHard {
				t.Fatalf("want 1 hard / 0 soft, got %d hard / %d soft", len(hard), len(soft))
			}
			if !tc.wantHard && !gotSoft {
				t.Fatalf("want 0 hard / 1 soft, got %d hard / %d soft", len(hard), len(soft))
			}
		})
	}
}

// The range lookup must use the OTHER window's actual touched file (MatchedFile),
// not the requested basename — so basename/suffix matches on nested files still
// grade at the right path.
func TestGradeConflicts_UsesMatchedFileForRangeLookup(t *testing.T) {
	ws := []collide.Window{{Branch: "winA", Worktree: "/wt/A"}}
	var gotPaths []string
	rf := func(worktree, base, path string) []gitx.LineRange {
		gotPaths = append(gotPaths, path)
		return []gitx.LineRange{rng(1, 3)}
	}
	cf := collide.Conflict{Path: "foo.go", Window: "winA", MatchedFile: "pkg/nested/foo.go"}
	gradeConflicts(&config.Config{Base: "main"}, []collide.Conflict{cf}, testRoot, ws, rf, rf)
	if len(gotPaths) == 0 {
		t.Fatal("range fn never called")
	}
	for _, p := range gotPaths {
		if p != "pkg/nested/foo.go" {
			t.Fatalf("range lookup used %q, want the MatchedFile pkg/nested/foo.go", p)
		}
	}
}

// A file touched by several windows must count once per (path, window), and the
// blocking-file count must be distinct paths — this is what keeps a 5-file
// commit from reporting 88 (#92 / #97).
func TestDistinctPaths(t *testing.T) {
	in := []collide.Conflict{
		{Path: "a", Window: "x"}, {Path: "a", Window: "y"}, {Path: "b", Window: "x"}, {Path: "a", Window: "x"},
	}
	if got, want := distinctPaths(in), []string{"a", "b"}; !reflect.DeepEqual(got, want) {
		t.Fatalf("distinctPaths = %v, want %v", got, want)
	}
}

func TestDedupConflicts(t *testing.T) {
	in := []collide.Conflict{
		{Path: "a", Window: "x"}, {Path: "a", Window: "x"}, {Path: "a", Window: "y"},
	}
	got := dedupConflicts(in)
	if len(got) != 2 { // (a,x) and (a,y) — the duplicate (a,x) collapses
		t.Fatalf("dedupConflicts len = %d, want 2", len(got))
	}
}

// TestOutgoingFrom is the #106 regression: which ref the pushed range is
// measured FROM.
//
// The bug was that a NON-fast-forward push — plain `git rebase origin/main &&
// git push --force-with-lease` — kept using the old remote head, so the range
// walked through the base's commits and reported every file the BASE gained as
// outgoing. The hook then blocked on files the pusher had never touched, it
// contradicted `wt check` on identical input, and it got worse the further
// behind the branch had been. Rebasing onto current main is what armed it.
func TestOutgoingFrom(t *testing.T) {
	const zero = "0000000000000000000000000000000000000000"
	cases := []struct {
		name             string
		remoteSHA        string
		remoteIsAncestor bool
		resolvedBase     string
		want             string
	}{
		{
			// Fast-forward: remote..local is EXACTLY the new commits, and is more
			// precise than base..local. Must not regress to the base.
			"fast-forward uses the remote head", "abc123", true, "refs/remotes/origin/main", "abc123",
		},
		{
			"brand-new branch measures against the resolved base", zero, false, "refs/remotes/origin/main", "refs/remotes/origin/main",
		},
		{
			// THE FIX.
			"force-push after rebase measures against the base, not the stale remote head",
			"abc123", false, "refs/remotes/origin/main", "refs/remotes/origin/main",
		},
		{
			// A remote sha we cannot prove is an ancestor is treated as non-ff.
			// Failing that way round is right: it over-scopes to the branch's own
			// diff, rather than inventing files from unrelated history.
			"unprovable ancestry falls back to the base", "deadbeef", false, "refs/remotes/origin/main", "refs/remotes/origin/main",
		},
		{
			"no resolvable remote base falls back to the bare base name", zero, false, "", "main",
		},
		{
			"force-push with no resolvable remote base still uses the base name", "abc123", false, "", "main",
		},
	}
	for _, tc := range cases {
		if got := outgoingFrom("main", tc.remoteSHA, tc.remoteIsAncestor, tc.resolvedBase); got != tc.want {
			t.Errorf("%s: outgoingFrom(base=main, remote=%q, isAncestor=%v, resolved=%q) = %q, want %q",
				tc.name, tc.remoteSHA, tc.remoteIsAncestor, tc.resolvedBase, got, tc.want)
		}
	}
}

// structuredDoc is laid out so line numbers map to known sections:
//
//	1 "# Title"   2 "intro"   3 ""        → preamble ""
//	4 "## Alpha"  5 "a1"      6 "a2"  7 "" → "## Alpha"
//	8 "## Beta"   9 "b1"     10 "b2"       → "## Beta"
const structuredDoc = "# Title\nintro\n\n## Alpha\na1\na2\n\n## Beta\nb1\nb2\n"

// twoWorktrees writes the same structured doc into a self/ and other/ dir and
// returns their paths, so the section grade runs against real files.
func twoWorktrees(t *testing.T) (self, other string) {
	t.Helper()
	dir := t.TempDir()
	self, other = filepath.Join(dir, "self"), filepath.Join(dir, "other")
	for _, wt := range []string{self, other} {
		if err := os.MkdirAll(wt, 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(wt, "CLAUDE.md"), []byte(structuredDoc), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	return self, other
}

// TestGradeConflictsStructuredDoc is the #98 regression.
//
// Before the fix the hooks stopped at "shared doc → advisory", so the ONE case a
// structured_doc is configured to catch — two windows editing the same lane of a
// hand-merged doc — blocked in `wt check` and sailed through the pre-push guard.
//
// Note the ranges in the same-section case are DISJOINT (line 5 vs line 6). That
// is deliberate: it proves the SECTION grade is what fires, not the hunk grade,
// which would call disjoint lines advisory.
func TestGradeConflictsStructuredDoc(t *testing.T) {
	self, other := twoWorktrees(t)
	ws := []collide.Window{{Branch: "winA", Worktree: other}}
	conflict := collide.Conflict{Path: "CLAUDE.md", Window: "winA"}

	cases := []struct {
		name       string
		structured map[string]string
		cur, other []gitx.LineRange
		wantHard   bool
	}{
		{
			"same section is HARD even when the line ranges are disjoint",
			map[string]string{"CLAUDE.md": "^## "},
			[]gitx.LineRange{rng(5, 5)}, []gitx.LineRange{rng(6, 6)}, true,
		},
		{
			"disjoint sections stay advisory",
			map[string]string{"CLAUDE.md": "^## "},
			[]gitx.LineRange{rng(5, 5)}, []gitx.LineRange{rng(9, 9)}, false,
		},
		{
			"both in the preamble is HARD (the preamble is a real lane)",
			map[string]string{"CLAUDE.md": "^## "},
			[]gitx.LineRange{rng(1, 1)}, []gitx.LineRange{rng(2, 2)}, true,
		},
		{
			"NOT configured as structured — blanket shared-doc advisory as before",
			nil,
			[]gitx.LineRange{rng(5, 5)}, []gitx.LineRange{rng(5, 5)}, false,
		},
		{
			"unparseable delimiter falls back to advisory, never to a hard block",
			map[string]string{"CLAUDE.md": "^## ("},
			[]gitx.LineRange{rng(5, 5)}, []gitx.LineRange{rng(5, 5)}, false,
		},
		{
			"a delimiter matching a DIFFERENT doc leaves this one advisory",
			map[string]string{"MEMORY.md": "^## "},
			[]gitx.LineRange{rng(5, 5)}, []gitx.LineRange{rng(5, 5)}, false,
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			c := &config.Config{Base: "main", SharedDocs: []string{"CLAUDE.md"}, StructuredDocs: tc.structured}
			ranges := func(worktree, base, path string) []gitx.LineRange {
				if worktree == self {
					return tc.cur
				}
				return tc.other
			}
			hard, soft := gradeConflicts(c, []collide.Conflict{conflict}, self, ws, ranges, ranges)
			gotHard := len(hard) == 1
			if gotHard != tc.wantHard {
				t.Fatalf("hard=%v want %v (hard=%d soft=%d)", gotHard, tc.wantHard, len(hard), len(soft))
			}
			if len(hard)+len(soft) != 1 {
				t.Fatalf("conflict must be graded exactly once: hard=%d soft=%d", len(hard), len(soft))
			}
		})
	}
}

// A structured doc that exists in NEITHER worktree cannot be section-graded, so
// it must fall back to the blanket advisory rather than fail into a hard block —
// the out-of-repo memory-doc case.
func TestGradeConflictsStructuredDocUngradable(t *testing.T) {
	dir := t.TempDir()
	self, other := filepath.Join(dir, "self"), filepath.Join(dir, "other")
	c := &config.Config{
		Base:           "main",
		SharedDocs:     []string{"CLAUDE.md"},
		StructuredDocs: map[string]string{"CLAUDE.md": "^## "},
	}
	ws := []collide.Window{{Branch: "winA", Worktree: other}}
	hard, soft := gradeConflicts(c,
		[]collide.Conflict{{Path: "CLAUDE.md", Window: "winA"}}, self, ws,
		stubRanges([]gitx.LineRange{rng(5, 5)}, []gitx.LineRange{rng(5, 5)}),
		stubRanges([]gitx.LineRange{rng(5, 5)}, []gitx.LineRange{rng(5, 5)}),
	)
	if len(hard) != 0 || len(soft) != 1 {
		t.Fatalf("ungradable structured doc must stay advisory: hard=%d soft=%d", len(hard), len(soft))
	}
}

// An append-only path must stay advisory even if it is ALSO named as a
// structured doc — the section grade must not promote it past append-only.
func TestGradeConflictsAppendOnlyBeatsSection(t *testing.T) {
	self, other := twoWorktrees(t)
	c := &config.Config{
		Base:            "main",
		AppendOnlyPaths: []string{"CLAUDE.md"},
		StructuredDocs:  map[string]string{"CLAUDE.md": "^## "},
	}
	ws := []collide.Window{{Branch: "winA", Worktree: other}}
	ranges := func(worktree, base, path string) []gitx.LineRange { return []gitx.LineRange{rng(5, 5)} }
	hard, soft := gradeConflicts(c, []collide.Conflict{{Path: "CLAUDE.md", Window: "winA"}}, self, ws, ranges, ranges)
	if len(hard) != 0 || len(soft) != 1 {
		t.Fatalf("append-only must stay advisory: hard=%d soft=%d", len(hard), len(soft))
	}
}
