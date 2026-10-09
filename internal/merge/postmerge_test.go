package merge

import (
	"reflect"
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
