package worktree

import (
	"fmt"

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

// TipRelation is how a local tip relates to the commit `wt adopt` must land on
// (#167).
type TipRelation int

const (
	TipNoTarget TipRelation = iota // nothing to compare against: no PR head and no origin/<branch>
	TipEqual                       // the local tip IS the target
	TipBehind                      // the target contains the local tip, so a fast-forward reaches it
	TipAhead                       // the local tip contains the target, plus commits the target lacks
	TipDiverged                    // each has commits the other lacks: the #167 stale branch
	TipUnknown                     // they differ and could not be compared (the target commit is not in this clone)
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

// AdoptAction is what `wt adopt` does with the local branch it is about to
// attach a new worktree to (#167).
type AdoptAction int

const (
	AdoptCreate      AdoptAction = iota // no local branch: worktree-add creates it from origin/<branch>, as before #167
	AdoptAsIs                           // the local branch is the target: attach it unchanged
	AdoptUnverified                     // nothing to compare against: attach the local branch as it is, and say so
	AdoptFastForward                    // the local branch is only behind the target: move it forward, then attach
	AdoptRefuseInUse                    // only behind, but a worktree has it checked out: wt will not move it
	AdoptRefuse                         // it has commits the target lacks, or could not be compared: do not check it out
)

// DecideAdopt picks what `wt adopt` does with an existing local branch of the
// target's name before it worktree-adds it (#167). inUse says a worktree has the
// branch checked out, or that this could not be ruled out. Pure.
//
// The #167 bug was this check missing. worktree-add takes refs/heads/<branch>
// whenever it exists, so a stale branch left by an earlier PR that reused the
// name was checked out instead of the PR head (ahead 6, behind 285), and a push
// from it would have been rejected or, forced, would have overwritten the PR.
// A branch with commits the target lacks is refused whether it diverged or is
// only ahead: wt cannot tell a leftover from unpushed work, and the remedy for
// unpushed work is one fast-forward push.
//
// Only a branch no worktree has checked out is fast-forwarded. Moving a
// checked-out branch underneath its worktree leaves that worktree's files at the
// old commit, so its next commit would revert what the move brought in. inUse
// comes from `git worktree list` (CheckedOutIn), which cannot see a worktree
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
	case TipBehind:
		if inUse {
			return AdoptRefuseInUse
		}
		return AdoptFastForward
	default: // TipAhead, TipDiverged, TipUnknown
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
// "" when none does (#167). A worktree mid-rebase or mid-bisect of the branch
// lists as detached, so it is not found here. Pure.
func CheckedOutIn(refs []gitx.WorktreeRef, branch string) string {
	if branch == "" {
		return ""
	}
	for _, r := range refs {
		if r.Branch == branch {
			return r.Path
		}
	}
	return ""
}

// adoptTarget resolves the commit `wt adopt` must land on, and how messages name
// it: the PR head when adopting by PR, else origin/<branch> as last fetched, else
// nothing (#167).
func adoptTarget(branch string, want AdoptWant, fetchFailed bool) (tip, label string) {
	if want.PRHead != "" {
		return want.PRHead, "PR #" + want.PR + "'s head"
	}
	if tip = gitx.RemoteTrackingTip(branch); tip == "" {
		return "", ""
	}
	label = "origin/" + branch
	if fetchFailed {
		label += " (as last fetched)"
	}
	return tip, label
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

// prepareLocalBranch checks the local branch worktree-add is about to take
// against the target before anything is checked out (#167). An equal branch is
// left alone, one that is only behind is fast-forwarded, and one with commits the
// target lacks is refused. With no local branch there is nothing to check:
// worktree-add creates it from origin/<branch>, as before.
func prepareLocalBranch(branch string, want AdoptWant, target, targetLabel string) error {
	local := gitx.BranchTip(branch)
	rel := relate(local, target)
	inUseAt, inUse := "", false
	if local != "" {
		refs, err := gitx.WorktreeList()
		inUseAt = CheckedOutIn(refs, branch)
		inUse = inUseAt != "" || err != nil // can't list them: assume it is checked out somewhere
	}
	localLabel := "local " + branch
	rerun := "wt adopt " + branch
	if want.PR != "" {
		rerun = "wt adopt " + want.PR
	}

	switch DecideAdopt(local != "", rel, inUse) {
	case AdoptCreate:
		return nil
	case AdoptAsIs:
		ui.OK("local branch %s matches %s (%s)", branch, targetLabel, short(local))
		return nil
	case AdoptUnverified:
		ui.Info("no origin/%s to compare the local branch with, so it is attached as it is (%s), unverified", branch, short(local))
		return nil
	case AdoptFastForward:
		_, behind, _ := gitx.AheadBehind(local, target)
		ui.Step("local branch %s is %d commit(s) behind %s: fast-forwarding it %s → %s", branch, behind, targetLabel, short(local), short(target))
		if err := gitx.FastForwardBranch(branch, local, target); err != nil {
			return fmt.Errorf("fast-forward local branch %q to %s: %w", branch, targetLabel, err)
		}
		return nil
	case AdoptRefuseInUse:
		where := "in a worktree wt could not identify (git worktree list failed)"
		if inUseAt != "" {
			where = "at " + inUseAt
		}
		ui.Warn("local branch %s is behind %s (%s → %s), but it is checked out %s, and wt does not move a branch a worktree has checked out (#167)", branch, targetLabel, short(local), short(target), where)
		if inUseAt != "" {
			ui.Info("work in that worktree instead, after fast-forwarding it there:")
			fmt.Printf("  git -C %s merge --ff-only %s\n", inUseAt, target)
		}
		return fmt.Errorf("local branch %q is checked out %s; wt did not move it", branch, where)
	}

	// AdoptRefuse: the local branch has commits the target lacks, or the two
	// could not be compared. Never check it out silently (#167).
	refused := fmt.Errorf("local branch %q (%s) does not match %s (%s): %s; wt did not check it out", branch, short(local), targetLabel, short(target), relationPhrase(rel))
	ui.Warn("local branch %s does not match %s, so wt will not check it out (#167):", branch, targetLabel)
	printTips(localLabel, local, targetLabel, target, rel)
	if inUseAt != "" {
		ui.Info("it is checked out at %s", inUseAt)
	}
	aside := fmt.Sprintf("git branch -m %s %s-old-%s", branch, branch, short(local))
	switch rel {
	case TipUnknown:
		// Not "set it aside and re-run": with no local branch, worktree-add starts
		// from origin/<branch>, and a target still missing after fetching that is
		// not on it, so a re-run would land on another commit of the same name.
		unreachableTarget(targetLabel, branch)
		return refused
	case TipAhead:
		ui.Info("if those commits are your own unpushed work, push them (a fast-forward) and re-run; if they are left over from an earlier PR that reused the name, set the branch aside or delete it, and re-run:")
		fmt.Printf("  git push origin %s\n", branch)
		fmt.Printf("  %s     # or: git branch -D %s\n", aside, branch)
	default: // TipDiverged
		ui.Info("it is most likely left over from an earlier PR that reused the name. Inspect it, then set it aside or delete it, and re-run:")
		fmt.Printf("  git log --oneline %s..%s\n", target, branch)
		fmt.Printf("  %s     # or: git branch -D %s\n", aside, branch)
	}
	fmt.Printf("  %s\n", rerun)
	return refused
}

// unreachableTarget explains a target commit that is still not in this clone
// after fetching origin/<branch> (#167).
func unreachableTarget(targetLabel, branch string) {
	ui.Info("%s is still not in this clone after fetching origin/%s. If that fetch failed, or the branch was just force-pushed, re-run. Otherwise the PR is probably from a fork: its head is not on origin, and wt adopt, which starts from origin/%s, cannot land on it.", targetLabel, branch, branch)
}

// checkExistingWorktree checks wt's existing worktree for the branch against the
// target before handing it back on a re-run (#167). It never moves the
// worktree's branch: a note when it is only behind or only ahead, a refusal when
// it diverged or could not be compared.
func checkExistingWorktree(wtDir, branch string, want AdoptWant, target, targetLabel string) error {
	head := gitx.HeadCommit(wtDir)
	rel := relate(head, target)
	action := DecideExisting(rel)
	if action == ExistingReuse {
		return nil
	}
	// The advice below assumes the worktree is on branch; say so when it is not.
	other := ""
	if b, err := gitx.CurrentBranchIn(wtDir); err == nil && b != branch {
		other = fmt.Sprintf(" (it has %s checked out, not %s)", b, branch)
		if b == "HEAD" {
			other = fmt.Sprintf(" (its HEAD is detached, not on %s)", branch)
		}
	}
	switch action {
	case ExistingReuseBehind:
		_, behind, _ := gitx.AheadBehind(head, target)
		ui.Warn("the existing worktree%s is %d commit(s) behind %s (%s → %s). wt does not move a checked-out branch, so update it there:", other, behind, targetLabel, short(head), short(target))
		fmt.Printf("  git -C %s merge --ff-only %s\n", wtDir, target)
		return nil
	case ExistingReuseAhead:
		ahead, _, _ := gitx.AheadBehind(head, target)
		ui.Info("the existing worktree%s has %d commit(s) that %s does not (not pushed yet?)", other, ahead, targetLabel)
		return nil
	}

	rerun := "wt adopt " + branch
	if want.PR != "" {
		rerun = "wt adopt " + want.PR
	}
	refused := fmt.Errorf("wt's worktree for %q (HEAD %s) does not match %s (%s): %s; wt did not adopt it", branch, short(head), targetLabel, short(target), relationPhrase(rel))
	ui.Warn("wt's worktree for %s already exists at %s%s, but its HEAD does not match %s (#167):", branch, wtDir, other, targetLabel)
	printTips("the worktree", head, targetLabel, target, rel)
	if rel == TipUnknown {
		unreachableTarget(targetLabel, branch)
		return refused
	}
	ui.Info("if it is left over from an earlier PR that reused the name, remove it (git refuses while it holds uncommitted work), set the branch aside, and re-run:")
	fmt.Printf("  git worktree remove %s\n", wtDir)
	fmt.Printf("  git branch -m %s %s-old-%s\n", branch, branch, short(head))
	fmt.Printf("  %s\n", rerun)
	ui.Info("if it is your own work and the target moved on underneath it, reconcile it there instead (rebase or merge onto %s)", short(target))
	return refused
}

// maxShownCommits caps the local-only commit list a refusal prints.
const maxShownCommits = 10

// printTips prints both tips, how far apart they are, and the commits only the
// local side has: what an operator needs to judge a stale branch (#167).
func printTips(localLabel, local, targetLabel, target string, rel TipRelation) {
	w := max(len(localLabel), len(targetLabel))
	if local == "" {
		local = "(none)"
	}
	if rel == TipUnknown {
		fmt.Printf("    %-*s  %s\n", w, localLabel, local)
		fmt.Printf("    %-*s  %s  (not in this clone, so the two cannot be compared)\n", w, targetLabel, target)
		return
	}
	ahead, behind, err := gitx.AheadBehind(local, target)
	if err != nil {
		fmt.Printf("    %-*s  %s\n", w, localLabel, local)
		fmt.Printf("    %-*s  %s\n", w, targetLabel, target)
		return
	}
	fmt.Printf("    %-*s  %s  has %d commit(s) that %s lacks\n", w, localLabel, local, ahead, targetLabel)
	fmt.Printf("    %-*s  %s  has %d commit(s) that %s lacks\n", w, targetLabel, target, behind, localLabel)
	if ahead == 0 {
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
	if ahead > len(lines) {
		fmt.Printf("      … and %d more\n", ahead-len(lines))
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
		return "the target commit is not in this clone to compare"
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
