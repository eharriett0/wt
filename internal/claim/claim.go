// Package claim implements the claim/release ritual: assign a GitHub issue,
// create a worktree, open a draft PR, and record the claim so parallel windows
// can see it.
package claim

import (
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"time"

	"github.com/eharriett0/wt/internal/activework"
	"github.com/eharriett0/wt/internal/config"
	"github.com/eharriett0/wt/internal/ghx"
	"github.com/eharriett0/wt/internal/gitx"
	"github.com/eharriett0/wt/internal/ui"
	"github.com/eharriett0/wt/internal/worktree"
)

var issueRe = regexp.MustCompile(`^[0-9]+$`)

// Claim adopts issue for the current window. epic (optional) tags the claim for
// cross-repo grouping (wt status --epic).
func Claim(c *config.Config, issue string, force, openPR bool, epic string) error {
	if !issueRe.MatchString(issue) {
		return fmt.Errorf("issue must be a positive integer, got %q", issue)
	}
	if !ghx.IssueExists(issue) {
		return fmt.Errorf("issue #%s not found in this repo (is gh authed? `wt doctor`)", issue)
	}
	if state, _ := ghx.IssueState(issue); state != "OPEN" {
		return fmt.Errorf("issue #%s is %s, not OPEN", issue, state)
	}

	assignees := ghx.IssueAssignees(issue)
	title, _ := ghx.IssueTitle(issue)
	branch := BranchName(c.Prefix, issue, SlugFromTitle(title))
	wtPath := filepath.Join(c.WorktreeRoot, strings.ReplaceAll(branch, "/", "-"))

	// Resume path (#41): an owned re-claim — this window is assigned, its
	// worktree still exists, and there's an active-work section. Refresh the
	// Last-seen timestamp and hand the worktree back, WITHOUT stacking a second
	// placeholder commit / draft PR / duplicate section. No --force needed.
	if user, _ := ghx.CurrentUser(); assignedTo(assignees, user) && isDir(wtPath) && hasSection(c, issue) {
		content := activework.Read(c.ActiveWork)
		e := activework.Entry{Issue: issue, Title: title, Branch: branch, Worktree: wtPath, Window: windowID(), Epic: epic, When: time.Now()}
		if err := activework.Write(c.ActiveWork, activework.UpsertSection(content, e)); err != nil {
			ui.Warn("active-work refresh failed (continuing): %v", err)
		}
		ui.OK("resumed #%s — worktree + claim already yours, refreshed Last-seen", issue)
		ui.Banner(fmt.Sprintf("Resumed #%s", issue))
		ui.Info("branch:   %s", branch)
		ui.Info("worktree: %s", wtPath)
		fmt.Println()
		ui.Step("cd %s", wtPath)
		return nil
	}

	if len(assignees) > 0 && !force {
		ui.Warn("issue #%s already assigned to: %s", issue, strings.Join(assignees, ", "))
		ui.Warn("another window may be working on this — override with: wt claim %s --force", issue)
		return fmt.Errorf("already assigned")
	}

	if err := ghx.IssueAddAssigneeMe(issue); err != nil {
		return fmt.Errorf("assign issue: %w", err)
	}
	ui.OK("assigned #%s to @me", issue)

	wtDir, err := worktree.New(c, branch)
	if err != nil {
		return err
	}

	title60 := truncate(title, 60)
	msg := fmt.Sprintf("WIP: claim #%s — %s\n\nPlaceholder commit for multi-window coordination (wt claim).\nReplaced by real work in subsequent commits.\n\nRefs #%s", issue, title60, issue)
	if err := gitx.CommitEmpty(wtDir, msg); err != nil {
		return fmt.Errorf("placeholder commit: %w", err)
	}
	if err := gitx.PushSetUpstream(wtDir, branch); err != nil {
		return fmt.Errorf("push branch: %w", err)
	}

	prURL := ""
	if openPR {
		body := fmt.Sprintf("Claimed at %s for multi-window coordination.\n\n- Issue: #%s\n- Worktree: `%s`\n\nThis draft PR signals intent to parallel windows. Others should run `wt status` (or check `gh pr list --draft`) before working on colliding scope. Mark ready when complete, or `wt release %s` to abandon.\n\nRefs #%s",
			time.Now().UTC().Format(time.RFC3339), issue, wtDir, issue, issue)
		if url, err := ghx.PRCreate(true, branch, c.Base, "WIP: #"+issue+" — "+title60, body); err != nil {
			ui.Warn("draft PR creation failed (continuing): %v", err)
		} else {
			prURL = url
			ui.OK("draft PR: %s", prURL)
		}
	}

	entry := activework.Entry{
		Issue: issue, Title: title, Branch: branch, Worktree: wtDir,
		PRURL: prURL, Window: windowID(), Epic: epic, When: time.Now(),
	}
	if err := activework.Write(c.ActiveWork, activework.AppendSection(activework.Read(c.ActiveWork), entry)); err != nil {
		ui.Warn("active-work update failed (continuing): %v", err)
	} else {
		ui.OK("recorded claim in active-work")
	}

	ui.Banner(fmt.Sprintf("Claimed #%s", issue))
	ui.Info("branch:   %s", branch)
	ui.Info("worktree: %s", wtDir)
	if prURL != "" {
		ui.Info("draft PR: %s", prURL)
	}
	fmt.Println()
	ui.Step("cd %s", wtDir)
	ui.Step("when done: mark the PR ready, or `wt release %s`", issue)
	return nil
}

