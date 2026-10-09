// Close-keyword lint + post-merge issue-state verification for `wt merge-pr`
// (eharriett0/wt#77). merge-pr is the one place that can see what a merge will
// auto-close — the PR body AND the squash commit body it forwards. GitHub's
// closingIssuesReferences only sees the PR title/body, so a `Fixes #N` living in
// a commit body closes an issue silently (trap 2). And negation does not disarm
// a close keyword — "does not close #N" still closes (trap 1). This surfaces both
// before the squash, and verifies issue state after it.
//
// A body forwarded to gh (`wt merge-pr <pr> -- --body-file f`) REPLACES the
// squash commit body, so the check judges that text instead of the commit
// bodies (#180) — it used to read the commit bodies regardless, refusing a
// forwarded body that closed nothing and passing one that did.
package cli

import (
	"bytes"
	"fmt"
	"io"
	"os"
	"sort"
	"strconv"
	"strings"

	"github.com/eharriett0/wt/internal/ghx"
	"github.com/eharriett0/wt/internal/merge"
	"github.com/eharriett0/wt/internal/ui"
)

// closePlan is the pre-merge close analysis, threaded to the post-merge verify.
type closePlan struct {
	refs    []merge.ClosingRef // every closing ref in the PR body + the squash commit text
	extra   []int              // same-repo closings NOT in closingIssuesReferences (trap 2)
	suspect []merge.ClosingRef // closes whose phrasing says they are not meant (#164)
	watch   []int              // same-repo issue numbers to re-check after the merge
	before  map[int]string     // issue → state snapshot before the merge

	forwarded string             // where a forwarded squash body came from; "" = none (#180)
	replaced  []merge.ClosingRef // closes only in the commit bodies that body replaces
}

// squashBody is the squash commit body merge-pr forwards to gh, read ONCE so
// the close check and gh see the same bytes (#180).
type squashBody struct {
	src     merge.ForwardedBody
	text    string    // the body gh will use (when forwarded)
	ghArgs  []string  // the passthrough to hand gh
	ghStdin io.Reader // what gh reads as stdin; nil = wt's own
}

func (s squashBody) forwarded() bool { return s.src.Source != merge.BodyDefault }

// readSquashBody resolves a parsed body source to its text, reading a file or
// stdin exactly once. Either one is then re-pointed at "-" and its bytes handed
// to gh on stdin (merge.RedirectToStdin says why). An unreadable source is an
// error, not a skipped check: gh would fail on it too, and a check that quietly
// fell back to the commit messages would be judging a body that cannot ship.
func readSquashBody(src merge.ForwardedBody, args []string, stdin io.Reader) (squashBody, error) {
	s := squashBody{src: src, ghArgs: args}
	var (
		b   []byte
		err error
	)
	switch src.Source {
	case merge.BodyText:
		s.text = src.Value
		return s, nil
	case merge.BodyStdin:
		b, err = io.ReadAll(stdin)
	case merge.BodyFile:
		b, err = os.ReadFile(src.Value)
	default:
		return s, nil
	}
	if err != nil {
		return squashBody{}, fmt.Errorf("cannot read the squash body from %s: %w", src.Describe(), err)
	}
	s.text = string(b)
	s.ghArgs = src.RedirectToStdin(args)
	s.ghStdin = bytes.NewReader(b)
	return s, nil
}

// forwardedSquashBody parses merge-pr's gh passthrough for a squash-body
// override and reads it (#180), a `-F -` body from stdin. On a terminal, `-F -`
// waits for typed input the way gh itself would, so say so rather than appear
// to hang.
func forwardedSquashBody(ghArgs []string, stdin io.Reader, tty bool) (squashBody, error) {
	src, err := merge.ParseForwardedBody(ghArgs)
	if err != nil {
		return squashBody{}, err
	}
	if src.Source == merge.BodyStdin && tty {
		ui.Info("reading the squash body from stdin (%s) — end it with Ctrl-D", src.Flag+" -")
	}
	sq, err := readSquashBody(src, ghArgs, stdin)
	if err == nil && emptyStdinBody(sq, tty) {
		ui.Warn("%s read an EMPTY body from stdin (was anything piped in?) — the squash commit will ship with an empty body", src.Flag+" -")
	}
	return sq, err
}

// emptyStdinBody reports whether a `-F -` body came back empty (or blank) from
// a stdin that is not a terminal: an agent or script that forwarded `-F -` and
// piped nothing in. gh merges that as an empty squash body, so merge-pr warns
// rather than refuses — an empty body can be deliberate. Pure.
func emptyStdinBody(sq squashBody, tty bool) bool {
	return sq.src.Source == merge.BodyStdin && !tty && strings.TrimSpace(sq.text) == ""
}

// closeReads are the gh reads the close check makes, as fields so a test can
// drive prepareMerge and analyzeClosings without gh (#180). liveCloseReads is
// the real thing.
type closeReads struct {
	prBody      func(pr string) (string, error)
	commitText  func(pr string) string // every commit's headline + body
	headlines   func(pr string) []string
	closingRefs func(pr string) []int // the PR's closingIssuesReferences
	issueState  func(n string) (string, error)
	issueTitle  func(n string) (string, error)
}

