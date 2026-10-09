package claim

import (
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"testing"

	"github.com/eharriett0/wt/internal/config"
	"github.com/eharriett0/wt/internal/worktree"
)

// #198 through claim's own wiring: `wt claim` re-attaches a local branch of the
// claim's name (#62), and before #198 it did so unchecked, so a branch left by
// an earlier attempt, or one behind or diverged from origin/<branch>, got the
// placeholder commit, the push was rejected, and the #159 rollback DELETED the
// branch, unpushed commits and all. These tests run Claim itself: real git
// against a scratch bare origin, and a fake gh first on PATH that records every
// call and never reaches GitHub.

const claimIssue = "42"

// claimBranch is BranchName("feat-", "42", SlugFromTitle("x")): the fake gh
// titles every issue "x".
const claimBranch = "feat-42-x"

// fakeGhScript answers the gh calls `wt claim` makes and logs each one. The
// issue is assigned while assigned-<n> exists.
const fakeGhScript = `#!/bin/sh
d=$(dirname "$0")
printf '%s\n' "$*" >> "$d/gh.log"
case "$1 $2" in
  "auth status") exit 0 ;;
  "api user") echo tester; exit 0 ;;
  "api graphql") exit 0 ;;
  "issue view")
    case "$*" in
      *"--json number"*) exit 0 ;;
      *"--json state"*) echo OPEN; exit 0 ;;
      *"--json assignees"*) if [ -f "$d/assigned-$3" ]; then echo tester; fi; exit 0 ;;
      *"--json title"*) echo x; exit 0 ;;
      *"--json url"*) echo "https://github.com/o/r/issues/$3"; exit 0 ;;
    esac ;;
  "issue edit")
    case "$*" in
      *--add-assignee*) touch "$d/assigned-$3"; exit 0 ;;
      *--remove-assignee*) rm -f "$d/assigned-$3"; exit 0 ;;
    esac ;;
  "pr create") echo "https://github.com/o/r/pull/7"; exit 0 ;;
esac
echo "fake gh: unhandled: $*" >&2
exit 1
`

type claimFixture struct {
	t                *testing.T
	origin, repo, gh string
	base, tree       string
	c                *config.Config
}

