package worktree

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/eharriett0/wt/internal/config"
	"github.com/eharriett0/wt/internal/gitx"
)

func TestClassifyTips(t *testing.T) {
	cases := []struct {
		name                         string
		local, target                string
		localInTarget, targetInLocal bool
		known                        bool
		want                         TipRelation
	}{
		{"no target", "a1", "", false, false, true, TipNoTarget},
		{"no target, even uncomparable", "a1", "", false, false, false, TipNoTarget},
		{"equal needs no ancestry", "a1", "a1", false, false, false, TipEqual},
		{"local is an ancestor: behind", "a1", "b2", true, false, true, TipBehind},
		{"target is an ancestor: ahead", "a1", "b2", false, true, true, TipAhead},
		{"neither: diverged (the #167 stale branch)", "a1", "b2", false, false, true, TipDiverged},
		{"could not compare", "a1", "b2", false, false, false, TipUnknown},
		{"no local tip", "", "b2", false, false, false, TipUnknown},
	}
	for _, c := range cases {
		if got := ClassifyTips(c.local, c.target, c.localInTarget, c.targetInLocal, c.known); got != c.want {
			t.Errorf("%s: ClassifyTips = %d, want %d", c.name, got, c.want)
		}
	}
}

func TestDecideAdopt(t *testing.T) {
	cases := []struct {
		name   string
		exists bool
		rel    TipRelation
		inUse  bool
		want   AdoptAction
	}{
		{"no local branch: worktree-add creates it, as before", false, TipNoTarget, false, AdoptCreate},
		{"no local branch, whatever the relation", false, TipUnknown, false, AdoptCreate},
		{"equal: attach unchanged", true, TipEqual, false, AdoptAsIs},
		{"nothing to compare: attach, unverified", true, TipNoTarget, false, AdoptUnverified},
		{"only behind: fast-forward", true, TipBehind, false, AdoptFastForward},
		{"behind but checked out: never moved", true, TipBehind, true, AdoptRefuseInUse},
		{"#167: diverged is refused", true, TipDiverged, false, AdoptRefuse},
		{"ahead is refused: leftover or unpushed, wt cannot tell", true, TipAhead, false, AdoptRefuse},
		{"uncomparable is refused", true, TipUnknown, false, AdoptRefuse},
		{"diverged and checked out: still refused", true, TipDiverged, true, AdoptRefuse},
	}
	for _, c := range cases {
		if got := DecideAdopt(c.exists, c.rel, c.inUse); got != c.want {
			t.Errorf("%s: DecideAdopt(%v, %d, %v) = %d, want %d", c.name, c.exists, c.rel, c.inUse, got, c.want)
		}
	}
}

// The #167 invariant over every input: an existing local branch is attached
// without a move ONLY when it is the target (or there is nothing to compare it
// with), and it is moved ONLY forward and ONLY when no worktree has it checked
// out. Anything else is a refusal, so a stale branch is never checked out.
func TestDecideAdopt_NeverAttachesAMismatchedBranch(t *testing.T) {
	for rel := TipNoTarget; rel <= TipUnknown; rel++ {
		for _, inUse := range []bool{false, true} {
			got := DecideAdopt(true, rel, inUse)
			switch got {
			case AdoptCreate:
				t.Errorf("rel=%d inUse=%v: an existing local branch must never take the create path", rel, inUse)
			case AdoptAsIs:
				if rel != TipEqual {
					t.Errorf("rel=%d inUse=%v: attached unchanged without being the target", rel, inUse)
				}
			case AdoptUnverified:
				if rel != TipNoTarget {
					t.Errorf("rel=%d inUse=%v: attached unverified although there was a target", rel, inUse)
				}
			case AdoptFastForward:
				if rel != TipBehind || inUse {
					t.Errorf("rel=%d inUse=%v: fast-forwarded a branch that is not only behind, or is checked out", rel, inUse)
				}
			}
		}
	}
}

func TestDecideExisting(t *testing.T) {
	cases := []struct {
		rel  TipRelation
		want ExistingAction
	}{
		{TipNoTarget, ExistingReuse},
		{TipEqual, ExistingReuse},
		{TipBehind, ExistingReuseBehind}, // the PR moved on: a re-run stays idempotent
		{TipAhead, ExistingReuseAhead},   // unpushed commits in your own worktree
		{TipDiverged, ExistingRefuse},    // #167: a worktree left over from an earlier PR
		{TipUnknown, ExistingRefuse},
	}
	for _, c := range cases {
		if got := DecideExisting(c.rel); got != c.want {
			t.Errorf("DecideExisting(%d) = %d, want %d", c.rel, got, c.want)
		}
	}
}

