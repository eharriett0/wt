package merge

import (
	"errors"
	"reflect"
	"strings"
	"testing"
	"time"
)

func TestPostMergeVerdict(t *testing.T) {
	cases := []struct {
		state string
		want  PostVerdict
	}{
		{"MERGED", PostMerged},
		{"merged", PostMerged},
		{" MERGED\n", PostMerged},
		{"OPEN", PostOpen}, // --help, --auto, a merge queue, -R … left it open
		{"CLOSED", PostClosed},
		{"", PostUnknown},     // a failed read is never a merge
		{"null", PostUnknown}, // nor a "no PR" placeholder (#168)
		{"MERGED?", PostUnknown},
		{"MERGEDX", PostUnknown},
	}
	for _, tc := range cases {
		if got := PostMergeVerdict(tc.state); got != tc.want {
			t.Errorf("PostMergeVerdict(%q) = %q, want %q", tc.state, got, tc.want)
		}
	}
}

func TestPostVerdictSettled(t *testing.T) {
	for v, want := range map[PostVerdict]bool{
		PostMerged: true, PostClosed: true, PostOpen: false, PostUnknown: false,
	} {
		if got := v.Settled(); got != want {
			t.Errorf("%q.Settled() = %v, want %v", v, got, want)
		}
	}
}

// TestConfirmMerged pins the re-read policy: a settled state stops at once, an
// unsettled one is re-read (API lag right after the merge) at most twice more,
// waiting 2.5s in all, and the verdict is the LAST read's.
func TestConfirmMerged(t *testing.T) {
	cases := []struct {
		name      string
		reads     []string
		wantState string
		want      PostVerdict
		wantReads int
		wantWait  time.Duration
	}{
		{"merged at once", []string{"MERGED"}, "MERGED", PostMerged, 1, 0},
		{"closed at once is final", []string{"CLOSED"}, "CLOSED", PostClosed, 1, 0},
		{"lag: open, then merged", []string{"OPEN", "MERGED"}, "MERGED", PostMerged, 2, time.Second},
		{"lag: no answer, then merged", []string{"", "", "MERGED"}, "MERGED", PostMerged, 3, 2500 * time.Millisecond},
		{"stays open: --help, --auto, a merge queue", []string{"OPEN", "OPEN", "OPEN", "MERGED"}, "OPEN", PostOpen, 3, 2500 * time.Millisecond},
		{"never answers", []string{"", "", "", ""}, "", PostUnknown, 3, 2500 * time.Millisecond},
		{"answers, then fails: the last read counts", []string{"OPEN", ""}, "", PostUnknown, 3, 2500 * time.Millisecond},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			reads, waited := 0, time.Duration(0)
			read := func() string {
				s := tc.reads[min(reads, len(tc.reads)-1)]
				reads++
				return s
			}
			state, v := ConfirmMerged(read, func(d time.Duration) { waited += d })
			got := []any{state, v, reads, waited}
			want := []any{tc.wantState, tc.want, tc.wantReads, tc.wantWait}
			if !reflect.DeepEqual(got, want) {
				t.Errorf("ConfirmMerged = (state %q, %q, %d reads, waited %v), want (%q, %q, %d, %v)",
					state, v, reads, waited, tc.wantState, tc.want, tc.wantReads, tc.wantWait)
			}
		})
	}
}

func TestLocalTipVerdict(t *testing.T) {
	const (
		head = "1111111111111111111111111111111111111111"
		tip  = "2222222222222222222222222222222222222222"
	)
	gitErr := errors.New("fatal: Not a valid commit name")
	cases := []struct {
		name        string
		tip, head   string
		ancestor    bool
		ancestorErr error
		want        TipVerdict
	}{
		{"the tip is the PR's head", head, head, false, nil, TipShipped},
		{"the same commit in another case", strings.ToUpper(head), head, false, nil, TipShipped},
		{"the tip is behind the PR's head", tip, head, true, nil, TipShipped},
		{"the tip has commits the PR did not", tip, head, false, nil, TipUnshipped},
		{"git could not compare (head not fetched)", tip, head, false, gitErr, TipUnknown},
		{"an error outranks a stale true", tip, head, true, gitErr, TipUnknown},
		{"no head commit from gh", tip, "", true, nil, TipUnknown},
		{"no local tip", "", head, true, nil, TipUnknown},
		{"neither", "", "", false, nil, TipUnknown},
	}
	for _, tc := range cases {
		if got := LocalTipVerdict(tc.tip, tc.head, tc.ancestor, tc.ancestorErr); got != tc.want {
			t.Errorf("%s: LocalTipVerdict = %q, want %q", tc.name, got, tc.want)
		}
	}
}

func TestKeptLaneMessage(t *testing.T) {
	const head = "1111111111111111111111111111111111111111"
	kept := "; kept the worktree and branch (push them, or delete them yourself)"
	cases := []struct {
		v         TipVerdict
		n         int
		tip, head string
		want      string
	}{
		{TipUnshipped, 2, "t", head, "feat-x has 2 commit(s) that were not in PR #5" + kept},
		{TipUnshipped, -1, "t", head, "feat-x has commits that were not in PR #5 (wt could not count them)" + kept},
		{TipUnknown, -1, "t", "", "feat-x may have commits that were not in PR #5: wt could not read the PR's head commit" + kept},
		{TipUnknown, -1, "", head, "feat-x may have commits that were not in PR #5: wt could not read the branch's tip" + kept},
		{TipUnknown, -1, "t", head, "feat-x may have commits that were not in PR #5: wt could not compare its tip with the PR's head 111111111111 (is that commit fetched here?)" + kept},
	}
	for _, tc := range cases {
		if got := KeptLaneMessage(tc.v, "feat-x", "5", tc.n, tc.tip, tc.head); got != tc.want {
			t.Errorf("KeptLaneMessage(%q, n=%d, tip=%q, head=%q) =\n %q\nwant\n %q", tc.v, tc.n, tc.tip, tc.head, got, tc.want)
		}
	}
}
