package worktree

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/eharriett0/wt/internal/activework"
	"github.com/eharriett0/wt/internal/gitx"
)

// --- pure: DecideDiscard (#177) ---

func TestDecideDiscard_Table(t *testing.T) {
	ok := DiscardCase{Counted: true} // clean, on a branch, nothing only on it
	with := func(f func(*DiscardCase)) DiscardCase { k := ok; f(&k); return k }
	unique := with(func(k *DiscardCase) { k.Unique = 1 })
	cases := []struct {
		name        string
		k           DiscardCase
		drop, roots bool
		want        DiscardVerdict
	}{
		{"clean, every commit on a branch origin has: go", ok, false, false, DiscardGo},
		{"#177 canary, one commit on no branch origin has: needs --drop-commits", unique, false, false, DiscardNeedsDropCommits},
		{"#177 canary with --drop-commits: go", unique, true, false, DiscardGo},
		{"--drop-commits with nothing to drop: go", ok, true, false, DiscardGo},
		{"dirty: refused, --drop-commits does not cover it", with(func(k *DiscardCase) { k.Dirty = true }), true, true, DiscardDirty},
		{"dirty with unique commits: the dirty tree is named first", with(func(k *DiscardCase) { k.Dirty = true; k.Unique = 3 }), false, false, DiscardDirty},
		{"commits could not be listed: refused even with --drop-commits", with(func(k *DiscardCase) { k.Counted = false }), true, true, DiscardUncounted},
		{"commits could not be listed: a stale count is not read", with(func(k *DiscardCase) { k.Counted = false; k.Unique = 0 }), false, false, DiscardUncounted},
		{"base branch: refused whatever the flags", with(func(k *DiscardCase) { k.OnBase = true }), true, true, DiscardBase},
		{"main checkout: refused whatever the flags", with(func(k *DiscardCase) { k.Primary = true }), true, true, DiscardPrimary},
		{"main checkout on the base: the main checkout is named", with(func(k *DiscardCase) { k.Primary = true; k.OnBase = true }), false, false, DiscardPrimary},
		{"detached HEAD (mid-rebase): refused", with(func(k *DiscardCase) { k.Detached = true }), true, true, DiscardDetached},
		{"the window the command runs in: refused", with(func(k *DiscardCase) { k.HoldsCwd = true }), true, true, DiscardHoldsCwd},
		{"outside worktree_root: refused without --all-roots", with(func(k *DiscardCase) { k.OutOfRoot = true }), true, false, DiscardOutOfRoot},
		{"outside worktree_root with --all-roots: go", with(func(k *DiscardCase) { k.OutOfRoot = true }), false, true, DiscardGo},
		{"--all-roots keeps every other guard: dirty", with(func(k *DiscardCase) { k.OutOfRoot = true; k.Dirty = true }), true, true, DiscardDirty},
		{"--all-roots keeps every other guard: base", with(func(k *DiscardCase) { k.OutOfRoot = true; k.OnBase = true }), true, true, DiscardBase},
		{"--all-roots keeps every other guard: unique commits", with(func(k *DiscardCase) { k.OutOfRoot = true; k.Unique = 1 }), false, true, DiscardNeedsDropCommits},
		{"another worktree inside it: refused", with(func(k *DiscardCase) { k.Nests = true }), true, true, DiscardNests},
		{"its branch checked out elsewhere too: refused", with(func(k *DiscardCase) { k.SharedBranch = true }), true, true, DiscardSharedBranch},
		{"locked: refused", with(func(k *DiscardCase) { k.Locked = true }), true, true, DiscardLocked},
		{"placement before content: the window it runs in, dirty too", with(func(k *DiscardCase) { k.HoldsCwd = true; k.Dirty = true }), false, false, DiscardHoldsCwd},
	}
	for _, c := range cases {
		if got := DecideDiscard(c.k, c.drop, c.roots); got != c.want {
			t.Errorf("%s: DecideDiscard(%+v, drop=%v, all-roots=%v) = %d, want %d", c.name, c.k, c.drop, c.roots, got, c.want)
		}
	}
}

// eachDiscardCase runs fn on every combination of DiscardCase's guards, with
// Unique 0 or 2.
func eachDiscardCase(fn func(DiscardCase)) {
	for bits := 0; bits < 1<<11; bits++ {
		b := func(i int) bool { return bits&(1<<i) != 0 }
		k := DiscardCase{Primary: b(0), Detached: b(1), OnBase: b(2), HoldsCwd: b(3), OutOfRoot: b(4),
			Nests: b(5), SharedBranch: b(6), Locked: b(7), Dirty: b(8), Counted: b(9)}
		if b(10) {
			k.Unique = 2
		}
		fn(k)
	}
}

