package worktree

import (
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"

	"github.com/eharriett0/wt/internal/gitx"
)

// #198: `wt new` and `wt claim` decide what to do with an existing local branch
// of the name they want as `wt adopt` does (#167), against origin/<branch>,
// except that a branch a worktree has checked out (or may) is refused whatever
// its relation (#198 review): git refuses a second checkout anyway, but only at
// worktree-add, which a claim reaches after it assigned the issue. This pins
// the table, so a change to DecideAdopt for adopt's sake cannot quietly change
// new and claim.
func TestDecideNew_Table(t *testing.T) {
	cases := []struct {
		name   string
		exists bool
		rel    TipRelation
		inUse  bool
		want   AdoptAction
	}{
		{"no local branch: cut a new one from the base, as before", false, TipNoTarget, false, AdoptCreate},
		{"no local branch, nothing in use to refuse", false, TipNoTarget, true, AdoptCreate},
		{"no origin/<branch>: the #62 never-pushed branch, attached as it is", true, TipNoTarget, false, AdoptUnverified},
		{"equal: attached as it is", true, TipEqual, false, AdoptAsIs},
		{"only ahead (unpushed, #62): attached, with a note naming the commits", true, TipAhead, false, AdoptAhead},
		{"only behind: fast-forwarded first", true, TipBehind, false, AdoptFastForward},
		{"diverged (an earlier attempt that reused the name): refused", true, TipDiverged, false, AdoptRefuse},
		{"not comparable: refused", true, TipUnknown, false, AdoptRefuse},
		{"never pushed, checked out elsewhere: refused", true, TipNoTarget, true, AdoptRefuseInUse},
		{"equal, checked out elsewhere: refused", true, TipEqual, true, AdoptRefuseInUse},
		{"ahead, checked out elsewhere: refused", true, TipAhead, true, AdoptRefuseInUse},
		{"behind, checked out elsewhere: never moved", true, TipBehind, true, AdoptRefuseInUse},
		{"diverged, checked out elsewhere: refused", true, TipDiverged, true, AdoptRefuseInUse},
		{"not comparable, checked out elsewhere: refused", true, TipUnknown, true, AdoptRefuseInUse},
	}
	for _, c := range cases {
		if got := DecideNew(c.exists, c.rel, c.inUse); got != c.want {
			t.Errorf("%s: DecideNew(%v, %d, %v) = %d, want %d", c.name, c.exists, c.rel, c.inUse, got, c.want)
		}
	}
}

// For `wt new` an existing worktree is checked as for adopt. `wt claim` commits
// its placeholder on that worktree's HEAD and pushes: on a HEAD only behind
// origin/<branch> the push is rejected, and wt never moves an existing
// worktree's branch, so claim refuses behind (#198).
func TestDecideExistingFor(t *testing.T) {
	for rel := TipNoTarget; rel <= TipUnknown; rel++ {
		if got, want := DecideExistingFor(rel, false), DecideExisting(rel); got != want {
			t.Errorf("rel=%d, no push: DecideExistingFor = %d, want DecideExisting's %d", rel, got, want)
		}
		want := DecideExisting(rel)
		if rel == TipBehind {
			want = ExistingRefuseBehind
		}
		if got := DecideExistingFor(rel, true); got != want {
			t.Errorf("rel=%d, pushes: DecideExistingFor = %d, want %d", rel, got, want)
		}
	}
}

// --- live-git: `wt new` against a scratch bare origin (adoptFixture). The
// remote side of each shape is written straight into the bare origin, so it
// reaches the operator's clone only through New's own fetch.

const newBranch = "feat/x"

// newDir is where New puts the worktree for newBranch.
func (f *adoptFixture) newDir() string { return filepath.Join(f.c.WorktreeRoot, "feat-x") }

// pushedBranch pushes a local feat/x at one commit on main and returns it.
func (f *adoptFixture) pushedBranch() string {
	tip := f.commitIn(f.repo, f.base, "x 1")
	gitW(f.t, f.repo, "update-ref", "refs/heads/"+newBranch, tip)
	gitW(f.t, f.repo, "push", "-q", "origin", newBranch)
	return tip
}