var liveCloseReads = closeReads{
	prBody:      ghx.PRBody,
	commitText:  ghx.PRCommitText,
	headlines:   ghx.PRCommitSubjects,
	closingRefs: ghx.PRClosingIssueNumbers,
	issueState:  ghx.IssueState,
	issueTitle:  ghx.IssueTitle,
}

// closeOpts are merge-pr's flags that steer the pre-merge close check.
type closeOpts struct {
	dryRun, closeOK, skip bool // skip = --no-close-check
}

// mergePrep is what merge-pr hands `gh pr merge` once the close check has run,
// and the plan the post-merge verify reads.
type mergePrep struct {
	args  []string  // the passthrough; a forwarded body file re-pointed at "-"
	stdin io.Reader // gh's stdin: the forwarded body wt already read; nil = wt's own
	plan  closePlan
}

// prepareMerge is merge-pr's pre-merge close check (#77) and its squash-body
// handling (#180), split out of cmdMergePR so the wiring is tested: stdin is
// where a `-F -` body comes from and tty whether that is a terminal. ok=false
// means refuse (exit 1). A dry run never refuses; it says what a real merge
// would do.
//
// A body forwarded after `--` (-b/--body, -F/--body-file, `-F -` for stdin) IS
// the squash body, so the check reads it instead of the commit bodies. It is
// read once and gh is handed those same bytes. If gh would not merge any body
// that can be named (an unreadable file, --body with --body-file, a value flag
// with no value) the merge is refused even with --close-ok: gh would fail on it,
// and the check has nothing to judge. --no-close-check reads nothing, so gh
// gets the passthrough untouched.
func prepareMerge(pr string, passthrough []string, o closeOpts, stdin io.Reader, tty bool, r closeReads) (mergePrep, bool) {
	p := mergePrep{args: passthrough}
	if o.skip {
		return p, true
	}
	sq, err := forwardedSquashBody(passthrough, stdin, tty)
	switch {
	case err != nil && o.dryRun:
		ui.Warn("--dry-run: a real merge would REFUSE here: %v", err)
		return p, true
	case err != nil:
		ui.Err("refusing to merge — %v", err)
		return p, false
	}
	p.args, p.stdin = sq.ghArgs, sq.ghStdin
	p.plan = analyzeClosings(pr, sq, r)
	if gate := renderClosePlan(p.plan, r); gate && !o.closeOK {
		if o.dryRun {
			ui.Warn("--dry-run: a real merge would REFUSE here. Verify the close set, then pass --close-ok.")
			return p, true
		}
		ui.Err("refusing to merge — the squash would close issues the PR doesn't declare, or close one whose phrasing says it is not meant (see above). Verify, then pass --close-ok to proceed.")
		return p, false
	}
	return p, true
}

// closeCheckTexts returns the text the close check GATES on and the wider text
// whose issues the post-merge verify re-reads. Pure.
//
// The gate reads the PR body (GitHub closes what that links, whatever the
// squash says) plus what the squash COMMIT will carry. With no forwarded body
// that is every commit message, which GitHub's default squash body is built
// from. A forwarded body replaces the commit BODIES (#180): they never ship, so
// a keyword left in them closes nothing, while one in the forwarded text does.
// The commit headlines stay in. --body does not replace the squash SUBJECT, and
// under GitHub's "Default message" squash setting a one-commit PR's subject is
// that commit's title; a keyword there still closes, and over-reporting is the
// cheap direction.
//
// The verify keeps watching the replaced bodies' issues too, so if the squash
// closes one of them after all, the same command still says so.
func closeCheckTexts(prBody, commitText string, headlines []string, sq squashBody) (gate, watch string) {
	if !sq.forwarded() {
		t := prBody + "\n\n" + commitText
		return t, t
	}
	parts := append([]string{prBody}, headlines...)
	gate = strings.Join(append(parts, sq.text), "\n\n")
	return gate, gate + "\n\n" + commitText
}

// replacedClosings returns the closes written in the commit messages that a
// forwarded body keeps from shipping (#180) — in neither the gate text nor the
// PR's own closingIssuesReferences (graph), which closes an issue linked in
// GitHub's sidebar with no keyword anywhere — so the analysis can say why a
// familiar warning is gone. Pure.
func replacedClosings(commitText, gate string, graph []int) []merge.ClosingRef {
	kept := map[string]bool{}
	for _, r := range merge.ClosingRefs(gate) {
		kept[refLabel(r)] = true
	}
	for _, n := range graph {
		kept["#"+strconv.Itoa(n)] = true
	}
	var out []merge.ClosingRef
	for _, r := range merge.ClosingRefs(commitText) {
		if !kept[refLabel(r)] {
			out = append(out, r)
		}
	}
	return out
}

