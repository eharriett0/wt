// Package claim implements the claim/release ritual: assign a GitHub issue,
// create a worktree, open a draft PR, and record the claim so parallel windows
// can see it.
package claim

import (
	"bufio"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"time"

	"github.com/eharriett0/wt/internal/activework"
	"github.com/eharriett0/wt/internal/config"
	"github.com/eharriett0/wt/internal/coord"
	"github.com/eharriett0/wt/internal/ghx"
	"github.com/eharriett0/wt/internal/gitx"
	"github.com/eharriett0/wt/internal/ui"
	"github.com/eharriett0/wt/internal/worktree"
)

var issueRe = regexp.MustCompile(`^[0-9]+$`)

// Claim adopts issue for the current window. epic (optional) tags the claim for
// cross-repo grouping (wt status --epic). yes skips the pre-claim confirmation
// (#157); force additionally overrides the already-assigned guard.
func Claim(c *config.Config, issue string, force, yes, openPR bool, epic string) error {
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

	user, _ := ghx.CurrentUser()

	// Resume path (#41): an owned re-claim — this window is assigned, its
	// worktree still exists, and there's an active-work section. Refresh the
	// Last-seen timestamp and hand the worktree back, WITHOUT stacking a second
	// placeholder commit / draft PR / duplicate section. No --force needed.
	if assignedTo(assignees, user) && isDir(wtPath) && hasSection(c, issue) {
		content := activework.Read(c.ActiveWork)
		e := activework.Entry{Issue: issue, Title: title, Branch: branch, Worktree: wtPath, Window: windowID(c), Epic: epic, When: time.Now()}
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

	// #134: the assign-check above only catches wt-claim-created PRs (claim
	// assigns the issue). A PR opened by hand — `gh pr create`, or `wt new` +
	// PR — that references the issue as `Refs #N` leaves it UNASSIGNED and is
	// NEVER a linked/closing reference, so pre-#134 a second `wt claim` sailed
	// past every guard and silently opened a DUPLICATE draft PR on a fresh
	// branch. Refuse instead: name the PR and point at `wt adopt`, which lands a
	// registered worktree on its branch. --force (already past the assign-check)
	// opens another deliberately. Best-effort — detection nil on gh error means
	// we fall through to the pre-#134 create-new behavior, never a hard block.
	if !force {
		if prs := ghx.OpenPRsReferencingIssue(issue); len(prs) > 0 {
			p := prs[0]
			draft := ""
			if p.IsDraft {
				draft = " (draft)"
			}
			ui.Warn("issue #%s already has open PR #%d%s on branch %q", issue, p.Number, draft, p.HeadRefName)
			if p.URL != "" {
				ui.Info("   %s", p.URL)
			}
			if len(prs) > 1 {
				ui.Warn("   (+%d more open PR(s) reference #%s)", len(prs)-1, issue)
			}
			ui.Step("get its worktree:  wt adopt %d", p.Number)
			ui.Step("or open another:   wt claim %s --force", issue)
			return fmt.Errorf("open PR #%d already references #%s (adopt it, or --force)", p.Number, issue)
		}
	}

	// #198: decide what to do with a local branch (or worktree) of this name
	// BEFORE anything is assigned, created or moved. An existing local branch is
	// re-attached (#62), and before #198 that happened unchecked: a branch left by
	// an earlier attempt, or one behind or diverged from origin/<branch>, got the
	// placeholder commit, the push was rejected, and the #159 rollback deleted the
	// branch. PlanNew checks it as `wt adopt` does (#167); a refusal here leaves no
	// partial claim behind, and Create acts on the plan after the assignment.
	plan, err := worktree.PlanNew(c, branch, worktree.NewFor{ClaimIssue: issue})
	if err != nil {
		return err
	}

	// #157: nothing above catches claiming the WRONG (unassigned) issue number —
	// the assign + dup-PR guards only fire on issues someone/something already
	// touched. Surface the title (a wrong number is obvious from it) and, on an
	// interactive terminal, confirm before assigning. --yes / --force skip it; an
	// agent/pipe (non-TTY) proceeds with the title shown either way.
	if !force && !yes && !confirmNewClaim(issue, title, promptInteractive(), os.Stdin) {
		return fmt.Errorf("claim cancelled (re-run with --yes to skip the prompt)")
	}

	if err := ghx.IssueAddAssigneeMe(issue); err != nil {
		return fmt.Errorf("assign issue: %w", err)
	}
	ui.OK("assigned #%s to @me", issue)
	// Undoing a failed claim unassigns only an assignment it made (#198 review):
	// a --force re-claim of an issue already yours keeps it.
	undo := claimUndo{issue: issue, user: user, assigned: !assignedTo(assignees, user)}

	// #159/#198 review: nothing durable is recorded until the push succeeds
	// (active-work is appended only after it), so every failure from here on
	// rolls back to a clean slate (the issue unassigned, and of the worktree and
	// branch only what this claim made), or a retry trips "already assigned".
	made, err := plan.Create()
	if err != nil {
		// Create leaves nothing to remove when it fails: it refused before acting,
		// or removed the worktree it added again (verifyAdopted). A branch it
		// fast-forwarded first only moved forward, onto origin/<branch>. Only the
		// assignment is left to undo.
		undo.unassign()
		return err
	}
	wtDir := made.Dir

	title60 := truncate(title, 60)
	msg := fmt.Sprintf("WIP: claim #%s — %s\n\nPlaceholder commit for multi-window coordination (wt claim).\nReplaced by real work in subsequent commits.\n\nRefs #%s", issue, title60, issue)
	before := gitx.HeadCommit(wtDir)
	if err := gitx.CommitEmpty(wtDir, msg); err != nil {
		// E.g. a worktree handed back in the middle of a merge, where the
		// placeholder must not conclude it (gitx.CommitEmpty).
		rollbackFailedClaim(c, branch, made, before, "", "placeholder commit failed", undo)
		return fmt.Errorf("placeholder commit: %w", err)
	}
	placeholder := gitx.HeadCommit(wtDir)
	if err := gitx.PushSetUpstream(wtDir, branch); err != nil {
		rollbackFailedClaim(c, branch, made, before, placeholder, "push failed", undo)
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
		PRURL: prURL, Window: windowID(c), Epic: epic, When: time.Now(),
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

// resolveAdopt turns `wt adopt`'s argument into the branch to adopt and what it
// must land on (#167): a PR number becomes its head branch plus its head commit
// (prHead is ghx.PRHead, in one gh call), which worktree.Adopt then requires
// origin/<branch> to carry; anything else is a branch name, checked against
// origin/<branch> alone. Pure apart from prHead.
func resolveAdopt(target string, prHead func(string) (string, string, error)) (string, worktree.AdoptWant, error) {
	if !issueRe.MatchString(target) {
		return target, worktree.AdoptWant{}, nil
	}
	branch, oid, err := prHead(target)
	if err != nil {
		return "", worktree.AdoptWant{}, fmt.Errorf("resolve PR #%s head (is gh authed? `wt doctor`): %w", target, err)
	}
	return branch, worktree.AdoptWant{PR: target, PRHead: oid}, nil
}

// Release clears the claim's active-work entry and unassigns the issue. With
// clean, it ALSO removes the worktree when the branch is abandoned — clean tree,
// no open/merged PR, only WIP placeholder commits (#42) — so releasing actually
// frees the slot instead of leaving an orphan `wt clean` can never sweep.
// Adopt puts a wt-registered worktree on an EXISTING branch instead of forking
// a new one — resolving a PR number to its head branch, or taking a branch name
// as-is — and records it in active-work exactly like Claim. It's the actionable
// half of the #134 refusal: when `wt claim` finds an in-flight PR it points here
// instead of opening a duplicate. It does NOT assign the issue, push, or open a
// PR — the branch (and usually its PR) already exist. (#134)
func Adopt(c *config.Config, target, epic string) error {
	if strings.TrimSpace(target) == "" {
		return fmt.Errorf("usage: wt adopt <branch|pr#>")
	}
	branch, want, err := resolveAdopt(target, ghx.PRHead)
	if err != nil {
		return err
	}
	prURL := ""
	if want.PR != "" {
		prURL = ghx.PRURL(target)
	}

	wtDir, err := worktree.Adopt(c, branch, want)
	if err != nil {
		return err
	}

	// Section identity: the issue the branch encodes (prefix+digits), else the
	// branch name — so UpsertSection stays idempotent for both PR-branch and
	// bare-branch adoptions instead of colliding on an empty "#".
	issue := issueFromBranch(c.Prefix, branch)
	ident := issue
	if ident == "" {
		ident = branch
	}
	title := ""
	if issue != "" {
		title, _ = ghx.IssueTitle(issue)
	}
	if prURL == "" {
		if n, ok := ghx.OpenPRForBranch(branch); ok {
			prURL = ghx.PRURL(n)
		}
	}

	entry := activework.Entry{
		Issue: ident, Title: title, Branch: branch, Worktree: wtDir,
		PRURL: prURL, Window: windowID(c), Epic: epic, When: time.Now(),
	}
	if err := activework.Write(c.ActiveWork, activework.UpsertSection(activework.Read(c.ActiveWork), entry)); err != nil {
		ui.Warn("active-work update failed (continuing): %v", err)
	} else {
		ui.OK("recorded adoption in active-work")
	}

	ui.Banner(fmt.Sprintf("Adopted %s", branch))
	if issue != "" {
		ui.Info("issue:    #%s", issue)
	}
	ui.Info("branch:   %s", branch)
	ui.Info("worktree: %s", wtDir)
	if prURL != "" {
		ui.Info("PR:       %s", prURL)
	}
	fmt.Println()
	ui.Step("cd %s", wtDir)
	return nil
}

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

// rollbackFailedClaim undoes a claim whose placeholder commit or push failed
// before anything durable was recorded (#159): the issue is unassigned (when
// this claim assigned it, claimUndo), so a retry starts clean instead of
// tripping the already-assigned guard, and of the worktree and branch only what
// this claim made goes (rollbackFor). A branch it cut from the base is deleted
// with its new worktree, as before. A local branch it re-attached (#62), or a
// worktree it was handed back, held work before the claim: the #159 rollback
// deleted them too, unpushed commits and all (#198). Now the placeholder commit
// is taken back off (before is its parent; placeholder is "" when the commit
// itself failed) and they stay. NON-force remove (#159 review): a worktree with
// uncommitted work is refused, never discarded. Best-effort: each step reports
// but never masks the error that got here (why).
func rollbackFailedClaim(c *config.Config, branch string, made worktree.Created, before, placeholder, why string, undo claimUndo) {
	ui.Info("rolling back partial claim of #%s (%s) …", undo.issue, why)
	s := rollbackFor(made.NewWorktree, made.NewBranch)
	if s.undoPlaceholder && placeholder != "" {
		if err := gitx.UndoCommit(made.Dir, placeholder, before); err != nil {
			ui.Warn("rollback: couldn't undo the placeholder commit in %s: %v", made.Dir, err)
		} else {
			ui.Info("rollback: took the placeholder commit back off %s (at %s again)", branch, abbrev(before))
		}
	}
	if s.removeWorktree {
		del := ""
		if s.deleteBranch {
			del = branch
		}
		if err := worktree.Remove(c, made.Dir, del, false); err != nil {
			ui.Warn("rollback: couldn't remove worktree %s: %v", made.Dir, err)
		}
	}
	switch {
	case !s.removeWorktree:
		ui.Info("rollback: kept worktree %s and its branch %s: they were there before this claim", made.Dir, branch)
	case !s.deleteBranch:
		ui.Info("rollback: kept local branch %s: it existed before this claim", branch)
	}
	undo.unassign()
}

// claimUndo is what undoing a failed claim may do to its issue (#159, #198
// review): unassign it, but only when this claim assigned it.
type claimUndo struct {
	issue, user string // user: the gh login the claim ran as, "" when gh could not say
	assigned    bool   // this claim's assign added user: false for an issue that was user's already (a --force re-claim)
}

// unassign removes the claim's assignment, best-effort.
func (u claimUndo) unassign() {
	if !u.assigned {
		ui.Info("rollback: left #%s assigned: it was yours before this claim", u.issue)
		return
	}
	user := u.user
	if user == "" {
		user, _ = ghx.CurrentUser()
	}
	if user == "" {
		ui.Warn("rollback: couldn't tell who to unassign from #%s; unassign yourself if you are", u.issue)
		return
	}
	if err := ghx.IssueRemoveAssignee(u.issue, user); err != nil {
		ui.Warn("rollback: couldn't unassign #%s: %v", u.issue, err)
		return
	}
	ui.Info("rollback: unassigned #%s", u.issue)
}

// rollbackScope is what undoing a claim whose push failed may touch (#159,
// #198).
type rollbackScope struct {
	undoPlaceholder bool // take the claim's placeholder commit back off the branch
	removeWorktree  bool // the claim added the worktree
	deleteBranch    bool // the claim cut the branch from the base
}

// rollbackFor decides the rollback from what worktree.Create made: only that
// is removed (#198). A branch the claim re-attached, or a worktree it was handed
// back, keeps everything but the placeholder commit. A new branch always comes
// with a new worktree, so newBranch without newWorktree cannot happen, and it
// deletes nothing. Pure.
func rollbackFor(newWorktree, newBranch bool) rollbackScope {
	switch {
	case newWorktree && newBranch:
		return rollbackScope{removeWorktree: true, deleteBranch: true}
	case newWorktree:
		return rollbackScope{undoPlaceholder: true, removeWorktree: true}
	default:
		return rollbackScope{undoPlaceholder: true}
	}
}

// abbrev shortens a commit id for a message.
func abbrev(sha string) string {
	if len(sha) > 8 {
		return sha[:8]
	}
	return sha
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
	// Capture the local placeholder tip BEFORE removing the branch — the remote we
	// delete must be the SAME placeholder we just proved abandoned (#159 review).
	localTip, _ := gitx.RunDir(c.Root, "rev-parse", "refs/heads/"+e.Branch)
	if err := worktree.Remove(c, e.Worktree, e.Branch, false); err != nil {
		// Kept whole, as a dirty worktree is above: the remote placeholder is
		// what a re-claim re-attaches to (#177 review). The gate now also refuses
		// a clean worktree (a git repository in its ignored files), so this is
		// no longer only git's rare refusal.
		ui.Warn("--clean: couldn't remove worktree, so it, its branch and origin's placeholder were left in place: %v", err)
		return
	}
	// #159: claim also PUSHED this branch, so removing only the worktree + local
	// branch leaves the remote placeholder behind — re-claiming the same issue then
	// pushes a fresh placeholder from base and is rejected non-fast-forward. Delete
	// the remote too, but ONLY when it matches the local placeholder we proved
	// abandoned: a remote that diverged with real commits (an out-of-band push, no
	// PR) must never be force-deleted. Best-effort — the remote may already be gone.
	switch remoteTip, rerr := gitx.RemoteBranchTip(c.Root, e.Branch); {
	case rerr != nil || remoteTip == "":
		// no remote branch (already gone) — nothing to do
	case localTip != "" && remoteTip == localTip:
		if err := gitx.DeleteRemoteBranch(c.Root, e.Branch); err != nil {
			ui.Info("--clean: remote origin/%s not deleted: %v", e.Branch, err)
		} else {
			ui.OK("--clean: deleted remote branch origin/%s", e.Branch)
		}
	default:
		ui.Info("--clean: remote origin/%s differs from the local placeholder — left in place (delete by hand if truly abandoned)", e.Branch)
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

var branchIssueRe = regexp.MustCompile(`^\d+`)

// issueFromBranch recovers the issue number a branch encodes — the inverse of
// BranchName (prefix + issue [+ -slug]). Returns "" when the branch isn't
// issue-shaped (a bare `spike/x`, or a branch without the configured prefix),
// so `wt adopt` can still register it, keyed by branch instead. (#134)
func issueFromBranch(prefix, branch string) string {
	return branchIssueRe.FindString(strings.TrimPrefix(branch, prefix))
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

// confirmNewClaim shows what's about to be claimed — a wrong issue NUMBER is
// obvious the moment its title is on screen (#157) — and, on an interactive
// terminal, asks before proceeding. Non-interactive (agent/pipe) proceeds: the
// title is surfaced either way and blocking would break the scripted `wt claim`
// flow. --yes / --force skip this entirely (checked by the caller).
func confirmNewClaim(issue, title string, interactive bool, in io.Reader) bool {
	ui.Info("about to claim #%s — %q", issue, title)
	if !interactive {
		return true // agent/pipe — proceed with the title shown, never hang on a prompt
	}
	fmt.Fprintf(os.Stderr, "%s claim #%s? [y/N] ", ui.Yellow("→"), issue)
	line, _ := bufio.NewReader(in).ReadString('\n')
	switch strings.ToLower(strings.TrimSpace(line)) {
	case "y", "yes":
		return true
	}
	return false
}

// promptInteractive reports whether to ASK a human before claiming. It requires
// BOTH stdin and stderr to be a terminal — the prompt is written to stderr, so a
// piped stderr means nobody is watching to answer — which keeps the scripted /
// agent `wt claim` flow (and a pty-wrapping harness that only makes stdin a TTY)
// from ever blocking on a prompt (#157 + review). WT_YES is an env escape hatch
// for an unattended run that can't pass --yes (e.g. a wrapper).
func promptInteractive() bool {
	if os.Getenv("WT_YES") != "" {
		return false
	}
	return isTTY(os.Stdin) && isTTY(os.Stderr)
}

func isTTY(f *os.File) bool {
	fi, err := f.Stat()
	return err == nil && fi.Mode()&os.ModeCharDevice != 0
}

// windowID is the STABLE identity recorded in the active-work file — the SAME
// value `wt doctor` prints and that coordCtx uses (#156). It must match, or a
// window that restarts its shell can't recognise its own claims: the old
// hostname-PID identity changed every shell, so a restarted window saw its OWN
// prior claims as another window's, and its old identity lingered as a claim
// nothing could ever update or release. coord.WindowID keys on WT_WINDOW → the
// worktree toplevel PATH (stable across shell restarts AND branch switches) →
// branch, so the recorded identity survives a restart.
func windowID(c *config.Config) string {
	branch, _ := gitx.CurrentBranch()
	return coord.WindowID(os.Getenv("WT_WINDOW"), c.Root, branch)
}
