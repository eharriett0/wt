package merge

import (
	"errors"
	"sort"
	"strconv"
	"strings"

	"github.com/eharriett0/wt/internal/ghx"
)

// ChecksStatus is what merge-pr's checks gate makes of the checks on a PR's
// head commit (#179).
type ChecksStatus string

const (
	ChecksGreen   ChecksStatus = "green"   // checks ran, none pending or failed, nothing required missing, the floor met
	ChecksNone    ChecksStatus = "none"    // nothing blocks, but no check ran: merge, and say so
	ChecksBlocked ChecksStatus = "blocked" // refuse (ask at a terminal): the verdict's Reasons say why
)

// CheckBucket is how the gate reads one of GitHub's check states.
type CheckBucket string

const (
	BucketPassed  CheckBucket = "passed"
	BucketSkipped CheckBucket = "skipped" // SKIPPED and NEUTRAL: a required check is satisfied by either
	BucketPending CheckBucket = "pending" // has not finished
	BucketFailed  CheckBucket = "failed"  // finished without passing
	BucketUnknown CheckBucket = "unknown" // a state wt does not know, or none: never green
)

// bucketOrder is the order counts are printed in.
var bucketOrder = []CheckBucket{BucketPassed, BucketPending, BucketFailed, BucketUnknown, BucketSkipped}

// BucketOf reads a check's state (ghx.Check.State) for the gate, through
// ghx.StateKind, the reading latestChecks uses too. Pure.
//
// ⚠ Every finished state that is not a pass is "failed", CANCELLED and STALE
// included: `--admin` merges past a red required check, so the gate is the
// only thing that would stop it. Anything wt does not know, including a state
// GitHub adds later, is unknown, and unknown blocks.
func BucketOf(state string) CheckBucket {
	switch ghx.StateKind(state) {
	case ghx.KindPassed:
		return BucketPassed
	case ghx.KindQuiet:
		return BucketSkipped
	case ghx.KindPending:
		return BucketPending
	case ghx.KindFailed, ghx.KindVoid:
		return BucketFailed
	}
	return BucketUnknown
}

// ChecksInput is what the gate decides on (#179).
type ChecksInput struct {
	ReadErr        string                 // the checks could not be read: why ("" = they were)
	Checks         []ghx.Check            // what still counts of each check on the head commit
	Required       []ghx.RequiredCheck    // what branch protection and rulesets require
	Workflows      []ghx.RequiredWorkflow // the workflows rulesets require to run, Repo resolved
	RequiredUnread []string               // where the required checks could not be read from, and why
	MinChecks      int                    // merge_min_checks: fewer checks ran blocks; 0 = no floor
	MinChecksBad   string                 // merge_min_checks set to something that is not a count
}

// ChecksVerdict is the gate's decision and everything it rests on.
type ChecksVerdict struct {
	Status   ChecksStatus
	ReadErr  string
	Counts   map[CheckBucket]int
	Pending  []ghx.Check
	Failed   []ghx.Check
	Unknown  []ghx.Check
	Missing  []string // required, and no check of that name is on the head
	OtherApp []string // required from one app, and only another app's check of that name is on the head
	// MissingWorkflows are the required workflows (repo:path) with no run on the
	// head; Unverifiable the ones wt cannot rule in or out, because GitHub did not
	// say which file some run's workflow came from.
	MissingWorkflows []string
	Unverifiable     []string
	Required         int    // how many checks and workflows are required
	Ran              int    // checks that ran or are running: all but SKIPPED
	Floor            int    // the merge_min_checks Ran fell short of; 0 = met, or none set
	FloorBad         string // merge_min_checks is not a count: its value
	RequiredUnread   []string
}

