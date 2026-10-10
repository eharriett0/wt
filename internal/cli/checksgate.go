// merge-pr's checks gate (eharriett0/wt#179). merge-pr said verdict=ok while
// the PR's checks were still queued, and when most of its workflows never
// started (a PR opened with several labels started one workflow). And --admin,
// which a required-review branch needs, bypasses GitHub's required status
// checks too, so nothing between wt and the merge looked at CI. The gate reads
// the head commit's checks and the base branch's required checks before the
// merge, refuses (asks, at a terminal) when they are not green, and pins the
// merge to the commit it read.
package cli

import (
	"errors"
	"fmt"
	"io"
	"os"
	"strings"
	"time"

	"github.com/eharriett0/wt/internal/ghx"
	"github.com/eharriett0/wt/internal/merge"
	"github.com/eharriett0/wt/internal/ui"
)

// checksReads are the gh reads the checks gate makes, as fields so a test can
// drive it without gh. liveChecksReads is the real thing.
type checksReads struct {
	checks     func(pr, repo string) (ghx.PRChecksRead, error)
	protection func(host, repo, base string) (ghx.Required, error)
	rulesets   func(host, repo, base string) (ghx.Required, error)
	repoName   func(host string, id int64) (string, error)
	sleep      func(time.Duration)
}

var liveChecksReads = checksReads{
	checks:     ghx.PRChecks,
	protection: ghx.BranchProtectionChecks,
	rulesets:   ghx.RulesetChecks,
	repoName:   ghx.RepoName,
	sleep:      time.Sleep,
}

// checksRetryWait is the pause before the one retry of a failed read.
const checksRetryWait = 2 * time.Second

// checksOpts are merge-pr's flags and settings that steer the checks gate.
type checksOpts struct {
	dryRun       bool
	checksOK     bool   // --checks-ok: merge past it, loudly
	admin        bool   // --admin: GitHub's required checks are bypassed as well
	bypass       bool   // --bypass, which does NOT cover this gate: the refusal says so
	auto         bool   // the passthrough arms auto-merge (--auto): the refusal says why that is not exempt
	repo         string // the passthrough's -R/--repo: gh merges that repository's PR, so that is the one read
	minChecks    int    // merge_min_checks
	minChecksBad string // merge_min_checks that is not a count
}

// gateAction is what the checks gate does with a verdict.
type gateAction int

const (
	gateProceed  gateAction = iota // green: merge
	gateNote                       // no check ran, nothing requires one: merge, and say so
	gateOverride                   // blocked, --checks-ok: merge, loudly
	gatePreview                    // blocked, --dry-run: say what a real merge would do
	gateAsk                        // blocked, at a terminal: ask
	gateRefuse                     // blocked: refuse
)

// checksAction decides what the gate does: a blocked verdict is refused unless
// --checks-ok overrides it, a dry run previews it (and never prompts, #170), and
// a terminal is asked. Pure.
func checksAction(s merge.ChecksStatus, dryRun, checksOK, tty bool) gateAction {
	switch {
	case s == merge.ChecksGreen:
		return gateProceed
	case s == merge.ChecksNone:
		return gateNote
	case dryRun:
		return gatePreview
	case checksOK:
		return gateOverride
	case tty:
		return gateAsk
	}
	return gateRefuse
}

// checksGate is merge-pr's checks gate (#179). It reads the checks on PR pr's
// head commit and what the base branch requires, decides (merge.DecideChecks),
// prints the summary, and returns the commit to pin the merge to (the one it
// read; "" when the checks could not be read), the verdict's label for the
// dry-run line (green, none, blocked, unread), the host of the PR it read ("" =
// none; the deploy gate's status check needs it, #178) and whether to go on.
// stdin and tty are where a terminal's answer comes from.
//
// ⚠ --admin never gets past it: --admin bypasses GitHub's required checks, so
// this is the one check left. --bypass does not either (it is for wt's
// empty-diff, placeholder, foreign-lane and hold guards); --checks-ok does.
func checksGate(pr string, o checksOpts, stdin io.Reader, tty bool, r checksReads) (pin, label, host string, ok bool) {
	read, in := readChecks(pr, o, r)
	v := merge.DecideChecks(in)
	label, host = v.Label(), read.Host
	fmt.Println(checksLine(pr, read.Head, v))
	act := checksAction(v.Status, o.dryRun, o.checksOK, tty)
	switch act {
	case gateProceed:
		return read.Head, label, host, true
	case gateNote:
		ui.Warn("%s", noChecksNote(pr, v))
		return read.Head, label, host, true
	}
	for _, l := range checksDetail(v, read.Head) {
		ui.Warn("%s", l)
	}
	why := strings.Join(v.Reasons(), ", ")
	switch act {
	case gatePreview:
		if o.checksOK {
			ui.Warn("--dry-run: %s; --checks-ok is set, so a real merge would merge anyway.", why)
			return read.Head, label, host, true
		}
		ui.Warn("--dry-run: a real merge would REFUSE here (or ask at a terminal): %s.", why)
		for _, h := range refuseHints(pr, v, o) {
			ui.Info("%s", h)
		}
		return read.Head, label, host, true
	case gateOverride:
		ui.Warn("--checks-ok set — merging PR #%s anyway: %s.%s", pr, why, adminTail(o.admin))
		return read.Head, label, host, true
	case gateAsk:
		fmt.Fprintf(os.Stderr, "%s PR #%s: %s.%s Type %s to merge anyway (anything else aborts): ",
			ui.Yellow("→"), pr, why, adminTail(o.admin), ui.Bold("merge"))
		if strings.TrimSpace(readLine(stdin)) == "merge" {
			return read.Head, label, host, true
		}
		ui.Err("aborted — PR #%s not merged.", pr)
		return "", label, host, false
	}
	ui.Err("refusing to merge PR #%s — %s.", pr, why)
	for _, h := range refuseHints(pr, v, o) {
		ui.Info("%s", h)
	}
	return "", label, host, false
}

