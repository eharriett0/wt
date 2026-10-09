package worktree

import (
	"errors"
	"fmt"
	"path/filepath"
	"strings"

	"github.com/eharriett0/wt/internal/gitx"
	"github.com/eharriett0/wt/internal/ui"
)

// AdoptWant says which commit `wt adopt` must land on (#167). Adopting by PR
// number sets PR and PRHead (the PR's headRefOid from gh). Adopting by branch
// name leaves both empty, and the target is origin/<branch> as just fetched.
type AdoptWant struct {
	PR     string
	PRHead string
}

// Refusals a caller, or a test, can tell apart with errors.Is (#167).
var (
	// ErrPRHeadNotOnOrigin: adopting by PR, origin/<branch> does not carry the
	// PR's head, so it is not the PR's branch (a PR from a fork) or is an old copy.
	ErrPRHeadNotOnOrigin = errors.New("the PR head is not on origin/<branch>")
	// ErrBranchInUse: a worktree has the branch checked out, or that could not be
	// ruled out, so wt does not move it.
	ErrBranchInUse = errors.New("the branch is checked out in a worktree")
	// ErrCaseTwin: a branch whose name differs only in letter case exists, and the
	// filesystem ignores case, so the two are one ref.
	ErrCaseTwin = errors.New("a branch differing only in case exists")
	// ErrLandedElsewhere: the new worktree was not on the branch at the commit wt
	// meant (a tag of the same name wins over origin/<branch>), so wt removed it.
	ErrLandedElsewhere = errors.New("the new worktree landed elsewhere")
)

// refusal is an error with a message of its own that still matches its kind
// under errors.Is (#167).
type refusal struct {
	kind error
	msg  string
}

func (r *refusal) Error() string { return r.msg }
func (r *refusal) Unwrap() error { return r.kind }

func refuse(kind error, format string, a ...any) error {
	return &refusal{kind: kind, msg: fmt.Sprintf(format, a...)}
}

// TipRelation is how a local tip relates to the commit `wt adopt` must land on
// (#167).
type TipRelation int

const (
	TipNoTarget TipRelation = iota // nothing to compare against: by branch name, with no origin/<branch>
	TipEqual                       // the local tip IS the target
	TipBehind                      // the target contains the local tip, so a fast-forward reaches it
	TipAhead                       // the local tip contains the target, plus commits the target lacks
	TipDiverged                    // each has commits the other lacks: the #167 stale branch
	TipUnknown                     // they differ and could not be compared (a commit is missing, or a shallow clone)
)

// ClassifyTips relates a local tip to the target (#167). localInTarget and
// targetInLocal are the two `merge-base --is-ancestor` answers, and known says
// both were computed. Equality needs no ancestry, so it is decided first. Pure.
func ClassifyTips(local, target string, localInTarget, targetInLocal, known bool) TipRelation {
	switch {
	case target == "":
		return TipNoTarget
	case local == target:
		return TipEqual
	case !known:
		return TipUnknown
	case localInTarget:
		return TipBehind
	case targetInLocal:
		return TipAhead
	default:
		return TipDiverged
	}
}

// HeadGate says whether origin/<branch>, as fetched, carries a PR's head (#167).
type HeadGate int

const (
	HeadOnOrigin    HeadGate = iota // origin/<branch> is the PR head or, just fetched, contains it (gh can lag a push)
	HeadNoRemote                    // this clone has no origin/<branch>
	HeadNotInClone                  // the PR head commit is not in this clone
	HeadNotOnOrigin                 // the PR head is in this clone, but origin/<branch> does not contain it, or was not re-fetched
)

// GatePRHead decides whether `wt adopt <pr>` may use origin/<branch> at all
// (#167). gh's headRefOid can sit on another repository (a PR from a fork),
// which `git fetch origin <branch>` never reaches: origin/<branch> is then a
// different branch that shares the name, or nothing, and when the fetch failed
// it is an old copy. Landing on it, creating the local branch from it, or
// fast-forwarding a local branch to the PR head (base `main`, for a fork PR
// from a contributor's main) would put commits that are not the PR's under the
// PR's name, so only HeadOnOrigin proceeds. headInRemote and known are
// `merge-base --is-ancestor <head> <remote>` and whether it could be computed;
// fetched says this run's fetch succeeded. Containment counts only after a
// successful fetch: it is what a push gh has not caught up with looks like,
// while an old copy that contains the head proves nothing about the PR now.
// Pure.
func GatePRHead(remote, head string, headInRemote, known, fetched bool) HeadGate {
	switch {
	case remote == "":
		return HeadNoRemote
	case remote == head:
		return HeadOnOrigin
	case !known:
		return HeadNotInClone
	case headInRemote && fetched:
		return HeadOnOrigin
	default:
		return HeadNotOnOrigin
	}
}

