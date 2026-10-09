package cli

import (
	"errors"
	"io"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"github.com/eharriett0/wt/internal/merge"
)

// The #180 shape: one commit whose BODY says "Closes #7", a PR body that only
// says "Refs #7" (so closingIssuesReferences is empty), squashed with a body the
// operator forwards to gh.
const (
	issuePRTitle    = "Add the widget"
	issuePRBody     = "Refs #7"
	issueHeadline   = "feat: add the widget"
	issueCommitText = issueHeadline + "\nCloses #7"
)

// defaultSquash is GitHub's "Default message" squash setting (#196).
var defaultSquash = merge.SquashSettings{Title: merge.TitleCommitOrPR, Message: merge.MessageCommits}

// forwarded builds the squashBody a parsed `-b <text>` would produce.
func forwarded(text string) squashBody {
	return squashBody{src: merge.ForwardedBody{Source: merge.BodyText, Value: text, Flag: "-b"}, text: text}
}

// gateTexts is closeCheckTexts for a PR with the given title, body and commits
// in a default-squash repo, with sq forwarded and no forwarded subject.
func gateTexts(title, body string, commits []merge.Commit, sq squashBody) (gate, watch string) {
	ship := merge.ShippedSquash(defaultSquash, title, body, commits, merge.Override{}, sq.override())
	return closeCheckTexts(body, title, commits, ship)
}

func TestCloseCheckTexts(t *testing.T) {
	one := []merge.Commit{merge.NewCommit(issueCommitText, 1)}
	// No forwarded body, one commit: the subject is its headline and the body
	// its body, both judged (#77).
	gate, watch := gateTexts(issuePRTitle, issuePRBody, one, squashBody{})
	for _, part := range []string{issuePRBody, issueHeadline, "Closes #7"} {
		if !strings.Contains(gate, part) {
			t.Errorf("gate %q is missing %q", gate, part)
		}
	}
	if strings.Contains(gate, issuePRTitle) {
		t.Errorf("gate %q reads the PR title, which a one-commit squash does not ship (#196)", gate)
	}
	if !strings.Contains(watch, issuePRTitle) {
		t.Errorf("watch %q dropped the PR title — the verify watches everything that could ship", watch)
	}

	// Forwarded: the PR body, the headline (still the subject) and the
	// forwarded text are judged; the commit BODY is not, but the verify still
	// watches it.
	gate, watch = gateTexts(issuePRTitle, issuePRBody, one, forwarded("Squash body."))
	for _, part := range []string{issuePRBody, issueHeadline, "Squash body."} {
		if !strings.Contains(gate, part) {
			t.Errorf("forwarded gate %q is missing %q", gate, part)
		}
	}
	if strings.Contains(gate, "Closes #7") {
		t.Errorf("forwarded gate %q still reads the replaced commit body", gate)
	}
	if !strings.Contains(watch, "Closes #7") {
		t.Errorf("forwarded watch %q dropped the replaced commit body — the post-merge verify must keep watching it", watch)
	}

	// Two commits and a merge commit: the subject is the PR title, the body
	// every commit message but the merge's; the verify watches the merge's too.
	two := []merge.Commit{merge.NewCommit("feat: one\n\nFixes #3", 1), merge.NewCommit("Merge branch 'main', closes #4", 2), merge.NewCommit("feat: two", 1)}
	gate, watch = gateTexts("Fixes #5 the thing", issuePRBody, two, squashBody{})
	for _, part := range []string{"Fixes #5 the thing", "Fixes #3", "feat: two"} {
		if !strings.Contains(gate, part) {
			t.Errorf("multi-commit gate %q is missing %q", gate, part)
		}
	}
	if strings.Contains(gate, "closes #4") {
		t.Errorf("multi-commit gate %q reads the merge commit, which a squash does not list", gate)
	}
	if !strings.Contains(watch, "closes #4") {
		t.Errorf("multi-commit watch %q dropped the merge commit", watch)
	}
}