// The whole contract in one line: go only when EVERY guard is clear, each flag
// clearing only its own.
func TestDecideDiscard_GoOnlyWhenEveryGuardIsClear(t *testing.T) {
	eachDiscardCase(func(k DiscardCase) {
		for _, drop := range []bool{false, true} {
			for _, roots := range []bool{false, true} {
				clear := !k.Primary && !k.Detached && !k.OnBase && !k.HoldsCwd && (!k.OutOfRoot || roots) &&
					!k.Nests && !k.SharedBranch && !k.Locked && !k.Dirty && k.Counted && (k.Unique == 0 || drop)
				if got := DecideDiscard(k, drop, roots); (got == DiscardGo) != clear {
					t.Fatalf("DecideDiscard(%+v, drop=%v, all-roots=%v) = %d, want go=%v", k, drop, roots, got, clear)
				}
			}
		}
	})
}

// --drop-commits answers only the commits question and --all-roots only the
// worktree_root one: neither ever changes any other verdict.
func TestDecideDiscard_FlagsUnlockOnlyTheirOwnGuard(t *testing.T) {
	eachDiscardCase(func(k DiscardCase) {
		for _, other := range []bool{false, true} {
			if without, with := DecideDiscard(k, false, other), DecideDiscard(k, true, other); without != with &&
				(without != DiscardNeedsDropCommits || with != DiscardGo) {
				t.Fatalf("--drop-commits changed %d into %d for %+v: it may only clear the commits", without, with, k)
			}
			if without, with := DecideDiscard(k, other, false), DecideDiscard(k, other, true); without != with && without != DiscardOutOfRoot {
				t.Fatalf("--all-roots changed %d into %d for %+v: it may only clear worktree_root", without, with, k)
			}
		}
	})
}

// --- pure: DiscardTarget, the one-name resolution (#177, #169's rules) ---

func TestDiscardTarget(t *testing.T) {
	wts := []gitx.WorktreeRef{
		{Path: "/r/repo", Branch: "main"}, // the main checkout
		{Path: "/w/feat-d", Branch: "feat/d"},
		{Path: "/w/other", Branch: "feat-d"},
		{Path: "/w/canary", Branch: "canary"},
		{Path: "/w/det", Branch: ""},
		{Path: "/w/x", Branch: "repo"},
	}
	for _, c := range []struct {
		name, resolved string
		want           int
	}{
		{"canary", "", 3},             // its branch (and its directory)
		{"feat/d", "", 1},             // a branch
		{"det", "", 4},                // a detached worktree, by its directory
		{"/w/canary", "/w/canary", 3}, // a path
		{"main", "", 0},               // the main checkout, by its branch: picked, then DecideDiscard refuses it
	} {
		got, err := DiscardTarget(c.name, c.resolved, wts)
		if err != nil || got != c.want {
			t.Errorf("DiscardTarget(%q) = %d, %v; want %d", c.name, got, err, c.want)
		}
	}
	for _, c := range []struct {
		name string
		want []string
	}{
		{"feat-d", []string{"more than one worktree matches feat-d (/w/feat-d, /w/other)", "by its path", "nothing was discarded"}},
		{"repo", []string{"more than one worktree matches repo (/r/repo, /w/x)", "nothing was discarded"}}, // the main checkout counts
		{"nope", []string{"no worktree matches nope", "nothing was discarded"}},
		{"HEAD", []string{"no worktree matches HEAD"}}, // never "every detached worktree"
		{"", []string{"name the worktree", "nothing was discarded"}},
	} {
		got, err := DiscardTarget(c.name, "", wts)
		if err == nil {
			t.Errorf("DiscardTarget(%q) = %d, want a refusal", c.name, got)
			continue
		}
		for _, w := range c.want {
			if !strings.Contains(err.Error(), w) {
				t.Errorf("DiscardTarget(%q) = %q, want it to contain %q", c.name, err, w)
			}
		}
	}
}

// --- live-git: Discard against a scratch bare origin (adoptFixture) ---

func newDiscardFixture(t *testing.T) *adoptFixture {
	t.Helper()
	f := newAdoptFixture(t)
	f.c.ActiveWork = filepath.Join(t.TempDir(), "active-work.md")
	return f
}