// onOrigin moves origin's feat/x to a new commit on parent, behind the clone's back.
func (f *adoptFixture) onOrigin(parent, msg string) string {
	c := f.commitIn(f.origin, parent, msg)
	gitW(f.t, f.origin, "update-ref", "refs/heads/"+newBranch, c)
	return c
}

// assertWorktreeAt checks New's worktree is on feat/x at want.
func (f *adoptFixture) assertWorktreeAt(dir, want string) {
	f.t.Helper()
	if got := gitx.SymbolicHead(dir); got != "refs/heads/"+newBranch {
		f.t.Errorf("worktree HEAD is %q, want refs/heads/%s", got, newBranch)
	}
	if got := f.tip(dir, "HEAD"); got != want {
		f.t.Errorf("worktree is at %s, want %s", got, want)
	}
}

func TestNew_AttachesAnEqualLocalBranch(t *testing.T) {
	f := newAdoptFixture(t)
	tip := f.pushedBranch()
	dir, err := New(f.c, newBranch)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	f.assertWorktreeAt(dir, tip)
}

// #62: a branch with commits origin does not have yet is the work the re-attach
// exists to keep. It is attached unchanged.
func TestNew_AttachesALocalBranchOnlyAhead(t *testing.T) {
	f := newAdoptFixture(t)
	pushed := f.pushedBranch()
	ahead := f.commitIn(f.repo, pushed, "not pushed yet")
	gitW(t, f.repo, "update-ref", "refs/heads/"+newBranch, ahead)
	dir, err := New(f.c, newBranch)
	if err != nil {
		t.Fatalf("New refused a branch that is only ahead (unpushed work): %v", err)
	}
	f.assertWorktreeAt(dir, ahead)
}

// #198: a local branch only behind what was pushed used to be resumed at its old
// commit. It is fast-forwarded to origin/<branch> first.
func TestNew_FastForwardsALocalBranchOnlyBehind(t *testing.T) {
	f := newAdoptFixture(t)
	old := f.pushedBranch()
	head := f.onOrigin(f.onOrigin(old, "x 2"), "x 3")
	dir, err := New(f.c, newBranch)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	f.assertWorktreeAt(dir, head)
	if got := f.tip(f.repo, "refs/heads/"+newBranch); got != head {
		t.Errorf("local branch is at %s, want it fast-forwarded to %s", got, head)
	}
}

// #198, as reported: a local branch left by an earlier attempt that reused the
// name, while origin/<branch> is the new attempt. Before the fix New attached
// it and said "worktree ready".
func TestNew_RefusesALocalBranchThatDiverged(t *testing.T) {
	f := newAdoptFixture(t)
	stale := f.commitIn(f.repo, f.commitIn(f.repo, f.base, "old attempt 1"), "old attempt 2")
	gitW(t, f.repo, "update-ref", "refs/heads/"+newBranch, stale)
	main := f.commitIn(f.origin, f.base, "main moves on")
	gitW(t, f.origin, "update-ref", "refs/heads/main", main)
	head := f.onOrigin(main, "new attempt")
	dir, err := New(f.c, newBranch)
	if err == nil {
		t.Fatalf("New attached the diverged local branch at %s; it must refuse", dir)
	}
	for _, sha := range []string{stale, head} {
		if !strings.Contains(err.Error(), sha[:8]) {
			t.Errorf("the refusal must name both tips; %q lacks %s", err, sha[:8])
		}
	}
	f.assertNothingAdopted(newBranch)
	if got := f.tip(f.repo, "refs/heads/"+newBranch); got != stale {
		t.Errorf("a refusal must leave the local branch alone: at %s, want %s", got, stale)
	}
}

// #62: a branch never pushed, with work on it, has nothing to compare against
// and is attached as it is.
func TestNew_AttachesANeverPushedBranch(t *testing.T) {
	f := newAdoptFixture(t)
	tip := f.commitIn(f.repo, f.base, "local work")
	gitW(t, f.repo, "update-ref", "refs/heads/"+newBranch, tip)
	dir, err := New(f.c, newBranch)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	f.assertWorktreeAt(dir, tip)
}

