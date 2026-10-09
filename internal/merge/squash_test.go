package merge

import (
	"fmt"
	"testing"
)

func TestNewCommit(t *testing.T) {
	cases := []struct {
		msg      string
		parents  int
		headline string
		body     string
		merge    bool
	}{
		{"feat: one", 1, "feat: one", "", false},
		{"Fix #6 flake\n\nthe body\nsecond line", 1, "Fix #6 flake", "the body\nsecond line", false},
		{"subject\n\n\n\nbody after blank lines", 1, "subject", "body after blank lines", false},
		{"two-line\nfirst paragraph\n\nbody", 1, "two-line", "first paragraph\n\nbody", false},
		{"\n  padded subject  \nbody\n", 1, "padded subject", "body", false},
		// a headline over 72 characters stays whole: GitHub's squash title is the
		// whole first line, unlike GraphQL's messageHeadline
		{"fix(gitx): record both rename paths — close two collision false-negatives (Closes #27)\n\nbody", 1,
			"fix(gitx): record both rename paths — close two collision false-negatives (Closes #27)", "body", false},
		{"Merge branch 'main' into feat", 2, "Merge branch 'main' into feat", "", true},
		{"octopus", 3, "octopus", "", true},
		{"a root commit", 0, "a root commit", "", false},
	}
	for _, c := range cases {
		got := NewCommit(c.msg, c.parents)
		if got.Headline != c.headline || got.Body != c.body || got.Merge != c.merge {
			t.Errorf("NewCommit(%q, %d) = %+v, want {%q %q %v}", c.msg, c.parents, got, c.headline, c.body, c.merge)
		}
	}
}

func TestSubjectOverride(t *testing.T) {
	const wip = `the PR title without "WIP:"`
	cases := []struct {
		name  string
		title string
		pass  []string
		want  Override
	}{
		{"nothing forwarded, no WIP", "Clean title", nil, Override{}},
		{"the WIP strip", "WIP: Fixes #5 the thing", nil, Override{Set: true, Text: "Fixes #5 the thing", From: wip}},
		{"a bare WIP: strips nothing", "WIP:", nil, Override{}},
		{"forwarded --subject", "Clean title", []string{"--subject", "Fixes #8"}, Override{Set: true, Text: "Fixes #8", From: "the forwarded --subject"}},
		{"forwarded -t beats the WIP strip", "WIP: Fixes #5", []string{"-t", "Mine"}, Override{Set: true, Text: "Mine", From: "the forwarded -t"}},
		// gh sends no subject for an empty one, so GitHub writes the repo's
		// default: the PR title, "WIP:" and all
		{"forwarded --subject '' undoes the WIP strip", "WIP: Fixes #5", []string{"--subject", ""}, Override{From: "the forwarded --subject"}},
		{"a body is not a subject", "Clean title", []string{"-b", "Fixes #9"}, Override{}},
	}
	for _, tc := range cases {
		got, err := SubjectOverride(tc.title, tc.pass)
		if err != nil || got != tc.want {
			t.Errorf("%s: SubjectOverride(%q, %q) = %+v (err %v), want %+v", tc.name, tc.title, tc.pass, got, err, tc.want)
		}
	}
	if _, err := SubjectOverride("Clean title", []string{"-t"}); err == nil {
		t.Error("a dangling -t must be an error, as gh fails on it")
	}
}

// TestSubjectOverride_isWhatGhKeeps ties the model to the argv Run builds: the
// WIP --subject and --admin in FRONT of the passthrough, read by gh's parser,
// last one wins, an empty one sends nothing. SubjectOverride must agree with
// that reading for every title and passthrough shape.
func TestSubjectOverride_isWhatGhKeeps(t *testing.T) {
	titles := []string{"Clean title", "WIP: Fixes #5", "WIP:", "WIP: #196 — title"}
	passes := [][]string{
		nil,
		{"--delete-branch"},
		{"--subject", "Mine"},
		{"-t", ""},
		{"--subject", "a", "--subject=b"},
		{"-b", "-t"}, // the body is "-t"
		{"-dt", "Fixes #8"},
		{"--", "--subject", "after the end of flags"},
	}
	for _, title := range titles {
		for _, pass := range passes {
			args := WithAdmin(true, pass)
			if s, ok := WIPSubject(title); ok {
				args = WithSubject(s, args)
			}
			gh, err := ParseForwardedSubject(args)
			if err != nil {
				t.Fatalf("ParseForwardedSubject(%q): %v", args, err)
			}
			got, err := SubjectOverride(title, pass)
			if err != nil {
				t.Fatalf("SubjectOverride(%q, %q): %v", title, pass, err)
			}
			if got.Set != (gh.Value != "") || got.Text != gh.Value {
				t.Errorf("title %q, passthrough %q: SubjectOverride = {Set:%v %q}, but gh keeps %q from %q",
					title, pass, got.Set, got.Text, gh.Value, args)
			}
		}
	}
}

