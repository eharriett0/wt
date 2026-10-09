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
// closes nothing (#180); the repo squashes with GitHub's default message. PATH
// is emptied so a read that bypassed closeReads would fail instead of reaching
// the real gh.
func fakeReads(t *testing.T) closeReads {
	t.Helper()
	return readsFor(t, issuePRTitle, issuePRBody, []merge.Commit{merge.NewCommit(issueCommitText, 1)}, defaultSquash, nil)
}

// readsFor answers the close check's gh reads for a PR with this title, body,
// commits and closingIssuesReferences (graph), in a repo with these squash
// settings. Every issue is OPEN.
func readsFor(t *testing.T, title, body string, commits []merge.Commit, s merge.SquashSettings, graph []int) closeReads {
	t.Helper()
	t.Setenv("PATH", t.TempDir())
	return closeReads{
		prBody:         func(string) (string, error) { return body, nil },
		prTitle:        func(string) (string, error) { return title, nil },
		commits:        func(string) []merge.Commit { return commits },
		squashSettings: func() merge.SquashSettings { return s },
		closingRefs:    func(string) []int { return graph },
		issueState:     func(string) (string, error) { return "OPEN", nil },
		issueTitle:     func(string) (string, error) { return "the issue", nil },
	}
}

// noReads fails the test if the close check reads anything from gh.
func noReads(t *testing.T) closeReads {
	t.Helper()
	t.Setenv("PATH", t.TempDir())
	fail := func() { t.Errorf("the close check read gh, but it must not run here") }
	return closeReads{
		prBody:         func(string) (string, error) { fail(); return "", nil },
		prTitle:        func(string) (string, error) { fail(); return "", nil },
		commits:        func(string) []merge.Commit { fail(); return nil },
		squashSettings: func() merge.SquashSettings { fail(); return merge.SquashSettings{} },
		closingRefs:    func(string) []int { fail(); return nil },
		issueState:     func(string) (string, error) { fail(); return "", nil },
		issueTitle:     func(string) (string, error) { fail(); return "", nil },
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
			if len(p.plan.leftOut) != 1 || p.plan.leftOut[0].ref.Number != 7 || p.plan.leftOut[0].where != "the commit messages" {
				t.Errorf("leftOut = %+v, want the commit body's #7", p.plan.leftOut)
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
// commit BODIES only. A one-commit PR's squash subject is that commit's
// headline, so a close keyword there still gates (#180, #196).
func TestPrepareMerge_headlineCloseStillGates(t *testing.T) {
	r := fakeReads(t)
	r.commits = func(string) []merge.Commit {
		return []merge.Commit{merge.NewCommit("Fix #12 crash on start\nplain body", 1)}
	}
	p, ok := prepareMerge("5", []string{"-b", "Squash body."}, realMerge, strings.NewReader(""), false, r)
	if ok || !reflect.DeepEqual(p.plan.extra, []int{12}) || !reflect.DeepEqual(p.plan.subjExt, []int{12}) {
		t.Errorf("ok=%v extra=%v subjExt=%v, want a refusal over the headline's #12", ok, p.plan.extra, p.plan.subjExt)
	}
}

// TestPrepareMerge_judgesTheShippedSquash drives the whole close check over
// the squash commit GitHub will write (#196): its SUBJECT (a forwarded
// --subject/-t, the WIP strip's, else the PR title or a one-commit PR's
// headline) and its body, per the repo's squash settings. Before #196 the
// subject was never read: a closing PR title merged silently (missed close),
// and a headline that no longer shipped still gated (false refusal).
func TestPrepareMerge_judgesTheShippedSquash(t *testing.T) {
	c := func(msg string) merge.Commit { return merge.NewCommit(msg, 1) }
	two := []merge.Commit{c("feat: one"), c("feat: two")}
	flakeOfTwo := []merge.Commit{c("Fix #6 flake\n\nthe fix"), c("feat: other")}
	unknown := merge.SquashSettings{}
	prTitleSq := merge.SquashSettings{Title: merge.TitlePR, Message: merge.MessageCommits}
	cases := []struct {
		name     string
		title    string
		body     string
		commits  []merge.Commit
		settings merge.SquashSettings
		graph    []int
		pass     []string
		closeOK  bool

		ok      bool
		extra   []int    // trap-2 closes
		subjExt []int    // the part of extra the subject carries
		suspect string   // the one suspect reason, "" = none
		leftOut []string // "#N in <where>"
		watch   []int
		unsure  bool
	}{
		{name: "missed close: a closing PR title with two commits ships as the subject",
			title: "Fixes #5 the thing", commits: two, settings: defaultSquash,
			extra: []int{5}, subjExt: []int{5}, watch: []int{5}},
		{name: "the same, declared in the PR body: listed and watched, not gated",
			title: "Fixes #5 the thing", body: "Fixes #5", commits: two, settings: defaultSquash, graph: []int{5},
			ok: true, watch: []int{5}},
		{name: "a closing PR title with ONE commit does not ship: the headline does",
			title: "Fixes #5 the thing", commits: []merge.Commit{c("feat: one")}, settings: defaultSquash,
			ok: true, leftOut: []string{"#5 in the PR title"}, watch: []int{5}},
		{name: "PR_TITLE: the closing title ships even for one commit",
			title: "Fixes #5 the thing", commits: []merge.Commit{c("feat: one")}, settings: prTitleSq,
			extra: []int{5}, subjExt: []int{5}, watch: []int{5}},
		{name: "false refusal: a closing headline (1 of 2) under a forwarded subject and body",
			title: "Two changes", commits: flakeOfTwo, settings: defaultSquash, pass: []string{"--subject", "clean", "--body", "clean"},
			ok: true, leftOut: []string{"#6 in the commit messages"}, watch: []int{6}},
		{name: "a forwarded subject alone: the default body still lists the headline",
			title: "Two changes", commits: flakeOfTwo, settings: defaultSquash, pass: []string{"--subject", "clean"},
			extra: []int{6}, watch: []int{6}},
		{name: "a forwarded subject alone, one commit: the default body is its body only",
			title: "Clean", commits: []merge.Commit{c("Fix #6 flake\n\nplain body")}, settings: defaultSquash, pass: []string{"-t", "clean"},
			ok: true, leftOut: []string{"#6 in the commit messages"}, watch: []int{6}},
		{name: "a forwarded --subject that closes is judged and watched",
			title: "Clean title", commits: two, settings: defaultSquash, pass: []string{"--subject", "Fixes #8 via subject"},
			extra: []int{8}, subjExt: []int{8}, watch: []int{8}},
		{name: "--close-ok lets it through, still watched",
			title: "Clean title", commits: two, settings: defaultSquash, pass: []string{"--subject", "Fixes #8 via subject"}, closeOK: true,
			ok: true, extra: []int{8}, subjExt: []int{8}, watch: []int{8}},
		{name: "the WIP strip forwards the title without WIP:, closes and all",
			title: "WIP: Fixes #5 the thing", commits: []merge.Commit{c("feat: one")}, settings: defaultSquash,
			extra: []int{5}, subjExt: []int{5}, watch: []int{5}},
		{name: "an operator's --subject beats the WIP strip",
			title: "WIP: Fixes #5 the thing", commits: []merge.Commit{c("feat: one")}, settings: defaultSquash, pass: []string{"--subject", "clean"},
			ok: true, leftOut: []string{"#5 in the PR title"}, watch: []int{5}},
		{name: "a negated close in the subject is suspect (#165)",
			title: "This does not fix #5", commits: two, settings: defaultSquash,
			extra: []int{5}, subjExt: []int{5}, suspect: merge.SuspectNegated, watch: []int{5}},
		{name: "settings unreadable, one commit: the PR title AND the headline (over-scan)",
			title: "Fixes #5 the thing", commits: []merge.Commit{c("feat: one")}, settings: unknown,
			extra: []int{5}, subjExt: []int{5}, watch: []int{5}, unsure: true},
		{name: "settings unreadable, a forwarded subject and body: nothing is guessed",
			title: "Fixes #5 the thing", commits: []merge.Commit{c("feat: one\n\nCloses #7")}, settings: unknown, pass: []string{"-t", "clean", "-b", "clean"},
			ok: true, leftOut: []string{"#5 in the PR title", "#7 in the commit messages"}, watch: []int{5, 7}},
		{name: "PR_BODY: a commit body's close does not ship",
			title: "Clean", commits: []merge.Commit{c(issueCommitText)}, settings: merge.SquashSettings{Title: merge.TitlePR, Message: merge.MessagePRBody},
			ok: true, leftOut: []string{"#7 in the commit messages"}, watch: []int{7}},
		{name: "BLANK: neither does it here",
			title: "Clean", commits: []merge.Commit{c(issueCommitText)}, settings: merge.SquashSettings{Title: merge.TitlePR, Message: merge.MessageBlank},
			ok: true, leftOut: []string{"#7 in the commit messages"}, watch: []int{7}},
		{name: "a merge commit is not counted: the one real commit's headline is the subject",
			title: "Clean", commits: []merge.Commit{c("Fixes #4 in the only commit"), merge.NewCommit("Merge branch 'main' into feat", 2)},
			settings: defaultSquash, pass: []string{"--body", "clean"},
			extra: []int{4}, subjExt: []int{4}, watch: []int{4}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			r := readsFor(t, tc.title, tc.body, tc.commits, tc.settings, tc.graph)
			p, ok := prepareMerge("5", tc.pass, closeOpts{closeOK: tc.closeOK}, strings.NewReader(""), false, r)
			if ok != tc.ok {
				t.Errorf("ok = %v, want %v", ok, tc.ok)
			}
			if !reflect.DeepEqual(p.plan.extra, tc.extra) || !reflect.DeepEqual(p.plan.subjExt, tc.subjExt) {
				t.Errorf("extra = %v subjExt = %v, want %v and %v", p.plan.extra, p.plan.subjExt, tc.extra, tc.subjExt)
			}
			var sus string
			if len(p.plan.suspect) == 1 {
				sus = p.plan.suspect[0].Suspect
			} else if len(p.plan.suspect) > 1 {
				sus = "several"
			}
			if sus != tc.suspect {
				t.Errorf("suspect = %+v, want %q", p.plan.suspect, tc.suspect)
			}
			var left []string
			for _, l := range p.plan.leftOut {
				left = append(left, refLabel(l.ref)+" in "+l.where)
			}
			if !reflect.DeepEqual(left, tc.leftOut) {
				t.Errorf("leftOut = %q, want %q", left, tc.leftOut)
			}
			if !reflect.DeepEqual(p.plan.watch, tc.watch) {
				t.Errorf("watch = %v, want %v", p.plan.watch, tc.watch)
			}
			if p.plan.ship.Unsure != tc.unsure {
				t.Errorf("unsure = %v, want %v (ship %+v)", p.plan.ship.Unsure, tc.unsure, p.plan.ship)
			}
		})
	}
}

// TestPrepareMerge_settingsOnlyForADefaultHalf: when gh is handed both the
// subject and the body, the repo's defaults decide nothing, so they are not
// read (#196).
func TestPrepareMerge_settingsOnlyForADefaultHalf(t *testing.T) {
	r := fakeReads(t)
	r.squashSettings = func() merge.SquashSettings {
		t.Error("the squash settings were read although both halves are forwarded")
		return merge.SquashSettings{}
	}
	if _, ok := prepareMerge("5", []string{"-t", "Subject", "-b", "Body"}, realMerge, strings.NewReader(""), false, r); !ok {
		t.Error("refused a squash that closes nothing")
	}
	read := 0
	r.squashSettings = func() merge.SquashSettings { read++; return defaultSquash }
	for _, pass := range [][]string{nil, {"-t", "Subject"}, {"-b", "Body"}, {"-t", "", "-b", "Body"}} {
		read = 0
		prepareMerge("5", pass, closeOpts{closeOK: true}, strings.NewReader(""), false, r)
		if read != 1 {
			t.Errorf("%q: the settings were read %d times, want once (a half is the default)", pass, read)
		}
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
