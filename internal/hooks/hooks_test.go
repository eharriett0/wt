package hooks

import (
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"strings"
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
	cases := []struct {
		name         string
		resolvedBase string
		want         string
	}{
		// The outgoing range is ALWAYS measured from the base — never the remote
		// branch head. That is what makes it history-shape-invariant: a rebase +
		// force-push (#106) and a `git merge origin/<base>` refresh (#136) both
		// keep the remote head an ancestor, so measuring from the remote head
		// leaked the base's own files as "outgoing" and blamed an innocent window.
		{"resolved remote base is used", "refs/remotes/origin/main", "refs/remotes/origin/main"},
		{"no resolvable remote base falls back to the bare base name", "", "main"},
	}
	for _, tc := range cases {
		if got := outgoingFrom("main", tc.resolvedBase); got != tc.want {
			t.Errorf("%s: outgoingFrom(base=main, resolved=%q) = %q, want %q",
				tc.name, tc.resolvedBase, got, tc.want)
		}
	}
}

// #136: a branch's outgoing set is its contribution OVER THE BASE — never the
// base's own files — regardless of how the branch was refreshed. A
// `git merge origin/main` refresh must not blame files that landed on main via
// another window's already-merged PR. Encodes the issue's invariant across all
// three history shapes (behind / rebased / merged) in one table so a future
// refactor cannot fix one and regress another.
func TestOutgoingPaths_HistoryShapeInvariant(t *testing.T) {
	origin := t.TempDir()
	runGitH(t, origin, "init", "-q", "--bare", "-b", "main")

	repo := t.TempDir()
	runGitH(t, repo, "clone", "-q", origin, ".")
	runGitH(t, repo, "config", "user.email", "t@t.test")
	runGitH(t, repo, "config", "user.name", "t")
	writeFileH(t, repo, "base.txt", "base\n")
	runGitH(t, repo, "add", "base.txt")
	runGitH(t, repo, "commit", "-qm", "base")
	runGitH(t, repo, "push", "-q", "origin", "main")

	// Feature branch whose ONLY real change vs base is feat.txt.
	runGitH(t, repo, "switch", "-qc", "feat")
	writeFileH(t, repo, "feat.txt", "feat\n")
	runGitH(t, repo, "add", "feat.txt")
	runGitH(t, repo, "commit", "-qm", "feat work")
	featBase := gitOutH(t, repo, "rev-parse", "HEAD") // feat tip before any refresh

	// Another window's PR lands 5 innocent files on main.
	runGitH(t, repo, "switch", "-q", "main")
	for _, f := range []string{"other1.txt", "other2.txt", "other3.txt", "other4.txt", "other5.txt"} {
		writeFileH(t, repo, f, "landed on main via another window's merged PR\n")
		runGitH(t, repo, "add", f)
	}
	runGitH(t, repo, "commit", "-qm", "another window's merged PR")
	runGitH(t, repo, "push", "-q", "origin", "main")
	runGitH(t, repo, "switch", "-q", "feat")
	runGitH(t, repo, "fetch", "-q", "origin")

	t.Chdir(repo) // outgoingPaths resolves origin/main via git in cwd

	shapes := []struct {
		name    string
		prepare func()
	}{
		{"behind base, not refreshed", func() {
			runGitH(t, repo, "reset", "-q", "--hard", featBase)
		}},
		{"rebased onto origin/main (#106)", func() {
			runGitH(t, repo, "reset", "-q", "--hard", featBase)
			runGitH(t, repo, "rebase", "-q", "origin/main")
		}},
		{"git merge origin/main refresh (#136)", func() {
			runGitH(t, repo, "reset", "-q", "--hard", featBase)
			runGitH(t, repo, "merge", "-q", "--no-edit", "origin/main")
		}},
	}
	for _, s := range shapes {
		t.Run(s.name, func(t *testing.T) {
			s.prepare()
			head := gitOutH(t, repo, "rev-parse", "HEAD")
			got := outgoingPaths(repo, "main", head)
			if !reflect.DeepEqual(got, []string{"feat.txt"}) {
				t.Errorf("outgoing set = %v, want [feat.txt]; the base's own files (other1..5) must never appear", got)
			}
		})
	}
}

func runGitH(t *testing.T, dir string, args ...string) {
	t.Helper()
	cmd := exec.Command("git", args...)
	cmd.Dir = dir
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("git %v: %v\n%s", args, err, out)
	}
}

func gitOutH(t *testing.T, dir string, args ...string) string {
	t.Helper()
	cmd := exec.Command("git", args...)
	cmd.Dir = dir
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("git %v: %v\n%s", args, err, out)
	}
	return strings.TrimSpace(string(out))
}

func writeFileH(t *testing.T, dir, rel, content string) {
	t.Helper()
	if err := os.WriteFile(filepath.Join(dir, rel), []byte(content), 0o644); err != nil {
		t.Fatal(err)
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

// #123: the section grade must use the NEW-frame sectionRanges, not the
// base-frame ranges used for line grading. This pins that wiring so an accidental
// swap of the two args (here, or at the prod call sites) is caught: sectionRanges
// puts both windows in ## Alpha (same section → HARD), while the line-grade ranges
// put them in DIFFERENT sections — a swap would flip the verdict to advisory.
func TestGradeConflictsStructuredDoc_UsesSectionRangesNotLineRanges(t *testing.T) {
	self, other := twoWorktrees(t)
	ws := []collide.Window{{Branch: "winA", Worktree: other}}
	c := &config.Config{Base: "main", SharedDocs: []string{"CLAUDE.md"}, StructuredDocs: map[string]string{"CLAUDE.md": "^## "}}

	// base-frame ranges (line grade) — DIFFERENT sections: self ## Beta, other ## Alpha.
	lineRanges := func(worktree, base, path string) []gitx.LineRange {
		if worktree == self {
			return []gitx.LineRange{rng(9, 9)} // ## Beta
		}
		return []gitx.LineRange{rng(5, 5)} // ## Alpha
	}
	// new-frame ranges (section grade) — SAME section: both ## Alpha.
	sectionRanges := func(worktree, base, path string) []gitx.LineRange {
		if worktree == self {
			return []gitx.LineRange{rng(5, 5)} // ## Alpha
		}
		return []gitx.LineRange{rng(6, 6)} // ## Alpha
	}
	hard, soft := gradeConflicts(c, []collide.Conflict{{Path: "CLAUDE.md", Window: "winA"}}, self, ws, lineRanges, sectionRanges)
	if len(hard) != 1 || len(soft) != 0 {
		t.Fatalf("section grade must use sectionRanges (same section → HARD); a swap uses lineRanges (different sections → advisory): hard=%d soft=%d", len(hard), len(soft))
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