func TestCheckedOutIn(t *testing.T) {
	refs := []gitx.WorktreeRef{
		{Path: "/r", Branch: "main"},
		{Path: "/w/detached", Branch: ""},
		{Path: "/w/bot-image", Branch: "bot/image"},
	}
	cases := []struct{ branch, want string }{
		{"bot/image", "/w/bot-image"},
		{"main", "/r"}, // the primary checkout counts too
		{"bot", ""},    // no prefix match
		{"", ""},       // a detached worktree is not "checked out on" an empty branch
	}
	for _, c := range cases {
		if got := CheckedOutIn(refs, c.branch); got != c.want {
			t.Errorf("CheckedOutIn(%q) = %q, want %q", c.branch, got, c.want)
		}
	}
}

// --- live-git regression tests: local repos only, a bare "origin", no network.
// Commits are made with commit-tree (no checkout), and the PR side is written
// straight into the bare origin, so its commits reach the operator's clone only
// through Adopt's own fetch, as they would from a real second pusher.

func gitW(t *testing.T, dir string, args ...string) {
	t.Helper()
	gitOutW(t, dir, args...)
}

func gitOutW(t *testing.T, dir string, args ...string) string {
	t.Helper()
	cmd := exec.Command("git", args...)
	cmd.Dir = dir
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("git %v: %v\n%s", args, err, out)
	}
	return strings.TrimSpace(string(out))
}

// adoptFixture is a bare origin and the operator's clone, where wt runs.
type adoptFixture struct {
	t            *testing.T
	origin, repo string
	base, tree   string // main's first commit, and the (empty) tree every commit reuses
	c            *config.Config
}

func newAdoptFixture(t *testing.T) *adoptFixture {
	t.Helper()
	for _, k := range []string{"GIT_AUTHOR_NAME", "GIT_COMMITTER_NAME"} {
		t.Setenv(k, "t")
	}
	for _, k := range []string{"GIT_AUTHOR_EMAIL", "GIT_COMMITTER_EMAIL"} {
		t.Setenv(k, "t@t.test")
	}
	tmp := t.TempDir()
	f := &adoptFixture{t: t, origin: filepath.Join(tmp, "origin.git"), repo: filepath.Join(tmp, "repo")}
	gitW(t, tmp, "init", "-q", "--bare", "-b", "main", f.origin)
	gitW(t, tmp, "init", "-q", "-b", "main", f.repo)
	gitW(t, f.repo, "remote", "add", "origin", f.origin)
	gitW(t, f.repo, "commit", "-q", "--allow-empty", "-m", "base")
	gitW(t, f.repo, "push", "-q", "origin", "main")
	f.base = gitOutW(t, f.repo, "rev-parse", "HEAD")
	f.tree = gitOutW(t, f.repo, "rev-parse", "HEAD^{tree}")
	f.c = &config.Config{Root: f.repo, Base: "main", WorktreeRoot: filepath.Join(tmp, "repo-worktrees")}
	t.Chdir(f.repo) // gitx runs git in cwd, as wt does from the operator's checkout
	return f
}

// commitIn makes a commit on parent inside dir's object store and returns it.
func (f *adoptFixture) commitIn(dir, parent, msg string) string {
	return gitOutW(f.t, dir, "commit-tree", "-p", parent, "-m", msg, f.tree)
}

func (f *adoptFixture) tip(dir, ref string) string { return gitOutW(f.t, dir, "rev-parse", ref) }

// staleBranch builds the #167 shape: the operator's local bot/image is left over
// from an EARLIER PR that reused the name (two commits on the old main), while
// the NEW PR's head, on origin only, sits on a main that has moved on.
func (f *adoptFixture) staleBranch() (stale, head string) {
	stale = f.commitIn(f.repo, f.commitIn(f.repo, f.base, "old pr 1"), "old pr 2")
	gitW(f.t, f.repo, "update-ref", "refs/heads/bot/image", stale)
	main := f.commitIn(f.origin, f.base, "main moves on")
	head = f.commitIn(f.origin, main, "new pr 1")
	gitW(f.t, f.origin, "update-ref", "refs/heads/main", main)
	gitW(f.t, f.origin, "update-ref", "refs/heads/bot/image", head)
	return stale, head
}

// behindBranch: the operator's local bot/image is the PR's first commit (pushed,
// so origin/bot/image is known but stale), and the PR has since moved on.
func (f *adoptFixture) behindBranch() (old, head string) {
	old = f.commitIn(f.repo, f.base, "pr 1")
	gitW(f.t, f.repo, "update-ref", "refs/heads/bot/image", old)
	gitW(f.t, f.repo, "push", "-q", "origin", "bot/image")
	head = f.commitIn(f.origin, f.commitIn(f.origin, old, "pr 2"), "pr 3")
	gitW(f.t, f.origin, "update-ref", "refs/heads/bot/image", head)
	return old, head
}