// TestCloseCheck_forwardedBodyDecidesTheGate runs the gate's own decisions
// (merge.ExtraClosings / merge.SuspectClosings) over the text closeCheckTexts
// picks. Before #180 the gate text ignored the forwarded body, so the first
// override case refused over #7 and the "Fixes #9" case never saw #9.
func TestCloseCheck_forwardedBodyDecidesTheGate(t *testing.T) {
	one := func(msg string) []merge.Commit { return []merge.Commit{merge.NewCommit(msg, 1)} }
	cases := []struct {
		name    string
		commits []merge.Commit
		sq      squashBody
		extra   []int  // trap-2: closes not in closingIssuesReferences (empty here)
		suspect string // "" = none, else the Suspect reason on the one ref
	}{
		{"no override: the commit body's close still gates (#77)", one(issueCommitText), squashBody{}, []int{7}, ""},
		{"keyword-free forwarded body: nothing ships, nothing gates", one(issueCommitText), forwarded("Squash body for the widget.\n\nRefs #7"), nil, ""},
		{"forwarded body that closes another issue gates on THAT issue", one(issueCommitText), forwarded("Squash body.\n\nFixes #9"), []int{9}, ""},
		{"#165 runs on the forwarded body", one(issueCommitText), forwarded("This does NOT close #7."), []int{7}, merge.SuspectNegated},
		{"an empty forwarded body (--body=) closes nothing", one(issueCommitText), forwarded(""), nil, ""},
		{"a close in a one-commit HEADLINE still ships as the squash subject", one("Fix #12 crash on start\nCloses #7"), forwarded("Squash body."), []int{12}, ""},
		{"a close in one of two headlines does not ship past a forwarded body (#196)",
			[]merge.Commit{merge.NewCommit("Fix #12 crash on start", 1), merge.NewCommit("feat: more", 1)}, forwarded("Squash body."), nil, ""},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			gate, _ := gateTexts(issuePRTitle, issuePRBody, tc.commits, tc.sq)
			if got := merge.ExtraClosings(gate, nil); !reflect.DeepEqual(got, tc.extra) {
				t.Errorf("ExtraClosings = %v, want %v (gate text %q)", got, tc.extra, gate)
			}
			sus := merge.SuspectClosings(gate)
			switch {
			case tc.suspect == "" && len(sus) != 0:
				t.Errorf("SuspectClosings = %+v, want none", sus)
			case tc.suspect != "" && (len(sus) != 1 || sus[0].Suspect != tc.suspect):
				t.Errorf("SuspectClosings = %+v, want one %q", sus, tc.suspect)
			}
		})
	}
}

func TestLeftOutClosings(t *testing.T) {
	label := func(refs []leftOutRef) []string {
		var out []string
		for _, r := range refs {
			out = append(out, refLabel(r.ref)+" in "+r.where)
		}
		return out
	}
	commits := []merge.Commit{merge.NewCommit("feat: x\nCloses #7\nFixes owner/repo#3", 1)}
	msgs := merge.Messages(commits)
	gate, watch := gateTexts(issuePRTitle, issuePRBody, commits, forwarded("plain"))
	if got, want := label(leftOutClosings(watch, gate, nil, issuePRTitle, msgs)), []string{"#7 in the commit messages", "owner/repo#3 in the commit messages"}; !reflect.DeepEqual(got, want) {
		t.Errorf("leftOutClosings = %v, want %v", got, want)
	}
	// A close the forwarded body repeats is not left out: it still ships.
	gate, watch = gateTexts(issuePRTitle, issuePRBody, commits, forwarded("Closes #7"))
	if got, want := label(leftOutClosings(watch, gate, nil, issuePRTitle, msgs)), []string{"owner/repo#3 in the commit messages"}; !reflect.DeepEqual(got, want) {
		t.Errorf("leftOutClosings = %v, want %v", got, want)
	}
	// Nor is one the PR closes anyway: an issue linked in GitHub's sidebar is in
	// closingIssuesReferences with no keyword anywhere, so saying the squash
	// "leaves it out" would be a false all-clear.
	gate, watch = gateTexts(issuePRTitle, issuePRBody, commits, forwarded("plain"))
	if got, want := label(leftOutClosings(watch, gate, []int{7}, issuePRTitle, msgs)), []string{"owner/repo#3 in the commit messages"}; !reflect.DeepEqual(got, want) {
		t.Errorf("leftOutClosings with #7 in closingIssuesReferences = %v, want %v", got, want)
	}
	// A one-commit squash ships the headline, not the PR title (#196).
	title := "Fixes #5, and see #6"
	gate, watch = gateTexts(title, issuePRBody, commits, forwarded("plain"))
	if got, want := label(leftOutClosings(watch, gate, nil, title, msgs)), []string{"#5 in the PR title", "#7 in the commit messages", "owner/repo#3 in the commit messages"}; !reflect.DeepEqual(got, want) {
		t.Errorf("leftOutClosings with a closing PR title = %v, want %v", got, want)
	}
}

