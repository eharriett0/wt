package worktree

import (
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"strings"

	"github.com/eharriett0/wt/internal/activework"
	"github.com/eharriett0/wt/internal/config"
	"github.com/eharriett0/wt/internal/gitx"
	"github.com/eharriett0/wt/internal/ui"
)

// `wt discard <name>` (#177) drops ONE worktree its owner decided is throwaway
// (a canary pushed, used and deleted on origin on purpose), with its local
// branch and its active-work claim. `wt clean` rightly keeps such a worktree
// ("never pushed", the #61 guard), and `wt release --clean` removes only a
// claim's WIP-only one, so the dead window stayed live in the collision engine,
// or its owner stepped outside wt (`git worktree remove` + `git branch -D`) and
// past every guard.
//
// It is the ONLY wt command that can drop commits no branch on origin has, and
// only for an exact name with --drop-commits. Never a sweep: one name, one
// worktree. Everything else (other worktrees and branches, origin itself, the
// remote-tracking refs) is left as it is.

// ErrDiscardRefused is the kind of every refusal Discard makes after it picked
// out the worktree (DecideDiscard said no).
var ErrDiscardRefused = errors.New("wt discard refused")

// DiscardVerdict is what `wt discard` does with the worktree a name picked out.
type DiscardVerdict int

const (
	DiscardGo               DiscardVerdict = iota // remove the worktree, delete its branch, drop its claim
	DiscardPrimary                                // the repository's main checkout: never
	DiscardDetached                               // HEAD on no branch (mid-rebase or bisect, or a detached checkout)
	DiscardBase                                   // on the base branch: never (#101)
	DiscardHoldsCwd                               // this command runs in it, or in a directory inside it
	DiscardOutOfRoot                              // outside worktree_root, without --all-roots
	DiscardNests                                  // another worktree lives inside its directory
	DiscardSharedBranch                           // another worktree has its branch checked out
	DiscardLocked                                 // `git worktree lock`ed
	DiscardDirty                                  // uncommitted or untracked changes, or a status that can't be read
	DiscardUncounted                              // the commits only on its branch could not be listed
	DiscardNeedsDropCommits                       // it has commits on no branch origin has, and no --drop-commits
)

// DiscardCase is what `wt discard` found out about the worktree a name picked
// out (#177). Every unknown reads as the refusing answer.
type DiscardCase struct {
	Primary      bool // the main checkout (git lists it first)
	Detached     bool // HEAD is on no branch
	OnBase       bool // its branch is the base branch
	HoldsCwd     bool // the cwd is it or inside it; true when the cwd can't be read
	OutOfRoot    bool // not under worktree_root
	Nests        bool // another worktree's directory is inside it
	SharedBranch bool // another worktree has the same branch checked out
	Locked       bool // `git worktree lock`ed
	Dirty        bool // uncommitted or untracked-but-not-ignored changes; true when its status can't be read
	Counted      bool // the commits only on its branch were listed
	Unique       int  // how many there are (read only when Counted)
}

// DecideDiscard is `wt discard`'s whole decision (#177). Placement first
// (discardPlacement): the main checkout, a detached HEAD, the base branch, the
// window the command runs in, a worktree outside worktree_root without
// --all-roots, one with another worktree inside it (whose files `git worktree
// remove` deletes too, measured), one whose branch another worktree has checked
// out, a locked one. Then content: a dirty tree, a branch whose commits could
// not be listed, and commits no branch on origin has without --drop-commits.
//
// No flag unlocks anything but its own guard: --all-roots only an out-of-root
// worktree, --drop-commits only the commits. A dirty tree, the base branch and
// the main checkout are refused whatever the flags. Pure.
func DecideDiscard(k DiscardCase, dropCommits, allRoots bool) DiscardVerdict {
	if v := discardPlacement(k, allRoots); v != DiscardGo {
		return v
	}
	switch {
	case k.Dirty:
		return DiscardDirty
	case !k.Counted:
		return DiscardUncounted
	case k.Unique > 0 && !dropCommits:
		return DiscardNeedsDropCommits
	}
	return DiscardGo
}

