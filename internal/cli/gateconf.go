package cli

import (
	"fmt"
	"reflect"
	"strings"
	"time"

	"github.com/eharriett0/wt/internal/config"
	"github.com/eharriett0/wt/internal/ghx"
	"github.com/eharriett0/wt/internal/gitx"
	"github.com/eharriett0/wt/internal/ui"
)

// gateSource is one set of gate settings merge-pr's gates read (#223), and
// where it came from.
type gateSource struct {
	from string
	gate config.Gate
	// unread is why the base branch's settings could not be read ("" = they
	// were). Such a source FAILS CLOSED: merge_is_deploy on for the whole
	// repository, a checks floor nobody can apply (the checks gate refuses, or
	// asks at a terminal, without --checks-ok), and holds kept at least the
	// default hold_max_age.
	unread string
}

// localBaseConf is this clone's copy of the base branch's .wt.conf, read from
// origin/<base> after a fetch: merge-pr's fallback when GitHub can't be read.
type localBaseConf struct {
	ref      string // refs/remotes/origin/<base>
	fetchErr error  // the fetch failed: the copy is as last fetched
	err      error  // the ref does not resolve, or the file can't be read
	text     string
	found    bool
}

// baseGateSource decides where the base branch's gate settings come from
// (#223), and what to warn. The checkout merge-pr runs in is usually the PR's
// own worktree, so its .wt.conf is the branch being merged: the base's copy is
// what the PR can't loosen.
//
//   - GitHub's copy at the PR's base branch, read just now: the file, or no
//     file (then the base holds the defaults: nothing on, no floor, holds at
//     least 24h; nothing is warned).
//   - GitHub unread, but the PR's base branch and repository known, and that
//     repository is this clone's origin (originIsBase): this clone's
//     origin/<base>, fetched first (local.fetchErr says whether that worked;
//     if not, the copy is as last fetched, and the warning says it may be
//     stale).
//   - Anything else FAILS CLOSED (unread): the base branch is not known (gh
//     could not name the PR), the PR's repository is not this clone's origin
//     (-R, a PR URL, GH_REPO, gh's default repository in a fork), or this
//     clone's copy can't be read. ⚠ Never the checkout's `base` key: the PR's
//     own .wt.conf sets it (#223 review).
//
// Pure.
func baseGateSource(fresh ghx.BaseConf, freshErr error, originIsBase bool, local localBaseConf) (gateSource, string) {
	if freshErr == nil {
		at := fresh.Commit
		if len(at) > 7 {
			at = at[:7]
		}
		if !fresh.Found {
			return gateSource{from: fmt.Sprintf("%s on GitHub, which has no %s", fresh.Base, ghx.ConfFile), gate: config.GateFromConf("")}, ""
		}
		return gateSource{from: fmt.Sprintf("%s's %s on GitHub (at %s)", fresh.Base, ghx.ConfFile, at), gate: config.GateFromConf(fresh.Text)}, ""
	}
	closed := func(why string) (gateSource, string) {
		return gateSource{from: "the base branch's " + ghx.ConfFile + ", which could not be read",
				gate: config.Gate{MergeIsDeploy: true, HoldMaxAge: config.DefaultHoldMaxAge}, unread: why},
			fmt.Sprintf("couldn't read the base branch's %s: %s. merge-pr can't rule out that this merge deploys or "+
				"that the base sets a checks floor, so the deploy gate gates and the checks gate refuses without --checks-ok", ghx.ConfFile, why)
	}
	switch {
	case fresh.Base == "":
		return closed(fmt.Sprintf("the PR's base branch could not be resolved (%v)", freshErr))
	case !originIsBase:
		return closed(fmt.Sprintf("GitHub did not answer (%v), and the PR's repository %s is not this clone's origin", freshErr, fresh.Repo()))
	case local.err != nil:
		return closed(fmt.Sprintf("GitHub did not answer (%v), and this clone's copy can't be read (%v)", freshErr, local.err))
	}
	name := strings.TrimPrefix(local.ref, "refs/remotes/")
	when, warn := "fetched just now", fmt.Sprintf("couldn't read the base branch's %s from GitHub (%v); read it from this clone's %s, fetched just now",
		ghx.ConfFile, freshErr, name)
	if local.fetchErr != nil {
		when = "as last fetched"
		warn = fmt.Sprintf("couldn't read the base branch's %s from GitHub (%v) or fetch %s (%v); using this clone's copy as last fetched, which may be stale",
			ghx.ConfFile, freshErr, name, local.fetchErr)
	}
	if !local.found {
		return gateSource{from: fmt.Sprintf("%s %s, which has no %s", name, when, ghx.ConfFile), gate: config.GateFromConf("")}, warn
	}
	return gateSource{from: fmt.Sprintf("%s's %s %s", name, ghx.ConfFile, when), gate: config.GateFromConf(local.text)}, warn
}

