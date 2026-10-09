package merge

import "strings"

// The values of a repository's squash-merge default message settings, as
// GitHub names them (REST squash_merge_commit_title / _message, GraphQL
// squashMergeCommitTitle / squashMergeCommitMessage).
const (
	TitlePR         = "PR_TITLE"
	TitleCommitOrPR = "COMMIT_OR_PR_TITLE"

	MessagePRBody  = "PR_BODY"
	MessageCommits = "COMMIT_MESSAGES"
	MessageBlank   = "BLANK"
)

// SquashSettings is a repository's default squash commit message: what GitHub
// writes for each half that gh does not send (#196). A field that is not one
// of GitHub's values ("" when it could not be read) is unknown, and
// ShippedSquash over-scans it.
type SquashSettings struct {
	Title   string // TitlePR | TitleCommitOrPR
	Message string // MessagePRBody | MessageCommits | MessageBlank
}

// Commit is one of a PR's commits, as GitHub's default squash message uses it.
type Commit struct {
	Headline string // the message's first line, whole
	Body     string // the rest of the message
	Merge    bool   // more than one parent
}

// NewCommit splits a commit's whole message the way GitHub's default squash
// message does (its first line is the title, the rest its body) and marks a
// merge commit by its parents. Pure.
func NewCommit(message string, parents int) Commit {
	head, body, _ := strings.Cut(strings.TrimSpace(message), "\n")
	return Commit{Headline: strings.TrimSpace(head), Body: strings.TrimLeft(body, "\n"), Merge: parents > 1}
}

// Messages joins commits' whole messages, headline and body, for a scan. Pure.
func Messages(commits []Commit) string {
	parts := make([]string, len(commits))
	for i, c := range commits {
		parts[i] = c.Headline + "\n" + c.Body
	}
	return strings.Join(parts, "\n\n")
}

// Override is a squash subject or body handed to gh, which GitHub writes in
// place of the repo's default (#180, #196). Set false: gh sends none.
type Override struct {
	Set  bool
	Text string
	From string // where it came from, for messages
}

// SubjectOverride is the squash subject gh is handed for merge-pr's passthrough
// and the PR's title (#196). wt's own #38 WIP strip goes in FRONT of the
// passthrough (WithSubject), so a forwarded --subject/-t wins: gh keeps the
// last one. gh sends a subject only when it is non-empty, so a forwarded
// `--subject ""` brings back the repo's default, the PR title "WIP:" and all.
// Pure; errors as ParseForwardedSubject does.
func SubjectOverride(title string, passthrough []string) (Override, error) {
	fs, err := ParseForwardedSubject(passthrough)
	switch {
	case err != nil:
		return Override{}, err
	case fs.Given:
		return Override{Set: fs.Value != "", Text: fs.Value, From: "the forwarded " + fs.Flag}, nil
	}
	if s, ok := WIPSubject(title); ok {
		return Override{Set: true, Text: s, From: `the PR title without "WIP:"`}, nil
	}
	return Override{}, nil
}

// SquashText is the squash commit's message as GitHub will write it (#196), and
// where each half comes from.
type SquashText struct {
	Subject, SubjectFrom string
	Body, BodyFrom       string
	// Unsure: a setting is unknown, so the subject or the body holds every text
	// it could be.
	Unsure bool
}

// ShippedSquash is what a squash merge's commit says (#196): the subject and
// the body it ships with. Pure.
//
// The subject is the one gh is handed (subject: a forwarded --subject/-t, or
// the WIP strip), else the repo's squash_merge_commit_title: PR_TITLE is the PR
// title; COMMIT_OR_PR_TITLE is the commit's headline when the PR has exactly one
// commit, else the PR title. The body is the one gh is handed (body: -b/-F),
// else squash_merge_commit_message: PR_BODY is the PR body, BLANK is empty, and
// COMMIT_MESSAGES is every commit's message, except that for ONE commit it is
// the commit's body alone unless its headline is not already the default
// subject (PR_TITLE with a headline other than the PR title).
//
// Measured against GitHub's own default text (GraphQL viewerMergeHeadlineText
// and viewerMergeBodyText, mergeType SQUASH) on 600 merged PRs across all four
// setting pairs: the subject matched every time, and the body up to formatting
// (bullets, wrapping, and the trailers GitHub gathers at the end). The headline
// is the whole first line (GraphQL's messageHeadline cuts it at 69 characters),
// and merge commits are neither counted nor listed.
//
// A setting that is unknown is over-scanned: a missed close is the #77 trap,
// a false refusal costs a --close-ok. The subject is then the PR title AND, for
// one commit, its headline (with more, both settings give the PR title); the
// body is every commit's message (PR_BODY's text is the PR body, which the close
// check reads anyway, and BLANK adds nothing). commits empty means they could
// not be read, so no commit text is scanned and the subject is the PR title.
func ShippedSquash(s SquashSettings, prTitle, prBody string, commits []Commit, subject, body Override) SquashText {
	var own []Commit
	for _, c := range commits {
		if !c.Merge {
			own = append(own, c)
		}
	}
	one := len(own) == 1
	titleKnown := s.Title == TitlePR || s.Title == TitleCommitOrPR
	var t SquashText
	switch {
	case subject.Set:
		t.Subject, t.SubjectFrom = subject.Text, subject.From
	case s.Title == TitlePR, s.Title == TitleCommitOrPR && !one:
		t.Subject, t.SubjectFrom = prTitle, "the PR title"
	case s.Title == TitleCommitOrPR:
		t.Subject, t.SubjectFrom = own[0].Headline, "the commit's headline"
	case one:
		t.Subject, t.SubjectFrom = prTitle+"\n\n"+own[0].Headline, "the PR title or the commit's headline"
		t.Unsure = true
	default:
		t.Subject, t.SubjectFrom = prTitle, "the PR title"
	}
	switch {
	case body.Set:
		t.Body, t.BodyFrom = body.Text, body.From
	case s.Message == MessagePRBody:
		t.Body, t.BodyFrom = prBody, "the PR body"
	case s.Message == MessageBlank:
		t.BodyFrom = "empty"
	case s.Message == MessageCommits && one && s.Title == TitleCommitOrPR,
		s.Message == MessageCommits && one && s.Title == TitlePR && own[0].Headline == strings.TrimSpace(prTitle):
		t.Body, t.BodyFrom = own[0].Body, "the commit's body"
	case s.Message == MessageCommits:
		t.Body, t.BodyFrom = Messages(own), "the commit messages"
		t.Unsure = t.Unsure || one && !titleKnown
	default:
		t.Body, t.BodyFrom = Messages(own), "the commit messages"
		t.Unsure = t.Unsure || len(own) > 0
	}
	return t
}