// Offline, with origin/<branch> as last fetched behind the local branch: the
// comparison uses that copy, and a branch with unpushed work stays attachable.
func TestNew_OfflineComparesWithOriginAsLastFetched(t *testing.T) {
	f := newAdoptFixture(t)
	pushed := f.pushedBranch()
	ahead := f.commitIn(f.repo, pushed, "not pushed yet")
	gitW(t, f.repo, "update-ref", "refs/heads/"+newBranch, ahead)
	f.offline()
	dir, err := New(f.c, newBranch)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	f.assertWorktreeAt(dir, ahead)
}

// No local branch: a new branch is cut from the base, as before #198, and the
// result says so (claim's rollback deletes only a branch it made).
func TestNew_NoLocalBranchCutsANewOneFromTheBase(t *testing.T) {
	f := newAdoptFixture(t)
	p, err := PlanNew(f.c, newBranch, NewFor{})
	if err != nil {
		t.Fatalf("PlanNew: %v", err)
	}
	made, err := p.Create()
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	if !made.NewWorktree || !made.NewBranch {
		t.Errorf("Create = %+v, want a new worktree on a new branch", made)
	}
	f.assertWorktreeAt(made.Dir, f.base)
}

// The behind fast-forward never moves a branch a worktree has checked out: its
// files would stay at the old commit and its next commit would revert the move.
func TestNew_DoesNotMoveABranchCheckedOutElsewhere(t *testing.T) {
	f := newAdoptFixture(t)
	old := f.pushedBranch()
	f.onOrigin(old, "x 2")
	gitW(t, f.repo, "worktree", "add", "-q", filepath.Join(t.TempDir(), "elsewhere"), newBranch)
	_, err := New(f.c, newBranch)
	if !errors.Is(err, ErrBranchInUse) {
		t.Fatalf("New = %v, want an ErrBranchInUse refusal", err)
	}
	if got := f.tip(f.repo, "refs/heads/"+newBranch); got != old {
		t.Errorf("the checked-out branch moved to %s, want it left at %s", got, old)
	}
}

// A failed `git worktree list` cannot rule out a worktree using the branch.
func TestNew_AFailedWorktreeListCountsAsInUse(t *testing.T) {
	f := newAdoptFixture(t)
	old := f.pushedBranch()
	f.onOrigin(old, "x 2")
	listWorktrees = func() ([]gitx.WorktreeRef, error) { return nil, errors.New("worktree list failed") }
	t.Cleanup(func() { listWorktrees = gitx.WorktreeList })
	_, err := New(f.c, newBranch)
	if !errors.Is(err, ErrBranchInUse) {
		t.Fatalf("New = %v, want an ErrBranchInUse refusal", err)
	}
	if got := f.tip(f.repo, "refs/heads/"+newBranch); got != old {
		t.Errorf("the branch moved to %s although wt could not list the worktrees; want it left at %s", got, old)
	}
	f.assertNothingAdopted(newBranch)
}

// core.ignorecase: a branch differing only in case is the same loose ref file
// there, so on such a disk feat/x resolves to Feat/x, and fast-forwarding it to
// origin/feat/x would move Feat/x underneath whatever has it; a new feat/x would
// be Feat/x's file too. Refused either way, before anything moves.
//
// The packed twin is the create path (#198 review): a loose Feat/x resolves as
// feat/x through a case-insensitive filesystem, so on macOS the other two cases
// reach the refusal with a "local branch" in hand. Packed, it does not resolve,
// and only the twin check stops `git worktree add -b feat/x` from writing a
// loose feat/x that shadows Feat/x from then on.
func TestNew_RefusesACaseOnlyTwin(t *testing.T) {
	for _, c := range []struct {
		name           string
		pushed, packed bool
	}{
		{"origin/feat/x ahead of the twin", true, false},
		{"no origin/feat/x", false, false},
		{"packed twin, no feat/x: the create path", false, true},
	} {
		t.Run(c.name, func(t *testing.T) {
			f := newAdoptFixture(t)
			gitW(t, f.repo, "config", "core.ignorecase", "true")
			old := f.commitIn(f.repo, f.base, "x 1")
			gitW(t, f.repo, "update-ref", "refs/heads/Feat/x", old)
			if c.pushed {
				gitW(t, f.repo, "push", "-q", "origin", old+":refs/heads/"+newBranch)
				f.onOrigin(old, "x 2")
			}
			if c.packed {
				gitW(t, f.repo, "pack-refs", "--all")
			}
			_, err := New(f.c, newBranch)
			if !errors.Is(err, ErrCaseTwin) {
				t.Fatalf("New = %v, want an ErrCaseTwin refusal", err)
			}
			if got := f.tip(f.repo, "refs/heads/Feat/x"); got != old {
				t.Errorf("Feat/x moved to %s; it must stay at %s", got, old)
			}
			f.assertNothingAdopted(newBranch)
		})
	}
}

