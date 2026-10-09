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
	issuePRBody     = "Refs #7"
	issueHeadline   = "feat: add the widget"
	issueCommitText = issueHeadline + "\nCloses #7"
)

// forwarded builds the squashBody a parsed `-b <text>` would produce.
func forwarded(text string) squashBody {
	return squashBody{src: merge.ForwardedBody{Source: merge.BodyText, Value: text, Flag: "-b"}, text: text}
}

func TestCloseCheckTexts(t *testing.T) {
	// No forwarded body: exactly the pre-#180 text, for both gate and watch.
	gate, watch := closeCheckTexts(issuePRBody, issueCommitText, []string{issueHeadline}, squashBody{})
	if want := issuePRBody + "\n\n" + issueCommitText; gate != want || watch != want {
		t.Fatalf("no override: gate %q watch %q, want both %q", gate, watch, want)
	}

	// Forwarded: the PR body, the headline and the forwarded text are judged; the
	// commit BODY is not, but the verify still watches it.
	gate, watch = closeCheckTexts(issuePRBody, issueCommitText, []string{issueHeadline}, forwarded("Squash body."))
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
}

// TestCloseCheck_forwardedBodyDecidesTheGate runs the gate's own decisions
// (merge.ExtraClosings / merge.SuspectClosings) over the text closeCheckTexts
// picks. Before #180 the gate text ignored the forwarded body, so the first
// override case refused over #7 and the "Fixes #9" case never saw #9.
func TestCloseCheck_forwardedBodyDecidesTheGate(t *testing.T) {
	cases := []struct {
		name      string
		headlines []string
		sq        squashBody
		extra     []int  // trap-2: closes not in closingIssuesReferences (empty here)
		suspect   string // "" = none, else the Suspect reason on the one ref
	}{
		{"no override: the commit body's close still gates (#77)", []string{issueHeadline}, squashBody{}, []int{7}, ""},
		{"keyword-free forwarded body: nothing ships, nothing gates", []string{issueHeadline}, forwarded("Squash body for the widget.\n\nRefs #7"), nil, ""},
		{"forwarded body that closes another issue gates on THAT issue", []string{issueHeadline}, forwarded("Squash body.\n\nFixes #9"), []int{9}, ""},
		{"#165 runs on the forwarded body", []string{issueHeadline}, forwarded("This does NOT close #7."), []int{7}, merge.SuspectNegated},
		{"an empty forwarded body (--body=) closes nothing", []string{issueHeadline}, forwarded(""), nil, ""},
		{"a close in a commit HEADLINE still ships as the squash subject", []string{"Fix #12 crash on start"}, forwarded("Squash body."), []int{12}, ""},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			gate, _ := closeCheckTexts(issuePRBody, issueCommitText, tc.headlines, tc.sq)
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

func TestReplacedClosings(t *testing.T) {
	label := func(refs []merge.ClosingRef) []string {
		var out []string
		for _, r := range refs {
			out = append(out, refLabel(r))
		}
		return out
	}
	commit := "feat: x\nCloses #7\nFixes owner/repo#3"
	gate, _ := closeCheckTexts(issuePRBody, commit, []string{"feat: x"}, forwarded("plain"))
	if got, want := label(replacedClosings(commit, gate, nil)), []string{"#7", "owner/repo#3"}; !reflect.DeepEqual(got, want) {
		t.Errorf("replacedClosings = %v, want %v", got, want)
	}
	// A close the forwarded body repeats is not "replaced": it still ships.
	gate, _ = closeCheckTexts(issuePRBody, commit, []string{"feat: x"}, forwarded("Closes #7"))
	if got, want := label(replacedClosings(commit, gate, nil)), []string{"owner/repo#3"}; !reflect.DeepEqual(got, want) {
		t.Errorf("replacedClosings = %v, want %v", got, want)
	}
	// Nor is one the PR closes anyway: an issue linked in GitHub's sidebar is in
	// closingIssuesReferences with no keyword anywhere, so saying the forwarded
	// body "leaves it out" would be a false all-clear.
	gate, _ = closeCheckTexts(issuePRBody, commit, []string{"feat: x"}, forwarded("plain"))
	if got, want := label(replacedClosings(commit, gate, []int{7})), []string{"owner/repo#3"}; !reflect.DeepEqual(got, want) {
		t.Errorf("replacedClosings with #7 in closingIssuesReferences = %v, want %v", got, want)
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
