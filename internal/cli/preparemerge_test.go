package cli

import (
	"io"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"github.com/eharriett0/wt/internal/merge"
)

// fakeReads answers the close check's gh reads for the issueCommitText PR: its
// one commit's body says "Closes #7", the PR body says "Refs #7", and the PR
// closes nothing (#180). PATH is emptied so a read that bypassed closeReads
// would fail instead of reaching the real gh.
func fakeReads(t *testing.T) closeReads {
	t.Helper()
	t.Setenv("PATH", t.TempDir())
	return closeReads{
		prBody:      func(string) (string, error) { return issuePRBody, nil },
		commitText:  func(string) string { return issueCommitText },
		headlines:   func(string) []string { return []string{issueHeadline} },
		closingRefs: func(string) []int { return nil },
		issueState:  func(string) (string, error) { return "OPEN", nil },
		issueTitle:  func(string) (string, error) { return "the issue", nil },
	}
}

// noReads fails the test if the close check reads anything from gh.
func noReads(t *testing.T) closeReads {
	t.Helper()
	t.Setenv("PATH", t.TempDir())
	fail := func() { t.Errorf("the close check read gh, but it must not run here") }
	return closeReads{
		prBody:      func(string) (string, error) { fail(); return "", nil },
		commitText:  func(string) string { fail(); return "" },
		headlines:   func(string) []string { fail(); return nil },
		closingRefs: func(string) []int { fail(); return nil },
		issueState:  func(string) (string, error) { fail(); return "", nil },
		issueTitle:  func(string) (string, error) { fail(); return "", nil },
	}
}

// ghStdin reads what gh would get on stdin; "<nil>" = wt's own stdin.
func ghStdin(t *testing.T, r io.Reader) string {
	t.Helper()
	if r == nil {
		return "<nil>"
	}
	b, err := io.ReadAll(r)
	if err != nil {
		t.Fatal(err)
	}
	return string(b)
}

var realMerge = closeOpts{}

// TestPrepareMerge_forwardedBodyReachesGhAndTheGate pins the #180 wiring end to
// end, short of exec: the body wt reads is the body the gate judges AND the
// bytes gh gets. Before #180 the gate read the commit body's "Closes #7" and
// refused this merge; and if the read body were not handed to gh, gh would
// read wt's already-drained stdin and merge an EMPTY body.
func TestPrepareMerge_forwardedBodyReachesGhAndTheGate(t *testing.T) {
	const body = "Squash body for the widget.\n\nRefs #7"
	file := filepath.Join(t.TempDir(), "body.txt")
	if err := os.WriteFile(file, []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}
	cases := []struct {
		name        string
		passthrough []string
		stdin       string
		wantArgs    []string
		wantUnread  string // what is left of wt's own stdin afterwards
	}{
		{"-F - (stdin)", []string{"-F", "-"}, body, []string{"-F", "-"}, ""},
		{"--body-file path", []string{"--delete-branch", "--body-file", file}, "not the body", []string{"--delete-branch", "--body-file", "-"}, "not the body"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			stdin := strings.NewReader(tc.stdin)
			p, ok := prepareMerge("5", tc.passthrough, realMerge, stdin, false, fakeReads(t))
			if !ok {
				t.Fatal("refused a forwarded body that closes nothing (the pre-#180 gate)")
			}
			if !reflect.DeepEqual(p.args, tc.wantArgs) {
				t.Errorf("gh args = %q, want %q", p.args, tc.wantArgs)
			}
			if got := ghStdin(t, p.stdin); got != body {
				t.Errorf("gh stdin = %q, want the forwarded body %q", got, body)
			}
			if rest, _ := io.ReadAll(stdin); string(rest) != tc.wantUnread {
				t.Errorf("wt's own stdin left = %q, want %q", rest, tc.wantUnread)
			}
			if len(p.plan.extra) != 0 || p.plan.forwarded == "" {
				t.Errorf("plan = %+v, want no trap-2 closes and a forwarded source", p.plan)
			}
			if len(p.plan.replaced) != 1 || p.plan.replaced[0].Number != 7 {
				t.Errorf("replaced = %+v, want the commit body's #7", p.plan.replaced)
			}
			if !reflect.DeepEqual(p.plan.watch, []int{7}) {
				t.Errorf("watch = %v, want [7]: the verify still re-reads a replaced close", p.plan.watch)
			}
		})
	}
}

// TestPrepareMerge_forwardedCloseGates: a close keyword in the forwarded body
// is what ships, so it gates (and --close-ok lets it through).
func TestPrepareMerge_forwardedCloseGates(t *testing.T) {
	args := []string{"--body", "Squash body.\n\nFixes #9"}
	if _, ok := prepareMerge("5", args, realMerge, strings.NewReader(""), false, fakeReads(t)); ok {
		t.Error("merged a forwarded body that closes #9, which the PR does not declare")
	}
	p, ok := prepareMerge("5", args, closeOpts{closeOK: true}, strings.NewReader(""), false, fakeReads(t))
	if !ok || !reflect.DeepEqual(p.plan.extra, []int{9}) {
		t.Errorf("--close-ok: ok=%v extra=%v, want ok and [9]", ok, p.plan.extra)
	}
	if !reflect.DeepEqual(p.args, args) || p.stdin != nil {
		t.Errorf("an inline body must reach gh as given: args=%q stdin=%v", p.args, p.stdin)
	}
}