// AdoptAction is what `wt adopt` does with the local branch it is about to
// attach a new worktree to (#167).
type AdoptAction int

const (
	AdoptCreate      AdoptAction = iota // no local branch: worktree-add creates it from origin/<branch>, as before #167
	AdoptAsIs                           // the local branch is the target: attach it unchanged
	AdoptAhead                          // the local branch is the target plus commits not on it (unpushed): attach it unchanged, and say so
	AdoptUnverified                     // nothing to compare against: attach the local branch as it is, and say so
	AdoptFastForward                    // the local branch is only behind the target: move it forward, then attach
	AdoptRefuseInUse                    // only behind, but a worktree has it checked out (or may): wt will not move it
	AdoptRefuse                         // diverged, or could not be compared: do not check it out
)

// DecideAdopt picks what `wt adopt` does with an existing local branch of the
// target's name before it worktree-adds it (#167). inUse says a worktree has the
// branch checked out, or that this could not be ruled out. Pure.
//
// The #167 bug was this check missing. worktree-add takes refs/heads/<branch>
// whenever it exists, so a stale branch left by an earlier PR that reused the
// name was checked out instead of the PR head (ahead 6, behind 285), and a push
// from it would have been rejected or, forced, would have overwritten the PR.
// Diverged is that shape, and is refused. Only ahead is not: a branch that
// contains the whole target plus more is not left over from an earlier PR
// (that cannot contain the new PR's commits); it is unpushed work, attached as
// it is, as a re-run hands back a worktree that is only ahead (DecideExisting).
//
// Only a branch no worktree has checked out is fast-forwarded. Moving a
// checked-out branch underneath its worktree leaves that worktree's files at the
// old commit, so its next commit would revert what the move brought in. inUse
// comes from `git worktree list` (BranchInUse), which cannot see a worktree
// that is mid-rebase of the branch; gitx.FastForwardBranch refuses that case
// itself.
func DecideAdopt(localExists bool, rel TipRelation, inUse bool) AdoptAction {
	if !localExists {
		return AdoptCreate
	}
	switch rel {
	case TipNoTarget:
		return AdoptUnverified
	case TipEqual:
		return AdoptAsIs
	case TipAhead:
		return AdoptAhead
	case TipBehind:
		if inUse {
			return AdoptRefuseInUse
		}
		return AdoptFastForward
	default: // TipDiverged, TipUnknown
		return AdoptRefuse
	}
}

// ExistingAction is what `wt adopt` does when wt's worktree for the branch
// already exists, which is the re-run case (#167).
type ExistingAction int

const (
	ExistingReuse       ExistingAction = iota // it matches, or there is nothing to compare: hand it back, as before #167
	ExistingReuseBehind                       // only behind the target: hand it back, and say how to update it there
	ExistingReuseAhead                        // only ahead (unpushed commits): hand it back, and say so
	ExistingRefuse                            // diverged, or could not be compared: it may be left over from an earlier PR
)

// DecideExisting picks what `wt adopt` does with wt's existing worktree for the
// branch, from how its HEAD relates to the target (#167). Before #167 it was
// handed back unchecked, so a worktree left over from an earlier PR that reused
// the name came back as "Adopted". Being only behind (the PR moved on) or only
// ahead (commits not pushed yet) is the normal state of a re-run, so those are
// handed back with a note. wt never moves the branch of an existing worktree.
// Pure.
func DecideExisting(rel TipRelation) ExistingAction {
	switch rel {
	case TipBehind:
		return ExistingReuseBehind
	case TipAhead:
		return ExistingReuseAhead
	case TipDiverged, TipUnknown:
		return ExistingRefuse
	default: // TipNoTarget, TipEqual
		return ExistingReuse
	}
}

// CheckedOutIn returns the path of the worktree that has branch checked out, or
// "" when none does (#167). With foldCase (core.ignorecase) a branch whose name
// differs only in letter case counts too: on such a filesystem the two names are
// one loose ref file, and git's own checked-out check compares names exactly. A
// worktree mid-rebase or mid-bisect of the branch lists as detached, so it is
// not found here. Pure.
func CheckedOutIn(refs []gitx.WorktreeRef, branch string, foldCase bool) string {
	if branch == "" {
		return ""
	}
	for _, r := range refs {
		if r.Branch == branch || (foldCase && r.Branch != "" && strings.EqualFold(r.Branch, branch)) {
			return r.Path
		}
	}
	return ""
}

