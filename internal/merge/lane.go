package merge

import (
	"strings"

	"github.com/eharriett0/wt/internal/ghx"
	"github.com/eharriett0/wt/internal/gitx"
)

// fetchPRHead is gitx.FetchPRHeadNoPrompt; a var so a test can count it.
var fetchPRHead = gitx.FetchPRHeadNoPrompt

// UnshippedLane returns "" when tip, the local branch's tip as the caller read
// it, shipped in merged PR pr (LocalTipVerdict), and otherwise the warning
// that keeps the lane (#187). merge-pr's auto-clean and `wt clean` both ask it
// (#230 review: clean reaped a merged PR's lane without it, and deleted the
// commits made there after the merge), and both then delete the branch only
// at that same tip.
func UnshippedLane(pr, branch, tip string) string {
	head, _ := ghx.PRHeadOid(pr)
	var ancestor bool
	var ancestorErr error
	if tip != "" && head != "" && !strings.EqualFold(tip, head) {
		ancestor, ancestorErr = gitx.IsAncestor(tip, head)
		if ancestorErr != nil && fetchPRHead("origin", pr) == nil {
			// #230 review: the head can be a commit this clone never had (the
			// branch updated on GitHub, then deleted with the merge); no plain
			// fetch brings it, so the lane was kept on every run. Once.
			ancestor, ancestorErr = gitx.IsAncestor(tip, head)
		}
	}
	v := LocalTipVerdict(tip, head, ancestor, ancestorErr)
	if v == TipShipped {
		return ""
	}
	n := -1
	if v == TipUnshipped {
		n = gitx.BehindCount(head, tip) // rev-list --count head..tip: the commits the PR lacks
	}
	return KeptLaneMessage(v, branch, pr, n, tip, head)
}