// The existing-worktree short-circuit (#62) stays, now checked against
// origin/<branch> the way #167's re-run is: behind or ahead is handed back with
// a note and never moved, diverged is refused, and nothing moves either way.
func TestNew_ChecksAnExistingWorktree(t *testing.T) {
	cases := []struct {
		name   string
		shape  func(f *adoptFixture) (local string)
		refuse bool
	}{
		{"equal", func(f *adoptFixture) string { return f.pushedBranch() }, false},
		{"behind: handed back, not moved", func(f *adoptFixture) string {
			old := f.pushedBranch()
			f.onOrigin(old, "x 2")
			return old
		}, false},
		{"ahead: handed back", func(f *adoptFixture) string {
			ahead := f.commitIn(f.repo, f.pushedBranch(), "not pushed yet")
			gitW(f.t, f.repo, "update-ref", "refs/heads/"+newBranch, ahead)
			return ahead
		}, false},
		{"diverged: refused", func(f *adoptFixture) string {
			f.onOrigin(f.pushedBranch(), "pushed from elsewhere")
			mine := f.commitIn(f.repo, f.tip(f.repo, "refs/heads/"+newBranch), "mine")
			gitW(f.t, f.repo, "update-ref", "refs/heads/"+newBranch, mine)
			return mine
		}, true},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			f := newAdoptFixture(t)
			local := c.shape(f)
			gitW(t, f.repo, "worktree", "add", "-q", f.newDir(), newBranch)
			p, err := PlanNew(f.c, newBranch, NewFor{})
			if c.refuse {
				if err == nil {
					t.Fatal("PlanNew handed back an existing worktree whose branch diverged from origin")
				}
			} else {
				if err != nil {
					t.Fatalf("PlanNew: %v", err)
				}
				made, err := p.Create()
				if err != nil || made.Dir != f.newDir() || made.NewWorktree || made.NewBranch {
					t.Errorf("Create = (%+v, %v), want the existing worktree handed back, nothing new", made, err)
				}
			}
			if got := f.tip(f.newDir(), "HEAD"); got != local {
				t.Errorf("wt moved the existing worktree to %s; it must stay at %s", got, local)
			}
		})
	}
}

// For claim, which commits on the existing worktree's HEAD and pushes, a HEAD
// only behind origin/<branch> is refused before anything happens: the push
// would be rejected, and wt does not move a checked-out branch.
func TestPlanNew_ClaimRefusesAnExistingWorktreeOnlyBehind(t *testing.T) {
	f := newAdoptFixture(t)
	old := f.pushedBranch()
	f.onOrigin(old, "x 2")
	gitW(t, f.repo, "worktree", "add", "-q", f.newDir(), newBranch)
	_, err := PlanNew(f.c, newBranch, NewFor{ClaimIssue: "42"})
	if !errors.Is(err, ErrBranchInUse) {
		t.Fatalf("PlanNew for a claim = %v, want an ErrBranchInUse refusal", err)
	}
	if got := f.tip(f.newDir(), "HEAD"); got != old {
		t.Errorf("wt moved the existing worktree to %s; it must stay at %s", got, old)
	}
}