// BranchInUse says whether wt must treat branch as checked out, and where (#167):
// a worktree has it (CheckedOutIn), or `git worktree list` failed, so that could
// not be ruled out (at is "" then). Pure.
func BranchInUse(refs []gitx.WorktreeRef, listErr error, branch string, foldCase bool) (at string, inUse bool) {
	if listErr != nil {
		return "", true
	}
	at = CheckedOutIn(refs, branch, foldCase)
	return at, at != ""
}

// CaseTwin returns the first of names that differs from branch only in letter
// case, or "" (#167). Pure.
func CaseTwin(names []string, branch string) string {
	for _, n := range names {
		if n != branch && strings.EqualFold(n, branch) {
			return n
		}
	}
	return ""
}

// AdoptedOnTarget reports whether a worktree `wt adopt` just added landed where
// it meant to (#167): on refs/heads/<branch> (gotRef is the worktree's full
// symbolic HEAD, "" when detached), at intended when that is set. worktree-add
// resolves <branch> itself, and a tag of the same name wins over origin/<branch>
// there, leaving a detached HEAD at the tag. Pure.
func AdoptedOnTarget(gotRef, gotHead, branch, intended string) bool {
	return gotRef == "refs/heads/"+branch && (intended == "" || gotHead == intended)
}

// listWorktrees is gitx.WorktreeList, as a variable so a test can make the
// listing fail (#167).
var listWorktrees = gitx.WorktreeList

// adoptTarget is the commit `wt adopt` lands on, and how messages name it
// (#167). tip is "" when there is nothing to compare against: adopting by
// branch name, with no origin/<branch> in this clone.
type adoptTarget struct {
	tip, label string
}

// resolveAdoptTarget finds what `wt adopt` must land on: origin/<branch> as
// fetched (#167). Adopting by PR, origin/<branch> must first carry the PR's
// head (GatePRHead), or Adopt refuses before it looks at an existing worktree
// and before it checks out, creates or moves anything. root is the worktree
// root, for the advice.
func resolveAdoptTarget(branch string, want AdoptWant, fetched bool, root string) (adoptTarget, error) {
	remote := gitx.RemoteTrackingTip(branch)
	if want.PRHead == "" { // by branch name
		if remote == "" {
			return adoptTarget{}, nil
		}
		t := adoptTarget{tip: remote, label: "origin/" + branch}
		if !fetched {
			t.label += " (as last fetched)"
		}
		return t, nil
	}
	in, known := false, true
	if remote != "" && remote != want.PRHead {
		var err error
		in, err = gitx.IsAncestor(want.PRHead, remote)
		known = err == nil
	}
	gate := GatePRHead(remote, want.PRHead, in, known, fetched)
	if gate == HeadOnOrigin {
		if remote == want.PRHead {
			return adoptTarget{tip: remote, label: "PR #" + want.PR + "'s head"}, nil
		}
		return adoptTarget{tip: remote, label: fmt.Sprintf("origin/%s (it contains PR #%s's head %s)", branch, want.PR, short(want.PRHead))}, nil
	}
	return adoptTarget{}, refuseOffOrigin(branch, want, remote, gate, fetched, root)
}

// refuseOffOrigin explains a PR head that origin/<branch> does not carry
// (#167). Nothing has been checked out, created or moved.
func refuseOffOrigin(branch string, want AdoptWant, remote string, gate HeadGate, fetched bool, root string) error {
	state := "this clone has no origin/" + branch
	if remote != "" {
		state = fmt.Sprintf("origin/%s is %s", branch, short(remote))
		switch {
		case !fetched:
			state += " as last fetched"
		case gate == HeadNotInClone:
			state += ", and the PR head is not in this clone"
		case gate == HeadNotOnOrigin:
			state += ", which does not contain it"
		}
	}
	ui.Warn("PR #%s's head (%s) is not on origin/%s (%s), so wt will not check out, create or move anything for it (#167)", want.PR, short(want.PRHead), branch, state)
	if !fetched {
		ui.Info("git fetch origin %s failed, so origin/%s is whatever this clone fetched last. Re-run once fetching works:", branch, branch)
		fmt.Printf("  wt adopt %s\n", want.PR)
	} else {
		dir := filepath.Join(root, "pr-"+want.PR)
		ui.Info("the PR's branch most likely lives on another repository (a PR from a fork), which `git fetch origin` does not reach; an origin/%s, if any, is a different branch with the same name. Check the PR out with gh, in a worktree of its own:", branch)
		fmt.Printf("  git worktree add --detach %s\n", dir)
		fmt.Printf("  cd %s && gh pr checkout %s --branch pr-%s\n", dir, want.PR, want.PR)
		ui.Info("(if the PR was force-pushed a moment ago, re-run: wt adopt %s)", want.PR)
	}
	return refuse(ErrPRHeadNotOnOrigin, "PR #%s's head (%s) is not on origin/%s (%s); wt did not adopt it", want.PR, short(want.PRHead), branch, state)
}