// floorSpec is the checks floor of every source together (#223).
type floorSpec struct {
	n       int    // the highest merge_min_checks
	bad     string // a value that is not a count, from any source (refused, #179)
	badFrom string // which source it came from
	unread  string // a source whose settings could not be read: no floor can be applied
}

// strictestFloor is the checks floor of every source together (#223): the
// highest merge_min_checks; a value that is not a count from any of them (the
// gate refuses for it, as for this checkout's own, #179); and an unread
// source, whose floor is unknown (the gate refuses for that too). Pure.
func strictestFloor(srcs []gateSource) floorSpec {
	var f floorSpec
	for _, s := range srcs {
		if s.unread != "" {
			if f.unread == "" {
				f.unread = s.unread
			}
			continue
		}
		if s.gate.MergeMinChecks > f.n {
			f.n = s.gate.MergeMinChecks
		}
		if f.bad == "" && s.gate.MergeMinChecksBad != "" {
			f.bad, f.badFrom = s.gate.MergeMinChecksBad, s.from
		}
	}
	if f.bad != "" {
		f.n = 0
	}
	return f
}

// strictestHolds is the hold gate's settings over every source (#223):
// hold_max_age 0 (never expire) when any says so, else the longest; and every
// coord_issue mirror any names, so a PR can neither shorten a hold's life nor
// drop the mirror that carries another machine's hold. A base without a
// .wt.conf holds at the default (24h), so the checkout's env or file can't
// shorten it either; an unread base counts as that default too (#223
// re-review: skipped, the PR's own hold_max_age decided), and names no mirror.
func strictestHolds(srcs []gateSource) (time.Duration, []int) {
	var age time.Duration
	never, any := false, false
	var issues []int
	for _, s := range srcs {
		any = true
		if s.gate.HoldMaxAge <= 0 {
			never = true
		} else if s.gate.HoldMaxAge > age {
			age = s.gate.HoldMaxAge
		}
		if n := s.gate.CoordIssue; n > 0 && !containsInt(issues, n) {
			issues = append(issues, n)
		}
	}
	switch {
	case never:
		age = 0
	case !any:
		age = config.DefaultHoldMaxAge
	}
	return age, issues
}

func containsInt(xs []int, x int) bool {
	for _, y := range xs {
		if y == x {
			return true
		}
	}
	return false
}

// gateNeedsFiles reports whether deciding the deploy gate needs the PR's file
// list: some source turns it on and scopes it with paths, and none turns it on
// for the whole repository. Pure.
func gateNeedsFiles(srcs []gateSource) bool {
	scoped := false
	for _, s := range srcs {
		if !s.gate.MergeIsDeploy {
			continue
		}
		if len(s.gate.MergeIsDeployPaths) == 0 {
			return false
		}
		scoped = true
	}
	return scoped
}