// wants returns the by-branch and by-PR forms of the same adoption.
func wants(head string) map[string]AdoptWant {
	return map[string]AdoptWant{
		"by branch": {},
		"by PR":     {PR: "1079", PRHead: head},
	}
}

// #167, as reported: a stale local branch left by an earlier PR that reused the
// name must not be checked out in place of the PR head. Before the fix Adopt
// worktree-added it and reported success.
func TestAdopt_RefusesAStaleLocalBranch(t *testing.T) {
	for name := range wants("") {
		t.Run(name, func(t *testing.T) {
			f := newAdoptFixture(t)
			stale, head := f.staleBranch()
			wtDir, err := Adopt(f.c, "bot/image", wants(head)[name])
			if err == nil {
				t.Fatalf("Adopt checked out the stale local branch at %s; it must refuse", wtDir)
			}
			for _, sha := range []string{stale, head} {
				if !strings.Contains(err.Error(), sha[:8]) {
					t.Errorf("the refusal must name both tips; %q lacks %s", err, sha[:8])
				}
			}
			if _, serr := os.Stat(filepath.Join(f.c.WorktreeRoot, "bot-image")); !os.IsNotExist(serr) {
				t.Errorf("a refusal must not create the worktree (stat: %v)", serr)
			}
			if got := f.tip(f.repo, "refs/heads/bot/image"); got != stale {
				t.Errorf("a refusal must leave the local branch alone: at %s, want %s", got, stale)
			}
		})
	}
}

// A local branch that is only behind the target is fast-forwarded to it, then
// adopted; before #167 the worktree landed on the old commit. The PR head is on
// origin only, so this also pins that the comparison runs AFTER the fetch.
func TestAdopt_FastForwardsABehindLocalBranch(t *testing.T) {
	for name := range wants("") {
		t.Run(name, func(t *testing.T) {
			f := newAdoptFixture(t)
			_, head := f.behindBranch()
			wtDir, err := Adopt(f.c, "bot/image", wants(head)[name])
			if err != nil {
				t.Fatalf("Adopt: %v", err)
			}
			if got := f.tip(wtDir, "HEAD"); got != head {
				t.Errorf("adopted worktree is at %s, want the target %s", got, head)
			}
			if got := gitOutW(t, wtDir, "rev-parse", "--abbrev-ref", "HEAD"); got != "bot/image" {
				t.Errorf("adopted worktree is on %q, want bot/image", got)
			}
		})
	}
}

// A local branch that is the target is attached unchanged, and a re-run on the
// worktree that creates stays idempotent.
func TestAdopt_AttachesAnEqualLocalBranch(t *testing.T) {
	for name := range wants("") {
		t.Run(name, func(t *testing.T) {
			f := newAdoptFixture(t)
			head := f.commitIn(f.repo, f.base, "pr 1")
			gitW(t, f.repo, "update-ref", "refs/heads/bot/image", head)
			gitW(t, f.repo, "push", "-q", "origin", "bot/image")
			wtDir, err := Adopt(f.c, "bot/image", wants(head)[name])
			if err != nil {
				t.Fatalf("Adopt: %v", err)
			}
			if got := f.tip(wtDir, "HEAD"); got != head {
				t.Errorf("adopted worktree is at %s, want %s", got, head)
			}
			again, err := Adopt(f.c, "bot/image", wants(head)[name])
			if err != nil || again != wtDir {
				t.Errorf("re-run = (%s, %v), want (%s, nil)", again, err, wtDir)
			}
		})
	}
}

// A local branch with commits the target lacks, but nothing missing from it, is
// refused too: wt cannot tell unpushed work from a leftover.
func TestAdopt_RefusesALocalBranchAheadOfTheTarget(t *testing.T) {
	f := newAdoptFixture(t)
	pushed := f.commitIn(f.repo, f.base, "pr 1")
	gitW(t, f.repo, "update-ref", "refs/heads/bot/image", pushed)
	gitW(t, f.repo, "push", "-q", "origin", "bot/image")
	ahead := f.commitIn(f.repo, pushed, "local only")
	gitW(t, f.repo, "update-ref", "refs/heads/bot/image", ahead)
	if _, err := Adopt(f.c, "bot/image", AdoptWant{PR: "7", PRHead: pushed}); err == nil {
		t.Fatal("Adopt checked out a local branch with commits the PR head lacks; it must refuse")
	}
	if got := f.tip(f.repo, "refs/heads/bot/image"); got != ahead {
		t.Errorf("a refusal must leave the local branch alone: at %s, want %s", got, ahead)
	}
}

