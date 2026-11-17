package cli

import (
	"fmt"
	"reflect"
	"strings"

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
}

// localBaseConf is this clone's copy of the base branch's .wt.conf, read from
// origin/<base> as last fetched: merge-pr's fallback when GitHub can't be read.
type localBaseConf struct {
	ref   string // refs/remotes/origin/<base>
	err   error  // the ref does not resolve, or the file can't be read
	text  string
	found bool
}

// baseGateSource decides where the base branch's gate settings come from
// (#223), and what to warn. The checkout merge-pr runs in is usually the PR's
// own worktree, so its .wt.conf is the branch being merged: the base's copy is
// what the PR can't loosen.
//
//   - GitHub's copy at the PR's base branch, read just now: the file, or no
//     file (then the base sets nothing, and nothing is warned).
//   - GitHub unread, no forwarded -R: this clone's origin/<base>, as last
//     fetched, with a warning that it may be stale.
//   - Neither, or a forwarded -R (this clone is another repository): FAIL
//     CLOSED. The base may turn the deploy gate on, so it is taken as on for
//     the whole repository; its checks floor is unknown and is not invented.
//
// Pure.
func baseGateSource(fresh ghx.BaseConf, freshErr error, forwarded bool, local localBaseConf) (gateSource, string) {
	if freshErr == nil {
		at := fresh.Commit
		if len(at) > 7 {
			at = at[:7]
		}
		from := fmt.Sprintf("%s's %s on GitHub (at %s)", fresh.Base, ghx.ConfFile, at)
		if !fresh.Found {
			return gateSource{from: fmt.Sprintf("%s on GitHub, which has no %s", fresh.Base, ghx.ConfFile)}, ""
		}
		return gateSource{from: from, gate: config.GateFromConf(fresh.Text)}, ""
	}
	closed := gateSource{from: "the base branch's " + ghx.ConfFile + ", which could not be read", gate: config.Gate{MergeIsDeploy: true}}
	if forwarded {
		return closed, fmt.Sprintf("couldn't read the base branch's %s from GitHub (%v), and this clone is not the PR's repository (-R): "+
			"the deploy gate can't rule out that this merge deploys, so it gates", ghx.ConfFile, freshErr)
	}
	name := strings.TrimPrefix(local.ref, "refs/remotes/")
	if local.err != nil {
		return closed, fmt.Sprintf("couldn't read the base branch's %s from GitHub (%v) or from this clone (%v): "+
			"the deploy gate can't rule out that this merge deploys, so it gates", ghx.ConfFile, freshErr, local.err)
	}
	warn := fmt.Sprintf("couldn't read the base branch's %s from GitHub (%v); using this clone's %s as last fetched, which may be stale",
		ghx.ConfFile, freshErr, name)
	if !local.found {
		return gateSource{from: fmt.Sprintf("%s as last fetched, which has no %s", name, ghx.ConfFile)}, warn
	}
	return gateSource{from: fmt.Sprintf("%s's %s as last fetched", name, ghx.ConfFile), gate: config.GateFromConf(local.text)}, warn
}

// strictestFloor is the checks floor of every source together (#223): the
// highest merge_min_checks, and a value that is not a count from any of them
// (the gate refuses for it, as for this checkout's own, #179). Pure.
func strictestFloor(srcs []gateSource) (int, string) {
	n, bad := 0, ""
	for _, s := range srcs {
		if s.gate.MergeMinChecks > n {
			n = s.gate.MergeMinChecks
		}
		if bad == "" && s.gate.MergeMinChecksBad != "" {
			bad = s.gate.MergeMinChecksBad
		}
	}
	if bad != "" {
		n = 0
	}
	return n, bad
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
// exclusions included). The file list is read only when gateNeedsFiles says
// so; a list that couldn't be read, or an empty one, gates (fail CLOSED, as
// before #223). by is the source that fired ("" when none did, or when it was
// the file list that couldn't be read). Pure.
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

// gateSummary words one source's settings for the merge output. Pure.
func gateSummary(s gateSource) string {
	deploy := "merge_is_deploy off"
	switch n := len(s.gate.MergeIsDeployPaths); {
	case s.gate.MergeIsDeploy && n == 0:
		deploy = "merge_is_deploy on (whole repository)"
	case s.gate.MergeIsDeploy:
		deploy = fmt.Sprintf("merge_is_deploy on (%d merge_is_deploy_paths glob(s))", n)
	}
	return fmt.Sprintf("%s: %s, merge_min_checks %s", s.from, deploy,
		config.MinChecksString(s.gate.MergeMinChecks, s.gate.MergeMinChecksBad))
}

// gatesDiffer reports whether the sources' settings are not all the same. Pure.
func gatesDiffer(srcs []gateSource) bool {
	for _, s := range srcs[1:] {
		if !reflect.DeepEqual(s.gate, srcs[0].gate) {
			return true
		}
	}
	return false
}

// mergeGates resolves the gate settings merge-pr's gates read (#223): this
// checkout's (its .wt.conf and the WT_* environment) and the PR's base
// branch's (baseGateSource). It warns when the base had to come from a stale
// copy or fail closed, and names both sources when they differ, or always on a
// dry run.
func mergeGates(c *config.Config, pr, repo string, dryRun bool) []gateSource {
	fresh, freshErr := ghx.ReadBaseConf(pr, repo)
	var local localBaseConf
	if freshErr != nil && repo == "" {
		base := fresh.Base
		if base == "" {
			base = c.Base
		}
		local.ref = "refs/remotes/origin/" + base
		local.text, local.found, local.err = gitx.RefFileText(local.ref, ghx.ConfFile)
	}
	baseSrc, warn := baseGateSource(fresh, freshErr, repo != "", local)
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
// pr under the sources' settings (deployDecision), reading the PR's file list
// only when the decision needs it. srcs[0] is this checkout's: when the gate
// fires only because of another source, it says which.
func deployGateApplies(srcs []gateSource, pr string) bool {
	var files []string
	var err error
	if gateNeedsFiles(srcs) {
		files, err = ghx.PRChangedFiles(pr)
	}
	fires, by := deployDecision(srcs, files, err)
	if !fires {
		if gateOn(srcs) {
			ui.Info("PR #%s changes no deploy-path files (merge_is_deploy_paths) — skipping the prod gate", pr)
		}
		return false
	}
	if err != nil {
		ui.Warn("merge_is_deploy_paths set but couldn't list PR #%s files (%v) — gating to be safe", pr, err)
	}
	if self, _ := deployDecision(srcs[:1], files, err); !self && by != "" {
		ui.Info("PR #%s gates under %s; this checkout's settings alone would skip the prod gate", pr, by)
	}
	return true
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
