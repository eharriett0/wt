package merge

import (
	"reflect"
	"strings"
	"testing"
)

func TestGuardVerdict(t *testing.T) {
	cases := []struct {
		name      string
		fileCount string
		subjects  []string
		want      Verdict
	}{
		{"empty: zero files", "0", nil, VerdictEmptyDiff},
		{"empty: unparseable", "", nil, VerdictEmptyDiff},
		{"empty: non-numeric", "abc", nil, VerdictEmptyDiff},
		{"empty: negative", "-3", nil, VerdictEmptyDiff},
		{"empty: zero files even with real commits", "0", []string{"feat: do the thing"}, VerdictEmptyDiff},
		{"ok: one file one real commit", "1", []string{"feat: do the thing"}, VerdictOK},
		{"ok: real + placeholder mixed", "3", []string{"WIP: claim #42 — foo", "feat: real work"}, VerdictOK},
		{"placeholder-only: single placeholder, nonzero files", "2", []string{"WIP: claim #42 — foo"}, VerdictPlaceholderOnly},
		{"placeholder-only: multiple placeholders", "5", []string{"WIP: claim #1 — a", "WIP: claim #2 — b"}, VerdictPlaceholderOnly},
		{"ok: nonzero files, no commit subjects", "4", nil, VerdictOK},
		{"ok: blank lines ignored, real present", "2", []string{"", "feat: x", ""}, VerdictOK},
		{"placeholder-only: blank lines + placeholder", "2", []string{"", "WIP: claim #9 — z", ""}, VerdictPlaceholderOnly},
		{"ok: whitespace-padded count", "  7 ", []string{"fix: y"}, VerdictOK},
		{"ok: prefix-similar but not placeholder", "1", []string{"WIPfoo claim # not a placeholder"}, VerdictOK},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := GuardVerdict(tc.fileCount, tc.subjects); got != tc.want {
				t.Errorf("GuardVerdict(%q, %v) = %q, want %q", tc.fileCount, tc.subjects, got, tc.want)
			}
		})
	}
}

func TestBranchIsForeign(t *testing.T) {
	managed := []string{"feat-1-alpha", "feat-2-beta", "fix-3-gamma"}
	cases := []struct {
		name     string
		head     string
		branches []string
		want     bool
	}{
		{"managed branch is not foreign", "feat-2-beta", managed, false},
		{"unknown branch is foreign", "feat-99-other-window", managed, true},
		{"empty head fails open (not foreign)", "", managed, false},
		{"whitespace-only head fails open", "   ", managed, false},
		{"empty worktree set fails open (can't determine)", "feat-2-beta", nil, false},
		{"empty worktree set + unknown head still fails open", "whatever", []string{}, false},
		{"head matches after trimming", "  feat-1-alpha  ", managed, false},
		{"managed entry padded, head clean", "fix-3-gamma", []string{" fix-3-gamma "}, false},
		{"case-sensitive: different case IS foreign", "Feat-1-Alpha", managed, true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := BranchIsForeign(tc.head, tc.branches); got != tc.want {
				t.Errorf("BranchIsForeign(%q, %v) = %v, want %v", tc.head, tc.branches, got, tc.want)
			}
		})
	}
}

func TestWithAdmin(t *testing.T) {
	cases := []struct {
		name  string
		admin bool
		extra []string
		want  []string
	}{
		{"off: nil unchanged", false, nil, nil},
		{"off: extra passthrough unchanged", false, []string{"--delete-branch"}, []string{"--delete-branch"}},
		{"on: appends to nil", true, nil, []string{"--admin"}},
		{"on: appends after existing passthrough", true, []string{"--delete-branch"}, []string{"--delete-branch", "--admin"}},
		{"on: dedups when passthrough already has --admin", true, []string{"--admin"}, []string{"--admin"}},
		{"on: dedups whitespace-padded --admin", true, []string{" --admin "}, []string{" --admin "}},
		{"on: dedups --admin alongside other args", true, []string{"--delete-branch", "--admin"}, []string{"--delete-branch", "--admin"}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := WithAdmin(tc.admin, tc.extra); !reflect.DeepEqual(got, tc.want) {
				t.Errorf("WithAdmin(%v, %v) = %v, want %v", tc.admin, tc.extra, got, tc.want)
			}
		})
	}
}

