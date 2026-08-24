// Close-keyword lint + post-merge issue-state verification for `wt merge-pr`
// (eharriett0/wt#77). merge-pr is the one place that can see what a merge will
// auto-close — the PR body AND the squash commit body it forwards. GitHub's
// closingIssuesReferences only sees the PR title/body, so a `Fixes #N` living in
// a commit body closes an issue silently (trap 2). And negation does not disarm
// a close keyword — "does not close #N" still closes (trap 1). This surfaces both
// before the squash, and verifies issue state after it.
package cli

import (
	"fmt"
	"sort"
	"strconv"
	"strings"

	"github.com/eharriett0/wt/internal/ghx"
	"github.com/eharriett0/wt/internal/merge"
	"github.com/eharriett0/wt/internal/ui"
)

// closePlan is the pre-merge close analysis, threaded to the post-merge verify.
type closePlan struct {
	refs   []merge.ClosingRef // every closing ref in PR body + commit messages
	extra  []int              // same-repo closings NOT in closingIssuesReferences (trap 2)
	watch  []int              // same-repo issue numbers to re-check after the merge
	before map[int]string     // issue → state snapshot before the merge
}

// analyzeClosings gathers what the squash will close (PR body + full commit
// messages), compares to the PR's own closingIssuesReferences, and snapshots the
// watched issues' states for the post-merge verify. Best-effort — a gh failure
// yields an empty plan so it never blocks a merge.
func analyzeClosings(pr string) closePlan {
	body, _ := ghx.PRBody(pr)
	text := body + "\n\n" + ghx.PRCommitText(pr)

	watchSet := map[int]bool{}
	for _, n := range merge.SameRepoClosings(text) {
		watchSet[n] = true
	}
	graph := ghx.PRClosingIssueNumbers(pr)
	for _, n := range graph {
		watchSet[n] = true
	}
	watch := make([]int, 0, len(watchSet))
	before := map[int]string{}
	for n := range watchSet {
		watch = append(watch, n)
		if st, err := ghx.IssueState(strconv.Itoa(n)); err == nil {
			before[n] = strings.TrimSpace(st)
		}
	}
	sort.Ints(watch)
	return closePlan{
		refs:   merge.ClosingRefs(text),
		extra:  merge.ExtraClosings(text, graph),
		watch:  watch,
		before: before,
	}
}

// renderClosePlan prints the resolved close set with each same-repo issue's
// current state + title, and returns whether a confirmation gate should fire —
// which is exactly when the squash closes something the PR's own closing
// references do NOT (the trap-2 signature). A normal "Fixes #N" PR prints its
// close set but does NOT gate, so the warning stays meaningful.
func renderClosePlan(p closePlan) bool {
	if len(p.refs) == 0 {
		return false
	}
	ui.Info("this merge will CLOSE:")
	for _, r := range p.refs {
		if r.Repo != "" {
			fmt.Printf("    %s  %s\n", ui.Bold(fmt.Sprintf("%s#%d", r.Repo, r.Number)), ui.Dim("(cross-repo)"))
			continue
		}
		title, _ := ghx.IssueTitle(strconv.Itoa(r.Number))
		st := p.before[r.Number]
		if st == "" {
			st = "?"
		}
		fmt.Printf("    %s  %s  %s\n", ui.Bold("#"+strconv.Itoa(r.Number)), ui.Yellow("["+st+"]"), ui.Dim(title))
	}
	if len(p.extra) > 0 {
		ui.Warn("the squash COMMIT body will close %s — NOT in the PR's own closing references. "+
			"GitHub's closingIssuesReferences is blind to the commit body, so this would close silently (#77 trap 2).",
			joinNums(p.extra))
		return true
	}
	return false
}

// verifyClosings re-checks the watched issues after the merge and reports any
// that changed state — surfacing a silent close in the SAME command instead of
// days later (#77). Needs no keyword parsing; catches everything.
func verifyClosings(p closePlan) {
	for _, n := range p.watch {
		after, err := ghx.IssueState(strconv.Itoa(n))
		if err != nil {
			continue
		}
		after = strings.TrimSpace(after)
		before := p.before[n]
		switch {
		case before != "" && !strings.EqualFold(before, after):
			ui.Warn("issue #%d changed state on merge: %s → %s", n, before, after)
		case strings.EqualFold(after, "CLOSED"):
			ui.Info("verified: #%d is CLOSED", n)
		}
	}
}

func joinNums(nums []int) string {
	parts := make([]string, len(nums))
	for i, n := range nums {
		parts[i] = "#" + strconv.Itoa(n)
	}
	return strings.Join(parts, ", ")
}