// worktreeOn adds a worktree under worktree_root on a new branch cut from from.
func (f *adoptFixture) worktreeOn(branch, from string) string {
	f.t.Helper()
	dir := filepath.Join(f.c.WorktreeRoot, strings.ReplaceAll(branch, "/", "-"))
	gitW(f.t, f.repo, "worktree", "add", "-q", "-b", branch, dir, from)
	return dir
}

// commitHere commits (empty) in the worktree at dir and returns the new HEAD.
func (f *adoptFixture) commitHere(dir, msg string) string {
	f.t.Helper()
	gitW(f.t, dir, "commit", "-q", "--allow-empty", "-m", msg)
	return f.tip(dir, "HEAD")
}

// canary builds #177's shape: a PR branch on origin, and a throwaway branch cut
// from it with one commit that must never merge, pushed under its own name.
func (f *adoptFixture) canary() (dir, canaryTip string) {
	f.t.Helper()
	pr := f.commitIn(f.repo, f.base, "pr work")
	gitW(f.t, f.repo, "update-ref", "refs/heads/feat/pr", pr)
	gitW(f.t, f.repo, "push", "-q", "origin", "feat/pr")
	dir = f.worktreeOn("canary", "feat/pr")
	canaryTip = f.commitHere(dir, "canary: never merge")
	gitW(f.t, dir, "push", "-q", "-u", "origin", "canary")
	return dir, canaryTip
}

func exists(p string) bool { _, err := os.Stat(p); return err == nil }

// refs lists every ref in the repository at dir with its commit.
func refs(t *testing.T, dir string) string {
	t.Helper()
	return gitOutW(t, dir, "for-each-ref", "--format=%(objectname) %(refname)")
}

// assertRefused checks a refusal of kind ErrDiscardRefused with verdict want,
// and that the worktree at dir and its branch are still there.
func (f *adoptFixture) assertRefused(r DiscardResult, err error, want DiscardVerdict, dir, branch string) {
	f.t.Helper()
	if !errors.Is(err, ErrDiscardRefused) || r.Verdict != want {
		f.t.Fatalf("Discard = verdict %d, err %v; want a refusal with verdict %d", r.Verdict, err, want)
	}
	if !exists(dir) {
		f.t.Errorf("a refusal removed the worktree %s", dir)
	}
	if branch != "" && !f.has("refs/heads/"+branch) {
		f.t.Errorf("a refusal deleted the branch %s", branch)
	}
}

// assertDiscarded checks the worktree at dir and its branch are gone, and git
// no longer lists the worktree.
func (f *adoptFixture) assertDiscarded(r DiscardResult, err error, dir, branch string) {
	f.t.Helper()
	if err != nil || !r.Done {
		f.t.Fatalf("Discard = done %v, err %v; want it discarded", r.Done, err)
	}
	if exists(dir) {
		f.t.Errorf("worktree %s is still there", dir)
	}
	if f.has("refs/heads/" + branch) {
		f.t.Errorf("branch %s is still there", branch)
	}
	// dir is gone, so its real path is read through its parent's.
	listed := "worktree " + filepath.Join(realPath(filepath.Dir(dir)), filepath.Base(dir)) + "\n"
	if list := gitOutW(f.t, f.repo, "worktree", "list", "--porcelain") + "\n"; strings.Contains(list, listed) {
		f.t.Errorf("git still lists %s:\n%s", dir, list)
	}
}

// #177 as reported: the canary was pushed, used, and deleted on origin from
// ANOTHER clone (or the web UI), so this clone's origin/canary is still there,
// stale. `rev-list canary --not --remotes` reads 0 commits: the stale ref hides
// the commit. Discard reads origin itself, lists the commit, and drops it only
// with --drop-commits; every other worktree, branch and ref stays.
func TestDiscard_Canary_DeletedOnOriginElsewhere(t *testing.T) {
	f := newDiscardFixture(t)
	dir, tip := f.canary()
	keep := f.worktreeOn("keep", f.base)
	gitW(t, f.origin, "update-ref", "-d", "refs/heads/canary") // deleted on origin, not from here
	if n := gitOutW(t, f.repo, "rev-list", "--count", "canary", "--not", "--remotes"); n != "0" {
		t.Fatalf("fixture: --not --remotes counts %s, want 0 (the stale origin/canary hides the commit)", n)
	}
	originBefore := refs(t, f.origin)

	r, err := Discard(f.c, "canary", DiscardOpts{})
	f.assertRefused(r, err, DiscardNeedsDropCommits, dir, "canary")
	if len(r.Unique) != 1 || r.Unique[0].SHA != tip || r.Unique[0].Subject != "canary: never merge" {
		t.Fatalf("listed %+v, want only the canary commit %s (the PR's commit is on origin's feat/pr)", r.Unique, tip)
	}
	if !r.OriginAsked || r.OriginTip != "" || !r.StaleTracking {
		t.Errorf("origin: asked %v, tip %q, stale tracking %v; want asked, no tip, stale", r.OriginAsked, r.OriginTip, r.StaleTracking)
	}

	r, err = Discard(f.c, "canary", DiscardOpts{DropCommits: true})
	f.assertDiscarded(r, err, dir, "canary")
	if !exists(keep) || !f.has("refs/heads/keep") || !f.has("refs/heads/feat/pr") {
		t.Error("another worktree or branch went with the canary")
	}
	if !f.has("refs/remotes/origin/canary") {
		t.Error("the stale origin/canary was deleted; discard leaves every remote-tracking ref alone")
	}
	if got := refs(t, f.origin); got != originBefore {
		t.Errorf("origin changed:\n%s\nwant\n%s", got, originBefore)
	}
}