// Claim asks gh, and may prompt, between PlanNew and Create: a local branch that
// moved in between is refused before worktree-add, not attached unchecked (the
// post-add check would catch it only after making and removing a worktree, and
// would blame a tag).
func TestCreate_RefusesALocalBranchThatMovedAfterThePlan(t *testing.T) {
	f := newAdoptFixture(t)
	f.pushedBranch()
	p, err := PlanNew(f.c, newBranch, NewFor{ClaimIssue: "42"})
	if err != nil {
		t.Fatalf("PlanNew: %v", err)
	}
	moved := f.commitIn(f.repo, f.base, "moved in between")
	gitW(t, f.repo, "update-ref", "refs/heads/"+newBranch, moved)
	_, err = p.Create()
	if err == nil {
		t.Fatal("Create attached a branch that moved after it was checked")
	}
	if errors.Is(err, ErrLandedElsewhere) || !strings.Contains(err.Error(), "moved after wt checked it") {
		t.Errorf("Create = %v, want the pre-add refusal for a branch that moved", err)
	}
	f.assertNothingAdopted(newBranch)
	if got := f.tip(f.repo, "refs/heads/"+newBranch); got != moved {
		t.Errorf("the branch is at %s, want it left where it moved to (%s)", got, moved)
	}
}

// A refusal leaves no worktree directory behind, even an empty one New made.
func TestNew_RefusalCreatesNoWorktreeRoot(t *testing.T) {
	f := newAdoptFixture(t)
	stale := f.commitIn(f.repo, f.base, "old attempt")
	gitW(t, f.repo, "update-ref", "refs/heads/"+newBranch, stale)
	f.onOrigin(f.base, "new attempt")
	if _, err := New(f.c, newBranch); err == nil {
		t.Fatal("New attached a diverged branch")
	}
	if _, err := os.Stat(f.c.WorktreeRoot); !os.IsNotExist(err) {
		t.Errorf("a refusal must not create the worktree root (stat: %v)", err)
	}
}

// #62 itself: the worktree's directory was removed out-of-band and never pruned,
// so git still lists it with the branch checked out. The prune runs before the
// in-use check, so the branch is re-attached (fast-forwarded first when only
// behind), not refused as checked out by a worktree that no longer exists.
func TestNew_ReattachesABranchWhoseWorktreeDirectoryWentAway(t *testing.T) {
	for _, behind := range []bool{false, true} {
		t.Run(map[bool]string{false: "equal", true: "behind"}[behind], func(t *testing.T) {
			f := newAdoptFixture(t)
			want := f.pushedBranch()
			if behind {
				want = f.onOrigin(want, "x 2")
			}
			gitW(t, f.repo, "worktree", "add", "-q", f.newDir(), newBranch)
			if err := os.RemoveAll(f.newDir()); err != nil {
				t.Fatal(err)
			}
			dir, err := New(f.c, newBranch)
			if err != nil {
				t.Fatalf("New: %v", err)
			}
			f.assertWorktreeAt(dir, want)
		})
	}
}

// captureStdout returns what f prints to os.Stdout, where wt prints the
// commands it suggests.
func captureStdout(t *testing.T, f func()) string {
	t.Helper()
	r, w, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	done := make(chan string)
	go func() {
		b, _ := io.ReadAll(r)
		done <- string(b)
	}()
	old := os.Stdout
	os.Stdout = w
	defer func() { os.Stdout = old }()
	f()
	_ = w.Close()
	return <-done
}

// #198 review (s1): a local branch another worktree has checked out is refused
// whatever its relation to origin/<branch>, before anything is created: git
// would refuse the second checkout at worktree-add, which a claim reaches only
// after it assigned the issue.
func TestNew_RefusesABranchCheckedOutElsewhere(t *testing.T) {
	shapes := map[string]func(f *adoptFixture) string{
		"equal": func(f *adoptFixture) string { return f.pushedBranch() },
		"ahead": func(f *adoptFixture) string {
			ahead := f.commitIn(f.repo, f.pushedBranch(), "not pushed yet")
			gitW(f.t, f.repo, "update-ref", "refs/heads/"+newBranch, ahead)
			return ahead
		},
		"never pushed": func(f *adoptFixture) string {
			tip := f.commitIn(f.repo, f.base, "local work")
			gitW(f.t, f.repo, "update-ref", "refs/heads/"+newBranch, tip)
			return tip
		},
		"diverged": func(f *adoptFixture) string {
			f.onOrigin(f.pushedBranch(), "pushed from elsewhere")
			mine := f.commitIn(f.repo, f.base, "mine")
			gitW(f.t, f.repo, "update-ref", "refs/heads/"+newBranch, mine)
			return mine
		},
	}
	for name, shape := range shapes {
		t.Run(name, func(t *testing.T) {
			f := newAdoptFixture(t)
			tip := shape(f)
			gitW(t, f.repo, "worktree", "add", "-q", filepath.Join(t.TempDir(), "elsewhere"), newBranch)
			_, err := New(f.c, newBranch)
			if !errors.Is(err, ErrBranchInUse) {
				t.Fatalf("New = %v, want an ErrBranchInUse refusal", err)
			}
			if got := f.tip(f.repo, "refs/heads/"+newBranch); got != tip {
				t.Errorf("the branch moved to %s, want it left at %s", got, tip)
			}
			f.assertNothingAdopted(newBranch)
		})
	}
}