func newClaimFixture(t *testing.T) *claimFixture {
	t.Helper()
	if runtime.GOOS == "windows" {
		t.Skip("the fake gh is a shell script")
	}
	// Hermetic, as worktree's adoptFixture: the runner's git config never reaches it.
	t.Setenv("GIT_CONFIG_GLOBAL", os.DevNull)
	t.Setenv("GIT_CONFIG_NOSYSTEM", "1")
	for _, k := range []string{"GIT_AUTHOR_NAME", "GIT_COMMITTER_NAME"} {
		t.Setenv(k, "t")
	}
	for _, k := range []string{"GIT_AUTHOR_EMAIL", "GIT_COMMITTER_EMAIL"} {
		t.Setenv(k, "t@t.test")
	}
	t.Setenv("WT_WINDOW", "claim-test")
	tmp := t.TempDir()
	f := &claimFixture{t: t, origin: filepath.Join(tmp, "origin.git"), repo: filepath.Join(tmp, "repo"), gh: filepath.Join(tmp, "fakegh")}

	if err := os.MkdirAll(f.gh, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(f.gh, "gh"), []byte(fakeGhScript), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", f.gh+string(os.PathListSeparator)+os.Getenv("PATH"))
	if got, err := exec.LookPath("gh"); err != nil || got != filepath.Join(f.gh, "gh") {
		t.Fatalf("gh resolves to %q (%v), not the fake", got, err)
	}

	f.git(tmp, "init", "-q", "--bare", "-b", "main", f.origin)
	f.git(tmp, "init", "-q", "-b", "main", f.repo)
	f.git(f.repo, "remote", "add", "origin", f.origin)
	f.git(f.repo, "commit", "-q", "--allow-empty", "-m", "base")
	f.git(f.repo, "push", "-q", "origin", "main")
	f.base = f.git(f.repo, "rev-parse", "HEAD")
	f.tree = f.git(f.repo, "rev-parse", "HEAD^{tree}")
	f.c = &config.Config{
		Root: f.repo, Base: "main", Prefix: "feat-", ClaimOpenPR: true,
		WorktreeRoot: filepath.Join(tmp, "repo-worktrees"),
		ActiveWork:   filepath.Join(tmp, "active-work.md"),
	}
	t.Chdir(f.repo)
	return f
}

func (f *claimFixture) git(dir string, args ...string) string {
	f.t.Helper()
	cmd := exec.Command("git", args...)
	cmd.Dir = dir
	out, err := cmd.CombinedOutput()
	if err != nil {
		f.t.Fatalf("git %v: %v\n%s", args, err, out)
	}
	return strings.TrimSpace(string(out))
}

// commit makes a commit on parent in dir's object store, without a checkout.
func (f *claimFixture) commit(dir, parent, msg string) string {
	return f.git(dir, "commit-tree", "-p", parent, "-m", msg, f.tree)
}

// ref resolves ref in dir, "" when it does not exist.
func (f *claimFixture) ref(dir, ref string) string {
	out, err := exec.Command("git", "-C", dir, "rev-parse", "--verify", "--quiet", ref).Output()
	if err != nil {
		return ""
	}
	return strings.TrimSpace(string(out))
}

func (f *claimFixture) local(sha string) {
	f.git(f.repo, "update-ref", "refs/heads/"+claimBranch, sha)
}

// pushed makes the claim's branch, at one commit on main, and pushes it.
func (f *claimFixture) pushed() string {
	tip := f.commit(f.repo, f.base, "x 1")
	f.local(tip)
	f.git(f.repo, "push", "-q", "origin", claimBranch)
	return tip
}

// onOrigin moves origin's branch to a new commit on parent, behind the clone's back.
func (f *claimFixture) onOrigin(parent, msg string) string {
	c := f.commit(f.origin, parent, msg)
	f.git(f.origin, "update-ref", "refs/heads/"+claimBranch, c)
	return c
}

// rejectPushes makes origin refuse every push from here on, as a protected
// branch or a lost credential does.
func (f *claimFixture) rejectPushes() {
	hook := filepath.Join(f.origin, "hooks", "pre-receive")
	if err := os.WriteFile(hook, []byte("#!/bin/sh\necho rejected by the test >&2\nexit 1\n"), 0o755); err != nil {
		f.t.Fatal(err)
	}
}

func (f *claimFixture) dir() string { return filepath.Join(f.c.WorktreeRoot, claimBranch) }

func (f *claimFixture) ghLog() string {
	b, _ := os.ReadFile(filepath.Join(f.gh, "gh.log"))
	return string(b)
}

func (f *claimFixture) assigned() bool {
	_, err := os.Stat(filepath.Join(f.gh, "assigned-"+claimIssue))
	return err == nil
}

// claim runs `wt claim 42 --yes`, or with --no-pr.
func (f *claimFixture) claim(openPR bool) error {
	return Claim(f.c, claimIssue, false, true, openPR, "")
}

// assertRefusedBeforeAnything checks a refusal left no partial claim: the issue
// never assigned, no PR, nothing recorded, the branch where it was, and no
// worktree, or the one that was already there (hadWorktree) unmoved.
func (f *claimFixture) assertRefusedBeforeAnything(err error, branchAt string, hadWorktree bool) {
	f.t.Helper()
	if err == nil {
		f.t.Fatal("Claim succeeded; it must refuse")
	}
	if log := f.ghLog(); strings.Contains(log, "issue edit") || strings.Contains(log, "pr create") {
		f.t.Errorf("a refusal must come before gh assigns anything or opens a PR; gh was asked:\n%s", log)
	}
	if hadWorktree {
		if got := f.ref(f.dir(), "HEAD"); got != branchAt {
			f.t.Errorf("the existing worktree is at %q after the refusal, want it left at %s", got, branchAt)
		}
	} else if _, serr := os.Stat(f.dir()); !os.IsNotExist(serr) {
		f.t.Errorf("a refusal must not create a worktree at %s (stat: %v)", f.dir(), serr)
	}
	if got := f.ref(f.repo, "refs/heads/"+claimBranch); got != branchAt {
		f.t.Errorf("the local branch is at %q after the refusal, want it left at %q", got, branchAt)
	}
	if b, _ := os.ReadFile(f.c.ActiveWork); strings.Contains(string(b), claimBranch) {
		f.t.Errorf("a refusal must not record the claim:\n%s", b)
	}
}

// assertClaimedOn checks a successful claim: the worktree is on the branch with
// the placeholder on top of parent, and origin has exactly that.
func (f *claimFixture) assertClaimedOn(err error, parent string) {
	f.t.Helper()
	if err != nil {
		f.t.Fatalf("Claim: %v", err)
	}
	head := f.ref(f.dir(), "HEAD")
	if got := f.ref(f.dir(), "HEAD^"); got != parent {
		f.t.Errorf("the placeholder sits on %s, want it on %s", got, parent)
	}
	if got := f.git(f.dir(), "log", "-1", "--format=%s"); !strings.HasPrefix(got, "WIP: claim #"+claimIssue) {
		f.t.Errorf("worktree HEAD is %q, want the claim's placeholder", got)
	}
	if got := f.ref(f.origin, "refs/heads/"+claimBranch); got != head {
		f.t.Errorf("origin has %s, want the claimed head %s pushed", got, head)
	}
	if !f.assigned() {
		f.t.Error("the issue is not assigned after a successful claim")
	}
}

var claimModes = map[string]bool{"claim": true, "claim --no-pr": false}

// #198, as reported: a local branch left by an earlier attempt that reused the
// name, while origin/<branch> is a newer one. Refused before the issue is
// assigned, with nothing created, moved or deleted. Before the fix the claim
// pushed into a rejection and its rollback deleted the branch.
func TestClaim_RefusesALocalBranchThatDiverged(t *testing.T) {
	for mode, openPR := range claimModes {
		t.Run(mode, func(t *testing.T) {
			f := newClaimFixture(t)
			stale := f.commit(f.repo, f.commit(f.repo, f.base, "old attempt 1"), "old attempt 2")
			f.local(stale)
			main := f.commit(f.origin, f.base, "main moves on")
			f.git(f.origin, "update-ref", "refs/heads/main", main)
			head := f.onOrigin(main, "new attempt")
			err := f.claim(openPR)
			f.assertRefusedBeforeAnything(err, stale, false)
			for _, sha := range []string{stale, head} {
				if err != nil && !strings.Contains(err.Error(), sha[:8]) {
					t.Errorf("the refusal must name both tips; %q lacks %s", err, sha[:8])
				}
			}
			if got := f.ref(f.origin, "refs/heads/"+claimBranch); got != head {
				t.Errorf("origin's branch moved to %s; it must stay at %s", got, head)
			}
		})
	}
}

// A local branch only behind origin/<branch> is fast-forwarded, then claimed:
// the placeholder lands on origin's tip and the push is a fast-forward. Before
// #198 the placeholder went on the old tip, origin rejected the push, and the
// rollback deleted the branch.
func TestClaim_FastForwardsALocalBranchOnlyBehind(t *testing.T) {
	for mode, openPR := range claimModes {
		t.Run(mode, func(t *testing.T) {
			f := newClaimFixture(t)
			old := f.pushed()
			head := f.onOrigin(f.onOrigin(old, "x 2"), "x 3")
			f.assertClaimedOn(f.claim(openPR), head)
			if made := strings.Contains(f.ghLog(), "pr create"); made != openPR {
				t.Errorf("pr create called = %v, want %v for %s", made, openPR, mode)
			}
		})
	}
}

// Only ahead (#62: commits not pushed yet) is attached as it is, and the claim
// pushes them with the placeholder on top.
func TestClaim_AttachesALocalBranchOnlyAhead(t *testing.T) {
	f := newClaimFixture(t)
	ahead := f.commit(f.repo, f.pushed(), "not pushed yet")
	f.local(ahead)
	f.assertClaimedOn(f.claim(true), ahead)
}

func TestClaim_AttachesAnEqualLocalBranch(t *testing.T) {
	f := newClaimFixture(t)
	tip := f.pushed()
	f.assertClaimedOn(f.claim(true), tip)
}

// #62: a branch never pushed, with work on it, has no origin/<branch> to compare
// with and is attached as it is.
func TestClaim_AttachesANeverPushedBranch(t *testing.T) {
	f := newClaimFixture(t)
	tip := f.commit(f.repo, f.base, "local work")
	f.local(tip)
	f.assertClaimedOn(f.claim(true), tip)
}

// The #159 rollback, on a branch the claim re-attached: a rejected push must not
// delete it (the bug deleted never-pushed work outright). The placeholder is
// taken back off, the worktree the claim made goes, and the issue is unassigned.
func TestClaim_RollbackKeepsABranchItReattached(t *testing.T) {
	shapes := map[string]func(f *claimFixture) string{
		"never pushed": func(f *claimFixture) string {
			tip := f.commit(f.repo, f.base, "local work")
			f.local(tip)
			return tip
		},
		"ahead of origin": func(f *claimFixture) string {
			tip := f.commit(f.repo, f.pushed(), "not pushed yet")
			f.local(tip)
			return tip
		},
	}
	for name, shape := range shapes {
		t.Run(name, func(t *testing.T) {
			f := newClaimFixture(t)
			tip := shape(f)
			f.rejectPushes()
			err := f.claim(true)
			if err == nil || !strings.Contains(err.Error(), "push branch") {
				t.Fatalf("Claim = %v, want the push failure", err)
			}
			if got := f.ref(f.repo, "refs/heads/"+claimBranch); got != tip {
				t.Errorf("after the rollback the local branch is at %q, want it kept at %s", got, tip)
			}
			if _, serr := os.Stat(f.dir()); !os.IsNotExist(serr) {
				t.Errorf("the worktree the claim made must be removed (stat: %v)", serr)
			}
			if f.assigned() {
				t.Error("the rollback must unassign the issue")
			}
		})
	}
}

// A branch the claim cut from the base holds only its placeholder: the #159
// rollback still deletes it with the worktree.
func TestClaim_RollbackDeletesABranchItCreated(t *testing.T) {
	f := newClaimFixture(t)
	f.rejectPushes()
	if err := f.claim(true); err == nil {
		t.Fatal("Claim succeeded although origin rejects every push")
	}
	if got := f.ref(f.repo, "refs/heads/"+claimBranch); got != "" {
		t.Errorf("the branch the claim created is still at %s; the rollback deletes it", got)
	}
	if _, serr := os.Stat(f.dir()); !os.IsNotExist(serr) {
		t.Errorf("the worktree the claim made must be removed (stat: %v)", serr)
	}
}

// wt's worktree for the branch already exists, only behind origin/<branch>: the
// placeholder on its HEAD could not be pushed, and wt never moves a checked-out
// branch, so the claim is refused before the issue is assigned.
func TestClaim_RefusesAnExistingWorktreeOnlyBehind(t *testing.T) {
	f := newClaimFixture(t)
	old := f.pushed()
	f.onOrigin(old, "x 2")
	f.git(f.repo, "worktree", "add", "-q", f.dir(), claimBranch)
	err := f.claim(true)
	if !errors.Is(err, worktree.ErrBranchInUse) {
		t.Fatalf("Claim = %v, want an ErrBranchInUse refusal", err)
	}
	f.assertRefusedBeforeAnything(err, old, true)
}

// ...and one that diverged is refused the same way.
func TestClaim_RefusesAnExistingWorktreeThatDiverged(t *testing.T) {
	f := newClaimFixture(t)
	f.onOrigin(f.pushed(), "pushed from elsewhere")
	mine := f.commit(f.repo, f.ref(f.repo, "refs/heads/"+claimBranch), "mine")
	f.local(mine)
	f.git(f.repo, "worktree", "add", "-q", f.dir(), claimBranch)
	f.assertRefusedBeforeAnything(f.claim(true), mine, true)
}

// The behind fast-forward never moves a branch another worktree has checked
// out, and the refusal comes before the issue is assigned.
func TestClaim_DoesNotMoveABranchCheckedOutElsewhere(t *testing.T) {
	f := newClaimFixture(t)
	old := f.pushed()
	f.onOrigin(old, "x 2")
	f.git(f.repo, "worktree", "add", "-q", filepath.Join(t.TempDir(), "elsewhere"), claimBranch)
	err := f.claim(true)
	if !errors.Is(err, worktree.ErrBranchInUse) {
		t.Fatalf("Claim = %v, want an ErrBranchInUse refusal", err)
	}
	f.assertRefusedBeforeAnything(err, old, false)
}

// core.ignorecase: a branch differing only in case is the same loose ref file,
// so the fast-forward would move it. Refused before the issue is assigned.
func TestClaim_RefusesACaseOnlyTwin(t *testing.T) {
	f := newClaimFixture(t)
	f.git(f.repo, "config", "core.ignorecase", "true")
	twin := "Feat-42-x"
	old := f.commit(f.repo, f.base, "x 1")
	f.git(f.repo, "update-ref", "refs/heads/"+twin, old)
	f.git(f.repo, "push", "-q", "origin", old+":refs/heads/"+claimBranch)
	f.onOrigin(old, "x 2")
	err := f.claim(true)
	if !errors.Is(err, worktree.ErrCaseTwin) {
		t.Fatalf("Claim = %v, want an ErrCaseTwin refusal", err)
	}
	if log := f.ghLog(); strings.Contains(log, "issue edit") {
		t.Errorf("the refusal must come before the issue is assigned; gh was asked:\n%s", log)
	}
	if got := f.ref(f.repo, "refs/heads/"+twin); got != old {
		t.Errorf("%s moved to %s; it must stay at %s", twin, got, old)
	}
}

// The rollback removes only what the claim made (#198). Pure.
func TestRollbackFor(t *testing.T) {
	cases := []struct {
		name                   string
		newWorktree, newBranch bool
		want                   rollbackScope
	}{
		{"a new branch cut from the base: worktree and branch go (#159)", true, true, rollbackScope{removeWorktree: true, deleteBranch: true}},
		{"a re-attached branch (#62): keep it, undo the placeholder, drop the new worktree", true, false, rollbackScope{undoPlaceholder: true, removeWorktree: true}},
		{"an existing worktree handed back: keep both, undo the placeholder", false, false, rollbackScope{undoPlaceholder: true}},
		{"impossible (a new branch needs a new worktree): delete nothing", false, true, rollbackScope{undoPlaceholder: true}},
	}
	for _, c := range cases {
		if got := rollbackFor(c.newWorktree, c.newBranch); got != c.want {
			t.Errorf("%s: rollbackFor(%v, %v) = %+v, want %+v", c.name, c.newWorktree, c.newBranch, got, c.want)
		}
	}
}
