package merge

import (
	"regexp"
	"strconv"
	"strings"

	"github.com/eharriett0/wt/internal/ghx"
	"github.com/eharriett0/wt/internal/gitx"
)

// fetchPRHead is gitx.FetchPRHeadNoPrompt; a var so a test can count it.
var fetchPRHead = gitx.FetchPRHeadNoPrompt

var prURLNumber = regexp.MustCompile(`/pull/(\d+)/?$`)

// PRNumber is pr's number: pr itself when it is one, else the number at the
// end of a PR URL (gh takes either), "" otherwise (#230 re-review: a URL got
// no fetch, and a broken fetch command). Pure.
func PRNumber(pr string) string {
	if _, err := strconv.Atoi(pr); err == nil {
		return pr
	}
	if m := prURLNumber.FindStringSubmatch(pr); m != nil {
		return m[1]
	}
	return ""
}

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
		if ancestorErr != nil && fetchPRHead("origin", PRNumber(pr)) == nil {
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