// Only behind, but checked out in another worktree: wt must not move the branch
// underneath that worktree (its next commit would revert the move).
func TestAdopt_DoesNotMoveABranchCheckedOutElsewhere(t *testing.T) {
	f := newAdoptFixture(t)
	old, _ := f.behindBranch()
	elsewhere := filepath.Join(t.TempDir(), "elsewhere")
	gitW(t, f.repo, "worktree", "add", "-q", elsewhere, "bot/image")
	if _, err := Adopt(f.c, "bot/image", AdoptWant{}); err == nil {
		t.Fatal("Adopt should refuse a branch checked out in another worktree")
	}
	if got := f.tip(f.repo, "refs/heads/bot/image"); got != old {
		t.Errorf("the checked-out branch was moved to %s, want it left at %s", got, old)
	}
}

// The PR head commit is not in this clone (a PR from a fork): a local branch of
// the same name is known NOT to be it, and cannot be compared, so it is refused.
func TestAdopt_RefusesWhenThePRHeadIsNotInTheClone(t *testing.T) {
	f := newAdoptFixture(t)
	stale, _ := f.staleBranch()
	missing := strings.Repeat("1", 40)
	if _, err := Adopt(f.c, "bot/image", AdoptWant{PR: "9", PRHead: missing}); err == nil {
		t.Fatal("Adopt checked out a local branch it could not compare with the PR head")
	}
	if got := f.tip(f.repo, "refs/heads/bot/image"); got != stale {
		t.Errorf("a refusal must leave the local branch alone: at %s, want %s", got, stale)
	}
}

// No local branch: the remote-only path (worktree-add DWIM) is unchanged.
func TestAdopt_NoLocalBranchStillMaterializesTheRemote(t *testing.T) {
	f := newAdoptFixture(t)
	head := f.commitIn(f.origin, f.base, "pr 1")
	gitW(t, f.origin, "update-ref", "refs/heads/bot/image", head)
	wtDir, err := Adopt(f.c, "bot/image", AdoptWant{PR: "3", PRHead: head})
	if err != nil {
		t.Fatalf("Adopt: %v", err)
	}
	if got := f.tip(wtDir, "HEAD"); got != head {
		t.Errorf("adopted worktree is at %s, want %s", got, head)
	}
}

// A purely local branch has nothing to compare against: attached as it is, as
// before #167.
func TestAdopt_LocalOnlyBranchIsAttachedUnverified(t *testing.T) {
	f := newAdoptFixture(t)
	tip := f.commitIn(f.repo, f.base, "spike")
	gitW(t, f.repo, "update-ref", "refs/heads/spike/x", tip)
	wtDir, err := Adopt(f.c, "spike/x", AdoptWant{})
	if err != nil {
		t.Fatalf("Adopt: %v", err)
	}
	if got := f.tip(wtDir, "HEAD"); got != tip {
		t.Errorf("adopted worktree is at %s, want %s", got, tip)
	}
}

// #167 through the re-run path: wt's worktree for the branch already exists and
// holds the stale branch. Before the fix it was handed back as "Adopted".
func TestAdopt_RefusesAnExistingStaleWorktree(t *testing.T) {
	f := newAdoptFixture(t)
	stale, head := f.staleBranch()
	wtDir := filepath.Join(f.c.WorktreeRoot, "bot-image")
	gitW(t, f.repo, "worktree", "add", "-q", wtDir, "bot/image")
	if _, err := Adopt(f.c, "bot/image", AdoptWant{PR: "1082", PRHead: head}); err == nil {
		t.Fatal("Adopt handed back an existing worktree that holds the stale branch")
	}
	if got := f.tip(wtDir, "HEAD"); got != stale {
		t.Errorf("a refusal must leave the existing worktree alone: at %s, want %s", got, stale)
	}
}

// A re-run on a worktree that is only behind (the PR moved on) stays idempotent:
// it is handed back, and wt does not move it.
func TestAdopt_ReRunOnABehindWorktreeStillSucceeds(t *testing.T) {
	f := newAdoptFixture(t)
	old, head := f.behindBranch()
	wtDir := filepath.Join(f.c.WorktreeRoot, "bot-image")
	gitW(t, f.repo, "worktree", "add", "-q", wtDir, "bot/image")
	got, err := Adopt(f.c, "bot/image", AdoptWant{PR: "1084", PRHead: head})
	if err != nil {
		t.Fatalf("a re-run on a worktree that is only behind must not fail: %v", err)
	}
	if got != wtDir {
		t.Errorf("Adopt returned %s, want the existing worktree %s", got, wtDir)
	}
	if h := f.tip(wtDir, "HEAD"); h != old {
		t.Errorf("wt moved an existing worktree's branch to %s; it must leave it at %s", h, old)
	}
}