// TestPrepareMerge_headlineCloseStillGates: a forwarded body replaces the
// commit BODIES only. The squash subject can still be a commit's headline, so
// a close keyword there still gates (#180).
func TestPrepareMerge_headlineCloseStillGates(t *testing.T) {
	r := fakeReads(t)
	r.commitText = func(string) string { return "Fix #12 crash on start\nplain body" }
	r.headlines = func(string) []string { return []string{"Fix #12 crash on start"} }
	p, ok := prepareMerge("5", []string{"-b", "Squash body."}, realMerge, strings.NewReader(""), false, r)
	if ok || !reflect.DeepEqual(p.plan.extra, []int{12}) {
		t.Errorf("ok=%v extra=%v, want a refusal over the headline's #12", ok, p.plan.extra)
	}
}

// TestPrepareMerge_noOverride: with no forwarded body nothing is read or
// re-pointed, and the commit body's close still gates (#77).
func TestPrepareMerge_noOverride(t *testing.T) {
	stdin := strings.NewReader("untouched")
	p, ok := prepareMerge("5", []string{"--delete-branch"}, realMerge, stdin, false, fakeReads(t))
	if ok {
		t.Error("the commit body's undeclared close of #7 must still gate")
	}
	if !reflect.DeepEqual(p.args, []string{"--delete-branch"}) || p.stdin != nil {
		t.Errorf("args=%q stdin=%v, want the passthrough as given and wt's own stdin", p.args, p.stdin)
	}
	if rest, _ := io.ReadAll(stdin); string(rest) != "untouched" {
		t.Errorf("stdin was read: %q left", rest)
	}
	if _, ok := prepareMerge("5", nil, closeOpts{closeOK: true}, stdin, false, fakeReads(t)); !ok {
		t.Error("--close-ok must let the commit-body close through")
	}
}

// TestPrepareMerge_refusesWhenNoBodyCanBeNamed: an unreadable file, --body with
// --body-file, or a dangling value flag is refused EVEN WITH --close-ok — gh
// would fail on it, so there is no body to judge — before gh is read. A dry
// run only warns and hands gh the passthrough unchanged.
func TestPrepareMerge_refusesWhenNoBodyCanBeNamed(t *testing.T) {
	for _, args := range [][]string{
		{"-F", filepath.Join(t.TempDir(), "missing.txt")},
		{"-b", "x", "-F", "f.txt"},
		{"-b"},
		{"--subject"},
	} {
		if _, ok := prepareMerge("5", args, closeOpts{closeOK: true}, strings.NewReader(""), false, noReads(t)); ok {
			t.Errorf("%q: merged with --close-ok, want a refusal", args)
		}
		p, ok := prepareMerge("5", args, closeOpts{dryRun: true}, strings.NewReader(""), false, noReads(t))
		if !ok || !reflect.DeepEqual(p.args, args) || p.stdin != nil {
			t.Errorf("%q --dry-run: ok=%v args=%q stdin=%v, want a warning and the passthrough as given", args, ok, p.args, p.stdin)
		}
	}
}

// TestPrepareMerge_noCloseCheckReadsNothing: --no-close-check parses and reads
// nothing; gh gets the passthrough and wt's own stdin untouched.
func TestPrepareMerge_noCloseCheckReadsNothing(t *testing.T) {
	stdin := strings.NewReader("still here")
	args := []string{"-F", "-"}
	p, ok := prepareMerge("5", args, closeOpts{skip: true}, stdin, false, noReads(t))
	if !ok || !reflect.DeepEqual(p.args, args) || p.stdin != nil {
		t.Errorf("ok=%v args=%q stdin=%v, want the passthrough untouched", ok, p.args, p.stdin)
	}
	if rest, _ := io.ReadAll(stdin); string(rest) != "still here" {
		t.Errorf("stdin was read: %q left", rest)
	}
}

func TestEmptyStdinBody(t *testing.T) {
	stdinBody := func(text string) squashBody {
		return squashBody{src: merge.ForwardedBody{Source: merge.BodyStdin, Value: "-", Flag: "-F"}, text: text}
	}
	cases := []struct {
		name string
		sq   squashBody
		tty  bool
		want bool
	}{
		{"nothing piped in", stdinBody(""), false, true},
		{"only a newline piped in", stdinBody("\n"), false, true},
		{"a body piped in", stdinBody("Squash body."), false, false},
		{"Ctrl-D on a terminal is a choice, not an accident", stdinBody(""), true, false},
		{"an empty FILE is not stdin", squashBody{src: merge.ForwardedBody{Source: merge.BodyFile, Value: "f", Flag: "-F"}}, false, false},
		{"--body '' is explicit", forwarded(""), false, false},
		{"no forwarded body", squashBody{}, false, false},
	}
	for _, tc := range cases {
		if got := emptyStdinBody(tc.sq, tc.tty); got != tc.want {
			t.Errorf("%s: emptyStdinBody = %v, want %v", tc.name, got, tc.want)
		}
	}
}

// TestStdinIsTTY: /dev/null (an agent's or CI's stdin, and what Go opens for a
// closed fd 0) and a file are not terminals, though /dev/null is a character
// device (#180). A real terminal cannot be faked here.
func TestStdinIsTTY(t *testing.T) {
	file := filepath.Join(t.TempDir(), "in")
	if err := os.WriteFile(file, []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}
	for _, path := range []string{os.DevNull, file} {
		in, err := os.Open(path)
		if err != nil {
			t.Fatal(err)
		}
		old := os.Stdin
		os.Stdin = in
		got := stdinIsTTY()
		os.Stdin = old
		in.Close()
		if got {
			t.Errorf("stdinIsTTY() with stdin = %s: true, want false", path)
		}
	}
}