// TestWithAdminDoesNotMutateCaller pins the copy-before-append: appending to a
// slice with spare capacity would clobber the caller's backing array. WithAdmin
// must leave the input slice untouched.
func TestWithAdminDoesNotMutateCaller(t *testing.T) {
	extra := make([]string, 1, 4) // len 1, cap 4 — spare capacity to clobber
	extra[0] = "--delete-branch"
	got := WithAdmin(true, extra)
	if len(extra) != 1 || extra[0] != "--delete-branch" {
		t.Errorf("caller slice mutated: %v", extra)
	}
	if want := []string{"--delete-branch", "--admin"}; !reflect.DeepEqual(got, want) {
		t.Errorf("WithAdmin returned %v, want %v", got, want)
	}
}

func TestDeWIPTitle(t *testing.T) {
	cases := []struct {
		in     string
		want   string
		wasWIP bool
	}{
		{"WIP: #1313 — Go runtime", "#1313 — Go runtime", true},
		{"WIP:no space", "no space", true},
		{"  WIP: trimmed  ", "trimmed", true},
		{"feat(x): real title", "feat(x): real title", false},
		{"not a wip", "not a wip", false},
	}
	for _, c := range cases {
		got, wip := DeWIPTitle(c.in)
		if got != c.want || wip != c.wasWIP {
			t.Errorf("DeWIPTitle(%q) = (%q,%v), want (%q,%v)", c.in, got, wip, c.want, c.wasWIP)
		}
	}
}

func TestPreMergeVerdict(t *testing.T) {
	cases := map[string]PreVerdict{
		"OPEN": PreProceed, "MERGED": PreAlreadyMerged, "CLOSED": PreClosed,
		"merged": PreAlreadyMerged, " closed ": PreClosed, "": PreProceed, "WEIRD": PreProceed,
	}
	for state, want := range cases {
		if got := PreMergeVerdict(state); got != want {
			t.Errorf("PreMergeVerdict(%q) = %v, want %v", state, got, want)
		}
	}
}

func TestClosingRefs(t *testing.T) {
	type want struct {
		num  int
		repo string
	}
	cases := []struct {
		name string
		text string
		want []want
	}{
		{"bare ref does NOT close", "see #123 for context", nil},
		{"closes same-repo", "Closes #1633", []want{{1633, ""}}},
		{"fixes/resolved variants", "Fixes #1 and resolved #2, fix #3", []want{{1, ""}, {2, ""}, {3, ""}}},
		{"negation still closes (GitHub ignores 'not')", "this does not close #77", []want{{77, ""}}},
		{"cross-repo owner/repo#N closes", "Closes eharriett0/wt#5", []want{{5, "eharriett0/wt"}}},
		{"single-segment repo#N does NOT close", "Closes wt#5", nil},
		{"full issue URL closes", "resolves https://github.com/o/r/issues/9", []want{{9, "o/r"}}},
		{"dedup by repo#num", "Closes #7 ... closes #7", []want{{7, ""}}},
		{"'postfix #9' — no keyword boundary match", "postfix #9", nil},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := ClosingRefs(tc.text)
			if len(got) != len(tc.want) {
				t.Fatalf("ClosingRefs(%q) = %+v, want %d refs", tc.text, got, len(tc.want))
			}
			for i, w := range tc.want {
				if got[i].Number != w.num || got[i].Repo != w.repo {
					t.Errorf("ref[%d] = (#%d,%q), want (#%d,%q)", i, got[i].Number, got[i].Repo, w.num, w.repo)
				}
			}
		})
	}
}

func TestExtraClosings(t *testing.T) {
	// commit body closes #1633; PR's closingIssuesReferences only knows #1586 →
	// #1633 is the trap-2 extra.
	got := ExtraClosings("Fixes #1633\n\nunrelated body Closes #1586", []int{1586})
	if len(got) != 1 || got[0] != 1633 {
		t.Fatalf("ExtraClosings = %v, want [1633]", got)
	}
	// all closings already in graph → no extras.
	if got := ExtraClosings("Closes #5", []int{5}); got != nil {
		t.Fatalf("ExtraClosings = %v, want nil", got)
	}
}