// #198 review (s5): wt's worktree for the branch is there but not on it, here a
// detached HEAD at origin's tip (a rebase stopped there, say) or another branch.
// new would hand it back, and claim commit its placeholder on whatever it has
// checked out while its push of the branch went "up to date"; both refuse before
// any comparison, and nothing moves.
func TestPlanNew_RefusesAnExistingWorktreeOffTheBranch(t *testing.T) {
	for _, onBranch := range []string{"", "other"} {
		for _, who := range []NewFor{{}, {ClaimIssue: "42"}} {
			t.Run(fmt.Sprintf("on %q, claim %q", onBranch, who.ClaimIssue), func(t *testing.T) {
				f := newAdoptFixture(t)
				tip := f.pushedBranch()
				if onBranch == "" {
					gitW(t, f.repo, "worktree", "add", "-q", "--detach", f.newDir(), tip)
				} else {
					gitW(t, f.repo, "worktree", "add", "-q", "-b", onBranch, f.newDir(), tip)
				}
				_, err := PlanNew(f.c, newBranch, who)
				if !errors.Is(err, ErrWorktreeOffBranch) {
					t.Fatalf("PlanNew = %v, want an ErrWorktreeOffBranch refusal", err)
				}
				if got := f.tip(f.newDir(), "HEAD"); got != tip {
					t.Errorf("the worktree moved to %s, want it left at %s", got, tip)
				}
			})
		}
	}
}

