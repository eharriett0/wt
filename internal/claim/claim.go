// Package claim implements the claim/release ritual: assign a GitHub issue,
// create a worktree, open a draft PR, and record the claim so parallel windows
// can see it.
package claim

import (
	"fmt"
	"os"
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

// Claim adopts issue for the current window.
func Claim(c *config.Config, issue string, force, openPR bool) error {
	if !issueRe.MatchString(issue) {
		return fmt.Errorf("issue must be a positive integer, got %q", issue)
	}
	if !ghx.IssueExists(issue) {
		return fmt.Errorf("issue #%s not found in this repo (is gh authed? `wt doctor`)", issue)
	}
	if state, _ := ghx.IssueState(issue); state != "OPEN" {
		return fmt.Errorf("issue #%s is %s, not OPEN", issue, state)
	}
	if a := ghx.IssueAssignees(issue); len(a) > 0 && !force {
		ui.Warn("issue #%s already assigned to: %s", issue, strings.Join(a, ", "))
		ui.Warn("another window may be working on this — override with: wt claim %s --force", issue)
		return fmt.Errorf("already assigned")
	}

	if err := ghx.IssueAddAssigneeMe(issue); err != nil {
		return fmt.Errorf("assign issue: %w", err)
	}
	ui.OK("assigned #%s to @me", issue)

	title, _ := ghx.IssueTitle(issue)
	branch := BranchName(c.Prefix, issue, SlugFromTitle(title))

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
		PRURL: prURL, Window: windowID(), When: time.Now(),
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

// Release clears the claim's active-work entry and unassigns the issue. Leaves
// the worktree + draft PR in place.
func Release(c *config.Config, issue string) error {
	if !issueRe.MatchString(issue) {
		return fmt.Errorf("issue must be a positive integer, got %q", issue)
	}
	if content := activework.Read(c.ActiveWork); content != "" {
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
	ui.Banner(fmt.Sprintf("Released #%s", issue))
	ui.Info("worktree + draft PR left in place — clean later with `wt clean` / `gh pr close`")
	return nil
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