func TestSuspectClosings(t *testing.T) {
	cases := []struct {
		name string
		text string
		want string // "" = must NOT be flagged
	}{
		// --- MUST FIRE. Every one of these is a real phrasing that closed a
		// live issue; the sentence existed to prevent the close in most of them.
		{"plain negation", "This does NOT close #1027.", SuspectNegated},
		{"lowercase negation", "this does not close #77", SuspectNegated},
		{"contraction", "it doesn't close #5", SuspectNegated},
		{"negation in a heading", "## Scope, and why this does NOT close #2083", SuspectNegated},
		{"never", "this will never close #9", SuspectNegated},
		{"without", "landed without closing #12, resolves #12 is wrong", SuspectNegated},
		{"rather than", "Refs #3 rather than closing it, so do not fix #3", SuspectNegated},
		{"hypothetical would have", "a whole-document compare would have closed #1583 as a false alarm", SuspectNegated},
		{"hypothetical might have", "that might have resolved #44, but it did not", SuspectNegated},
		{"qualifier partially", "Closes #599 partially (IAM precondition only)", SuspectQualified},
		{"qualifier partly", "Fixes #12 partly", SuspectQualified},
		{"qualifier in part", "Resolves #12 in part", SuspectQualified},
		{"cross-repo negated", "does not close owner/repo#5", SuspectNegated},

		// --- MUST NOT FIRE. A false positive blocks a legitimate merge and
		// makes the override routine, which is the #104 failure.
		{"ordinary close", "Closes #1633", ""},
		{"ordinary close with body", "Adds the guard.\n\nCloses #5", ""},
		{"negation in a PRIOR sentence", "This is not a revert. Closes #5", ""},
		{"negation on a PRIOR line", "This does not revert anything\nCloses #5", ""},
		{"negation AFTER the ref", "Closes #5, not #6 as previously stated", ""},
		{"qualifier not adjacent", "Closes #5 and the work is partially done elsewhere", ""},
		{"bare ref with negation", "this does not affect #5", ""},
		{"'note' must not match 'not'", "note: closes #5", ""},
		{"'cannot' must not match", "cannot be reverted once it closes #5", ""},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := SuspectClosings(tc.text)
			if tc.want == "" {
				if len(got) != 0 {
					t.Fatalf("SuspectClosings(%q) = %+v, want none", tc.text, got)
				}
				return
			}
			if len(got) != 1 {
				t.Fatalf("SuspectClosings(%q) = %+v, want exactly 1", tc.text, got)
			}
			if got[0].Suspect != tc.want {
				t.Errorf("Suspect = %q, want %q", got[0].Suspect, tc.want)
			}
			if got[0].Context == "" {
				t.Error("Context is empty — the warning must be able to show the phrasing, " +
					"because a bare issue number reads as the ordinary case")
			}
		})
	}
}

// An ordinary close must not be made suspect by LATER prose describing the trap.
// Quoting the phrase in a postmortem is how the earlier advice kept failing, so
// the first match wins and the dedup keeps it.
func TestSuspectClosings_ordinaryCloseNotPoisonedByLaterProse(t *testing.T) {
	text := "Closes #5\n\nEarlier I wrote that it does not close #5, which was wrong."
	if got := SuspectClosings(text); len(got) != 0 {
		t.Fatalf("SuspectClosings = %+v, want none (first match is the ordinary close)", got)
	}
	if refs := ClosingRefs(text); len(refs) != 1 || refs[0].Suspect != "" {
		t.Fatalf("ClosingRefs = %+v, want one non-suspect ref", refs)
	}
}

// The classifier must not change WHICH issues close — that set is GitHub's and
// this change only annotates it.
func TestSuspectClosings_doesNotChangeTheCloseSet(t *testing.T) {
	text := "This does NOT close #1027. Closes #5 partially. Fixes #9"
	var nums []int
	for _, r := range ClosingRefs(text) {
		nums = append(nums, r.Number)
	}
	if len(nums) != 3 || nums[0] != 1027 || nums[1] != 5 || nums[2] != 9 {
		t.Fatalf("ClosingRefs numbers = %v, want [1027 5 9] — all three still close", nums)
	}
}

func TestSentenceAround(t *testing.T) {
	// The bound is the line break as well as the terminator, because a markdown
	// heading carries no terminator at all.
	text := "alpha. beta #1 gamma\ndelta"
	start := strings.Index(text, "#1")
	lo, hi := sentenceAround(text, start, start+2)
	if got := text[lo:hi]; strings.TrimSpace(got) != "beta #1 gamma" {
		t.Fatalf("sentenceAround = %q, want %q", got, "beta #1 gamma")
	}
}