// discardPlacement is DecideDiscard's half that needs no fetch: where the
// worktree is and what it is checked out on. Discard refuses on it before
// asking origin anything. Pure.
func discardPlacement(k DiscardCase, allRoots bool) DiscardVerdict {
	switch {
	case k.Primary:
		return DiscardPrimary
	case k.Detached:
		return DiscardDetached
	case k.OnBase:
		return DiscardBase
	case k.HoldsCwd:
		return DiscardHoldsCwd
	case k.OutOfRoot && !allRoots:
		return DiscardOutOfRoot
	case k.Nests:
		return DiscardNests
	case k.SharedBranch:
		return DiscardSharedBranch
	case k.Locked:
		return DiscardLocked
	}
	return DiscardGo
}

// DiscardTarget returns the index in wts of the one worktree name picks out
// (#177). It matches as `wt clean <name>` does (MatchedName: the worktree's
// directory, its branch, or its path; resolved is the name's real path when it
// is one), and refuses with CheckNames' wording a name that picks out none (a
// typo) or more than one (a directory name that is also another worktree's
// branch). The main checkout, wts[0], is a candidate too: a name that also
// picks it out is ambiguous instead of quietly meaning a secondary worktree,
// and DecideDiscard refuses the main checkout itself. wts' paths are real paths,
// and a detached worktree's branch is "", so the name "HEAD" picks out no
// detached worktree. Pure.
func DiscardTarget(name, resolved string, wts []gitx.WorktreeRef) (int, error) {
	if name == "" {
		return -1, fmt.Errorf("name the worktree to discard (a worktree directory, a branch, or a path); nothing was discarded")
	}
	var hits []string
	at := -1
	for i, w := range wts {
		if MatchedName([]string{name}, []string{resolved}, w.Path, w.Branch) != "" {
			hits = append(hits, w.Path)
			at = i
		}
	}
	if err := checkNames([]string{name}, map[string][]string{name: hits}, "discarded"); err != nil {
		return -1, err
	}
	return at, nil
}

// DiscardOpts are `wt discard`'s flags.
type DiscardOpts struct {
	DropCommits bool // drop commits that no branch on origin has
	AllRoots    bool // a worktree outside worktree_root may be discarded, as `wt clean --all-roots` evaluates it
	DryRun      bool // say what would happen; change nothing but the fetch
}

// DiscardResult is what Discard found out, and did.
type DiscardResult struct {
	Dir, Branch, Tip string
	Case             DiscardCase
	Verdict          DiscardVerdict
	Gone             bool          // its directory no longer exists (git lists it as prunable): nothing on disk to lose
	Dirty            []string      // the worktree's `git status --porcelain` lines
	Unique           []gitx.Commit // the commits only on the branch, newest first (when Case.Counted)
	OriginAsked      bool          // origin answered which branches it has
	OriginTip        string        // origin's tip of the branch, "" when it lacks it or was not asked
	StaleTracking    bool          // origin lacks the branch, and origin/<branch> here is as last fetched
	Claims           []string      // the active-work claims recorded on the branch
	Done             bool          // the worktree and the branch were removed

	inside, alsoOn []string // the worktrees inside it, and the others on its branch (placementCase)
}

// discardShown caps how many dirty entries and commits a run lists.
const discardShown = 20

