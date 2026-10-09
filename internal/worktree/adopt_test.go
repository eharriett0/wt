package worktree

import (
	"errors"
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

// Adopting by PR, only an origin/<branch> that carries the PR head may be used
// at all: a fork PR's head is not on origin, and a failed fetch leaves an old
// copy (#167).
func TestGatePRHead(t *testing.T) {
	cases := []struct {
		name               string
		remote, head       string
		in, known, fetched bool
		want               HeadGate
	}{
		{"origin/<branch> is the head", "h", "h", false, true, true, HeadOnOrigin},
		{"is the head, though the fetch failed (offline, nothing moved)", "h", "h", false, true, false, HeadOnOrigin},
		{"origin contains the head: gh lags a push", "r", "h", true, true, true, HeadOnOrigin},
		{"contains it, but the fetch failed: an old copy proves nothing", "r", "h", true, true, false, HeadNotOnOrigin},
		{"no origin/<branch>: a fork PR whose branch origin lacks", "", "h", false, true, true, HeadNoRemote},
		{"no origin/<branch>, fetch failed", "", "h", false, true, false, HeadNoRemote},
		{"head not in this clone: a fork PR (s4)", "r", "h", false, false, true, HeadNotInClone},
		{"head not in this clone, fetch failed: stale origin (s2)", "r", "h", false, false, false, HeadNotInClone},
		{"origin lacks a head this clone has: a fork PR from a same-named branch (s3)", "r", "h", false, true, true, HeadNotOnOrigin},
		{"origin lacks it, fetch failed: stale origin (s2)", "r", "h", false, true, false, HeadNotOnOrigin},
	}
	for _, c := range cases {
		if got := GatePRHead(c.remote, c.head, c.in, c.known, c.fetched); got != c.want {
			t.Errorf("%s: GatePRHead = %d, want %d", c.name, got, c.want)
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
		{"only ahead: unpushed work, attached as it is", true, TipAhead, false, AdoptAhead},
		{"only ahead and checked out: not moved either way", true, TipAhead, true, AdoptAhead},
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
// without a move ONLY when it is the target, contains it (unpushed commits), or
// there is nothing to compare it with, and it is moved ONLY forward and ONLY
// when no worktree has it checked out. Anything else is a refusal, so a stale
// branch is never checked out.
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
			case AdoptAhead:
				if rel != TipAhead {
					t.Errorf("rel=%d inUse=%v: attached as ahead without containing the target", rel, inUse)
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
		{Path: "/w/feat", Branch: "Feat"},
	}
	cases := []struct {
		branch string
		fold   bool
		want   string
	}{
		{"bot/image", false, "/w/bot-image"},
		{"main", false, "/r"}, // the primary checkout counts too
		{"bot", false, ""},    // no prefix match
		{"", false, ""},       // a detached worktree is not "checked out on" an empty branch
		{"", true, ""},
		{"feat", false, ""},       // a case-sensitive filesystem: two branches
		{"feat", true, "/w/feat"}, // core.ignorecase: one ref, and Feat's worktree has it
		{"BOT/Image", true, "/w/bot-image"},
	}
	for _, c := range cases {
		if got := CheckedOutIn(refs, c.branch, c.fold); got != c.want {
			t.Errorf("CheckedOutIn(%q, fold=%v) = %q, want %q", c.branch, c.fold, got, c.want)
		}
	}
}

// A failed `git worktree list` cannot rule out a worktree using the branch, so
// it counts as in use: wt does not move what it cannot see (#167).
func TestBranchInUse(t *testing.T) {
	refs := []gitx.WorktreeRef{{Path: "/r", Branch: "main"}, {Path: "/w/x", Branch: "Feat"}}
	cases := []struct {
		name    string
		listErr error
		branch  string
		fold    bool
		at      string
		inUse   bool
	}{
		{"checked out", nil, "main", false, "/r", true},
		{"not checked out", nil, "bot/image", false, "", false},
		{"case-only, case-insensitive filesystem", nil, "feat", true, "/w/x", true},
		{"case-only, case-sensitive filesystem", nil, "feat", false, "", false},
		{"listing failed", errors.New("boom"), "bot/image", false, "", true},
	}
	for _, c := range cases {
		at, inUse := BranchInUse(refs, c.listErr, c.branch, c.fold)
		if at != c.at || inUse != c.inUse {
			t.Errorf("%s: BranchInUse = (%q, %v), want (%q, %v)", c.name, at, inUse, c.at, c.inUse)
		}
	}
}

func TestCaseTwin(t *testing.T) {
	names := []string{"main", "Feat", "bot/image"}
	cases := []struct{ branch, want string }{
		{"feat", "Feat"},
		{"FEAT", "Feat"},
		{"Feat", ""}, // the branch itself is not its own twin
		{"BOT/Image", "bot/image"},
		{"bot/image", ""},
		{"other", ""},
	}
	for _, c := range cases {
		if got := CaseTwin(names, c.branch); got != c.want {
			t.Errorf("CaseTwin(%q) = %q, want %q", c.branch, got, c.want)
		}
	}
}

func TestAdoptedOnTarget(t *testing.T) {
	cases := []struct {
		name                        string
		ref, head, branch, intended string
		want                        bool
	}{
		{"on the branch at the commit", "refs/heads/bot/image", "h", "bot/image", "h", true},
		{"on the branch, nothing to check the commit against", "refs/heads/bot/image", "h", "bot/image", "", true},
		{"on the branch at another commit", "refs/heads/bot/image", "x", "bot/image", "h", false},
		{"detached at a tag of the same name (s5)", "", "h", "bot/image", "h", false},
		{"detached, nothing to check the commit against", "", "x", "bot/image", "", false},
		{"another branch", "refs/heads/bot/image-2", "h", "bot/image", "h", false},
	}
	for _, c := range cases {
		if got := AdoptedOnTarget(c.ref, c.head, c.branch, c.intended); got != c.want {
			t.Errorf("%s: AdoptedOnTarget = %v, want %v", c.name, got, c.want)
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
	// Hermetic: the runner's ~/.gitconfig (commit.gpgsign with a key it cannot
	// use, hooksPath, ...) and the system config never reach the fixture or Adopt.
	t.Setenv("GIT_CONFIG_GLOBAL", os.DevNull)
	t.Setenv("GIT_CONFIG_NOSYSTEM", "1")
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

// has reports whether ref resolves in the operator's clone.
func (f *adoptFixture) has(ref string) bool {
	return exec.Command("git", "-C", f.repo, "rev-parse", "--verify", "--quiet", ref).Run() == nil
}

// assertNothingAdopted checks that a refusal created no worktree for branch:
// neither the directory nor a registration git knows about.
func (f *adoptFixture) assertNothingAdopted(branch string) {
	f.t.Helper()
	dir := filepath.Join(f.c.WorktreeRoot, strings.ReplaceAll(branch, "/", "-"))
	if _, err := os.Stat(dir); !os.IsNotExist(err) {
		f.t.Errorf("a refusal must not leave a worktree at %s (stat: %v)", dir, err)
	}
	if list := gitOutW(f.t, f.repo, "worktree", "list", "--porcelain"); strings.Contains(list, dir) {
		f.t.Errorf("a refusal must not leave %s registered:\n%s", dir, list)
	}
}

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

// offline makes every later fetch from origin fail, as a locked SSH agent does
// while gh, over HTTPS, still answers.
func (f *adoptFixture) offline() {
	gitW(f.t, f.repo, "remote", "set-url", "origin", filepath.Join(f.t.TempDir(), "unreachable.git"))
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
			f.assertNothingAdopted("bot/image")
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

// A local branch that contains the target plus commits of its own (not pushed
// yet) is attached as it is, the way a re-run hands back a worktree that is only
// ahead: it cannot be a leftover from an earlier PR, which would not contain the
// new PR's head.
func TestAdopt_AttachesALocalBranchOnlyAheadOfTheTarget(t *testing.T) {
	for name := range wants("") {
		t.Run(name, func(t *testing.T) {
			f := newAdoptFixture(t)
			pushed := f.commitIn(f.repo, f.base, "pr 1")
			gitW(t, f.repo, "update-ref", "refs/heads/bot/image", pushed)
			gitW(t, f.repo, "push", "-q", "origin", "bot/image")
			ahead := f.commitIn(f.repo, pushed, "local only")
			gitW(t, f.repo, "update-ref", "refs/heads/bot/image", ahead)
			wtDir, err := Adopt(f.c, "bot/image", wants(pushed)[name])
			if err != nil {
				t.Fatalf("Adopt refused a branch that is only ahead (unpushed work): %v", err)
			}
			if got := f.tip(wtDir, "HEAD"); got != ahead {
				t.Errorf("adopted worktree is at %s, want the local tip %s, unmoved", got, ahead)
			}
			if got := f.tip(f.repo, "refs/heads/bot/image"); got != ahead {
				t.Errorf("the local branch moved to %s; it must stay at %s", got, ahead)
			}
		})
	}
}

// Only behind, but checked out in another worktree: wt must not move the branch
// underneath that worktree (its next commit would revert the move). The refusal
// must be wt's own (ErrBranchInUse), not git's from inside the fast-forward.
func TestAdopt_DoesNotMoveABranchCheckedOutElsewhere(t *testing.T) {
	f := newAdoptFixture(t)
	old, _ := f.behindBranch()
	elsewhere := filepath.Join(t.TempDir(), "elsewhere")
	gitW(t, f.repo, "worktree", "add", "-q", elsewhere, "bot/image")
	_, err := Adopt(f.c, "bot/image", AdoptWant{})
	if !errors.Is(err, ErrBranchInUse) {
		t.Fatalf("Adopt = %v, want an ErrBranchInUse refusal", err)
	}
	if got := f.tip(f.repo, "refs/heads/bot/image"); got != old {
		t.Errorf("the checked-out branch was moved to %s, want it left at %s", got, old)
	}
}

// When `git worktree list` fails, wt cannot rule out a worktree using the
// branch, so it does not move it.
func TestAdopt_AFailedWorktreeListCountsAsInUse(t *testing.T) {
	f := newAdoptFixture(t)
	old, _ := f.behindBranch()
	listWorktrees = func() ([]gitx.WorktreeRef, error) { return nil, errors.New("worktree list failed") }
	t.Cleanup(func() { listWorktrees = gitx.WorktreeList })
	_, err := Adopt(f.c, "bot/image", AdoptWant{})
	if !errors.Is(err, ErrBranchInUse) {
		t.Fatalf("Adopt = %v, want an ErrBranchInUse refusal", err)
	}
	if got := f.tip(f.repo, "refs/heads/bot/image"); got != old {
		t.Errorf("the branch was moved to %s although wt could not list the worktrees; want it left at %s", got, old)
	}
	f.assertNothingAdopted("bot/image")
}

// The PR head commit is not in this clone (a PR from a fork), while the local
// branch equals origin/<branch>: origin's branch of that name is not the PR's,
// so nothing is attached, though the local branch matches origin.
func TestAdopt_RefusesWhenThePRHeadIsNotInTheClone(t *testing.T) {
	f := newAdoptFixture(t)
	local := f.commitIn(f.repo, f.base, "origin's own bot/image")
	gitW(t, f.repo, "update-ref", "refs/heads/bot/image", local)
	gitW(t, f.repo, "push", "-q", "origin", "bot/image")
	missing := strings.Repeat("1", 40)
	_, err := Adopt(f.c, "bot/image", AdoptWant{PR: "9", PRHead: missing})
	if !errors.Is(err, ErrPRHeadNotOnOrigin) {
		t.Fatalf("Adopt = %v, want an ErrPRHeadNotOnOrigin refusal", err)
	}
	if got := f.tip(f.repo, "refs/heads/bot/image"); got != local {
		t.Errorf("a refusal must leave the local branch alone: at %s, want %s", got, local)
	}
	f.assertNothingAdopted("bot/image")
}

// s4: a PR from a fork whose head branch shares its name with an unrelated
// branch on origin, and no local branch. Before, worktree-add materialized
// origin's branch and reported it adopted. Whether or not the fork's head
// commit is in this clone, nothing is created.
func TestAdopt_RefusesAForkPRWhoseBranchNameOriginAlsoHas(t *testing.T) {
	for _, inClone := range []bool{false, true} {
		t.Run(map[bool]string{false: "head not in the clone", true: "head in the clone"}[inClone], func(t *testing.T) {
			f := newAdoptFixture(t)
			theirs := f.commitIn(f.origin, f.base, "origin's own patch-1")
			gitW(t, f.origin, "update-ref", "refs/heads/patch-1", theirs)
			fork := filepath.Join(t.TempDir(), "fork.git")
			gitW(t, f.repo, "init", "-q", "--bare", fork)
			forkHead := f.commitIn(f.repo, f.base, "the fork's patch-1")
			gitW(t, f.repo, "push", "-q", fork, forkHead+":refs/heads/patch-1")
			if !inClone {
				// a fresh clone of the same origin, where the fork's commit never was
				fresh := filepath.Join(t.TempDir(), "fresh")
				gitW(t, f.repo, "clone", "-q", f.origin, fresh)
				t.Chdir(fresh)
				f.repo = fresh
			}
			_, err := Adopt(f.c, "patch-1", AdoptWant{PR: "88", PRHead: forkHead})
			if !errors.Is(err, ErrPRHeadNotOnOrigin) {
				t.Fatalf("Adopt = %v, want an ErrPRHeadNotOnOrigin refusal", err)
			}
			f.assertNothingAdopted("patch-1")
			if f.has("refs/heads/patch-1") {
				t.Error("a refusal must not create the local branch")
			}
		})
	}
}

// s3/s3b: a PR from a fork, from the contributor's own main, whose head commit
// this clone has (an earlier `gh pr checkout`, or a pull/* refspec). Before the
// gate, wt fast-forwarded local main onto the fork's unmerged commit when no
// worktree had main, and otherwise advised that fast-forward. Now it refuses
// first, whether or not main is checked out, and main never moves.
func TestAdopt_NeverMovesALocalBranchToAForkHead(t *testing.T) {
	for _, parked := range []bool{true, false} {
		t.Run(map[bool]string{true: "main checked out nowhere", false: "main checked out in the primary"}[parked], func(t *testing.T) {
			f := newAdoptFixture(t)
			forkHead := f.commitIn(f.repo, f.base, "the fork's change")
			gitW(t, f.repo, "update-ref", "refs/remotes/origin/pr/77", forkHead)
			if parked {
				gitW(t, f.repo, "checkout", "-q", "--detach")
			}
			_, err := Adopt(f.c, "main", AdoptWant{PR: "77", PRHead: forkHead})
			if !errors.Is(err, ErrPRHeadNotOnOrigin) {
				t.Fatalf("Adopt = %v, want an ErrPRHeadNotOnOrigin refusal (not a move, nor an in-use refusal advising one)", err)
			}
			if got := f.tip(f.repo, "refs/heads/main"); got != f.base {
				t.Errorf("local main moved to %s, the fork's head is %s; it must stay at %s", got, forkHead, f.base)
			}
			f.assertNothingAdopted("main")
		})
	}
}

// s2: the fetch fails (gh still answers) while this clone's origin/<branch> is
// an old copy, and there is no local branch. Before, worktree-add materialized
// the old copy and reported it adopted, two commits behind the PR.
func TestAdopt_RefusesAnOldOriginCopyWhenTheFetchFails(t *testing.T) {
	cases := map[string]bool{"PR head not in the clone": false, "PR head in the clone": true}
	for name, inClone := range cases {
		t.Run(name, func(t *testing.T) {
			f := newAdoptFixture(t)
			old := f.commitIn(f.repo, f.base, "pr 1")
			gitW(t, f.repo, "push", "-q", "origin", old+":refs/heads/bot/image")
			gitW(t, f.repo, "fetch", "-q", "origin")
			var head string
			if inClone {
				head = f.commitIn(f.repo, old, "pr 2")
				gitW(t, f.repo, "push", "-q", "origin", head+":refs/heads/bot/image")
				gitW(t, f.repo, "update-ref", "refs/remotes/origin/bot/image", old) // the push moved it: put the old copy back
			} else {
				head = f.commitIn(f.origin, old, "pr 2")
				gitW(t, f.origin, "update-ref", "refs/heads/bot/image", head)
			}
			f.offline()
			_, err := Adopt(f.c, "bot/image", AdoptWant{PR: "50", PRHead: head})
			if !errors.Is(err, ErrPRHeadNotOnOrigin) {
				t.Fatalf("Adopt = %v, want an ErrPRHeadNotOnOrigin refusal", err)
			}
			f.assertNothingAdopted("bot/image")
			if f.has("refs/heads/bot/image") {
				t.Error("a refusal must not create the local branch from the old copy")
			}
		})
	}
}

// Offline is fine when nothing moved: this clone's origin/<branch> already is
// the PR head, so the failed fetch changes nothing.
func TestAdopt_OfflineButOriginIsThePRHead(t *testing.T) {
	f := newAdoptFixture(t)
	head := f.commitIn(f.repo, f.base, "pr 1")
	gitW(t, f.repo, "push", "-q", "origin", head+":refs/heads/bot/image")
	gitW(t, f.repo, "fetch", "-q", "origin")
	f.offline()
	wtDir, err := Adopt(f.c, "bot/image", AdoptWant{PR: "51", PRHead: head})
	if err != nil {
		t.Fatalf("Adopt: %v", err)
	}
	if got := f.tip(wtDir, "HEAD"); got != head {
		t.Errorf("adopted worktree is at %s, want %s", got, head)
	}
}

// gh can report a head a push has already moved past: origin/<branch>, just
// fetched, contains it, and Adopt lands on origin's newer tip.
func TestAdopt_GhLaggingAPushLandsOnOrigin(t *testing.T) {
	f := newAdoptFixture(t)
	reported := f.commitIn(f.origin, f.base, "pr 1")
	newer := f.commitIn(f.origin, reported, "pr 2, pushed after gh answered")
	gitW(t, f.origin, "update-ref", "refs/heads/bot/image", newer)
	wtDir, err := Adopt(f.c, "bot/image", AdoptWant{PR: "52", PRHead: reported})
	if err != nil {
		t.Fatalf("Adopt: %v", err)
	}
	if got := f.tip(wtDir, "HEAD"); got != newer {
		t.Errorf("adopted worktree is at %s, want origin's tip %s", got, newer)
	}
}

// No local branch: worktree-add materializes origin/<branch>, and the new
// worktree is on the branch at the PR head.
func TestAdopt_NoLocalBranchLandsOnThePRHead(t *testing.T) {
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
	if got := gitx.SymbolicHead(wtDir); got != "refs/heads/bot/image" {
		t.Errorf("adopted worktree HEAD is %q, want refs/heads/bot/image", got)
	}
}

// s5: a tag named like the branch, and no local branch. git worktree-add takes
// the tag over origin/<branch> and leaves a detached HEAD at it; wt must notice,
// remove that worktree, and touch nothing else.
func TestAdopt_ASameNamedTagIsNeverAdoptedAsTheBranch(t *testing.T) {
	for name := range wants("") {
		t.Run(name, func(t *testing.T) {
			f := newAdoptFixture(t)
			head := f.commitIn(f.origin, f.base, "pr 1")
			gitW(t, f.origin, "update-ref", "refs/heads/bot/image", head)
			gitW(t, f.repo, "tag", "bot/image", f.base)
			_, err := Adopt(f.c, "bot/image", wants(head)[name])
			if !errors.Is(err, ErrLandedElsewhere) {
				t.Fatalf("Adopt = %v, want an ErrLandedElsewhere refusal", err)
			}
			f.assertNothingAdopted("bot/image")
			if f.has("refs/heads/bot/image") {
				t.Error("no local branch should exist after the refusal")
			}
			if got := f.tip(f.repo, "refs/tags/bot/image"); got != f.base {
				t.Errorf("the tag moved to %s; it must be left at %s", got, f.base)
			}
		})
	}
}

// A branch whose name differs only in case, on a filesystem that ignores case
// (core.ignorecase): there the two are one loose ref file, so a fast-forward of
// feat moved Feat underneath the worktree that had it checked out, and git's own
// checked-out check compares names exactly. wt refuses either way.
func TestAdopt_RefusesACaseOnlyTwin(t *testing.T) {
	for _, checkedOut := range []bool{true, false} {
		t.Run(map[bool]string{true: "twin checked out", false: "twin not checked out"}[checkedOut], func(t *testing.T) {
			f := newAdoptFixture(t)
			gitW(t, f.repo, "config", "core.ignorecase", "true")
			old := f.commitIn(f.repo, f.base, "pr 1")
			gitW(t, f.repo, "push", "-q", "origin", old+":refs/heads/feat")
			head := f.commitIn(f.origin, old, "pr 2")
			gitW(t, f.origin, "update-ref", "refs/heads/feat", head)
			gitW(t, f.repo, "update-ref", "refs/heads/Feat", old)
			if checkedOut {
				gitW(t, f.repo, "worktree", "add", "-q", filepath.Join(t.TempDir(), "mine"), "Feat")
			}
			_, err := Adopt(f.c, "feat", AdoptWant{PR: "11", PRHead: head})
			if !errors.Is(err, ErrCaseTwin) {
				t.Fatalf("Adopt = %v, want an ErrCaseTwin refusal", err)
			}
			if got := f.tip(f.repo, "refs/heads/Feat"); got != old {
				t.Errorf("Feat moved to %s; it must stay at %s", got, old)
			}
			f.assertNothingAdopted("feat")
		})
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

// The gate runs before the re-run short-circuit too: an existing worktree is
// not handed back for a PR whose head origin/<branch> does not carry.
func TestAdopt_ReRunRefusesAPRHeadNotOnOrigin(t *testing.T) {
	f := newAdoptFixture(t)
	head := f.commitIn(f.repo, f.base, "origin's own bot/image")
	gitW(t, f.repo, "push", "-q", "origin", head+":refs/heads/bot/image")
	wtDir := filepath.Join(f.c.WorktreeRoot, "bot-image")
	gitW(t, f.repo, "worktree", "add", "-q", wtDir, "bot/image")
	_, err := Adopt(f.c, "bot/image", AdoptWant{PR: "9", PRHead: strings.Repeat("1", 40)})
	if !errors.Is(err, ErrPRHeadNotOnOrigin) {
		t.Fatalf("Adopt = %v, want an ErrPRHeadNotOnOrigin refusal", err)
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