// refLabel renders a ClosingRef the way GitHub addresses it, so the warning can
// be copied straight into a search.
func refLabel(r merge.ClosingRef) string {
	if r.Repo != "" {
		return fmt.Sprintf("%s#%d", r.Repo, r.Number)
	}
	return fmt.Sprintf("#%d", r.Number)
}

// analyzeClosings gathers what the squash will close (PR body + the squash
// commit text: the full commit messages, or a forwarded body in place of their
// bodies — closeCheckTexts), compares to the PR's own closingIssuesReferences,
// and snapshots the watched issues' states for the post-merge verify.
// Best-effort — a gh failure yields an empty plan so it never blocks a merge.
func analyzeClosings(pr string, sq squashBody, r closeReads) closePlan {
	body, _ := r.prBody(pr)
	commitText := r.commitText(pr)
	var headlines []string
	if sq.forwarded() {
		headlines = r.headlines(pr)
	}
	text, watchText := closeCheckTexts(body, commitText, headlines, sq)

	watchSet := map[int]bool{}
	for _, n := range merge.SameRepoClosings(watchText) {
		watchSet[n] = true
	}
	graph := r.closingRefs(pr)
	for _, n := range graph {
		watchSet[n] = true
	}
	watch := make([]int, 0, len(watchSet))
	before := map[int]string{}
	for n := range watchSet {
		watch = append(watch, n)
		if st, err := r.issueState(strconv.Itoa(n)); err == nil {
			before[n] = strings.TrimSpace(st)
		}
	}
	sort.Ints(watch)
	plan := closePlan{
		refs:    merge.ClosingRefs(text),
		extra:   merge.ExtraClosings(text, graph),
		suspect: merge.SuspectClosings(text),
		watch:   watch,
		before:  before,
	}
	if sq.forwarded() {
		plan.forwarded = sq.src.Describe()
		plan.replaced = replacedClosings(commitText, text, graph)
	}
	return plan
}

// renderClosePlan prints the resolved close set with each same-repo issue's
// current state + title, and returns whether a confirmation gate should fire —
// which is exactly when the squash closes something the PR's own closing
// references do NOT (the trap-2 signature). A normal "Fixes #N" PR prints its
// close set but does NOT gate, so the warning stays meaningful.
func renderClosePlan(p closePlan, reads closeReads) bool {
	if p.forwarded != "" {
		ui.Info("squash body: forwarded via %s, so the close check reads it instead of the commit bodies (#180)", p.forwarded)
		if len(p.replaced) > 0 {
			labels := make([]string, len(p.replaced))
			for i, r := range p.replaced {
				labels[i] = refLabel(r)
			}
			ui.Info("the forwarded body leaves out the close of %s that only the commit bodies carry", strings.Join(labels, ", "))
		}
	}
	if len(p.refs) == 0 {
		return false
	}
	ui.Info("this merge will CLOSE:")
	for _, r := range p.refs {
		if r.Repo != "" {
			fmt.Printf("    %s  %s\n", ui.Bold(fmt.Sprintf("%s#%d", r.Repo, r.Number)), ui.Dim("(cross-repo)"))
			continue
		}
		title, _ := reads.issueTitle(strconv.Itoa(r.Number))
		st := p.before[r.Number]
		if st == "" {
			st = "?"
		}
		fmt.Printf("    %s  %s  %s\n", ui.Bold("#"+strconv.Itoa(r.Number)), ui.Yellow("["+st+"]"), ui.Dim(title))
	}
	gate := false
	if len(p.extra) > 0 {
		ui.Warn("the squash COMMIT body will close %s — NOT in the PR's own closing references. "+
			"GitHub's closingIssuesReferences is blind to the commit body, so this would close silently (#77 trap 2).",
			joinNums(p.extra))
		gate = true
	}
	// A close keyword written inside a negation or a qualifier is never a
	// deliberate close, and it fires whether or not the ref is in the PR's own
	// closing references — so it is checked independently of trap 2 (#164).
	for _, r := range p.suspect {
		ui.Warn("%s reads as a close that is NOT meant (%s), and GitHub ignores the surrounding words:\n    %s",
			ui.Bold(refLabel(r)), r.Suspect, ui.Dim(r.Context))
		gate = true
	}
	return gate
}

// verifyClosings re-checks the watched issues after the merge and reports any
// that changed state — surfacing a silent close in the SAME command instead of
// days later (#77). Needs no keyword parsing; catches everything.
func verifyClosings(p closePlan) {
	for _, n := range p.watch {
		after, err := ghx.IssueState(strconv.Itoa(n))
		if err != nil {
			continue
		}
		after = strings.TrimSpace(after)
		before := p.before[n]
		switch {
		case before != "" && !strings.EqualFold(before, after):
			ui.Warn("issue #%d changed state on merge: %s → %s", n, before, after)
		case strings.EqualFold(after, "CLOSED"):
			ui.Info("verified: #%d is CLOSED", n)
		}
	}
}

func joinNums(nums []int) string {
	parts := make([]string, len(nums))
	for i, n := range nums {
		parts[i] = "#" + strconv.Itoa(n)
	}
	return strings.Join(parts, ", ")
}
