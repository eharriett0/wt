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
		{"on: --admin alone", true, nil, []string{"--admin"}},
		// #180: in FRONT of the passthrough, never after it
		{"on: goes before the passthrough", true, []string{"--delete-branch"}, []string{"--admin", "--delete-branch"}},
		{"on: a dangling passthrough flag cannot take it as its value", true, []string{"--subject"}, []string{"--admin", "--subject"}},
		// no dedupe: gh takes a repeated bool fine, and a token match misreads a VALUE
		{"on: a forwarded --admin is just repeated", true, []string{"--admin"}, []string{"--admin", "--admin"}},
		{"on: the body text --admin keeps the real flag (M13)", true, []string{"-b", "--admin"}, []string{"--admin", "-b", "--admin"}},
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
	if want := []string{"--admin", "--delete-branch"}; !reflect.DeepEqual(got, want) {
		t.Errorf("WithAdmin returned %v, want %v", got, want)
	}
}

// TestWithSubject pins the WIP strip's --subject IN FRONT of the passthrough
// (#180): gh keeps the last --subject, so an operator's own forwarded one wins,
// and a dangling passthrough flag cannot take wt's subject as its value.
func TestWithSubject(t *testing.T) {
	cases := []struct {
		args, want []string
	}{
		{nil, []string{"--subject", "Real title"}},
		{[]string{"--admin"}, []string{"--subject", "Real title", "--admin"}},
		{[]string{"--subject", "Operator's"}, []string{"--subject", "Real title", "--subject", "Operator's"}},
		{[]string{"-b"}, []string{"--subject", "Real title", "-b"}},
	}
	for _, tc := range cases {
		in := append([]string(nil), tc.args...)
		if got := WithSubject("Real title", in); !reflect.DeepEqual(got, tc.want) {
			t.Errorf("WithSubject(%q) = %q, want %q", tc.args, got, tc.want)
		}
		if !reflect.DeepEqual(in, tc.args) {
			t.Errorf("WithSubject mutated its input: %q, was %q", in, tc.args)
		}
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
		// ⚠ A BACKTICK CODE SPAN DOES NOT SUPPRESS THIS MATCHER (#164). Pinned as
		// current behaviour rather than as a preference: whether it SHOULD suppress
		// depends on whether GitHub's own parser honours code spans, which is the
		// open question on that issue. Until that is settled by measurement, the
		// lint deliberately over-reports here — flagging a keyword GitHub would
		// ignore is noise, missing one it would honour is a closed issue.
		{"a code span is still matched", "see `Closes #8` in the postmortem", []want{{8, ""}}},
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

// TestParseForwardedBody pins the gh-faithful reading of a merge-pr passthrough
// (#180): which squash body `gh pr merge` (gh v2.68.1 / pflag v1.0.6) will use.
// The close check judges that body, so a misread here judges the wrong text.
func TestParseForwardedBody(t *testing.T) {
	type want struct {
		src   BodySource
		value string
		flag  string
	}
	none := want{BodyDefault, "", ""}
	cases := []struct {
		name string
		args []string
		want want
	}{
		{"no passthrough", nil, none},
		{"unrelated flags only", []string{"--delete-branch", "--admin", "-d"}, none},

		// --body / -b, every spelling
		{"--body x", []string{"--body", "x"}, want{BodyText, "x", "--body"}},
		{"--body=x", []string{"--body=x"}, want{BodyText, "x", "--body"}},
		{"-b x", []string{"-b", "x"}, want{BodyText, "x", "-b"}},
		{"-bx attached", []string{"-bx"}, want{BodyText, "x", "-b"}},
		{"-b=x", []string{"-b=x"}, want{BodyText, "x", "-b"}},
		{"-b= is the text '=' (pflag)", []string{"-b="}, want{BodyText, "=", "-b"}},
		{"--body= is SET and empty", []string{"--body="}, want{BodyText, "", "--body"}},
		{"--body '' is SET and empty", []string{"--body", ""}, want{BodyText, "", "--body"}},
		{"value with spaces and #", []string{"-b", "Squash body.\n\nFixes #9, see #10"}, want{BodyText, "Squash body.\n\nFixes #9, see #10", "-b"}},

		// --body-file / -F, every spelling
		{"--body-file f", []string{"--body-file", "f.txt"}, want{BodyFile, "f.txt", "--body-file"}},
		{"--body-file=f", []string{"--body-file=f.txt"}, want{BodyFile, "f.txt", "--body-file"}},
		{"-F f", []string{"-F", "f.txt"}, want{BodyFile, "f.txt", "-F"}},
		{"-Ff attached", []string{"-Ff.txt"}, want{BodyFile, "f.txt", "-F"}},
		{"-F=f", []string{"-F=f.txt"}, want{BodyFile, "f.txt", "-F"}},
		{"path with spaces and #", []string{"-F", "my dir/body #2.txt"}, want{BodyFile, "my dir/body #2.txt", "-F"}},

		// stdin
		{"-F -", []string{"-F", "-"}, want{BodyStdin, "-", "-F"}},
		{"--body-file -", []string{"--body-file", "-"}, want{BodyStdin, "-", "--body-file"}},
		{"--body-file=-", []string{"--body-file=-"}, want{BodyStdin, "-", "--body-file"}},
		{"-F- attached", []string{"-F-"}, want{BodyStdin, "-", "-F"}},

		// shorthand clusters: bools first, a value flag ends the cluster
		{"-dF f", []string{"-dF", "f.txt"}, want{BodyFile, "f.txt", "-F"}},
		{"-dFf", []string{"-dFf.txt"}, want{BodyFile, "f.txt", "-F"}},
		{"-sdb text", []string{"-sdb", "text"}, want{BodyText, "text", "-b"}},
		{"-Fd is the file d", []string{"-Fd"}, want{BodyFile, "d", "-F"}},
		{"-d=false then -b", []string{"-d=false", "-b", "x"}, want{BodyText, "x", "-b"}},

		// repeats: the last occurrence wins
		{"--body twice", []string{"--body", "Closes #5", "--body", "plain"}, want{BodyText, "plain", "--body"}},
		{"-F twice", []string{"-F", "a.txt", "--body-file", "b.txt"}, want{BodyFile, "b.txt", "--body-file"}},
		{"-F then -F -", []string{"-F", "a.txt", "-F", "-"}, want{BodyStdin, "-", "-F"}},
		{"-F - then -F file", []string{"-F", "-", "-F", "a.txt"}, want{BodyFile, "a.txt", "-F"}},
		// gh tests `bodyFile != ""`, so a final empty --body-file is no override
		{"-F then empty -F", []string{"-F", "a.txt", "-F", ""}, none},
		{"--body then empty --body-file", []string{"--body", "x", "--body-file="}, want{BodyText, "x", "--body"}},

		// a value flag swallows the next token even if it looks like a flag
		{"--subject -b: -b is the subject", []string{"--subject", "-b", "x"}, none},
		{"-t -b: -b is the subject", []string{"-t", "-b", "x"}, none},
		{"-t-b attached subject", []string{"-t-b"}, none},
		{"--subject=--body", []string{"--subject=--body", "x"}, none},
		{"-R -F: -F is the repo", []string{"-R", "-F", "f.txt"}, none},
		{"-R o/r then -F", []string{"-R", "o/r", "-F", "f.txt"}, want{BodyFile, "f.txt", "-F"}},
		{"--body --admin: body is the text --admin", []string{"--body", "--admin"}, want{BodyText, "--admin", "--body"}},
		{"--body --: body is the text --", []string{"--body", "--"}, want{BodyText, "--", "--body"}},

		// `--` ends the flags
		{"-- before the body flag", []string{"--", "--body", "x"}, none},
		{"body, then -- then another", []string{"--body", "x", "--", "-F", "y"}, want{BodyText, "x", "--body"}},

		// flags gh rejects or skips do not derail the read
		{"bool with = then body", []string{"--squash=false", "-b", "x"}, want{BodyText, "x", "-b"}},
		{"-test.v is skipped", []string{"-test.v", "-b", "x"}, want{BodyText, "x", "-b"}},
		{"positional tokens skipped", []string{"stray", "-", "-b", "x"}, want{BodyText, "x", "-b"}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got, err := ParseForwardedBody(tc.args)
			if err != nil {
				t.Fatalf("ParseForwardedBody(%q) error: %v", tc.args, err)
			}
			if got.Source != tc.want.src || got.Value != tc.want.value || got.Flag != tc.want.flag {
				t.Fatalf("ParseForwardedBody(%q) = {%v %q %q}, want {%v %q %q}",
					tc.args, got.Source, got.Value, got.Flag, tc.want.src, tc.want.value, tc.want.flag)
			}
		})
	}
}

// TestParseForwardedSubject pins the gh-faithful reading of a forwarded squash
// SUBJECT (#196), through the same scanner as the body: the last --subject/-t
// wins, a value flag swallows the next token, and an empty value is still given
// (gh then sends none, so GitHub writes the repo's default).
func TestParseForwardedSubject(t *testing.T) {
	type want struct {
		given bool
		value string
		flag  string
	}
	none := want{}
	cases := []struct {
		name string
		args []string
		want want
	}{
		{"no passthrough", nil, none},
		{"unrelated flags only", []string{"--delete-branch", "--admin", "-d"}, none},
		{"a body is not a subject", []string{"-b", "Fixes #5"}, none},

		{"--subject x", []string{"--subject", "x"}, want{true, "x", "--subject"}},
		{"--subject=x", []string{"--subject=x"}, want{true, "x", "--subject"}},
		{"-t x", []string{"-t", "x"}, want{true, "x", "-t"}},
		{"-tx attached", []string{"-tx"}, want{true, "x", "-t"}},
		{"-t=x", []string{"-t=x"}, want{true, "x", "-t"}},
		{"-dt x: -d is a bool", []string{"-dt", "x"}, want{true, "x", "-t"}},
		{"value with a close keyword", []string{"-t", "Fixes #8 via subject"}, want{true, "Fixes #8 via subject", "-t"}},

		// the last one wins, an empty one included
		{"twice: the last wins", []string{"--subject", "Fixes #5", "-t", "clean"}, want{true, "clean", "-t"}},
		{"--subject '' is given and empty", []string{"--subject", ""}, want{true, "", "--subject"}},
		{"--subject= after a real one", []string{"-t", "Fixes #5", "--subject="}, want{true, "", "--subject"}},
		{"the WIP strip's subject, then the operator's", []string{"--subject", "Strip", "--admin", "--subject", "Mine"}, want{true, "Mine", "--subject"}},

		// a value flag takes the next token, whatever it looks like
		{"-b -t: -t is the body", []string{"-b", "-t", "x"}, none},
		{"--body --subject: the body is the text --subject", []string{"--body", "--subject"}, none},
		{"-F -t: -t is the file", []string{"-F", "-t"}, none},
		{"-t -b: -b is the subject", []string{"-t", "-b"}, want{true, "-b", "-t"}},
		{"-R o/r then -t", []string{"-R", "o/r", "-t", "x"}, want{true, "x", "-t"}},

		// `--` ends the flags
		{"-- before the subject flag", []string{"--", "--subject", "x"}, none},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got, err := ParseForwardedSubject(tc.args)
			if err != nil {
				t.Fatalf("ParseForwardedSubject(%q) error: %v", tc.args, err)
			}
			if got.Given != tc.want.given || got.Value != tc.want.value || got.Flag != tc.want.flag {
				t.Fatalf("ParseForwardedSubject(%q) = {%v %q %q}, want {%v %q %q}",
					tc.args, got.Given, got.Value, got.Flag, tc.want.given, tc.want.value, tc.want.flag)
			}
		})
	}
	// A value flag left without a value is gh's own failure: an error, as for
	// the body, never a guessed subject.
	for _, args := range [][]string{{"--subject"}, {"-t"}, {"-dt"}, {"-t", "x", "-b"}} {
		if got, err := ParseForwardedSubject(args); err == nil {
			t.Errorf("ParseForwardedSubject(%q) = %+v, want an error", args, got)
		}
	}
}