type failingReader struct{}

func (failingReader) Read([]byte) (int, error) { return 0, errors.New("stdin went away") }

func TestReadSquashBody(t *testing.T) {
	body := "Squash body: `code`, $HOME and #9 survive.\n\nRefs #7\n"
	dir := t.TempDir()
	path := filepath.Join(dir, "body #1.txt")
	if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
	parse := func(args ...string) merge.ForwardedBody {
		t.Helper()
		src, err := merge.ParseForwardedBody(args)
		if err != nil {
			t.Fatalf("ParseForwardedBody(%q): %v", args, err)
		}
		return src
	}
	ghReads := func(r io.Reader) string {
		t.Helper()
		if r == nil {
			return "<wt's own stdin>"
		}
		b, err := io.ReadAll(r)
		if err != nil {
			t.Fatal(err)
		}
		return string(b)
	}

	// No override: nothing read, gh keeps wt's stdin and the args.
	args := []string{"--delete-branch"}
	sq, err := readSquashBody(parse(args...), args, failingReader{})
	if err != nil || sq.forwarded() || !reflect.DeepEqual(sq.ghArgs, args) || sq.ghStdin != nil {
		t.Fatalf("default: %+v err %v", sq, err)
	}

	// Inline text: the args already carry it; nothing to read or re-point.
	args = []string{"-b", body}
	sq, err = readSquashBody(parse(args...), args, failingReader{})
	if err != nil || sq.text != body || !reflect.DeepEqual(sq.ghArgs, args) || sq.ghStdin != nil {
		t.Fatalf("text: %+v err %v", sq, err)
	}

	// A file: read once, byte-exact; gh is re-pointed at stdin and fed the SAME
	// bytes (a pipe or process substitution cannot be read a second time).
	args = []string{"--admin", "--body-file", path}
	sq, err = readSquashBody(parse(args...), args, failingReader{})
	if err != nil {
		t.Fatalf("file: %v", err)
	}
	if sq.text != body {
		t.Errorf("file text = %q, want %q", sq.text, body)
	}
	if want := []string{"--admin", "--body-file", "-"}; !reflect.DeepEqual(sq.ghArgs, want) {
		t.Errorf("file ghArgs = %q, want %q", sq.ghArgs, want)
	}
	if got := ghReads(sq.ghStdin); got != body {
		t.Errorf("gh would read %q, want %q", got, body)
	}

	// stdin: read once; gh keeps `-F -` and gets the bytes wt consumed.
	args = []string{"-F", "-"}
	sq, err = readSquashBody(parse(args...), args, strings.NewReader(body))
	if err != nil || sq.text != body || !reflect.DeepEqual(sq.ghArgs, args) {
		t.Fatalf("stdin: %+v err %v", sq, err)
	}
	if got := ghReads(sq.ghStdin); got != body {
		t.Errorf("gh would read %q, want %q", got, body)
	}

	// Unreadable → an error naming the source, never a silently skipped check.
	missing := filepath.Join(dir, "nope.txt")
	args = []string{"-F", missing}
	if _, err := readSquashBody(parse(args...), args, failingReader{}); err == nil || !strings.Contains(err.Error(), missing) {
		t.Errorf("missing file: err %v, want one naming %s", err, missing)
	}
	args = []string{"--body-file=-"}
	if _, err := readSquashBody(parse(args...), args, failingReader{}); err == nil {
		t.Error("failing stdin: want an error")
	}
}
