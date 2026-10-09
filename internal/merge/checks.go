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

// BucketOf reads a check's state (ghx.Check.State) for the gate. Pure.
//
//   - Commit status states (StatusState): SUCCESS, PENDING, EXPECTED, FAILURE,
//     ERROR.
//   - A check run's status until it completes (CheckStatusState): QUEUED,
//     IN_PROGRESS, WAITING, PENDING, REQUESTED.
//   - Its conclusion after (CheckConclusionState): SUCCESS, NEUTRAL, SKIPPED,
//     FAILURE, CANCELLED, TIMED_OUT, ACTION_REQUIRED, STARTUP_FAILURE, STALE.
//
// ⚠ Every finished state that is not a pass is "failed", CANCELLED included:
// `--admin` merges past a red required check, so the gate is the only thing
// that would stop it. STALE (GitHub gave up on a run left incomplete) is
// failed, not pending: it will never finish. Anything else, including a state
// GitHub adds later, is unknown, and unknown blocks.
func BucketOf(state string) CheckBucket {
	switch strings.ToUpper(strings.TrimSpace(state)) {
	case "SUCCESS":
		return BucketPassed
	case "SKIPPED", "NEUTRAL":
		return BucketSkipped
	case "PENDING", "EXPECTED", "QUEUED", "IN_PROGRESS", "WAITING", "REQUESTED":
		return BucketPending
	case "FAILURE", "ERROR", "CANCELLED", "TIMED_OUT", "ACTION_REQUIRED", "STARTUP_FAILURE", "STALE":
		return BucketFailed
	}
	return BucketUnknown
}

// ChecksInput is what the gate decides on (#179).
type ChecksInput struct {
	ReadErr        string      // the checks could not be read: why ("" = they were)
	Checks         []ghx.Check // the newest of each check on the head commit
	Required       []string    // what branch protection and rulesets require
	RequiredUnread []string    // where the required checks could not be read from, and why
	MinChecks      int         // merge_min_checks: fewer checks ran blocks; 0 = no floor
	MinChecksBad   string      // merge_min_checks set to something that is not a count
}

// ChecksVerdict is the gate's decision and everything it rests on.
type ChecksVerdict struct {
	Status         ChecksStatus
	ReadErr        string
	Counts         map[CheckBucket]int
	Pending        []ghx.Check
	Failed         []ghx.Check
	Unknown        []ghx.Check
	Missing        []string // required, and no check of that name is on the head
	Required       int      // how many checks are required
	Ran            int      // checks that ran or are running: all but SKIPPED
	Floor          int      // the merge_min_checks Ran fell short of; 0 = met, or none set
	FloorBad       string   // merge_min_checks is not a count: its value
	RequiredUnread []string
}

// DecideChecks is merge-pr's checks gate (#179). Pure.
//
// It blocks when the checks could not be read, any check is pending, failed or
// in a state wt does not know, a required check has no run on the head,
// fewer checks ran than merge_min_checks (or that setting is not a count), or
// the required checks could not be read: a failed read never counts as green.
// Otherwise it is green, or none when no check ran at all (zero checks, or
// every one SKIPPED).
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
	present := map[string]bool{}
	for _, c := range in.Checks {
		present[c.Name] = true
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
	v.Required = len(in.Required)
	for _, r := range in.Required {
		if !present[r] {
			v.Missing = append(v.Missing, r)
		}
	}
	sort.Strings(v.Missing)
	if in.MinChecks > 0 && v.Ran < in.MinChecks {
		v.Floor = in.MinChecks
	}
	v.FloorBad = in.MinChecksBad
	v.RequiredUnread = in.RequiredUnread
	switch {
	case len(v.Pending)+len(v.Failed)+len(v.Unknown)+len(v.Missing) > 0,
		v.Floor > 0, v.FloorBad != "", len(v.RequiredUnread) > 0:
		v.Status = ChecksBlocked
	case v.Ran == 0:
		v.Status = ChecksNone
	default:
		v.Status = ChecksGreen
	}
	return v
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

// RequiredChecks merges the two sources of required checks (#179): branch
// protection and the active rulesets on the base branch. names is their union;
// unread says which source could not be read, and why, so the gate can refuse
// rather than call a missing required check absent. A server with no rulesets
// for the repo (ghx.ErrRulesetsUnavailable: a 404, or a private repo on a plan
// without them) has none to require, which is an answer, not a failed read.
// Pure.
func RequiredChecks(protection []string, protectionErr error, rulesets []string, rulesetsErr error) (names, unread []string) {
	seen := map[string]bool{}
	add := func(ns []string) {
		for _, n := range ns {
			if !seen[n] {
				seen[n] = true
				names = append(names, n)
			}
		}
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
	sort.Strings(names)
	return names, unread
}

func plural(n int, noun string) string {
	if n == 1 {
		return "1 " + noun
	}
	return strconv.Itoa(n) + " " + noun + "s"
}