// The fixtures of the ShippedSquash tables: every text is distinct, so the
// expected subject or body names exactly where it comes from.
const (
	sqTitle = "PR title"
	sqBody  = "PR body"
)

var (
	sqC1    = NewCommit("H1 headline\n\nb1 body", 1)
	sqC2    = NewCommit("H2 headline\n\nb2 body", 1)
	sqMerge = NewCommit("Merge branch 'main' into feat", 2)

	sqOne      = []Commit{sqC1}
	sqMany     = []Commit{sqC1, sqC2}
	sqOneMerge = []Commit{sqC1, sqMerge}       // one commit, as GitHub counts
	sqMany3    = []Commit{sqC1, sqMerge, sqC2} // two, the merge not listed
	sqFwdSubj  = Override{Set: true, Text: "forwarded subject", From: "the forwarded --subject"}
	sqFwdBody  = Override{Set: true, Text: "forwarded body", From: "the forwarded --body"}
	sqBothMsgs = "H1 headline\nb1 body\n\nH2 headline\nb2 body"
)

// TestShippedSquash_subject: every squash_merge_commit_title × forwarded or not
// × one or many commits (#196). The body is forwarded throughout, so Unsure
// speaks for the subject alone.
func TestShippedSquash_subject(t *testing.T) {
	cases := []struct {
		name    string
		setting string
		subject Override
		commits []Commit
		want    string
		from    string
		unsure  bool
	}{
		{"PR_TITLE, one commit", TitlePR, Override{}, sqOne, sqTitle, "the PR title", false},
		{"PR_TITLE, many", TitlePR, Override{}, sqMany, sqTitle, "the PR title", false},
		{"PR_TITLE, one, forwarded", TitlePR, sqFwdSubj, sqOne, "forwarded subject", "the forwarded --subject", false},
		{"PR_TITLE, many, forwarded", TitlePR, sqFwdSubj, sqMany, "forwarded subject", "the forwarded --subject", false},

		{"COMMIT_OR_PR_TITLE, one commit: its headline", TitleCommitOrPR, Override{}, sqOne, "H1 headline", "the commit's headline", false},
		{"COMMIT_OR_PR_TITLE, many: the PR title", TitleCommitOrPR, Override{}, sqMany, sqTitle, "the PR title", false},
		{"COMMIT_OR_PR_TITLE, one, forwarded", TitleCommitOrPR, sqFwdSubj, sqOne, "forwarded subject", "the forwarded --subject", false},
		{"COMMIT_OR_PR_TITLE, many, forwarded", TitleCommitOrPR, sqFwdSubj, sqMany, "forwarded subject", "the forwarded --subject", false},
		// GitHub does not count merge commits
		{"COMMIT_OR_PR_TITLE, one commit plus a merge: its headline", TitleCommitOrPR, Override{}, sqOneMerge, "H1 headline", "the commit's headline", false},
		{"COMMIT_OR_PR_TITLE, two plus a merge: the PR title", TitleCommitOrPR, Override{}, sqMany3, sqTitle, "the PR title", false},
		{"COMMIT_OR_PR_TITLE, commits unread: the PR title", TitleCommitOrPR, Override{}, nil, sqTitle, "the PR title", false},

		// unknown: over-scan where the settings disagree (one commit) only
		{"unknown, one commit: the PR title AND the headline", "", Override{}, sqOne, sqTitle + "\n\nH1 headline", "the PR title or the commit's headline", true},
		{"unknown, many: both settings say the PR title", "", Override{}, sqMany, sqTitle, "the PR title", false},
		{"unknown, one, forwarded", "", sqFwdSubj, sqOne, "forwarded subject", "the forwarded --subject", false},
		{"unknown, many, forwarded", "", sqFwdSubj, sqMany, "forwarded subject", "the forwarded --subject", false},
		{"unknown, one plus a merge", "", Override{}, sqOneMerge, sqTitle + "\n\nH1 headline", "the PR title or the commit's headline", true},
		{"a value GitHub never documented is unknown", "MERGE_MESSAGE", Override{}, sqOne, sqTitle + "\n\nH1 headline", "the PR title or the commit's headline", true},
		{"unknown, commits unread", "", Override{}, nil, sqTitle, "the PR title", false},

		// an empty forwarded subject is no override (gh sends none)
		{"an unset override is ignored", TitleCommitOrPR, Override{Text: "ignored", From: "the forwarded --subject"}, sqOne, "H1 headline", "the commit's headline", false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := ShippedSquash(SquashSettings{Title: tc.setting, Message: MessageCommits}, sqTitle, sqBody, tc.commits, tc.subject, sqFwdBody)
			if got.Subject != tc.want || got.SubjectFrom != tc.from || got.Unsure != tc.unsure {
				t.Errorf("subject = %q from %q (unsure %v), want %q from %q (unsure %v)",
					got.Subject, got.SubjectFrom, got.Unsure, tc.want, tc.from, tc.unsure)
			}
		})
	}
}