// #198 review (s14): worktree_root inside the repo (a gitignored .worktrees/),
// and a leftover empty directory at wt's path. That directory is inside the
// main checkout, so it used to count as the branch's worktree: new handed back
// the main checkout, claim committed its placeholder onto main, and the
// behind-refusal printed a `git -C <dir> merge` that moved main. It is not a
// worktree; it is cleared, and the branch gets a real one.
func TestNew_ALeftoverDirInsideTheCheckoutIsNotAWorktree(t *testing.T) {
	f := newAdoptFixture(t)
	f.c.WorktreeRoot = filepath.Join(f.repo, ".worktrees")
	if err := os.WriteFile(filepath.Join(f.repo, ".git", "info", "exclude"), []byte(".worktrees/\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	old := f.pushedBranch()
	head := f.onOrigin(old, "x 2")
	if err := os.MkdirAll(f.newDir(), 0o755); err != nil {
		t.Fatal(err)
	}
	main := f.tip(f.repo, "refs/heads/main")
	dir, err := New(f.c, newBranch)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	f.assertWorktreeAt(dir, head)
	if got := f.tip(f.repo, "refs/heads/main"); got != main {
		t.Errorf("main moved to %s; it must stay at %s", got, main)
	}
	if got := gitx.SymbolicHead(f.repo); got != "refs/heads/main" {
		t.Errorf("the main checkout is on %q, want refs/heads/main", got)
	}
}

// #198 review (s6): origin deleted a branch that was pushed there (its PR
// merged), and the clone may still hold origin/<branch> as it was. That is not
// what origin has, so nothing is compared with it: a local branch behind it was
// fast-forwarded onto the deleted branch's last state. new attaches the branch
// as it is (with a warning); claim, which would push it back, refuses before
// anything happens.
func TestPlanNew_ABranchDeletedOnOrigin(t *testing.T) {
	for _, pruned := range []bool{false, true} {
		t.Run(fmt.Sprintf("tracking ref pruned %v", pruned), func(t *testing.T) {
			f := newAdoptFixture(t)
			old := f.commitIn(f.repo, f.base, "x 1")
			gitW(t, f.repo, "update-ref", "refs/heads/"+newBranch, old)
			gitW(t, f.repo, "push", "-q", "-u", "origin", newBranch)
			f.onOrigin(old, "x 2")
			gitW(t, f.repo, "fetch", "-q", "origin") // origin/feat/x: one ahead of the local branch
			gitW(t, f.origin, "update-ref", "-d", "refs/heads/"+newBranch)
			if pruned {
				gitW(t, f.repo, "fetch", "-q", "--prune", "origin")
			}

			_, err := PlanNew(f.c, newBranch, NewFor{ClaimIssue: "42"})
			if !errors.Is(err, ErrGoneFromOrigin) {
				t.Fatalf("PlanNew for a claim = %v, want an ErrGoneFromOrigin refusal", err)
			}
			if got := f.tip(f.repo, "refs/heads/"+newBranch); got != old {
				t.Fatalf("the refusal moved the branch to %s, want %s", got, old)
			}

			dir, err := New(f.c, newBranch)
			if err != nil {
				t.Fatalf("New: %v", err)
			}
			f.assertWorktreeAt(dir, old)

			// claim refuses the worktree new made, too: it would push the branch back.
			if _, err := PlanNew(f.c, newBranch, NewFor{ClaimIssue: "42"}); !errors.Is(err, ErrGoneFromOrigin) {
				t.Errorf("PlanNew for a claim, worktree present = %v, want an ErrGoneFromOrigin refusal", err)
			}
		})
	}
}

// A branch never pushed (no upstream of its own) that origin lacks is the #62
// case, attached as it is, also for a claim; only one pushed before and gone
// from origin is refused (PushedUnderItsName). Pure.
func TestPushedUnderItsName(t *testing.T) {
	cases := []struct {
		remote, merge string
		want          bool
	}{
		{"origin", "refs/heads/feat/x", true},
		{"origin", "refs/heads/main", false}, // a `wt new` branch tracks the base until pushed (#175)
		{"origin", "refs/heads/feat/y", false},
		{"upstream", "refs/heads/feat/x", false},
		{"", "", false},
		{".", "refs/heads/feat/x", false},
	}
	for _, c := range cases {
		if got := PushedUnderItsName(c.remote, c.merge, "feat/x"); got != c.want {
			t.Errorf("PushedUnderItsName(%q, %q) = %v, want %v", c.remote, c.merge, got, c.want)
		}
	}
}

// #198 review (s7): in a single-branch clone (and every --depth clone) the
// plain `git fetch origin <branch>` never wrote origin/<branch>, so a stale
// local branch diverged from what was pushed was attached "unverified", the
// #198 bug itself. It is refused.
func TestNew_RefusesADivergedBranchInASingleBranchClone(t *testing.T) {
	f := newAdoptFixture(t)
	clone := filepath.Join(t.TempDir(), "single")
	gitW(t, f.repo, "clone", "-q", "--single-branch", "-b", "main", f.origin, clone)
	t.Chdir(clone)
	f.c.Root = clone
	f.c.WorktreeRoot = filepath.Join(filepath.Dir(clone), "single-worktrees")
	stale := f.commitIn(clone, f.base, "old attempt")
	gitW(t, clone, "update-ref", "refs/heads/"+newBranch, stale)
	head := f.onOrigin(f.base, "new attempt")
	_, err := New(f.c, newBranch)
	if err == nil {
		t.Fatal("New attached a branch that diverged from origin/feat/x in a single-branch clone")
	}
	for _, sha := range []string{stale, head} {
		if !strings.Contains(err.Error(), sha[:8]) {
			t.Errorf("the refusal must name both tips; %q lacks %s", err, sha[:8])
		}
	}
	if got := f.tip(clone, "refs/heads/"+newBranch); got != stale {
		t.Errorf("the branch moved to %s, want it left at %s", got, stale)
	}
}

// #198 review (s8): origin has Feat/x (someone else's), the clone fetched it,
// and my feat/x was never pushed. With core.ignorecase the loose
// refs/remotes/origin/Feat/x is the file origin/feat/x resolves to, so new
// fast-forwarded my branch onto their commits. It is attached as it is. Packed,
// the twin never resolved; that case stays as a control. (On a case-sensitive
// filesystem both pass trivially.)
func TestNew_DoesNotFastForwardOntoARemoteCaseTwin(t *testing.T) {
	for _, packed := range []bool{false, true} {
		t.Run(fmt.Sprintf("packed %v", packed), func(t *testing.T) {
			f := newAdoptFixture(t)
			mine := f.commitIn(f.repo, f.base, "my never-pushed feat/x")
			gitW(t, f.repo, "update-ref", "refs/heads/"+newBranch, mine)
			theirs := f.commitIn(f.repo, mine, "someone else's Feat/x")
			gitW(t, f.repo, "push", "-q", "origin", theirs+":refs/heads/Feat/x")
			gitW(t, f.repo, "fetch", "-q", "origin")
			if packed {
				gitW(t, f.repo, "pack-refs", "--all")
			}
			dir, err := New(f.c, newBranch)
			if err != nil {
				t.Fatalf("New: %v", err)
			}
			f.assertWorktreeAt(dir, mine)
			if got := f.tip(f.repo, "refs/heads/"+newBranch); got != mine {
				t.Errorf("feat/x moved to %s, want it left at %s", got, mine)
			}
		})
	}
}

// Offline, the local branch is compared with origin/<branch> as last fetched,
// and says so (#198): a diverged one is refused, naming that copy.
func TestNew_OfflineRefusesADivergedBranchAsLastFetched(t *testing.T) {
	f := newAdoptFixture(t)
	f.pushedBranch()
	mine := f.commitIn(f.repo, f.base, "rewritten locally")
	gitW(t, f.repo, "update-ref", "refs/heads/"+newBranch, mine)
	f.offline()
	_, err := New(f.c, newBranch)
	if err == nil || !strings.Contains(err.Error(), "as last fetched") {
		t.Fatalf("New offline = %v, want a refusal naming origin/feat/x as last fetched", err)
	}
	if got := f.tip(f.repo, "refs/heads/"+newBranch); got != mine {
		t.Errorf("the branch moved to %s, want it left at %s", got, mine)
	}
}

// Create checks where the worktree it attached landed (verifyAdopted), not only
// what it planned: here a post-checkout hook moves the branch while
// worktree-add runs. The worktree is removed again and Create refuses.
func TestCreate_RemovesAWorktreeThatLandedOffThePlannedCommit(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("the hook is a shell script")
	}
	f := newAdoptFixture(t)
	f.pushedBranch()
	moved := f.commitIn(f.repo, f.base, "moved by a hook")
	p, err := PlanNew(f.c, newBranch, NewFor{})
	if err != nil {
		t.Fatalf("PlanNew: %v", err)
	}
	hook := "#!/bin/sh\ngit update-ref refs/heads/" + newBranch + " " + moved + "\n"
	if err := os.WriteFile(filepath.Join(f.repo, ".git", "hooks", "post-checkout"), []byte(hook), 0o755); err != nil {
		t.Fatal(err)
	}
	if _, err := p.Create(); !errors.Is(err, ErrLandedElsewhere) {
		t.Fatalf("Create = %v, want an ErrLandedElsewhere refusal", err)
	}
	f.assertNothingAdopted(newBranch)
}

// #198 review: claim's way on for a branch it would have to push over origin's
// names the leased force-push for the commonest divergence, the operator's own
// rebase (s15): rebasing that onto origin/<branch>, the other advice, replays
// the base's commits onto the old branch.
func TestNew_DivergedAdviceNamesTheLeasedForcePush(t *testing.T) {
	f := newAdoptFixture(t)
	pushed := f.pushedBranch()
	mine := f.commitIn(f.repo, f.base, "rebased locally")
	gitW(t, f.repo, "update-ref", "refs/heads/"+newBranch, mine)
	var err error
	out := captureStdout(t, func() { _, err = PlanNew(f.c, newBranch, NewFor{ClaimIssue: "42"}) })
	if err == nil {
		t.Fatal("PlanNew attached a diverged branch")
	}
	if want := "git push --force-with-lease=" + newBranch + ":" + pushed + " origin " + newBranch; !strings.Contains(out, want) {
		t.Errorf("the advice lacks %q:\n%s", want, out)
	}
}