// The same canary deleted on origin from this clone (`git push --delete`, which
// drops origin/canary here too).
func TestDiscard_Canary_DeletedOnOriginFromHere(t *testing.T) {
	f := newDiscardFixture(t)
	dir, tip := f.canary()
	gitW(t, f.repo, "push", "-q", "origin", "--delete", "canary")

	r, err := Discard(f.c, "canary", DiscardOpts{})
	f.assertRefused(r, err, DiscardNeedsDropCommits, dir, "canary")
	if len(r.Unique) != 1 || r.Unique[0].SHA != tip {
		t.Fatalf("listed %+v, want only %s", r.Unique, tip)
	}
	r, err = Discard(f.c, "canary", DiscardOpts{DropCommits: true})
	f.assertDiscarded(r, err, dir, "canary")
}

// A never-pushed branch: every commit on it is listed, newest first, and goes
// only with --drop-commits.
func TestDiscard_UnpushedCommitsNeedDropCommits(t *testing.T) {
	f := newDiscardFixture(t)
	dir := f.worktreeOn("spike", f.base)
	one := f.commitHere(dir, "spike 1")
	two := f.commitHere(dir, "spike 2")

	r, err := Discard(f.c, "spike", DiscardOpts{})
	f.assertRefused(r, err, DiscardNeedsDropCommits, dir, "spike")
	if len(r.Unique) != 2 || r.Unique[0].SHA != two || r.Unique[1].SHA != one {
		t.Fatalf("listed %+v, want %s then %s", r.Unique, two, one)
	}
	r, err = Discard(f.c, "spike", DiscardOpts{DropCommits: true})
	f.assertDiscarded(r, err, dir, "spike")
}

// A worktree with no commits of its own is discarded without --drop-commits.
func TestDiscard_NothingUniqueNeedsNoFlag(t *testing.T) {
	f := newDiscardFixture(t)
	dir := f.worktreeOn("empty", f.base)
	r, err := Discard(f.c, dir, DiscardOpts{}) // by path
	f.assertDiscarded(r, err, dir, "empty")
}

// Origin still has the branch: its commits are on origin, so none is dropped;
// the local branch goes and origin's stays (the command to delete it is printed).
func TestDiscard_BranchStillOnOrigin(t *testing.T) {
	f := newDiscardFixture(t)
	dir := f.worktreeOn("shared", f.base)
	tip := f.commitHere(dir, "pushed work")
	gitW(t, dir, "push", "-q", "-u", "origin", "shared")
	originBefore := refs(t, f.origin)

	r, err := Discard(f.c, "shared", DiscardOpts{})
	f.assertDiscarded(r, err, dir, "shared")
	if len(r.Unique) != 0 || r.OriginTip != tip {
		t.Errorf("listed %+v, origin tip %q; want nothing listed and origin at %s", r.Unique, r.OriginTip, tip)
	}
	if got := refs(t, f.origin); got != originBefore {
		t.Errorf("origin changed:\n%s\nwant\n%s", got, originBefore)
	}
	if !f.has("refs/remotes/origin/shared") {
		t.Error("origin/shared was deleted here; discard leaves remote-tracking refs alone")
	}
}