// relate classifies local against target from git's ancestry answers (#167).
// IsAncestor errors on an empty or missing commit, which classifies as unknown.
func relate(local, target string) TipRelation {
	if target == "" || local == target {
		return ClassifyTips(local, target, false, false, true)
	}
	inTarget, err1 := gitx.IsAncestor(local, target)
	inLocal, err2 := gitx.IsAncestor(target, local)
	return ClassifyTips(local, target, inTarget, inLocal, err1 == nil && err2 == nil)
}

// refuseCaseTwin refuses a branch that has a case-only twin on a filesystem that
// ignores case (#167): there the two share one loose ref file, so moving or
// attaching one moves or attaches the other, and git's checked-out check, which
// compares names exactly, does not see a worktree that has the twin.
func refuseCaseTwin(branch string) error {
	names, err := gitx.LocalBranches()
	if err != nil {
		return refuse(ErrCaseTwin, "could not list the local branches to rule out one that differs from %q only in case (core.ignorecase is set): %v; wt did not adopt it", branch, err)
	}
	twin := CaseTwin(names, branch)
	if twin == "" {
		return nil
	}
	ui.Warn("local branch %s differs from %s only in letter case, and this filesystem ignores case (core.ignorecase), so the two are one ref: wt will not check out, create or move either (#167)", twin, branch)
	ui.Info("rename the local one, then re-run:")
	fmt.Printf("  git branch -m %s %s-local\n", twin, twin)
	return refuse(ErrCaseTwin, "local branch %q differs from %q only in letter case, and this filesystem ignores case, so they are one ref; wt did not adopt it", twin, branch)
}

// prepareLocalBranch checks the local branch worktree-add is about to take
// against the target before anything is checked out (#167), and returns the
// commit the new worktree must then be on ("" when there is none to check: no
// local branch and no origin/<branch>). Equal and only-ahead (unpushed commits)
// are attached as they are, only-behind is fast-forwarded first, and anything
// else is refused. With no local branch, worktree-add creates it from
// origin/<branch>, as before.
func prepareLocalBranch(branch string, want AdoptWant, target adoptTarget) (string, error) {
	fold := gitx.IgnoreCase()
	if fold {
		if err := refuseCaseTwin(branch); err != nil {
			return "", err
		}
	}
	local := gitx.BranchTip(branch)
	rel := relate(local, target.tip)
	inUseAt, inUse := "", false
	if local != "" {
		refs, listErr := listWorktrees()
		inUseAt, inUse = BranchInUse(refs, listErr, branch, fold)
	}
	rerun := "wt adopt " + branch
	if want.PR != "" {
		rerun = "wt adopt " + want.PR
	}

	switch DecideAdopt(local != "", rel, inUse) {
	case AdoptCreate:
		return target.tip, nil
	case AdoptAsIs:
		ui.OK("local branch %s matches %s (%s)", branch, target.label, short(local))
		return local, nil
	case AdoptAhead:
		ahead, _, _ := gitx.AheadBehind(local, target.tip)
		ui.Info("local branch %s is %s plus %d commit(s) not on it (not pushed yet?); attaching it as it is:", branch, target.label, ahead)
		printOnlyIn("local "+branch, local, target.tip, ahead)
		return local, nil
	case AdoptUnverified:
		ui.Info("no origin/%s to compare the local branch with, so it is attached as it is (%s), unverified", branch, short(local))
		return local, nil
	case AdoptFastForward:
		_, behind, _ := gitx.AheadBehind(local, target.tip)
		ui.Step("local branch %s is %d commit(s) behind %s: fast-forwarding it %s → %s", branch, behind, target.label, short(local), short(target.tip))
		if err := gitx.FastForwardBranch(branch, local, target.tip); err != nil {
			return "", fmt.Errorf("fast-forward local branch %q to %s: %w", branch, target.label, err)
		}
		return target.tip, nil
	case AdoptRefuseInUse:
		where := "in a worktree wt could not identify (git worktree list failed)"
		if inUseAt != "" {
			where = "at " + inUseAt
		}
		ui.Warn("local branch %s is behind %s (%s → %s), but it is checked out %s, and wt does not move a branch a worktree has checked out (#167)", branch, target.label, short(local), short(target.tip), where)
		if inUseAt != "" {
			ui.Info("work in that worktree instead, after fast-forwarding it there:")
			fmt.Printf("  git -C %s merge --ff-only %s\n", inUseAt, target.tip)
		}
		return "", refuse(ErrBranchInUse, "local branch %q is behind %s but checked out %s; wt did not move it", branch, target.label, where)
	}

	// AdoptRefuse: the local branch diverged from the target, or the two could
	// not be compared. Never check it out silently (#167).
	ui.Warn("local branch %s does not match %s, so wt will not check it out (#167):", branch, target.label)
	printTips("local "+branch, local, target.label, target.tip, rel)
	if inUseAt != "" {
		ui.Info("it is checked out at %s", inUseAt)
	}
	ui.Info("it is most likely left over from an earlier PR that reused the name. Inspect it, then set it aside (or delete it), and re-run:")
	fmt.Printf("  git log --oneline %s..%s\n", target.tip, local)
	fmt.Printf("  git branch -m %s %s-old-%s     # or: git branch -D %s\n", branch, branch, short(local), branch)
	fmt.Printf("  %s\n", rerun)
	return "", fmt.Errorf("local branch %q (%s) does not match %s (%s): %s; wt did not check it out", branch, short(local), target.label, short(target.tip), relationPhrase(rel))
}