// Release clears the claim's active-work entry and unassigns the issue. With
// clean, it ALSO removes the worktree when the branch is abandoned — clean tree,
// no open/merged PR, only WIP placeholder commits (#42) — so releasing actually
// frees the slot instead of leaving an orphan `wt clean` can never sweep.
func Release(c *config.Config, issue string, clean bool) error {
	if !issueRe.MatchString(issue) {
		return fmt.Errorf("issue must be a positive integer, got %q", issue)
	}

	// Capture the recorded branch/worktree BEFORE we drop the section (#42).
	var recorded activework.Entry
	content := activework.Read(c.ActiveWork)
	for _, e := range activework.Parse(content) {
		if e.Issue == issue {
			recorded = e
			break
		}
	}

	if content != "" {
		if newC, changed := activework.RemoveSection(content, issue); changed {
			if err := activework.Write(c.ActiveWork, newC); err != nil {
				ui.Warn("active-work update failed: %v", err)
			} else {
				ui.OK("removed #%s from active-work", issue)
			}
		} else {
			ui.Info("no active-work entry for #%s", issue)
		}
	}
	if user, err := ghx.CurrentUser(); err == nil && user != "" {
		if err := ghx.IssueRemoveAssignee(issue, user); err == nil {
			ui.OK("unassigned #%s from %s", issue, user)
		} else {
			ui.Info("couldn't unassign #%s (issue may be closed, or you weren't assigned)", issue)
		}
	}

	if clean {
		cleanAbandonedWorktree(c, recorded)
	}

	ui.Banner(fmt.Sprintf("Released #%s", issue))
	if !clean {
		ui.Info("worktree + draft PR left in place — remove with `wt release %s --clean` or `wt clean`", issue)
	}
	return nil
}

// cleanAbandonedWorktree removes the released claim's worktree iff it's under
// the worktree root, clean, and abandoned (no live PR, WIP-only commits). Any
// non-abandoned/dirty case is reported, never forced.
func cleanAbandonedWorktree(c *config.Config, e activework.Entry) {
	if e.Branch == "" || e.Worktree == "" {
		ui.Info("--clean: no recorded worktree/branch for this claim — nothing to remove")
		return
	}
	if !isDir(e.Worktree) {
		ui.Info("--clean: worktree %s already gone", e.Worktree)
		_ = gitx.WorktreePrune()
		return
	}
	if !gitx.IsClean(e.Worktree) {
		ui.Warn("--clean: worktree %s has uncommitted changes — left in place", e.Worktree)
		return
	}
	subjects, serr := gitx.CommitSubjects("origin/"+c.Base, "refs/heads/"+e.Branch)
	if serr != nil {
		subjects, serr = gitx.CommitSubjects(c.Base, "refs/heads/"+e.Branch)
	}
	if serr != nil {
		ui.Warn("--clean: can't inspect %s vs %s (%v) — left in place", e.Branch, c.Base, serr)
		return
	}
	_, prOpen := ghx.OpenPRForBranch(e.Branch)
	prMerged := ghx.MergedPRForBranch(e.Branch)
	if !worktree.IsAbandonedBranch(subjects, prOpen, prMerged) {
		switch {
		case prMerged:
			ui.Info("--clean: %s has a MERGED PR — leave it for `wt clean`", e.Branch)
		case prOpen:
			ui.Info("--clean: %s still has an OPEN PR — not abandoned, left in place", e.Branch)
		default:
			ui.Info("--clean: %s has real (non-placeholder) commits — left in place", e.Branch)
		}
		return
	}
	if err := worktree.Remove(c, e.Worktree, e.Branch, false); err != nil {
		ui.Warn("--clean: couldn't remove worktree: %v", err)
	}
}

// assignedTo reports whether user (case-insensitive) is in assignees.
func assignedTo(assignees []string, user string) bool {
	if user == "" {
		return false
	}
	for _, a := range assignees {
		if strings.EqualFold(a, user) {
			return true
		}
	}
	return false
}

// hasSection reports whether the active-work file records a claim for issue.
func hasSection(c *config.Config, issue string) bool {
	for _, e := range activework.Parse(activework.Read(c.ActiveWork)) {
		if e.Issue == issue {
			return true
		}
	}
	return false
}

func isDir(p string) bool {
	fi, err := os.Stat(p)
	return err == nil && fi.IsDir()
}

// SlugFromTitle lowercases, collapses non-alphanumeric runs to single dashes,
// trims, and caps at 40 chars (matches the bash slug logic).
func SlugFromTitle(title string) string {
	var b strings.Builder
	prevDash := false
	for _, r := range strings.ToLower(title) {
		if (r >= 'a' && r <= 'z') || (r >= '0' && r <= '9') {
			b.WriteRune(r)
			prevDash = false
		} else if !prevDash {
			b.WriteByte('-')
			prevDash = true
		}
	}
	s := strings.Trim(b.String(), "-")
	if len(s) > 40 {
		s = s[:40]
	}
	return strings.TrimRight(s, "-")
}

// BranchName builds prefix+issue[-slug].
func BranchName(prefix, issue, slug string) string {
	if slug == "" {
		return prefix + issue
	}
	return prefix + issue + "-" + slug
}

func truncate(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n]
}

func windowID() string {
	if v := os.Getenv("WT_WINDOW"); v != "" {
		return v
	}
	if v := os.Getenv("TERM_SESSION_ID"); v != "" {
		return v
	}
	host, _ := os.Hostname()
	return fmt.Sprintf("%s-%d", host, os.Getpid())
}