// Offline, origin/<branch> as last fetched is not trusted: origin may have
// deleted the branch since (the #177 canary), so its commits are listed.
func TestDiscard_OfflineDoesNotTrustTheBranchsOwnTrackingRef(t *testing.T) {
	f := newDiscardFixture(t)
	dir := f.worktreeOn("pushed", f.base)
	tip := f.commitHere(dir, "pushed work")
	gitW(t, dir, "push", "-q", "-u", "origin", "pushed")
	f.offline()

	r, err := Discard(f.c, "pushed", DiscardOpts{})
	f.assertRefused(r, err, DiscardNeedsDropCommits, dir, "pushed")
	if r.OriginAsked || len(r.Unique) != 1 || r.Unique[0].SHA != tip {
		t.Errorf("asked %v, listed %+v; want origin unasked and %s listed", r.OriginAsked, r.Unique, tip)
	}
}

// A dirty tree is refused whatever the flags, untracked files included, also
// when status.showUntrackedFiles=no hides them from a plain status (and from
// `git worktree remove`'s own check, which then deleted them).
func TestDiscard_RefusesADirtyTree(t *testing.T) {
	for _, hide := range []bool{false, true} {
		f := newDiscardFixture(t)
		if hide {
			gitW(t, f.repo, "config", "status.showUntrackedFiles", "no")
		}
		dir := f.worktreeOn("dirty", f.base)
		file := filepath.Join(dir, "notes.txt")
		if err := os.WriteFile(file, []byte("not committed\n"), 0o644); err != nil {
			t.Fatal(err)
		}
		r, err := Discard(f.c, "dirty", DiscardOpts{DropCommits: true, AllRoots: true})
		f.assertRefused(r, err, DiscardDirty, dir, "dirty")
		if len(r.Dirty) != 1 || !strings.Contains(r.Dirty[0], "notes.txt") {
			t.Errorf("hidden=%v: dirty entries %q, want notes.txt named", hide, r.Dirty)
		}
		if !exists(file) {
			t.Errorf("hidden=%v: the untracked file is gone", hide)
		}
	}
}

