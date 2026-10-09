package merge

import (
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