func TestWIPSubject(t *testing.T) {
	cases := []struct {
		title string
		want  string
		ok    bool
	}{
		{"WIP: Fixes #5 the thing", "Fixes #5 the thing", true},
		{"WIP: #196 — title", "#196 — title", true},
		{"WIP:", "", false},    // nothing after the prefix: Run leaves the title
		{"WIP:   ", "", false}, // the same, padded
		{"Fixes #5", "Fixes #5", false},
		{"", "", false},
	}
	for _, c := range cases {
		got, ok := WIPSubject(c.title)
		if ok != c.ok || (ok && got != c.want) {
			t.Errorf("WIPSubject(%q) = (%q, %v), want (%q, %v)", c.title, got, ok, c.want, c.ok)
		}
	}
}

// No single body can be named → an error, never a guess: gh refuses --body with
// --body-file, and a body flag with no value would make gh take whatever wt
// appends next (--admin, --subject …) as its value.
func TestParseForwardedBody_errors(t *testing.T) {
	for _, args := range [][]string{
		{"--body"},
		{"-b"},
		{"--body-file"},
		{"-F"},
		{"-dF"},
		{"--admin", "-b"},
		// #180: ANY value flag left dangling at the end, body or not — wt's own
		// flags go in front now, so gh fails on it ("flag needs an argument")
		{"-b", "x", "--subject"},
		{"-t"},
		{"--repo"},
		{"-dR"},
		{"--match-head-commit"},
		{"-F", "f.txt", "-A"},
		{"-b", "x", "-F", "f.txt"},
		{"--body-file=f.txt", "--body", "x"},
		{"--body=", "--body-file=f.txt"}, // an EMPTY --body still counts as given
		{"-F", "-", "-b", "x"},
	} {
		if got, err := ParseForwardedBody(args); err == nil {
			t.Errorf("ParseForwardedBody(%q) = %+v, want an error", args, got)
		}
	}
}