// verifyAdopted checks the worktree Adopt just added (#167). worktree-add
// resolves <branch> itself, and a tag of the same name, for one, wins over
// origin/<branch> and leaves a detached HEAD at the tag. On a mismatch it
// removes that worktree, and only it, and refuses.
func verifyAdopted(wtDir, branch, intended string) error {
	ref, head := gitx.SymbolicHead(wtDir), gitx.HeadCommit(wtDir)
	if AdoptedOnTarget(ref, head, branch, intended) {
		return nil
	}
	got := "a detached HEAD"
	if ref != "" {
		got = strings.TrimPrefix(ref, "refs/heads/")
	}
	want := branch
	if intended != "" {
		want += " at " + short(intended)
	}
	ui.Warn("the new worktree is on %s at %s, not on %s: git worktree add resolved %q to something else, such as a tag of that name (#167)", got, short(head), want, branch)
	removed := "removed it again"
	if err := gitx.WorktreeRemove(wtDir, false); err != nil {
		removed = "could not remove it (" + err.Error() + "); remove it with: git worktree remove " + wtDir
		ui.Warn("%s", removed)
	} else {
		ui.Info("removed that worktree again")
	}
	if tags, err := gitx.Run("tag", "--list", branch); err == nil && strings.TrimSpace(tags) == branch {
		ui.Info("a tag named %s exists and git takes it over the branch; rename or delete the tag and re-run", branch)
		if gitx.RemoteTrackingTip(branch) != "" {
			ui.Info("or attach the branch yourself:")
			fmt.Printf("  git worktree add --track -b %s %s origin/%s\n", branch, wtDir, branch)
		}
	}
	return refuse(ErrLandedElsewhere, "the new worktree for %q landed on %s at %s, not on %s; wt %s and did not adopt it", branch, got, short(head), want, removed)
}