// Discard is `wt discard <name>` (#177): it picks out exactly one worktree
// (DiscardTarget), refuses on placement before asking origin anything, then
// reads the worktree's status, fetches origin (never pruning), lists the
// commits on its branch that no branch on origin has, and decides
// (DecideDiscard). On go it removes the worktree (never forced), deletes the
// local branch while it is still at the tip it listed, and drops the
// active-work claims recorded on that branch (merge-pr's auto-clean drops a
// merged branch's claim too). It never deletes the branch on origin; when origin
// still has it, it says so and prints the command.
//
// "On no branch origin has" is read from origin itself (`ls-remote`), not from
// the remote-tracking refs: a branch deleted on origin from another clone or the
// web UI leaves origin/<branch> here, and `rev-list <branch> --not --remotes`
// then hid the very commits the #177 canary carries. A fetch makes origin's tips
// present here; a tip this clone still lacks is skipped, which only lists more.
// When origin can't be asked, the remote-tracking refs as last fetched stand in,
// all but origin/<branch> itself, and the run says so.
//
// DryRun prints the same plan and verdict and changes nothing but the fetch (so
// its list is the real run's). A refusal, dry or not, is an error of kind
// ErrDiscardRefused.
func Discard(c *config.Config, name string, o DiscardOpts) (DiscardResult, error) {
	var r DiscardResult
	wts, err := gitx.WorktreeList()
	if err != nil {
		return r, fmt.Errorf("git worktree list: %w", err)
	}
	for i := range wts {
		wts[i].Path = realPath(wts[i].Path)
	}
	resolved := ""
	if strings.ContainsRune(name, filepath.Separator) {
		resolved = realPath(name)
	}
	i, err := DiscardTarget(name, resolved, wts)
	if err != nil {
		return r, err
	}
	w := wts[i]
	r.Dir, r.Branch = w.Path, w.Branch
	r.Case, r.inside, r.alsoOn = placementCase(c, wts, i, gitx.IgnoreCase())

	if v := discardPlacement(r.Case, o.AllRoots); v != DiscardGo {
		r.Verdict = v
		ui.Step("discard %s: worktree %s%s", name, w.Path, onBranch(w.Branch))
		return r, discardRefusal(c, r, name, nil, wts, o.DryRun)
	}

	r.Tip = gitx.BranchTip(w.Branch)
	ui.Step("discard %s: worktree %s on branch %s (at %s)", name, w.Path, w.Branch, short(r.Tip))
	// A directory that is gone holds no uncommitted work to lose: git lists the
	// worktree as prunable and removes just its record. A locked one was refused
	// above (an unmounted volume is what a lock is for). A directory that is no
	// longer a work tree's top (replaced since git recorded it) would have its
	// status read from whatever checkout encloses it (#198 review), so it counts
	// as unreadable, as does any status git can't give: dirty.
	var statusErr error
	switch _, err := os.Stat(w.Path); {
	case errors.Is(err, fs.ErrNotExist):
		r.Gone = true
	case !isValidWorktree(w.Path):
		statusErr = fmt.Errorf("it is not the top of a work tree any more (replaced since git recorded it?)")
	default:
		r.Dirty, statusErr = gitx.StatusEntries(w.Path)
	}
	r.Case.Dirty = !r.Gone && (statusErr != nil || len(r.Dirty) > 0)

	countErr := r.countUnique()
	_, r.Claims = activework.RemoveBranchSections(activework.Read(c.ActiveWork), w.Branch)
	r.printPlan(statusErr)

	r.Verdict = DecideDiscard(r.Case, o.DropCommits, o.AllRoots)
	if r.Verdict != DiscardGo {
		why := statusErr
		if r.Verdict == DiscardUncounted {
			why = countErr
		}
		return r, discardRefusal(c, r, name, why, wts, o.DryRun)
	}
	if o.DryRun {
		r.printWouldDo()
		return r, nil
	}
	return r, r.apply(c)
}

// placementCase reads where the worktree at wts[i] is and what it is on, and
// returns the worktrees inside it and the others on its branch. fold is
// core.ignorecase: there a branch differing only in case is the same loose ref
// file (#167), and git's in-use check compares names exactly, so `git branch -D
// Main` deletes main's ref, and deleting a branch removes it from under a
// worktree on its case twin. Both count as the same branch.
func placementCase(c *config.Config, wts []gitx.WorktreeRef, i int, fold bool) (k DiscardCase, inside, alsoOn []string) {
	w := wts[i]
	k = DiscardCase{
		Primary:   i == 0,
		Detached:  w.Branch == "",
		OnBase:    w.Branch != "" && sameBranch(w.Branch, c.Base, fold),
		OutOfRoot: !under(w.Path, c.WorktreeRoot),
		Locked:    w.Locked,
	}
	cwd, err := os.Getwd()
	k.HoldsCwd = err != nil || under(cwd, w.Path)
	for j, other := range wts {
		if j == i {
			continue
		}
		if under(other.Path, w.Path) {
			inside = append(inside, other.Path)
		}
		if w.Branch != "" && sameBranch(other.Branch, w.Branch, fold) {
			alsoOn = append(alsoOn, other.Path)
		}
	}
	k.Nests, k.SharedBranch = len(inside) > 0, len(alsoOn) > 0
	return k, inside, alsoOn
}