// deployDecision decides the merge==deploy gate over every source (#223): it
// fires when ANY source turns merge_is_deploy on and gates this PR, so a PR
// can't loosen the base's gate, and one that tightens it is held to its own.
// A source gates the whole repository when merge_is_deploy_paths is unset,
// else a PR changing a file one of its globs matches (anyDeployPath, `!`
// exclusions included). A list that couldn't be read, or an empty one, gates
// (fail CLOSED, as before #223). by is the source that fired ("" when none
// did, or when it was the file list that couldn't be read). Pure.
func deployDecision(srcs []gateSource, files []string, filesErr error) (fires bool, by string) {
	on := false
	for _, s := range srcs {
		if !s.gate.MergeIsDeploy {
			continue
		}
		on = true
		if len(s.gate.MergeIsDeployPaths) == 0 {
			return true, s.from
		}
	}
	switch {
	case !on:
		return false, ""
	case filesErr != nil, len(files) == 0:
		return true, ""
	}
	for _, s := range srcs {
		if s.gate.MergeIsDeploy && anyDeployPath(files, s.gate.MergeIsDeployPaths) {
			return true, s.from
		}
	}
	return false, ""
}

// readSources is srcs without the ones that could not be read. Pure.
func readSources(srcs []gateSource) []gateSource {
	var out []gateSource
	for _, s := range srcs {
		if s.unread == "" {
			out = append(out, s)
		}
	}
	return out
}

// gateSummary words one source's settings for the merge output: the globs
// themselves, so two different lists never read alike. Pure.
func gateSummary(s gateSource) string {
	if s.unread != "" {
		return fmt.Sprintf("%s (%s): taken as merge_is_deploy on (whole repository), merge_min_checks unknown, hold_max_age at least %s (the default)",
			s.from, s.unread, config.DefaultHoldMaxAge)
	}
	g := s.gate
	deploy := "merge_is_deploy off"
	if g.MergeIsDeploy {
		deploy = "merge_is_deploy on"
	}
	switch {
	case len(g.MergeIsDeployPaths) > 0:
		deploy += " [merge_is_deploy_paths " + strings.Join(g.MergeIsDeployPaths, ",") + "]"
	case g.MergeIsDeploy:
		deploy += " (whole repository)"
	}
	holds := "hold_max_age never"
	if g.HoldMaxAge > 0 {
		holds = "hold_max_age " + g.HoldMaxAge.String()
	}
	if g.CoordIssue > 0 {
		holds += fmt.Sprintf(", coord_issue #%d", g.CoordIssue)
	}
	return fmt.Sprintf("%s: %s, merge_min_checks %s, %s", s.from, deploy,
		config.MinChecksString(g.MergeMinChecks, g.MergeMinChecksBad), holds)
}

// gatesDiffer reports whether the sources' settings are not all the same (an
// unread source always differs). Pure.
func gatesDiffer(srcs []gateSource) bool {
	for _, s := range srcs {
		if s.unread != "" {
			return true
		}
	}
	for i := 1; i < len(srcs); i++ {
		if !reflect.DeepEqual(srcs[i].gate, srcs[0].gate) {
			return true
		}
	}
	return false
}

// fetchBase fetches origin's <base> into refs/remotes/origin/<base>: the
// fallback's first step, never stopping at a credential prompt. A variable so
// a test never reaches a network.
var fetchBase = func(base string) error { return gitx.FetchNoPrompt("origin", base) }

// readBaseConf is ghx.ReadBaseConf, a variable for tests.
var readBaseConf = ghx.ReadBaseConf