// DecideChecks is merge-pr's checks gate (#179). Pure.
//
// It blocks when the checks could not be read, any check is pending, failed or
// in a state wt does not know, a required check has no run on the head (or,
// pinned to one app, only another app's), a required workflow did not run or
// cannot be told apart, fewer checks ran than merge_min_checks (or that
// setting is not a count), or the required checks could not be read: a failed
// read never counts as green. Otherwise it is green, or none when no check ran
// at all (zero checks, or every one SKIPPED).
//
// A required check is reported when a check of its name is on the head; one
// pinned to an app needs a check GitHub itself marks required for this PR
// (isRequired), or a run of that app. GitHub's isRequired also counts toward
// what is required when the rules endpoints did not name the check.
//
// ⚠ "None" merges. Whether a repo's workflows should have run on THIS PR is in
// their triggers (paths, branches, event types, `if:`), which wt cannot
// evaluate; awesome-o's PRs carry no checks because its one workflow is
// path-filtered. A required check and merge_min_checks are the deterministic
// "CI never started" signals, so the gate refuses on those and only says so
// otherwise.
func DecideChecks(in ChecksInput) ChecksVerdict {
	v := ChecksVerdict{Counts: map[CheckBucket]int{}}
	if in.ReadErr != "" {
		v.Status, v.ReadErr = ChecksBlocked, in.ReadErr
		return v
	}
	byName := map[string][]ghx.Check{}
	required := map[string]bool{}
	for _, c := range in.Checks {
		byName[c.Name] = append(byName[c.Name], c)
		if c.Required {
			required[c.Name] = true
		}
		b := BucketOf(c.State)
		v.Counts[b]++
		switch b {
		case BucketPending:
			v.Pending = append(v.Pending, c)
		case BucketFailed:
			v.Failed = append(v.Failed, c)
		case BucketUnknown:
			v.Unknown = append(v.Unknown, c)
		}
		if !strings.EqualFold(strings.TrimSpace(c.State), "SKIPPED") {
			v.Ran++
		}
	}
	missing, otherApp := map[string]bool{}, map[string]bool{}
	for _, r := range in.Required {
		required[r.Name] = true
		switch runs := byName[r.Name]; {
		case len(runs) == 0:
			missing[r.Name] = true
		case !reportedBy(runs, r):
			otherApp[r.Name] = true
		}
	}
	v.Missing, v.OtherApp = sortedKeys(missing), sortedKeys(otherApp)
	for _, w := range in.Workflows {
		switch ran, known := workflowRan(in.Checks, w); {
		case ran:
		case known:
			v.MissingWorkflows = append(v.MissingWorkflows, workflowLabel(w))
		default:
			v.Unverifiable = append(v.Unverifiable, workflowLabel(w))
		}
	}
	v.Required = len(required) + len(in.Workflows)
	if in.MinChecks > 0 && v.Ran < in.MinChecks {
		v.Floor = in.MinChecks
	}
	v.FloorBad = in.MinChecksBad
	v.RequiredUnread = in.RequiredUnread
	switch {
	case len(v.Pending)+len(v.Failed)+len(v.Unknown)+len(v.Missing)+len(v.OtherApp) > 0,
		len(v.MissingWorkflows)+len(v.Unverifiable) > 0,
		v.Floor > 0, v.FloorBad != "", len(v.RequiredUnread) > 0:
		v.Status = ChecksBlocked
	case v.Ran == 0:
		v.Status = ChecksNone
	default:
		v.Status = ChecksGreen
	}
	return v
}

// reportedBy reports whether runs (the checks of r's name on the head) satisfy
// r: any of them when r takes any app; when r is pinned to one app, a check
// GitHub marks required for this PR, or a check run of that app. Pure.
func reportedBy(runs []ghx.Check, r ghx.RequiredCheck) bool {
	if r.App == 0 {
		return true
	}
	for _, c := range runs {
		if c.Required || (!c.Status && c.App == r.App) {
			return true
		}
	}
	return false
}

// workflowRan reports whether a run of required workflow w is on the head, and
// whether wt can tell: known=false when it found none, but some Actions run
// does not say which workflow file it came from. Pure.
func workflowRan(checks []ghx.Check, w ghx.RequiredWorkflow) (ran, known bool) {
	known = w.Repo != ""
	for _, c := range checks {
		if c.Status || c.Workflow == "" {
			continue // a status, or another app's run: not a workflow
		}
		if c.WorkflowPath == "" || c.WorkflowRepo == "" {
			known = false
			continue
		}
		if w.Repo != "" && c.WorkflowPath == w.Path && strings.EqualFold(c.WorkflowRepo, w.Repo) {
			return true, true
		}
	}
	return false, known
}

