// Package doctor runs preflight checks for wt.
package doctor

import (
	"github.com/eharriett0/wt/internal/config"
	"github.com/eharriett0/wt/internal/ghx"
	"github.com/eharriett0/wt/internal/gitx"
	"github.com/eharriett0/wt/internal/ui"
)

// Run prints a checklist and returns a process exit code (0 healthy, 1 if a
// hard prerequisite is missing).
func Run(c *config.Config) int {
	ui.Banner("wt doctor")
	hardOK := true

	if gitx.Present() {
		ui.OK("git — found")
	} else {
		ui.Err("git — NOT FOUND on PATH (required)")
		hardOK = false
	}

	if c != nil && c.Root != "" {
		ui.OK("repo — %s", c.Root)
	} else {
		ui.Err("repo — not inside a git repository")
		hardOK = false
	}

	if ghx.Present() {
		if ghx.Authed() {
			ui.OK("gh — authenticated")
		} else {
			ui.Warn("gh — found but NOT authenticated (claim/release/merge-pr need `gh auth login`)")
		}
	} else {
		ui.Warn("gh — not found (claim/release/merge-pr need it; new/clean/status/check/hooks don't)")
	}

	if c != nil && c.Root != "" {
		ui.Info("base branch:   %s", c.Base)
		ui.Info("worktree root: %s", c.WorktreeRoot)
		ui.Info("active-work:   %s", c.ActiveWork)
		ui.Info("branch prefix: %s", c.Prefix)
	}

	if !hardOK {
		return 1
	}
	return 0
}