// A worktree directory that was replaced (removed by hand, then made again) is
// not read as that worktree: with worktree_root inside the main checkout its
// status would be the main checkout's. It counts as dirty, and its files stay.
func TestDiscard_ReplacedDirectoryIsNotRead(t *testing.T) {
	f := newDiscardFixture(t)
	f.c.WorktreeRoot = filepath.Join(f.repo, ".worktrees") // inside the main checkout
	dir := f.worktreeOn("replaced", f.base)
	if err := os.RemoveAll(dir); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	file := filepath.Join(dir, "keep.txt")
	if err := os.WriteFile(file, []byte("someone's file\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	// An in-repo worktree root is ignored, so the main checkout reads clean.
	if err := os.WriteFile(filepath.Join(f.repo, ".git", "info", "exclude"), []byte(".worktrees/\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if st := gitOutW(t, dir, "status", "--porcelain", "--untracked-files=normal"); st != "" {
		t.Fatalf("fixture: status read in the replaced dir = %q, want the main checkout's clean status", st)
	}
	r, err := Discard(f.c, "replaced", DiscardOpts{DropCommits: true})
	f.assertRefused(r, err, DiscardDirty, dir, "replaced")
	if r.Gone || !strings.Contains(err.Error(), "not the top of a work tree") || !exists(file) {
		t.Errorf("gone %v, err %v, file kept %v; want it refused as unreadable with the file kept", r.Gone, err, exists(file))
	}
}

// A modified tracked file is dirty too.
func TestDiscard_RefusesAModifiedTrackedFile(t *testing.T) {
	f := newDiscardFixture(t)
	dir := f.worktreeOn("edited", f.base)
	file := filepath.Join(dir, "a.txt")
	if err := os.WriteFile(file, []byte("v1\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	gitW(t, dir, "add", "a.txt")
	f.commitHere(dir, "add a")
	if err := os.WriteFile(file, []byte("v2\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	r, err := Discard(f.c, "edited", DiscardOpts{DropCommits: true})
	f.assertRefused(r, err, DiscardDirty, dir, "edited")
}

// Never the base branch (#101), and never the main checkout, by any name.
func TestDiscard_RefusesTheBaseAndTheMainCheckout(t *testing.T) {
	f := newDiscardFixture(t)
	gitW(t, f.repo, "checkout", "-q", "-b", "elsewhere")
	dir := filepath.Join(f.c.WorktreeRoot, "base-copy")
	gitW(t, f.repo, "worktree", "add", "-q", dir, "main")
	r, err := Discard(f.c, "base-copy", DiscardOpts{DropCommits: true, AllRoots: true})
	f.assertRefused(r, err, DiscardBase, dir, "main")

	r, err = Discard(f.c, filepath.Base(f.repo), DiscardOpts{DropCommits: true, AllRoots: true})
	f.assertRefused(r, err, DiscardPrimary, f.repo, "elsewhere")
}

// Never the window the command runs in, also from a directory inside it.
func TestDiscard_RefusesTheWindowItRunsIn(t *testing.T) {
	f := newDiscardFixture(t)
	dir := f.worktreeOn("here", f.base)
	sub := filepath.Join(dir, "sub")
	if err := os.Mkdir(sub, 0o755); err != nil {
		t.Fatal(err)
	}
	for _, cwd := range []string{dir, sub} {
		t.Chdir(cwd)
		r, err := Discard(f.c, "here", DiscardOpts{DropCommits: true, AllRoots: true})
		f.assertRefused(r, err, DiscardHoldsCwd, dir, "here")
	}
}

// A worktree outside worktree_root needs --all-roots, as `wt clean` does.
func TestDiscard_OutsideTheRootNeedsAllRoots(t *testing.T) {
	f := newDiscardFixture(t)
	dir := filepath.Join(t.TempDir(), "legacy")
	gitW(t, f.repo, "worktree", "add", "-q", "-b", "legacy", dir, f.base)
	r, err := Discard(f.c, "legacy", DiscardOpts{DropCommits: true})
	f.assertRefused(r, err, DiscardOutOfRoot, dir, "legacy")
	r, err = Discard(f.c, "legacy", DiscardOpts{AllRoots: true})
	f.assertDiscarded(r, err, dir, "legacy")
}

// `git worktree remove` deletes a worktree nested inside the one removed when
// the nest is ignored (measured, git 2.39): discard refuses the outer one.
func TestDiscard_RefusesAWorktreeWithAnotherInside(t *testing.T) {
	f := newDiscardFixture(t)
	outer := f.worktreeOn("outer", f.base)
	if err := os.WriteFile(filepath.Join(outer, ".gitignore"), []byte(".nest/\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	gitW(t, outer, "add", ".gitignore")
	f.commitHere(outer, "ignore .nest")
	inner := filepath.Join(outer, ".nest", "inner")
	gitW(t, f.repo, "worktree", "add", "-q", "-b", "inner", inner, f.base)

	r, err := Discard(f.c, "outer", DiscardOpts{DropCommits: true})
	f.assertRefused(r, err, DiscardNests, outer, "outer")
	if !exists(filepath.Join(inner, ".git")) {
		t.Error("the nested worktree was deleted")
	}
}

// A detached worktree (mid-rebase, say), a locked one, and one whose branch is
// checked out in another worktree too, are refused.
func TestDiscard_RefusesDetachedLockedAndSharedBranch(t *testing.T) {
	f := newDiscardFixture(t)
	det := filepath.Join(f.c.WorktreeRoot, "det")
	gitW(t, f.repo, "worktree", "add", "-q", "--detach", det, f.base)
	r, err := Discard(f.c, "det", DiscardOpts{DropCommits: true})
	f.assertRefused(r, err, DiscardDetached, det, "")

	locked := f.worktreeOn("locked", f.base)
	gitW(t, f.repo, "worktree", "lock", locked)
	r, err = Discard(f.c, "locked", DiscardOpts{DropCommits: true})
	f.assertRefused(r, err, DiscardLocked, locked, "locked")

	one := f.worktreeOn("twice", f.base)
	two := filepath.Join(f.c.WorktreeRoot, "twice-again")
	gitW(t, f.repo, "worktree", "add", "-q", "--force", two, "twice")
	r, err = Discard(f.c, one, DiscardOpts{DropCommits: true})
	f.assertRefused(r, err, DiscardSharedBranch, one, "twice")
	if !exists(two) {
		t.Error("the other worktree on the branch is gone")
	}
}

// A name that picks out no worktree, or more than one, stops with nothing done
// (#169's rules and wording).
func TestDiscard_NameMustPickOutExactlyOne(t *testing.T) {
	f := newDiscardFixture(t)
	a := f.worktreeOn("feat/d", f.base) // directory feat-d
	b := filepath.Join(f.c.WorktreeRoot, "other")
	gitW(t, f.repo, "worktree", "add", "-q", "-b", "feat-d", b, f.base) // branch feat-d
	for name, want := range map[string]string{
		"feat-d": "more than one worktree matches feat-d",
		"nope":   "no worktree matches nope",
	} {
		_, err := Discard(f.c, name, DiscardOpts{DropCommits: true})
		if err == nil || !strings.Contains(err.Error(), want) || !strings.Contains(err.Error(), "nothing was discarded") {
			t.Errorf("Discard(%q) = %v, want %q and nothing discarded", name, err, want)
		}
	}
	if !exists(a) || !exists(b) || !f.has("refs/heads/feat/d") || !f.has("refs/heads/feat-d") {
		t.Error("an unresolved name removed something")
	}
}

// A claimed worktree's active-work section goes with it (as merge-pr's
// auto-clean drops it); every other claim stays, another worktree's claim on the
// same issue included.
func TestDiscard_DropsTheClaimSection(t *testing.T) {
	f := newDiscardFixture(t)
	dir, _ := f.canary()
	when := time.Date(2026, 1, 2, 3, 4, 5, 0, time.UTC)
	content := activework.AppendSection("", activework.Entry{Issue: "7", Title: "canary", Branch: "canary", Worktree: dir, When: when})
	content = activework.AppendSection(content, activework.Entry{Issue: "7", Title: "same issue", Branch: "feat/seven", Worktree: "/elsewhere/7", When: when})
	content = activework.AppendSection(content, activework.Entry{Issue: "8", Title: "other", Branch: "feat/other", Worktree: "/elsewhere", When: when})
	if err := activework.Write(f.c.ActiveWork, content); err != nil {
		t.Fatal(err)
	}
	r, err := Discard(f.c, "canary", DiscardOpts{DropCommits: true})
	f.assertDiscarded(r, err, dir, "canary")
	if strings.Join(r.Claims, ",") != "7" {
		t.Errorf("claims = %v, want [7]", r.Claims)
	}
	left := activework.Parse(activework.Read(f.c.ActiveWork))
	if len(left) != 2 || left[0].Branch != "feat/seven" || left[1].Issue != "8" {
		t.Errorf("active-work after the discard = %+v, want #7 on feat/seven and #8", left)
	}
}

// A worktree whose directory is already gone (removed by hand) holds no
// uncommitted work: git lists it as prunable, discard removes just its record
// and the branch, through the same commit gate. Another prunable record stays.
func TestDiscard_ADirectoryAlreadyGone(t *testing.T) {
	f := newDiscardFixture(t)
	dir := f.worktreeOn("gone", f.base)
	tip := f.commitHere(dir, "work in a deleted directory")
	other := f.worktreeOn("also-gone", f.base)
	for _, d := range []string{dir, other} {
		if err := os.RemoveAll(d); err != nil {
			t.Fatal(err)
		}
	}
	r, err := Discard(f.c, "gone", DiscardOpts{})
	if !errors.Is(err, ErrDiscardRefused) || r.Verdict != DiscardNeedsDropCommits || !r.Gone || len(r.Unique) != 1 || r.Unique[0].SHA != tip {
		t.Fatalf("Discard = verdict %d, gone %v, listed %+v, err %v; want the commit listed and --drop-commits asked for", r.Verdict, r.Gone, r.Unique, err)
	}
	r, err = Discard(f.c, "gone", DiscardOpts{DropCommits: true})
	if err != nil || !r.Done || f.has("refs/heads/gone") {
		t.Fatalf("Discard --drop-commits = done %v, err %v (branch still there: %v)", r.Done, err, f.has("refs/heads/gone"))
	}
	list := gitOutW(t, f.repo, "worktree", "list", "--porcelain") + "\n"
	root := realPath(f.c.WorktreeRoot)
	if strings.Contains(list, "worktree "+filepath.Join(root, "gone")+"\n") || !strings.Contains(list, "worktree "+filepath.Join(root, "also-gone")+"\n") {
		t.Errorf("worktree list after the discard:\n%s\nwant gone's record removed and also-gone's kept", list)
	}
}

// --dry-run says what would happen and changes nothing: the worktree, its files,
// every ref here and on origin, the worktree list and active-work are as they
// were. A dry run a real run would refuse is a refusal too.
func TestDiscard_DryRunTouchesNothing(t *testing.T) {
	f := newDiscardFixture(t)
	dir, _ := f.canary()
	gitW(t, f.origin, "update-ref", "-d", "refs/heads/canary")
	gitW(t, f.repo, "fetch", "-q", "origin") // so the dry run's own fetch has nothing new
	content := activework.AppendSection("", activework.Entry{Issue: "7", Branch: "canary", Worktree: dir, When: time.Now()})
	if err := activework.Write(f.c.ActiveWork, content); err != nil {
		t.Fatal(err)
	}
	snapshot := func() string {
		return refs(t, f.repo) + "\n--\n" + refs(t, f.origin) + "\n--\n" +
			gitOutW(t, f.repo, "worktree", "list", "--porcelain") + "\n--\n" + activework.Read(f.c.ActiveWork)
	}
	before := snapshot()

	r, err := Discard(f.c, "canary", DiscardOpts{DryRun: true})
	if !errors.Is(err, ErrDiscardRefused) || r.Verdict != DiscardNeedsDropCommits || !strings.Contains(err.Error(), "--dry-run") {
		t.Fatalf("dry run without --drop-commits = %d, %v; want the refusal a real run makes, marked as a dry run", r.Verdict, err)
	}
	r, err = Discard(f.c, "canary", DiscardOpts{DryRun: true, DropCommits: true})
	if err != nil || r.Verdict != DiscardGo || r.Done || len(r.Unique) != 1 || strings.Join(r.Claims, ",") != "7" {
		t.Fatalf("dry run = verdict %d, done %v, listed %d, claims %v, err %v; want go, not done, 1 listed, claim 7", r.Verdict, r.Done, len(r.Unique), r.Claims, err)
	}
	if after := snapshot(); after != before {
		t.Errorf("a dry run changed something:\n%s\nwant\n%s", after, before)
	}
	if !exists(filepath.Join(dir, ".git")) {
		t.Error("a dry run removed the worktree")
	}
}

// The tip is read once, and a branch that moved before the removal is not
// discarded (its new commit was never listed).
func TestDiscard_BranchThatMovedIsKept(t *testing.T) {
	f := newDiscardFixture(t)
	dir := f.worktreeOn("moving", f.base)
	f.commitHere(dir, "listed")
	r := DiscardResult{Dir: realPath(dir), Branch: "moving", Tip: f.tip(dir, "HEAD")}
	moved := f.commitHere(dir, "made after the listing")
	err := r.apply(f.c)
	if !errors.Is(err, ErrDiscardRefused) || !exists(dir) || f.tip(f.repo, "refs/heads/moving") != moved {
		t.Fatalf("apply on a moved branch = %v; want a refusal with the worktree and branch kept at %s", err, moved)
	}
}

// The worktree list's lock state reaches DiscardCase.
func TestWorktreeListReadsLocked(t *testing.T) {
	f := newDiscardFixture(t)
	dir := f.worktreeOn("l", f.base)
	gitW(t, f.repo, "worktree", "lock", "--reason", "on a stick", dir)
	wts, err := gitx.WorktreeList()
	if err != nil {
		t.Fatal(err)
	}
	for _, w := range wts {
		if realPath(w.Path) == realPath(dir) && !w.Locked {
			t.Errorf("%s reads unlocked", dir)
		}
		if realPath(w.Path) == realPath(f.repo) && w.Locked {
			t.Errorf("the main checkout reads locked")
		}
	}
}

func TestSameBranch(t *testing.T) {
	for _, c := range []struct {
		a, b string
		fold bool
		want bool
	}{
		{"main", "main", false, true},
		{"Main", "main", false, false}, // case-sensitive refs: two branches
		{"Main", "main", true, true},   // core.ignorecase: one loose ref file
		{"feat/X", "feat/x", true, true},
		{"main", "mainline", true, false},
	} {
		if got := sameBranch(c.a, c.b, c.fold); got != c.want {
			t.Errorf("sameBranch(%q, %q, fold=%v) = %v, want %v", c.a, c.b, c.fold, got, c.want)
		}
	}
}

// With core.ignorecase a worktree can be on a case twin of a branch (#167): one
// loose ref file under two names, which git's in-use check tells apart. Deleting
// one name deletes the other's ref, so a twin of the base is the base, and a twin
// checked out elsewhere is the branch checked out elsewhere.
func TestDiscard_CaseTwins(t *testing.T) {
	f := newDiscardFixture(t)
	if !gitx.IgnoreCase() {
		t.Skip("core.ignorecase is off: branch names differing in case are separate refs here")
	}
	gitW(t, f.repo, "checkout", "-q", "-b", "elsewhere")
	twin := filepath.Join(f.c.WorktreeRoot, "base-twin")
	gitW(t, f.repo, "worktree", "add", "-q", twin, "Main")
	r, err := Discard(f.c, "base-twin", DiscardOpts{DropCommits: true})
	f.assertRefused(r, err, DiscardBase, twin, "main")

	one := f.worktreeOn("canary", f.base)
	other := filepath.Join(f.c.WorktreeRoot, "canary-twin")
	gitW(t, f.repo, "worktree", "add", "-q", other, "Canary")
	r, err = Discard(f.c, one, DiscardOpts{DropCommits: true})
	f.assertRefused(r, err, DiscardSharedBranch, one, "canary")
	if !exists(other) {
		t.Error("the worktree on the twin is gone")
	}
}