// sameBranch reports whether two branch names are one branch: equal, or, with
// fold (core.ignorecase), equal but for case. Pure.
func sameBranch(a, b string, fold bool) bool {
	return a == b || (fold && strings.EqualFold(a, b))
}

// countUnique fetches origin and lists the commits on the branch that no branch
// on origin has (see Discard). It sets Case.Counted only on a full answer.
func (r *DiscardResult) countUnique() error {
	ui.Step("fetching origin")
	if err := gitx.FetchRemote("origin"); err != nil {
		ui.Warn("git fetch origin failed (listing against what this clone has from origin): %v", err)
	}
	var exclude []string
	heads, err := gitx.RemoteHeads("origin")
	if err == nil {
		r.OriginAsked = true
		r.OriginTip = heads[r.Branch]
		r.StaleTracking = r.OriginTip == "" && gitx.RemoteTrackingTip(r.Branch) != ""
		for _, sha := range heads {
			exclude = append(exclude, sha)
		}
	} else {
		ui.Warn("could not ask origin which branches it has (%v): the remote-tracking refs as last fetched stand in, all but origin/%s, which origin may have deleted since", err, r.Branch)
		exclude, err = gitx.RemoteTrackingTips("refs/remotes/origin/" + r.Branch)
		if err != nil {
			return fmt.Errorf("reading the remote-tracking refs: %w", err)
		}
	}
	if r.Tip == "" {
		return fmt.Errorf("branch %s does not resolve to a commit", r.Branch)
	}
	commits, err := gitx.CommitsOnlyOn(r.Tip, exclude)
	if err != nil {
		return err
	}
	r.Unique = commits
	r.Case.Counted = true
	r.Case.Unique = len(commits)
	return nil
}

// printPlan lists what the worktree holds that a discard would drop or leave.
func (r *DiscardResult) printPlan(statusErr error) {
	switch {
	case r.Gone:
		fmt.Println("  its directory is gone (git lists the worktree as prunable): nothing on disk to lose")
	case statusErr != nil:
		fmt.Printf("  its status could not be read (%v), so it counts as dirty\n", statusErr)
	case len(r.Dirty) > 0:
		fmt.Printf("  uncommitted changes (%d):\n", len(r.Dirty))
		printCapped(r.Dirty)
	default:
		fmt.Println("  no uncommitted changes")
	}
	switch {
	case !r.Case.Counted:
		fmt.Printf("  the commits only on %s could not be listed\n", r.Branch)
	case len(r.Unique) == 0:
		fmt.Printf("  no commits only on %s: every one is on a branch origin has\n", r.Branch)
	default:
		fmt.Printf("  commits only on %s, on no branch origin has (%d):\n", r.Branch, len(r.Unique))
		lines := make([]string, len(r.Unique))
		for i, cm := range r.Unique {
			lines[i] = short(cm.SHA) + " " + cm.Subject
		}
		printCapped(lines)
	}
	switch {
	case r.OriginTip != "":
		fmt.Printf("  origin has %s (at %s): left alone\n", r.Branch, short(r.OriginTip))
	case r.StaleTracking:
		fmt.Printf("  origin no longer has %s; origin/%s here is as last fetched before origin deleted it\n", r.Branch, r.Branch)
	case r.OriginAsked:
		fmt.Printf("  origin does not have %s\n", r.Branch)
	}
	if len(r.Claims) > 0 {
		fmt.Printf("  active-work claim %s is recorded on %s\n", issueList(r.Claims), r.Branch)
	}
}

// printCapped prints up to discardShown of lines, indented, and how many more.
func printCapped(lines []string) {
	for i, ln := range lines {
		if i == discardShown {
			fmt.Printf("    … and %d more\n", len(lines)-discardShown)
			return
		}
		fmt.Printf("    %s\n", ln)
	}
}

