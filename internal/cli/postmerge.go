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