// readChecks reads the head commit's checks and, from its base branch in its
// base repository, what is required, each read retried once (a failed read
// stays a failed read: the verdict blocks on it). ghx.ErrRulesetsUnavailable
// is an answer, so it is not retried.
func readChecks(pr string, o checksOpts, r checksReads) (ghx.PRChecksRead, merge.ChecksInput) {
	in := merge.ChecksInput{MinChecks: o.minChecks, MinChecksBad: o.minChecksBad}
	read, err := r.checks(pr, o.repo)
	if err != nil {
		r.sleep(checksRetryWait)
		read, err = r.checks(pr, o.repo)
	}
	if err != nil {
		in.ReadErr = err.Error()
		return ghx.PRChecksRead{}, in
	}
	in.Checks = read.Checks
	required := func(f func(host, repo, base string) (ghx.Required, error)) (ghx.Required, error) {
		req, err := f(read.Host, read.Repo, read.Base)
		if err != nil && !errors.Is(err, ghx.ErrRulesetsUnavailable) {
			r.sleep(checksRetryWait)
			req, err = f(read.Host, read.Repo, read.Base)
		}
		return req, err
	}
	prot, perr := required(r.protection)
	rules, rerr := required(r.rulesets)
	req, unread := merge.RequiredChecks(prot, perr, rules, rerr)
	workflows, wunread := resolveWorkflows(read, req.Workflows, r)
	in.Required, in.Workflows, in.RequiredUnread = req.Checks, workflows, append(unread, wunread...)
	return read, in
}

// resolveWorkflows names the repository each required workflow lives in, so
// the gate can match runs to it (#179): the PR's base repository when the rule
// names its id, else as gh resolves the id (retried once). A workflow whose
// repository gh cannot name is left out and reported as unread, which blocks.
func resolveWorkflows(read ghx.PRChecksRead, ws []ghx.RequiredWorkflow, r checksReads) (out []ghx.RequiredWorkflow, unread []string) {
	names := map[int64]string{read.RepoID: read.Repo}
	failed := map[int64]error{}
	for _, w := range ws {
		name, known := names[w.RepoID]
		if !known && failed[w.RepoID] == nil {
			n, err := r.repoName(read.Host, w.RepoID)
			if err != nil {
				r.sleep(checksRetryWait)
				n, err = r.repoName(read.Host, w.RepoID)
			}
			if err != nil {
				failed[w.RepoID] = err
			} else {
				name, known, names[w.RepoID] = n, true, n
			}
		}
		if !known {
			unread = append(unread, fmt.Sprintf("the repository (id %d) of required workflow %s: %v", w.RepoID, w.Path, failed[w.RepoID]))
			continue
		}
		w.Repo = name
		out = append(out, w)
	}
	return out, unread
}

// readLine reads one line from r, a byte at a time, so it never consumes what
// comes after: a `-F -` body read from the same stdin is next (#180). Buffered,
// the answer's read took the body with it whenever stdin is not a line-at-a-
// time terminal. The line is returned without its newline; at EOF, what there
// was.
func readLine(r io.Reader) string {
	var line []byte
	b := make([]byte, 1)
	for {
		n, err := r.Read(b)
		if n == 1 {
			if b[0] == '\n' {
				return string(line)
			}
			line = append(line, b[0])
		}
		if err != nil {
			return string(line)
		}
	}
}

// checksLine is the gate's summary, printed on every merge-pr run, dry or not:
// a clean dry run therefore says the checks were read: green, or none when no
// check ran. Pure.
//
//	merge-pr: PR #7 checks=green on 1a2b3c4d5e6f: 12 passed, 2 skipped; required: 3, all reported
func checksLine(pr, head string, v merge.ChecksVerdict) string {
	if v.ReadErr != "" {
		return fmt.Sprintf("merge-pr: PR #%s checks=unread: %s", pr, v.ReadErr)
	}
	var req string
	notReported := len(v.Missing) + len(v.OtherApp) + len(v.MissingWorkflows) + len(v.Unverifiable)
	switch {
	case len(v.RequiredUnread) > 0:
		req = "required: could not be read"
	case v.Required == 0:
		req = "no required checks"
	case notReported == 0:
		req = fmt.Sprintf("required: %d, all reported", v.Required)
	default:
		req = fmt.Sprintf("required: %d, %d never reported", v.Required, notReported)
	}
	return fmt.Sprintf("merge-pr: PR #%s checks=%s on %.12s: %s; %s", pr, v.Label(), head, v.CountsText(), req)
}