// printWouldDo is a dry run's answer when the real run would go ahead.
func (r *DiscardResult) printWouldDo() {
	parts := []string{"remove worktree " + r.Dir, fmt.Sprintf("delete local branch %s (at %s)", r.Branch, short(r.Tip))}
	if n := len(r.Unique); n > 0 {
		parts = append(parts, fmt.Sprintf("drop the %d commit(s) listed above", n))
	}
	if len(r.Claims) > 0 {
		parts = append(parts, fmt.Sprintf("drop claim %s from active-work", issueList(r.Claims)))
	}
	ui.Info("--dry-run: a real run would %s. Nothing was changed (origin was fetched, as a real run fetches it).", strings.Join(parts, ", "))
	r.printOriginAdvice()
}

// apply removes the worktree (never forced), then the branch while it is still
// at the tip whose commits were listed, then the claim.
func (r *DiscardResult) apply(c *config.Config) error {
	if cur := gitx.BranchTip(r.Branch); cur != r.Tip {
		return refuse(ErrDiscardRefused, "%s moved to %s after its commits were listed (at %s); re-run wt discard to see what it holds now. Nothing was discarded", r.Branch, short(cur), short(r.Tip))
	}
	if err := gitx.RemoveCleanWorktree(r.Dir); err != nil {
		return fmt.Errorf("git worktree remove %s failed: %w; branch %s and its claim were left as they are", r.Dir, err, r.Branch)
	}
	ui.OK("removed worktree %s", r.Dir)
	if err := gitx.DeleteBranchAt(r.Branch, r.Tip); err != nil {
		ui.Warn("the worktree is gone, but local branch %s was kept: %v", r.Branch, err)
		ui.Info("check what it holds now, then delete it yourself if it is still throwaway:")
		fmt.Printf("  git log --oneline -n %d %s\n", discardShown, r.Branch)
		fmt.Printf("  git branch -D %s\n", r.Branch)
		return fmt.Errorf("worktree %s removed, local branch %s not deleted: %w", r.Dir, r.Branch, err)
	}
	ui.OK("deleted local branch %s (was %s); to bring it back: git branch %s %s", r.Branch, short(r.Tip), r.Branch, r.Tip)
	if n := len(r.Unique); n > 0 {
		ui.Info("dropped %d commit(s) that no branch on origin has (--drop-commits)", n)
	}
	r.dropClaim(c)
	r.Done = true
	r.printOriginAdvice()
	return nil
}

// dropClaim removes the active-work claims recorded on the branch, and only
// those (activework.RemoveBranchSections): merge-pr's auto-clean drops a merged
// branch's claim the same way (#40), but by issue, which would also drop another
// worktree's claim on that issue. Best-effort: the worktree and branch are gone.
func (r *DiscardResult) dropClaim(c *config.Config) {
	if len(r.Claims) == 0 {
		return
	}
	newC, dropped := activework.RemoveBranchSections(activework.Read(c.ActiveWork), r.Branch)
	if len(dropped) == 0 {
		return
	}
	if err := activework.Write(c.ActiveWork, newC); err != nil {
		ui.Warn("claim %s not dropped from active-work: %v", issueList(dropped), err)
		return
	}
	ui.Info("dropped claim %s from active-work (the issue, its assignee and any PR are untouched; `wt release <issue>` unassigns it)", issueList(dropped))
}

// issueList renders issues as "#7, #9".
func issueList(issues []string) string {
	out := make([]string, len(issues))
	for i, n := range issues {
		out[i] = "#" + n
	}
	return strings.Join(out, ", ")
}

// printOriginAdvice says what was left on origin: wt discard never deletes a
// remote branch.
func (r *DiscardResult) printOriginAdvice() {
	switch {
	case r.OriginTip != "":
		ui.Info("origin still has %s (at %s); wt discard never deletes a remote branch. To delete it:", r.Branch, short(r.OriginTip))
		fmt.Printf("  git push origin --delete %s\n", r.Branch)
	case !r.OriginAsked:
		ui.Info("origin could not be asked whether it still has %s; if it does, this deletes it:", r.Branch)
		fmt.Printf("  git push origin --delete %s\n", r.Branch)
	}
}

