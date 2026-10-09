package merge

import (
	"fmt"
	"strings"
	"time"
)

// PostVerdict is what merge-pr may do once `gh pr merge` has exited 0 (#185).
type PostVerdict string

const (
	PostMerged  PostVerdict = "merged"  // MERGED: verify the closes, auto-clean the worktree
	PostOpen    PostVerdict = "open"    // OPEN: not merged (yet): keep everything
	PostClosed  PostVerdict = "closed"  // CLOSED without a merge: keep everything
	PostUnknown PostVerdict = "unknown" // no state could be read: keep everything
)

// PostMergeVerdict maps the PR state read back after `gh pr merge` exited 0 to
// what merge-pr may do next (#185). An exit 0 is not a merge: gh exits 0
// WITHOUT merging for --help/-h, --auto (armed until the checks pass),
// --disable-auto, a merge queue (queued), a PR already in the queue, and -R
// (it merged ANOTHER repo's PR). Only MERGED proves the merge, and a failed
// read or a missing PR ("", or a placeholder like "null") never counts as one
// (#168). Pure.
func PostMergeVerdict(state string) PostVerdict {
	switch strings.ToUpper(strings.TrimSpace(state)) {
	case "MERGED":
		return PostMerged
	case "OPEN":
		return PostOpen
	case "CLOSED":
		return PostClosed
	}
	return PostUnknown
}

// Settled reports whether another read could not change the verdict: MERGED
// and CLOSED are final, while OPEN or no answer may be API lag right after
// the merge.
func (v PostVerdict) Settled() bool { return v == PostMerged || v == PostClosed }

// postMergeWaits are the pauses before each re-read of an unsettled state:
// three reads and 2.5s of waiting at most.
var postMergeWaits = []time.Duration{time.Second, 1500 * time.Millisecond}

// ConfirmMerged reads the PR's state after `gh pr merge` exited 0, re-reading
// while it is unsettled, and returns the last state read and its verdict
// (#185). read is the gh state read and sleep the pause, so the policy is
// testable without either.
func ConfirmMerged(read func() string, sleep func(time.Duration)) (string, PostVerdict) {
	state := read()
	v := PostMergeVerdict(state)
	for _, wait := range postMergeWaits {
		if v.Settled() {
			break
		}
		sleep(wait)
		state = read()
		v = PostMergeVerdict(state)
	}
	return state, v
}

// TipVerdict is whether merge-pr's auto-clean may delete a merged PR's local
// branch, and its worktree with it (#187).
type TipVerdict string

const (
	TipShipped   TipVerdict = "shipped"   // the local tip IS the PR's head, or an ancestor of it
	TipUnshipped TipVerdict = "unshipped" // the branch has commits that were not in the PR
	TipUnknown   TipVerdict = "unknown"   // no tip or head, or git could not compare them
)

// LocalTipVerdict decides whether every commit on a merged PR's local branch
// shipped in that PR (#187). A squash merge never makes the branch merged in
// git's eyes, so the auto-clean deletes it with `git branch -D`, which is safe
// only when the local tip is the PR's head (headRefOid) or an ancestor of it.
// ancestor and ancestorErr are `git merge-base --is-ancestor <tip> <head>`,
// read only when the two differ. A missing tip or head, or a git error (say,
// the head was never fetched here), is unknown, and only shipped may delete.
// Pure.
func LocalTipVerdict(tip, head string, ancestor bool, ancestorErr error) TipVerdict {
	switch {
	case tip == "" || head == "":
		return TipUnknown
	case strings.EqualFold(tip, head):
		return TipShipped
	case ancestorErr != nil:
		return TipUnknown
	case ancestor:
		return TipShipped
	}
	return TipUnshipped
}

// KeptLaneMessage is the warning when the auto-clean keeps a merged PR's
// worktree and branch (#187). n is how many commits on branch were not in PR
// pr, below 1 when they could not be counted; tip and head say what wt could
// not read when the verdict is unknown. Pure.
func KeptLaneMessage(v TipVerdict, branch, pr string, n int, tip, head string) string {
	const kept = "kept the worktree and branch (push them, or delete them yourself)"
	switch {
	case v == TipUnshipped && n > 0:
		return fmt.Sprintf("%s has %d commit(s) that were not in PR #%s; %s", branch, n, pr, kept)
	case v == TipUnshipped:
		return fmt.Sprintf("%s has commits that were not in PR #%s (wt could not count them); %s", branch, pr, kept)
	case head == "":
		return fmt.Sprintf("%s may have commits that were not in PR #%s: wt could not read the PR's head commit; %s", branch, pr, kept)
	case tip == "":
		return fmt.Sprintf("%s may have commits that were not in PR #%s: wt could not read the branch's tip; %s", branch, pr, kept)
	}
	return fmt.Sprintf("%s may have commits that were not in PR #%s: wt could not compare its tip with the PR's head %.12s (is that commit fetched here?); %s", branch, pr, head, kept)
}