// workflowLabel names a required workflow for a message: repo:path, or the
// repository's id when its name was never resolved. Pure.
func workflowLabel(w ghx.RequiredWorkflow) string {
	repo := w.Repo
	if repo == "" {
		repo = "repository " + strconv.FormatInt(w.RepoID, 10)
	}
	return repo + ":" + w.Path
}

func sortedKeys(set map[string]bool) []string {
	var out []string
	for k := range set {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}

// Label is the verdict's checks= value: its status, or "unread". Pure.
func (v ChecksVerdict) Label() string {
	if v.ReadErr != "" {
		return "unread"
	}
	return string(v.Status)
}

// Reasons are the short reasons a blocked verdict blocks, in a fixed order;
// none for a verdict that does not block. Pure.
func (v ChecksVerdict) Reasons() []string {
	if v.ReadErr != "" {
		return []string{"its checks could not be read"}
	}
	var out []string
	add := func(n int, what string) {
		if n > 0 {
			out = append(out, plural(n, "check")+" "+what)
		}
	}
	add(len(v.Failed), "failed")
	add(len(v.Pending), "pending")
	add(len(v.Unknown), "in a state wt does not know")
	if n := len(v.Missing); n > 0 {
		out = append(out, plural(n, "required check")+" never reported on this head")
	}
	if n := len(v.OtherApp); n > 0 {
		out = append(out, plural(n, "required check")+" reported only by another app")
	}
	if n := len(v.MissingWorkflows); n > 0 {
		out = append(out, plural(n, "required workflow")+" never ran on this head")
	}
	if n := len(v.Unverifiable); n > 0 {
		out = append(out, plural(n, "required workflow")+" that wt cannot verify")
	}
	if v.Floor > 0 {
		out = append(out, "fewer checks ran than merge_min_checks")
	}
	if v.FloorBad != "" {
		out = append(out, "merge_min_checks is not a count")
	}
	if len(v.RequiredUnread) > 0 {
		out = append(out, "the required checks could not be read")
	}
	return out
}

// CountsText is the per-bucket counts, "3 passed, 1 pending", or "no checks
// reported". Pure.
func (v ChecksVerdict) CountsText() string {
	var parts []string
	for _, b := range bucketOrder {
		if n := v.Counts[b]; n > 0 {
			parts = append(parts, strconv.Itoa(n)+" "+string(b))
		}
	}
	if len(parts) == 0 {
		return "no checks reported"
	}
	return strings.Join(parts, ", ")
}

// RequiredChecks merges the two sources of what a branch requires (#179):
// branch protection and the active rulesets on the base branch. required is
// their union; unread says which source could not be read, and why, so the
// gate can refuse rather than call a missing required check absent. A server
// with no rulesets for the repo (ghx.ErrRulesetsUnavailable: a 404, or a
// private repo on a plan without them) has none to require, which is an
// answer, not a failed read. Pure.
func RequiredChecks(protection ghx.Required, protectionErr error, rulesets ghx.Required, rulesetsErr error) (required ghx.Required, unread []string) {
	add := func(r ghx.Required) {
		required.Checks = append(required.Checks, r.Checks...)
		required.Workflows = append(required.Workflows, r.Workflows...)
	}
	if protectionErr != nil {
		unread = append(unread, "branch protection: "+protectionErr.Error())
	} else {
		add(protection)
	}
	switch {
	case errors.Is(rulesetsErr, ghx.ErrRulesetsUnavailable):
	case rulesetsErr != nil:
		unread = append(unread, "rulesets: "+rulesetsErr.Error())
	default:
		add(rulesets)
	}
	required.Checks = uniqueRequired(required.Checks)
	return required, unread
}

// uniqueRequired is checks without repeats, sorted by name and app. Pure.
func uniqueRequired(checks []ghx.RequiredCheck) []ghx.RequiredCheck {
	seen := map[ghx.RequiredCheck]bool{}
	var out []ghx.RequiredCheck
	for _, c := range checks {
		if !seen[c] {
			seen[c] = true
			out = append(out, c)
		}
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i].Name != out[j].Name {
			return out[i].Name < out[j].Name
		}
		return out[i].App < out[j].App
	})
	return out
}

func plural(n int, noun string) string {
	if n == 1 {
		return "1 " + noun
	}
	return strconv.Itoa(n) + " " + noun + "s"
}