// discardRefusal prints what to do about a refusal and returns it, as an error
// of kind ErrDiscardRefused. why is the error behind a dirty or uncounted
// verdict, when there was one.
func discardRefusal(c *config.Config, r DiscardResult, name string, why error, wts []gitx.WorktreeRef, dry bool) error {
	msg := discardRefusalText(c, r, why)
	if dry {
		msg = "--dry-run: a real run would refuse: " + msg
	}
	switch r.Verdict {
	case DiscardHoldsCwd:
		ui.Info("cd out of it first, for example:")
		fmt.Printf("  cd %s\n", wts[0].Path)
	case DiscardNeedsDropCommits:
		ui.Info("to drop them with the worktree:")
		fmt.Printf("  wt discard %s --drop-commits\n", name)
	case DiscardOutOfRoot:
		ui.Info("to discard it anyway:")
		fmt.Printf("  wt discard %s --all-roots\n", name)
	}
	return refuse(ErrDiscardRefused, "%s", msg)
}

// discardRefusalText is a refusal's one-line reason.
func discardRefusalText(c *config.Config, r DiscardResult, why error) string {
	const none = "; nothing was discarded"
	switch r.Verdict {
	case DiscardPrimary:
		return fmt.Sprintf("%s is the main checkout of this repository, which wt discard never removes%s", r.Dir, none)
	case DiscardDetached:
		return fmt.Sprintf("%s is on no branch (a detached HEAD: mid-rebase or bisect, or checked out detached), and wt discard removes a worktree with its branch; finish what it is doing first (git -C %s status)%s", r.Dir, r.Dir, none)
	case DiscardBase:
		if r.Branch != c.Base {
			return fmt.Sprintf("%s is on %s, which is the base branch %s on this case-insensitive filesystem, and wt discard never deletes the base branch%s", r.Dir, r.Branch, c.Base, none)
		}
		return fmt.Sprintf("%s is on the base branch %s, which wt discard never deletes%s", r.Dir, c.Base, none)
	case DiscardHoldsCwd:
		return fmt.Sprintf("this command runs inside %s, and wt discard never removes the window it runs in%s", r.Dir, none)
	case DiscardOutOfRoot:
		return fmt.Sprintf("%s is outside worktree_root %s, and --all-roots (as in `wt clean --all-roots`) was not passed%s", r.Dir, c.WorktreeRoot, none)
	case DiscardNests:
		return fmt.Sprintf("another worktree is inside %s (%s), and removing it would delete that worktree's files too%s", r.Dir, strings.Join(r.inside, ", "), none)
	case DiscardSharedBranch:
		return fmt.Sprintf("branch %s is also checked out in %s%s", r.Branch, strings.Join(r.alsoOn, ", "), none)
	case DiscardLocked:
		return fmt.Sprintf("%s is locked (git worktree lock); unlock it first: git worktree unlock %s%s", r.Dir, r.Dir, none)
	case DiscardDirty:
		if why != nil {
			return fmt.Sprintf("could not read %s's status (%v), so it counts as dirty%s", r.Dir, why, none)
		}
		return fmt.Sprintf("%s has %d uncommitted change(s), listed above (untracked files count); commit, stash or delete them first%s", r.Dir, len(r.Dirty), none)
	case DiscardUncounted:
		return fmt.Sprintf("could not list the commits only on %s (%v)%s", r.Branch, why, none)
	case DiscardNeedsDropCommits:
		return fmt.Sprintf("%s has %d commit(s) on no branch origin has, listed above, and --drop-commits was not passed%s", r.Branch, len(r.Unique), none)
	}
	return fmt.Sprintf("refused (verdict %d)%s", r.Verdict, none)
}

// onBranch renders " on branch <b>", or " (detached HEAD)".
func onBranch(branch string) string {
	if branch == "" {
		return " (detached HEAD)"
	}
	return " on branch " + branch
}
