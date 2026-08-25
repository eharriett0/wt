package hooks

import (
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
			hard, soft := gradeConflicts(tc.cfg, []collide.Conflict{tc.conflict}, testRoot, ws, stubRanges(tc.cur, tc.other))
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
	gradeConflicts(&config.Config{Base: "main"}, []collide.Conflict{cf}, testRoot, ws, rf)
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