// mergeGates resolves the gate settings merge-pr's gates read (#223): this
// checkout's (its .wt.conf and the WT_* environment) and the PR's base
// branch's (baseGateSource). It warns when the base had to come from this
// clone or fail closed, and names both sources when they differ, or always on
// a dry run. ⚠ The fallback reads the base branch GitHub NAMED for this PR,
// and only when the PR's repository is this clone's origin (owner and name,
// case-insensitive: a host can be an ssh alias), never c.Base.
func mergeGates(c *config.Config, pr, repo string, dryRun bool) []gateSource {
	fresh, freshErr := readBaseConf(pr, repo)
	if retryable(freshErr) {
		// One retry, as the checks gate's reads get (#223 re-review): a single
		// 502 would otherwise fail closed. A rate limit lifts in minutes, not
		// here.
		first := fresh
		liveChecksReads.sleep(checksRetryWait)
		fresh, freshErr = readBaseConf(pr, repo)
		if freshErr != nil && fresh.Base == "" && first.Base != "" {
			// The retry failed earlier than the first try did: keep what the
			// first one resolved, so the fallback still reads the PR's base
			// (#223 final review).
			fresh.Base, fresh.Owner, fresh.Name, fresh.Host = first.Base, first.Owner, first.Name, first.Host
		}
	}
	var local localBaseConf
	originIsBase := false
	if freshErr != nil && fresh.Base != "" {
		_, owner, name := ghx.OriginRepo()
		originIsBase = owner != "" && strings.EqualFold(owner, fresh.Owner) && strings.EqualFold(name, fresh.Name)
		if originIsBase {
			local.ref = "refs/remotes/origin/" + fresh.Base
			local.fetchErr = fetchBase(fresh.Base)
			local.text, local.found, local.err = gitx.RefFileText(local.ref, ghx.ConfFile)
		}
	}
	baseSrc, warn := baseGateSource(fresh, freshErr, originIsBase, local)
	srcs := []gateSource{{from: "this checkout's " + ghx.ConfFile + " and WT_* environment", gate: c.Gate()}, baseSrc}
	if warn != "" {
		ui.Warn("%s", warn)
	}
	if dryRun || gatesDiffer(srcs) {
		if gatesDiffer(srcs) {
			ui.Info("merge-pr's gate settings differ between this checkout and the base branch; the stricter of the two applies:")
		} else {
			ui.Info("merge-pr's gate settings:")
		}
		for _, s := range srcs {
			ui.Info("  %s", gateSummary(s))
		}
	}
	return srcs
}

// deployGateApplies reports whether the merge==deploy prod gate fires for PR
// pr (in repo when not "": a forwarded -R) under the sources' settings
// (deployDecision), reading the PR's file list only when the sources that
// were read need it. unsure is set when the gate fires only because a source
// could not be read: then nothing says the merge deploys, only that it can't
// be ruled out, and the banner says so. srcs[0] is this checkout's: when the
// gate fires only because of another source, it says which.
func deployGateApplies(srcs []gateSource, pr, repo string) (fires bool, unsure string) {
	read := readSources(srcs)
	var files []string
	var err error
	listed := gateNeedsFiles(read)
	if listed {
		files, err = ghx.PRChangedFiles(pr, repo)
	}
	fires, by := deployDecision(srcs, files, err)
	if !fires {
		if gateOn(srcs) {
			ui.Info("PR #%s changes no deploy-path files (merge_is_deploy_paths) — skipping the prod gate", pr)
		}
		return false, ""
	}
	if err != nil {
		ui.Warn("merge_is_deploy_paths set but couldn't list PR #%s files (%v) — gating to be safe", pr, err)
	}
	firesRead, byRead := deployDecision(read, files, err)
	if !firesRead {
		for _, s := range srcs {
			if s.unread != "" {
				return true, s.unread
			}
		}
	}
	if byRead != "" {
		by = byRead
	}
	self := srcs[0].gate
	selfSkips := !self.MergeIsDeploy
	if self.MergeIsDeploy && len(self.MergeIsDeployPaths) > 0 && listed {
		selfSkips, _ = deployDecision(srcs[:1], files, err)
		selfSkips = !selfSkips
	}
	if selfSkips && by != "" {
		ui.Info("PR #%s gates under %s; this checkout's settings alone would skip the prod gate", pr, by)
	}
	return true, ""
}

// gateOn reports whether any source turns merge_is_deploy on. Pure.
func gateOn(srcs []gateSource) bool {
	for _, s := range srcs {
		if s.gate.MergeIsDeploy {
			return true
		}
	}
	return false
}