// TestShippedSquash_body: every squash_merge_commit_message × forwarded or not
// × one or many commits (#196), under the default title setting, plus the
// one-commit shapes the title setting decides. The subject is forwarded
// throughout, so Unsure speaks for the body alone.
func TestShippedSquash_body(t *testing.T) {
	cases := []struct {
		name    string
		title   string
		message string
		body    Override
		commits []Commit
		want    string
		from    string
		unsure  bool
	}{
		{"PR_BODY, one", TitleCommitOrPR, MessagePRBody, Override{}, sqOne, sqBody, "the PR body", false},
		{"PR_BODY, many", TitleCommitOrPR, MessagePRBody, Override{}, sqMany, sqBody, "the PR body", false},
		{"PR_BODY, one, forwarded", TitleCommitOrPR, MessagePRBody, sqFwdBody, sqOne, "forwarded body", "the forwarded --body", false},
		{"PR_BODY, many, forwarded", TitleCommitOrPR, MessagePRBody, sqFwdBody, sqMany, "forwarded body", "the forwarded --body", false},

		{"BLANK, one", TitleCommitOrPR, MessageBlank, Override{}, sqOne, "", "empty", false},
		{"BLANK, many", TitleCommitOrPR, MessageBlank, Override{}, sqMany, "", "empty", false},
		{"BLANK, one, forwarded", TitleCommitOrPR, MessageBlank, sqFwdBody, sqOne, "forwarded body", "the forwarded --body", false},
		{"BLANK, many, forwarded", TitleCommitOrPR, MessageBlank, sqFwdBody, sqMany, "forwarded body", "the forwarded --body", false},

		// one commit: its headline is the subject, its body the body (measured)
		{"COMMIT_MESSAGES, one: the commit's body", TitleCommitOrPR, MessageCommits, Override{}, sqOne, "b1 body", "the commit's body", false},
		{"COMMIT_MESSAGES, many: every message", TitleCommitOrPR, MessageCommits, Override{}, sqMany, sqBothMsgs, "the commit messages", false},
		{"COMMIT_MESSAGES, one, forwarded", TitleCommitOrPR, MessageCommits, sqFwdBody, sqOne, "forwarded body", "the forwarded --body", false},
		{"COMMIT_MESSAGES, many, forwarded", TitleCommitOrPR, MessageCommits, sqFwdBody, sqMany, "forwarded body", "the forwarded --body", false},
		{"an EMPTY forwarded body is still one", TitleCommitOrPR, MessageCommits, Override{Set: true, From: "the forwarded --body"}, sqMany, "", "the forwarded --body", false},
		// merge commits are neither counted nor listed (measured)
		{"COMMIT_MESSAGES, one plus a merge: the commit's body", TitleCommitOrPR, MessageCommits, Override{}, sqOneMerge, "b1 body", "the commit's body", false},
		{"COMMIT_MESSAGES, two plus a merge: the two", TitleCommitOrPR, MessageCommits, Override{}, sqMany3, sqBothMsgs, "the commit messages", false},
		{"COMMIT_MESSAGES, commits unread", TitleCommitOrPR, MessageCommits, Override{}, nil, "", "the commit messages", false},
		// PR_TITLE with one commit: the body leads with the headline unless it
		// is the PR title, the subject already (measured: 35/35 on vscode)
		{"PR_TITLE + COMMIT_MESSAGES, one: the whole message", TitlePR, MessageCommits, Override{}, sqOne, "H1 headline\nb1 body", "the commit messages", false},
		{"PR_TITLE + COMMIT_MESSAGES, one headlined as the PR title: its body", TitlePR, MessageCommits, Override{},
			[]Commit{NewCommit(sqTitle+"\n\nb1 body", 1)}, "b1 body", "the commit's body", false},
		{"... and with a merge commit beside it", TitlePR, MessageCommits, Override{},
			[]Commit{NewCommit(sqTitle+"\n\nb1 body", 1), sqMerge}, "b1 body", "the commit's body", false},
		{"title unknown + COMMIT_MESSAGES, one: the whole message, unsure", "", MessageCommits, Override{}, sqOne, "H1 headline\nb1 body", "the commit messages", true},
		{"title unknown + COMMIT_MESSAGES, many", "", MessageCommits, Override{}, sqMany, sqBothMsgs, "the commit messages", false},

		// unknown: every commit message (PR_BODY's text is read anyway, BLANK adds nothing)
		{"unknown, one", TitleCommitOrPR, "", Override{}, sqOne, "H1 headline\nb1 body", "the commit messages", true},
		{"unknown, many", TitleCommitOrPR, "", Override{}, sqMany, sqBothMsgs, "the commit messages", true},
		{"unknown, one, forwarded", TitleCommitOrPR, "", sqFwdBody, sqOne, "forwarded body", "the forwarded --body", false},
		{"unknown, many, forwarded", TitleCommitOrPR, "", sqFwdBody, sqMany, "forwarded body", "the forwarded --body", false},
		{"unknown, commits unread: nothing to scan", TitleCommitOrPR, "", Override{}, nil, "", "the commit messages", false},
		{"unknown: a value GitHub never documented", TitleCommitOrPR, "SQUASH_LOG", Override{}, sqMany, sqBothMsgs, "the commit messages", true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := ShippedSquash(SquashSettings{Title: tc.title, Message: tc.message}, sqTitle, sqBody, tc.commits, sqFwdSubj, tc.body)
			if got.Body != tc.want || got.BodyFrom != tc.from || got.Unsure != tc.unsure {
				t.Errorf("body = %q from %q (unsure %v), want %q from %q (unsure %v)",
					got.Body, got.BodyFrom, got.Unsure, tc.want, tc.from, tc.unsure)
			}
		})
	}
}