func TestRedirectToStdin(t *testing.T) {
	cases := []struct {
		args, want []string
	}{
		{[]string{"-F", "f.txt"}, []string{"-F", "-"}},
		{[]string{"--body-file", "f.txt"}, []string{"--body-file", "-"}},
		{[]string{"--body-file=f.txt"}, []string{"--body-file=-"}},
		{[]string{"-F=f.txt"}, []string{"-F=-"}},
		{[]string{"-Ff.txt"}, []string{"-F-"}},
		{[]string{"-dFf.txt"}, []string{"-dF-"}},
		{[]string{"-dF", "f.txt"}, []string{"-dF", "-"}},
		{[]string{"-F", "-"}, []string{"-F", "-"}},
		// only the occurrence gh uses (the last) is re-pointed
		{[]string{"--admin", "-F", "a.txt", "-d", "-F", "b.txt"}, []string{"--admin", "-F", "a.txt", "-d", "-F", "-"}},
		// not a file source → unchanged
		{[]string{"-b", "text"}, []string{"-b", "text"}},
		{[]string{"--delete-branch"}, []string{"--delete-branch"}},
		{nil, nil},
	}
	for _, tc := range cases {
		b, err := ParseForwardedBody(tc.args)
		if err != nil {
			t.Fatalf("ParseForwardedBody(%q): %v", tc.args, err)
		}
		in := append([]string(nil), tc.args...)
		got := b.RedirectToStdin(in)
		if !reflect.DeepEqual(got, tc.want) {
			t.Errorf("RedirectToStdin(%q) = %q, want %q", tc.args, got, tc.want)
		}
		if !reflect.DeepEqual(in, tc.args) {
			t.Errorf("RedirectToStdin mutated its input: %q, was %q", in, tc.args)
		}
		// Whatever the source was, gh must now read the body from stdin.
		if b.Source == BodyFile || b.Source == BodyStdin {
			if after, err := ParseForwardedBody(got); err != nil || after.Source != BodyStdin {
				t.Errorf("after RedirectToStdin(%q) gh would read %+v (err %v), want stdin", tc.args, after, err)
			}
		}
	}
}
