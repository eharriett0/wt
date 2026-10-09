package worktree

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/eharriett0/wt/internal/gitx"
)

// #198: `wt new` and `wt claim` decide what to do with an existing local branch
// of the name they want exactly as `wt adopt` does (#167), against
// origin/<branch>. This pins the table the issue asks for, so a change to
// DecideAdopt for adopt's sake cannot quietly change new and claim.
func TestDecideAdopt_NewAndClaimTable(t *testing.T) {
	cases := []struct {
		name   string
		exists bool
		rel    TipRelation
		inUse  bool
		want   AdoptAction
	}{
		{"no local branch: cut a new one from the base, as before", false, TipNoTarget, false, AdoptCreate},
		{"no origin/<branch>: the #62 never-pushed branch, attached as it is", true, TipNoTarget, false, AdoptUnverified},
		{"equal: attached as it is", true, TipEqual, false, AdoptAsIs},
		{"only ahead (unpushed, #62): attached, with a note naming the commits", true, TipAhead, false, AdoptAhead},
		{"only behind: fast-forwarded first", true, TipBehind, false, AdoptFastForward},
		{"only behind, checked out elsewhere: never moved", true, TipBehind, true, AdoptRefuseInUse},
		{"diverged (an earlier attempt that reused the name): refused", true, TipDiverged, false, AdoptRefuse},
		{"not comparable: refused", true, TipUnknown, false, AdoptRefuse},
	}
	for _, c := range cases {
		if got := DecideAdopt(c.exists, c.rel, c.inUse); got != c.want {
			t.Errorf("%s: DecideAdopt(%v, %d, %v) = %d, want %d", c.name, c.exists, c.rel, c.inUse, got, c.want)
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
func TestNew_RefusesACaseOnlyTwin(t *testing.T) {
	for _, pushed := range []bool{true, false} {
		t.Run(map[bool]string{true: "origin/feat/x ahead of the twin", false: "no origin/feat/x"}[pushed], func(t *testing.T) {
			f := newAdoptFixture(t)
			gitW(t, f.repo, "config", "core.ignorecase", "true")
			old := f.commitIn(f.repo, f.base, "x 1")
			gitW(t, f.repo, "update-ref", "refs/heads/Feat/x", old)
			if pushed {
				gitW(t, f.repo, "push", "-q", "origin", old+":refs/heads/"+newBranch)
				f.onOrigin(old, "x 2")
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