// checkExistingWorktree checks wt's existing worktree for the branch against the
// target before handing it back on a re-run (#167). It never moves the
// worktree's branch: a note when it is only behind or only ahead, a refusal when
// it diverged or could not be compared.
func checkExistingWorktree(wtDir, branch string, want AdoptWant, target adoptTarget) error {
	head := gitx.HeadCommit(wtDir)
	rel := relate(head, target.tip)
	action := DecideExisting(rel)
	if action == ExistingReuse {
		return nil
	}
	// The advice below assumes the worktree is on branch; say so when it is not.
	ref := gitx.SymbolicHead(wtDir)
	detached := ref == ""
	other := ""
	if detached {
		other = fmt.Sprintf(" (its HEAD is detached, not on %s)", branch)
	} else if ref != "refs/heads/"+branch {
		other = fmt.Sprintf(" (it has %s checked out, not %s)", strings.TrimPrefix(ref, "refs/heads/"), branch)
	}
	switch action {
	case ExistingReuseBehind:
		_, behind, _ := gitx.AheadBehind(head, target.tip)
		ui.Warn("the existing worktree%s is %d commit(s) behind %s (%s → %s). wt does not move a checked-out branch, so update it there:", other, behind, target.label, short(head), short(target.tip))
		fmt.Printf("  git -C %s merge --ff-only %s\n", wtDir, target.tip)
		return nil
	case ExistingReuseAhead:
		ahead, _, _ := gitx.AheadBehind(head, target.tip)
		ui.Info("the existing worktree%s has %d commit(s) that %s does not (not pushed yet?)", other, ahead, target.label)
		return nil
	}

	rerun := "wt adopt " + branch
	if want.PR != "" {
		rerun = "wt adopt " + want.PR
	}
	ui.Warn("wt's worktree for %s already exists at %s%s, but its HEAD does not match %s (#167):", branch, wtDir, other, target.label)
	printTips("the worktree", head, target.label, target.tip, rel)
	ui.Info("if it is left over from an earlier PR that reused the name, keep what it holds, remove it, set the branch aside, and re-run:")
	if detached {
		// git worktree remove keeps nothing that only a detached HEAD has.
		fmt.Printf("  git -C %s branch %s-wip-%s     # its detached HEAD: removing the worktree would orphan these commits\n", wtDir, branch, short(head))
	}
	fmt.Printf("  git worktree remove %s     # refuses while it holds uncommitted or untracked changes: commit or stash them first\n", wtDir)
	if tip := gitx.BranchTip(branch); tip != "" {
		fmt.Printf("  git branch -m %s %s-old-%s\n", branch, branch, short(tip))
	}
	fmt.Printf("  %s\n", rerun)
	ui.Info("if it is your own work and the target moved on underneath it, reconcile it there instead (rebase or merge onto %s)", short(target.tip))
	return fmt.Errorf("wt's worktree for %q (HEAD %s) does not match %s (%s): %s; wt did not adopt it", branch, short(head), target.label, short(target.tip), relationPhrase(rel))
}

// maxShownCommits caps the commit lists printed below.
const maxShownCommits = 10

// printTips prints both tips, how far apart they are, and the commits only the
// local side has: what an operator needs to judge a stale branch (#167).
func printTips(localLabel, local, targetLabel, target string, rel TipRelation) {
	w := max(len(localLabel), len(targetLabel))
	if local == "" {
		local = "(none)"
	}
	ahead, behind, err := gitx.AheadBehind(local, target)
	if rel == TipUnknown || err != nil {
		fmt.Printf("    %-*s  %s\n", w, localLabel, local)
		fmt.Printf("    %-*s  %s  (the two cannot be compared)\n", w, targetLabel, target)
		return
	}
	fmt.Printf("    %-*s  %s  has %d commit(s) that %s lacks\n", w, localLabel, local, ahead, targetLabel)
	fmt.Printf("    %-*s  %s  has %d commit(s) that %s lacks\n", w, targetLabel, target, behind, localLabel)
	printOnlyIn(localLabel, local, target, ahead)
}

// printOnlyIn lists, newest first, up to maxShownCommits of the n commits in
// local that are not in target.
func printOnlyIn(localLabel, local, target string, n int) {
	if n == 0 {
		return
	}
	lines, err := gitx.OnelineLog(target, local, maxShownCommits)
	if err != nil || len(lines) == 0 {
		return
	}
	fmt.Printf("    commits only in %s, newest first:\n", localLabel)
	for _, ln := range lines {
		fmt.Printf("      %s\n", ln)
	}
	if n > len(lines) {
		fmt.Printf("      … and %d more\n", n-len(lines))
	}
}

// relationPhrase names a refused relation for the one-line error (#167).
func relationPhrase(rel TipRelation) string {
	switch rel {
	case TipAhead:
		return "it has commits the target lacks"
	case TipDiverged:
		return "the two have diverged"
	case TipUnknown:
		return "the two cannot be compared"
	}
	return "they differ"
}

// short abbreviates a commit id for a message; anything shorter is returned as is.
func short(sha string) string {
	if len(sha) > 8 {
		return sha[:8]
	}
	if sha == "" {
		return "(none)"
	}
	return sha
}