// TestShippedSquash_halvesAreIndependent: GitHub writes each half it is not
// handed from the settings alone, so a forwarded body never changes the
// subject, nor a forwarded subject the body — over every combination.
func TestShippedSquash_halvesAreIndependent(t *testing.T) {
	titles := []string{TitlePR, TitleCommitOrPR, ""}
	messages := []string{MessagePRBody, MessageCommits, MessageBlank, ""}
	subjects := []Override{{}, sqFwdSubj}
	bodies := []Override{{}, sqFwdBody}
	shapes := [][]Commit{nil, sqOne, sqMany, sqOneMerge, sqMany3}
	for _, ti := range titles {
		for _, me := range messages {
			for si, sub := range subjects {
				for bi, bod := range bodies {
					for ci, cs := range shapes {
						got := ShippedSquash(SquashSettings{Title: ti, Message: me}, sqTitle, sqBody, cs, sub, bod)
						name := fmt.Sprintf("title %q message %q subject#%d body#%d shape#%d", ti, me, si, bi, ci)
						for _, other := range bodies {
							for _, om := range messages {
								if s := ShippedSquash(SquashSettings{Title: ti, Message: om}, sqTitle, sqBody, cs, sub, other); s.Subject != got.Subject {
									t.Errorf("%s: the subject moved with the body's inputs: %q vs %q", name, s.Subject, got.Subject)
								}
							}
						}
						for _, other := range subjects {
							if b := ShippedSquash(SquashSettings{Title: ti, Message: me}, sqTitle, sqBody, cs, other, bod); b.Body != got.Body {
								t.Errorf("%s: the body moved with the subject override: %q vs %q", name, b.Body, got.Body)
							}
						}
						if sub.Set && got.Subject != sub.Text {
							t.Errorf("%s: a forwarded subject must ship as is, got %q", name, got.Subject)
						}
						if bod.Set && got.Body != bod.Text {
							t.Errorf("%s: a forwarded body must ship as is, got %q", name, got.Body)
						}
					}
				}
			}
		}
	}
}
