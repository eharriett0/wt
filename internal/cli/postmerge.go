package cli

import (
	"strings"
	"time"

	"github.com/eharriett0/wt/internal/ghx"
	"github.com/eharriett0/wt/internal/gitx"
	"github.com/eharriett0/wt/internal/merge"
	"github.com/eharriett0/wt/internal/ui"
)

// postMergeSleep is the pause between re-reads; a test swaps it out.
var postMergeSleep = time.Sleep

// mergeConfirmed reports whether PR pr reads MERGED now that `gh pr merge` has
// exited 0, and otherwise says why its worktree and branch are kept (#185).
// merge-pr verifies the closes and auto-cleans only on true: gh also exits 0
// for --help, --auto, --disable-auto, a merge queue and -R, none of which
// merged this PR, and auto-cleaning then deleted a live lane, unpushed commits
// included.
func mergeConfirmed(pr string) bool {
	state, v := merge.ConfirmMerged(func() string { return ghx.PRState(pr) }, postMergeSleep)
	switch v {
	case merge.PostMerged:
		return true
	case merge.PostUnknown:
		ui.Warn("gh exited 0 but PR #%s's state could not be read: not confirmed merged, so the worktree and branch are kept; `wt clean` removes them once it ships", pr)
	default:
		ui.Warn("gh exited 0 but PR #%s is %s: not merged, so the worktree and branch are kept; `wt clean` removes them once it ships", pr, state)
	}
	if v == merge.PostOpen {
		ui.Info("gh exits 0 without merging when it arms auto-merge (--auto) or hands the PR to a merge queue, and GitHub merges it later (gh's output above says which); also for --help, --disable-auto and -R <another repo>")
	}
	return false
}

// mergedDespiteFailure reports whether PR pr reads MERGED although `gh pr
// merge` exited non-zero (#196), and says so. gh can fail after the merge:
// `-d` merges, then cannot delete a local branch that a wt worktree has checked
// out. It re-reads the state as mergeConfirmed does; anything but MERGED
// (OPEN, CLOSED, no answer) is a merge that failed, and merge-pr exits 1 on it
// as before, gh's own error being what the operator needs.
func mergedDespiteFailure(pr string) bool {
	if _, v := merge.ConfirmMerged(func() string { return ghx.PRState(pr) }, postMergeSleep); v != merge.PostMerged {
		return false
	}
	ui.Warn("gh pr merge exited non-zero, but PR #%s is MERGED: gh failed after the merge (its error is above; `-d` does when a wt worktree has the branch checked out), so merge-pr verifies the closes and cleans up as for any merge", pr)
	return true
}

// unshippedLane returns "" when every commit on the merged PR's local branch
// shipped in the PR, and otherwise the warning merge-pr prints instead of
// removing the branch's worktree and deleting the branch (#187).
func unshippedLane(pr, branch string) string {
	head, _ := ghx.PRHeadOid(pr)
	tip := gitx.BranchTip(branch)
	var ancestor bool
	var ancestorErr error
	if tip != "" && head != "" && !strings.EqualFold(tip, head) {
		ancestor, ancestorErr = gitx.IsAncestor(tip, head)
	}
	v := merge.LocalTipVerdict(tip, head, ancestor, ancestorErr)
	if v == merge.TipShipped {
		return ""
	}
	n := -1
	if v == merge.TipUnshipped {
		n = gitx.BehindCount(head, tip) // rev-list --count head..tip: the commits the PR lacks
	}
	return merge.KeptLaneMessage(v, branch, pr, n, tip, head)
}