// skippedChecksLine is the gate's line when the passthrough merges nothing
// (merge.NonMerging): no checks are read, and nothing is pinned. Pure.
func skippedChecksLine(pr, flag string) string {
	return fmt.Sprintf("merge-pr: PR #%s checks=skipped: the forwarded %s merges nothing, so the checks gate does not apply", pr, flag)
}

// checksDetail lists what blocks a verdict, one line per kind, for the lines
// under the summary. Pure.
func checksDetail(v merge.ChecksVerdict, head string) []string {
	if v.ReadErr != "" {
		return []string{"the checks on the PR could not be read (" + v.ReadErr + "), so wt cannot tell whether CI passed"}
	}
	var out []string
	if len(v.Failed) > 0 {
		out = append(out, "failed: "+checkList(v.Failed))
	}
	if len(v.Pending) > 0 {
		out = append(out, "pending: "+checkList(v.Pending))
	}
	if len(v.Unknown) > 0 {
		out = append(out, "in a state wt does not know: "+checkList(v.Unknown))
	}
	if len(v.Missing) > 0 {
		out = append(out, fmt.Sprintf("required, but never reported on %.12s: %s (CI may not have started)", head, nameList(v.Missing)))
	}
	if len(v.OtherApp) > 0 {
		out = append(out, fmt.Sprintf("required from one app, but reported on %.12s only by another: %s", head, nameList(v.OtherApp)))
	}
	if len(v.MissingWorkflows) > 0 {
		out = append(out, fmt.Sprintf("required workflow never ran on %.12s: %s (CI may not have started)", head, nameList(v.MissingWorkflows)))
	}
	if len(v.Unverifiable) > 0 {
		out = append(out, fmt.Sprintf("required workflow wt cannot verify on %.12s: %s (GitHub did not say which workflow file some runs came from)", head, nameList(v.Unverifiable)))
	}
	if v.Floor > 0 {
		out = append(out, fmt.Sprintf("only %d check(s) ran, fewer than merge_min_checks = %d (CI may not have started)", v.Ran, v.Floor))
	}
	if v.FloorBad != "" {
		out = append(out, fmt.Sprintf("merge_min_checks = %q is not a count, so the floor cannot be applied: fix .wt.conf or WT_MERGE_MIN_CHECKS", v.FloorBad))
	}
	for _, u := range v.RequiredUnread {
		out = append(out, "could not read the required checks from "+u+", so a required check that never started cannot be ruled out")
	}
	return out
}

// noChecksNote is the note for a PR on which no check ran and nothing requires
// one: it merges, and says that nothing in CI verified it. Pure.
func noChecksNote(pr string, v merge.ChecksVerdict) string {
	return fmt.Sprintf("no check ran on PR #%s (%s): nothing in CI verified this merge. "+
		"If CI should run on this PR, it may not have started; set merge_min_checks, or require a check, to make merge-pr refuse this.",
		pr, v.CountsText())
}

// refuseHints say what to do about a refusal. Pure.
func refuseHints(pr string, v merge.ChecksVerdict, o checksOpts) []string {
	var out []string
	if v.ReadErr != "" {
		out = append(out, "re-run once gh can read them, or pass --checks-ok to merge without them")
	} else {
		out = append(out, fmt.Sprintf("wait for CI (gh pr checks %s --watch) and re-run, or pass --checks-ok to merge anyway", pr))
	}
	if o.auto {
		out = append(out, "--auto is not exempt: when the only checks pending or failing are ones no branch rule requires, gh merges at once instead of waiting for them")
	}
	if o.admin {
		out = append(out, "--admin bypasses GitHub's required checks too, so nothing after wt would stop this merge")
	}
	if o.bypass {
		out = append(out, "--bypass does not cover the checks gate; --checks-ok does")
	}
	return out
}

// adminTail is the sentence a --checks-ok merge or a prompt adds under --admin.
func adminTail(admin bool) string {
	if !admin {
		return ""
	}
	return " With --admin, GitHub will not stop it either."
}

// listCap is how many names a detail line lists before "and N more".
const listCap = 8

// checkList renders checks as `name [STATE]`. Pure.
func checkList(cs []ghx.Check) string {
	names := make([]string, len(cs))
	for i, c := range cs {
		state := c.State
		if strings.TrimSpace(state) == "" {
			state = "no state"
		}
		names[i] = c.Name + " [" + state + "]"
	}
	return nameList(names)
}

// nameList joins names, the first listCap of them. Pure.
func nameList(names []string) string {
	if len(names) <= listCap {
		return strings.Join(names, ", ")
	}
	return fmt.Sprintf("%s and %d more", strings.Join(names[:listCap], ", "), len(names)-listCap)
}
