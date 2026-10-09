// Package worktree creates and prunes per-window git worktrees.
package worktree

import (
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"time"

	"github.com/eharriett0/wt/internal/config"
	"github.com/eharriett0/wt/internal/ghx"
	"github.com/eharriett0/wt/internal/gitx"
	"github.com/eharriett0/wt/internal/ui"
)

// cleanGraceWindow protects a just-created worktree from being reaped by another
// window's `wt clean` before it has had a chance to be pushed / PR'd (#61). A
// worktree whose .git entry is younger than this is never swept, regardless of
// commit/PR state.
const cleanGraceWindow = 10 * time.Minute

// ShippedVerdict decides whether a branch is shipped and its worktree safe to
// prune (#37). A MERGED PR means shipped regardless of `git cherry` — wt's only
// merge path is squash, and a wt-claimed branch carries an empty placeholder
// commit + real work, so it's never patch-equivalent to the squash and cherry
// never reads 0. Otherwise fall back to cherry: shipped iff it succeeded and
// reported 0 unshipped commits.
func ShippedVerdict(unshipped int, cherryFailed, prMerged bool) bool {
	if prMerged {
		return true
	}
	return !cherryFailed && unshipped == 0
}

// IsAbandonedBranch decides whether a released branch's worktree is safe to
// auto-remove (#42): no OPEN or MERGED PR keeps it alive, AND every commit ahead
// of base is a `WIP: claim #` placeholder (empty subjects — nothing ahead — is
// vacuously abandoned). A branch with any real commit, or any live PR, is never
// abandoned. Pure.
func IsAbandonedBranch(unshippedSubjects []string, prOpen, prMerged bool) bool {
	if prOpen || prMerged {
		return false
	}
	for _, s := range unshippedSubjects {
		if !strings.HasPrefix(strings.TrimSpace(s), "WIP: claim #") {
			return false
		}
	}
	return true
}

// ReapVerdict is the SAFE invariant for `wt clean` (#61): only ever remove a
// worktree that is provably shipped, and never one that could hold live work.
//   - within the grace window (just created)      → keep (race protection)
//   - uncommitted changes (dirty)                  → keep (live work, #174)
//   - a MERGED PR                                  → reap (pushed + merged)
//   - never pushed (PushedUpstream)                → keep (new / unshared work)
//   - pushed AND patch-equivalent on base (cherry) → reap
//
// The old ShippedVerdict classified a commitless worktree (cherry reports 0)
// as shipped — reaping a brand-new `wt new` checkout. Requiring a push closes
// that: `wt new` never pushes, so its worktree is never swept. ⚠ "Has an
// upstream" is NOT "pushed": `wt new` branches from origin/<base>, and git sets
// that as the new branch's upstream, so until #175 this guard never fired and a
// commitless checkout was swept once its grace window passed.
//
// dirty keeps the LISTING honest (#174): remove() already refused a dirty tree,
// but the listing said "safe to remove" and printed a remove command for a
// worktree whose only content was uncommitted work. Pure.
func ReapVerdict(unshipped int, cherryFailed, prMerged, pushed, withinGrace, dirty bool) bool {
	if withinGrace || dirty {
		return false
	}
	if prMerged {
		return true
	}
	if !pushed {
		return false
	}
	return !cherryFailed && unshipped == 0
}

// PushedUpstream reports whether a branch's upstream shows it was pushed (#175).
// An upstream that is the base itself (mergeRef = refs/heads/<base>) is where the
// branch was cut from, which is what `wt new` produces, not where it was pushed.
// No upstream, or one whose merge ref cannot be read, is not proof of a push. Pure.
func PushedUpstream(hasUpstream bool, mergeRef, base string) bool {
	return hasUpstream && mergeRef != "" && mergeRef != "refs/heads/"+base
}

// dirtyCount counts uncommitted changes in the worktree at wt (porcelain lines),
// for the --stale-index preview message; 0 on any error.
func dirtyCount(wt string) int {
	out, err := gitx.RunDir(wt, "status", "--porcelain")
	if err != nil {
		return 0
	}
	n := 0
	for _, ln := range strings.Split(out, "\n") {
		if strings.TrimSpace(ln) != "" {
			n++
		}
	}
	return n
}

// StaleIndexReportable decides whether `wt clean --stale-index` should REPORT a
// worktree (#88): the opt-in flag is set, the worktree is past the just-created
// grace window, its most-recent PR resolved to MERGED, AND it has a dirty index
// — the exact worktree a plain clean silently leaves in place forever (#79:
// clean reaps a merged branch, but Remove then refuses the dirty index).
//
// Report-ONLY, never auto-remove — the #88 adversarial review proved a
// force-remove here is a permanent-data-loss footgun: a MERGED PR certifies only
// that the COMMITTED work shipped; the dirty index could just as well be FRESH
// post-merge work the operator started in that worktree, which is
// indistinguishable from stale pre-merge cruft and is not reflog-recoverable
// once `git worktree remove --force` discards it. So wt surfaces these worktrees
// + the exact manual command, and the operator inspects the changes and makes
// the destructive decision themselves. MERGED-only (a CLOSED-PR branch is kept
// on purpose per #79/#87, so it isn't reported either). Pure — the testable core.
func StaleIndexReportable(prState string, prOK, dirty, staleIndex, withinGrace bool) bool {
	if !staleIndex || withinGrace || !dirty || !prOK {
		return false
	}
	return prState == "MERGED"
}

// worktreeAge returns how long ago the worktree at wt was created, via the mtime
// of its `.git` entry (written by `git worktree add`). ok=false when it can't be
// stat'd — treated by callers as "not fresh" (don't over-protect an unknowable).
func worktreeAge(wt string, now time.Time) (age time.Duration, ok bool) {
	fi, err := os.Stat(filepath.Join(wt, ".git"))
	if err != nil {
		return 0, false
	}
	return now.Sub(fi.ModTime()), true
}

// isValidWorktree reports whether wtDir is a live git worktree — the directory
// exists AND git recognizes it (#62). A leftover empty dir from an out-of-band
// removal is NOT valid, so `wt new` recreates rather than short-circuiting.
func isValidWorktree(wtDir string) bool {
	return isDir(wtDir) && gitx.IsInsideWorktree(wtDir)
}

// New creates a worktree for branch under c.WorktreeRoot, based on the repo's
// base branch. Idempotent: if the worktree already exists, prints the cd hint
// and returns its path. Returns the worktree path. It is PlanNew, then Create.
func New(c *config.Config, branch string) (string, error) {
	p, err := PlanNew(c, branch, NewFor{})
	if err != nil {
		return "", err
	}
	made, err := p.Create()
	if err != nil {
		return "", err
	}
	return made.Dir, nil
}

// NewFor says which command PlanNew runs for (#198), for its notes and the
// re-run line a refusal prints. The zero value is `wt new <branch>`.
type NewFor struct {
	// ClaimIssue is the issue `wt claim` is claiming. A claim commits a
	// placeholder on the branch and pushes it right after Create.
	ClaimIssue string
}

func (f NewFor) attachFor(branch, dir string) attachFor {
	if f.ClaimIssue != "" {
		return attachFor{kind: forClaim, rerun: "wt claim " + f.ClaimIssue, dir: dir}
	}
	return attachFor{kind: forNew, rerun: "wt new " + branch, dir: dir}
}

// NewPlan is what `wt new <branch>` (and `wt claim`, through it) will do,
// decided before anything is created, assigned or moved (#198). PlanNew makes
// it and Create carries it out, so claim can refuse before it assigns the issue.
type NewPlan struct {
	c        *config.Config
	branch   string
	dir      string
	who      attachFor
	existing bool       // wt's live worktree for the branch is already there: handed back, never moved
	attach   attachPlan // otherwise: what happens to a local branch of that name
}

// Created is what NewPlan.Create made (#198). A claim whose push fails undoes
// only that: a branch it re-attached, or a worktree it was handed back, held
// work before the claim.
type Created struct {
	Dir         string
	NewWorktree bool // Create added the worktree; false when it handed back an existing one
	NewBranch   bool // Create made the branch from the base; false when it attached an existing one (#62)
}

// PlanNew decides what `wt new <branch>` does, before anything is created,
// assigned or moved (#198).
//
// A live worktree at wt's path for the branch is handed back (#62), after a
// check against origin/<branch> as just fetched that never moves it
// (checkExistingWorktree: a note when only behind or only ahead, a refusal when
// diverged or not comparable, and for a claim a refusal when only behind too).
//
// Otherwise a local branch of that name is re-attached, as #62 wants for a
// worktree whose directory went away, but first checked against origin/<branch>
// exactly as `wt adopt` checks it (#167): equal is attached, only ahead
// (unpushed commits) is attached with a note naming them, only behind is
// fast-forwarded when no worktree has it, and diverged or not comparable is
// refused with both tips, the local-only commits and the ways on. With no
// origin/<branch> to compare against (never pushed, or never fetched and
// offline) it is attached as it is and labelled unverified. Before #198 every
// one of those was attached silently: a branch left by an earlier attempt that
// reused the name, or one behind or diverged from what was pushed, was resumed,
// and a claim then committed on it, failed to push, and its rollback deleted
// the branch. With no local branch, a new one is cut from the base, as before.
func PlanNew(c *config.Config, branch string, f NewFor) (*NewPlan, error) {
	slug := strings.ReplaceAll(branch, "/", "-")
	dir := filepath.Join(c.WorktreeRoot, slug)
	p := &NewPlan{c: c, branch: branch, dir: dir, who: f.attachFor(branch, dir)}

	// Short-circuit only when it's a LIVE worktree (#62) — not a stale/empty dir
	// left by another window's clean or an out-of-band `git worktree remove`.
	if isValidWorktree(dir) {
		if err := checkExistingWorktree(dir, branch, fetchBranchTarget(branch, c.WorktreeRoot), p.who); err != nil {
			return nil, err
		}
		p.existing = true
		return p, nil
	}
	// Before the in-use check: a worktree whose directory is already gone must
	// not count as having the branch checked out (#167).
	if err := reconcileWorktreeDir(dir); err != nil {
		return nil, err
	}

	ui.Step("fetching origin/%s", c.Base)
	if err := gitx.Fetch("origin", c.Base); err != nil {
		ui.Warn("git fetch failed (continuing with local refs): %v", err)
	}

	fold := gitx.IgnoreCase()
	if fold {
		if err := refuseCaseTwin(branch, p.who); err != nil {
			return nil, err
		}
	}
	local := gitx.BranchTip(branch)
	var target adoptTarget
	if local != "" {
		target = fetchBranchTarget(branch, c.WorktreeRoot)
	}
	a, err := planAttach(branch, local, target, fold, p.who)
	if err != nil {
		return nil, err
	}
	p.attach = a
	return p, nil
}

// fetchBranchTarget fetches origin/<branch> and returns what a local branch, or
// wt's existing worktree, is checked against (#198): the target `wt adopt
// <branch>` uses (#167). Offline-tolerant: when the fetch fails, origin/<branch>
// as last fetched is the target, labelled so, and with no origin/<branch> in the
// clone there is nothing to compare against. The fetch failing for a branch
// this clone never fetched is what a never-pushed branch (#62) looks like, so
// it is not warned about.
func fetchBranchTarget(branch, root string) adoptTarget {
	ui.Step("fetching origin/%s", branch)
	err := gitx.Fetch("origin", branch)
	if err != nil && gitx.RemoteTrackingTip(branch) != "" {
		ui.Warn("git fetch origin %s failed (comparing with origin/%s as last fetched): %v", branch, branch, err)
	}
	t, _ := resolveAdoptTarget(branch, AdoptWant{}, err == nil, root) // by branch name: never refuses
	return t
}

// Create carries out the plan (#198): hands back the existing worktree, or
// fast-forwards a local branch that is only behind, attaches it (or cuts a new
// branch from the base), and checks where the new worktree landed. A local
// branch that changed after PlanNew looked is refused (apply).
func (p *NewPlan) Create() (Created, error) {
	if p.existing {
		ui.OK("worktree already exists at %s", p.dir)
		ui.Step("cd %s", p.dir)
		return Created{Dir: p.dir}, nil
	}
	intended, err := p.attach.apply(p.who)
	if err != nil {
		return Created{}, err
	}
	if err := os.MkdirAll(p.c.WorktreeRoot, 0o755); err != nil {
		return Created{}, fmt.Errorf("mkdir worktree root: %w", err)
	}
	made := Created{Dir: p.dir, NewWorktree: true}
	if p.attach.action == AdoptCreate {
		base := resolveBaseRef(p.c.Base)
		ui.Step("creating worktree at %s on a new branch %s (from %s)", p.dir, p.branch, base)
		if err := gitx.WorktreeAddNewBranch(p.dir, p.branch, base); err != nil {
			return Created{}, fmt.Errorf("git worktree add: %w", err)
		}
		made.NewBranch = true
	} else {
		ui.Step("attaching worktree at %s to existing local branch %s", p.dir, p.branch)
		if err := gitx.WorktreeAdopt(p.dir, p.branch); err != nil {
			return Created{}, fmt.Errorf("could not attach a worktree to local branch %q (it may be checked out in another worktree): %w", p.branch, err)
		}
		if err := verifyAdopted(p.dir, p.branch, intended, p.who); err != nil {
			return Created{}, err
		}
	}

	linkSharedFiles(p.c, p.dir)
	ui.OK("worktree ready")
	ui.Step("cd %s", p.dir)
	return made, nil
}

// Adopt attaches a worktree to an EXISTING branch — a colleague's or a previous
// session's PR branch — instead of forking a fresh one from base. It's the
// missing primitive behind #134: without it, picking up an in-flight PR branch
// meant a raw `git worktree add` that sidesteps wt's own registration, so claim
// could only ever create-new (and duplicated). It fetches origin/<branch> first
// so a branch living only on the remote materializes as a local tracking branch,
// then worktree-adds it (WorktreeAdopt — never `-b`, so it lands on that exact
// branch). Same live-worktree short-circuit + stale-dir reconcile + link_files
// wiring as New. (#134)
//
// #167: worktree-add takes refs/heads/<branch> whenever it exists, so a stale
// local branch left by an earlier PR that reused the name used to be checked out
// in place of the PR head. Now, after the fetch, the target is origin/<branch>;
// adopting by PR, it must first carry the PR's head (resolveAdoptTarget), or
// nothing is checked out, created or moved: a fork PR's head is not on origin.
// The local branch is then checked against it by prepareLocalBranch: equal or
// only ahead (unpushed) is attached, only-behind is fast-forwarded first, and a
// diverged one is refused with both SHAs. The new worktree must then be on the
// branch at that commit (verifyAdopted). The fetch runs before the short-circuit
// so an existing worktree is checked too (checkExistingWorktree) instead of being
// handed back unchecked.
func Adopt(c *config.Config, branch string, want AdoptWant) (string, error) {
	slug := strings.ReplaceAll(branch, "/", "-")
	wtDir := filepath.Join(c.WorktreeRoot, slug)

	ui.Step("fetching origin/%s", branch)
	fetchErr := gitx.Fetch("origin", branch)
	if fetchErr != nil {
		// Not fatal: the branch may be purely local, or origin may be offline —
		// WorktreeAdopt still succeeds on a local ref and errors clearly otherwise.
		ui.Warn("git fetch origin %s failed (trying local refs): %v", branch, fetchErr)
	}
	target, err := resolveAdoptTarget(branch, want, fetchErr == nil, c.WorktreeRoot)
	if err != nil {
		return "", err
	}

	if isValidWorktree(wtDir) {
		if err := checkExistingWorktree(wtDir, branch, target, adoptFor(branch, want)); err != nil {
			return "", err
		}
		ui.OK("worktree already exists at %s", wtDir)
		ui.Step("cd %s", wtDir)
		return wtDir, nil
	}
	if err := reconcileWorktreeDir(wtDir); err != nil {
		return "", err
	}
	// After the reconcile's prune, so a worktree whose directory is already gone
	// does not count as having the branch checked out.
	intended, err := prepareLocalBranch(branch, want, target)
	if err != nil {
		return "", err
	}

	if err := os.MkdirAll(c.WorktreeRoot, 0o755); err != nil {
		return "", fmt.Errorf("mkdir worktree root: %w", err)
	}

	ui.Step("attaching worktree at %s to existing branch %s", wtDir, branch)
	if err := gitx.WorktreeAdopt(wtDir, branch); err != nil {
		// Don't assert "not found": the branch may be checked out in another
		// worktree (common for adopt), or absent locally and on origin. The
		// error already carries git's own stderr with the real reason. (#134)
		return "", fmt.Errorf("could not attach a worktree to branch %q — it may be checked out in another worktree, or absent locally and on origin: %w", branch, err)
	}
	if err := verifyAdopted(wtDir, branch, intended, adoptFor(branch, want)); err != nil {
		return "", err
	}

	linkSharedFiles(c, wtDir)
	ui.OK("worktree ready")
	ui.Step("cd %s", wtDir)
	return wtDir, nil
}

// reconcileWorktreeDir prunes git's worktree admin metadata and clears a
// leftover EMPTY dir at wtDir so `git worktree add` won't refuse "already
// exists" (#62). os.Remove only succeeds on an empty dir, so it never nukes
// files.
func reconcileWorktreeDir(wtDir string) error {
	_ = gitx.WorktreePrune()
	if isDir(wtDir) && !gitx.IsInsideWorktree(wtDir) {
		if err := os.Remove(wtDir); err != nil {
			return fmt.Errorf("stale worktree dir %s exists but isn't a git worktree and isn't empty; remove it and retry: %w", wtDir, err)
		}
		ui.Step("cleared stale empty worktree dir %s", wtDir)
	}
	return nil
}

// linkSharedFiles symlinks each configured link_file from the main checkout into
// wtDir. Best-effort — a symlink that can't be created is silently skipped.
func linkSharedFiles(c *config.Config, wtDir string) {
	for _, f := range c.LinkFiles {
		src := filepath.Join(c.Root, f)
		if fileExists(src) {
			if err := os.Symlink(src, filepath.Join(wtDir, f)); err == nil {
				ui.Step("symlinked %s from main checkout", f)
			}
		}
	}
}

// Clean finds worktrees whose branch is fully shipped (patch-equivalent on the
// base, via git cherry). With apply=false (default) it only LISTS them and
// prints the remove commands; with apply=true it removes each (worktree +
// local branch) via Remove, skipping any that still have uncommitted changes.
//
// staleIndex (#88) additionally REPORTS (never removes) a worktree whose PR is
// MERGED but that holds a leftover uncommitted index a plain clean silently
// leaves forever — the #79 case (`check` won't stop flagging it, `clean` won't
// remove it). wt deliberately does NOT force-remove it: a MERGED PR only proves
// the committed work shipped, so the dirty index might be fresh post-merge work
// (the #88 review), and discarding uncommitted work wt can't prove is cruft is a
// permanent-loss footgun. It surfaces the worktree + the manual command so the
// operator inspects the changes and makes the destructive decision themselves.
// ManagedByClean reports whether `wt clean` should evaluate a worktree at all.
//
// Pure decision behind #101. By default clean only manages worktrees under the
// configured worktree_root — but the COLLISION ENGINE scans every worktree git
// knows about. A repo whose worktree_root ever changed (a legacy `<repo>.worktrees`
// beside the current `<repo>-worktrees`, say) therefore accumulates worktrees that
// are authoritative enough to hard-block a push and out of scope for the one
// command whose job is removing worktrees that should no longer matter. Left
// alone they never age out either, because dormant-branch suppression is gated on
// max_age, which is unset in a repo with no .wt.conf.
//
// Suppressing them in the collision engine instead would be the wrong direction:
// a worktree outside the root is still a real window that may be actively editing,
// and a false negative there is worse than the noise. So clean grows the reach.
func ManagedByClean(wtPath, root string, allRoots bool) bool {
	return allRoots || under(wtPath, root)
}

// ReapableBranch reports whether a worktree's branch is even a CANDIDATE for
// reaping, before any shipped-ness is considered.
//
// The base branch is the load-bearing case. A worktree checked out on the base
// is not shipped work, it is a base checkout — but "patch-equivalent on base" is
// trivially true for the base itself, so every downstream verdict says "shipped,
// safe to remove" and the printed command is `git branch -D main`.
//
// Surfaced by the #101 e2e: a legacy out-of-root worktree sat on main, and
// widening clean's reach would have armed a footgun the under-root filter had
// been hiding by accident rather than by design. Widening a blast radius means
// re-checking what the old narrowness was silently protecting.
func ReapableBranch(branch, base string) bool {
	return branch != "" && branch != "HEAD" && branch != base
}

// MatchedName returns the first of names that picks out the worktree at wtPath
// on branch, or "" (#169). A name matches the worktree's directory basename, its
// branch, or its path (`resolved` holds each name's real path, "" when it is not
// one). Matching only SELECTS: a named worktree still goes through every
// shipped-ness guard, so naming an unshipped one removes nothing. Pure.
func MatchedName(names, resolved []string, wtPath, branch string) string {
	for i, n := range names {
		if n == "" {
			continue
		}
		if n == filepath.Base(wtPath) || (branch != "" && n == branch) ||
			(i < len(resolved) && resolved[i] != "" && resolved[i] == wtPath) {
			return n
		}
	}
	return ""
}

// CheckNames refuses a name list that clean cannot act on exactly (#169), before
// anything is evaluated or removed. hits maps each name to the worktrees it picks
// out ON ITS OWN. Two ways a list is refused:
//   - a name that picks out nothing: almost always a typo, and "nothing to clean"
//     would read as "that worktree is not shipped";
//   - a name that picks out more than one worktree: a directory name in one and a
//     branch name in another (dir feat-d on feat/d, branch feat-d elsewhere).
//
// Either way nothing is cleaned, so a mistyped or ambiguous `-y` list never removes
// "the rest" of what it named. The review round found both. Pure.
func CheckNames(names []string, hits map[string][]string) error {
	var missing, ambiguous []string
	seen := map[string]bool{}
	for _, n := range names {
		if n == "" || seen[n] {
			continue
		}
		seen[n] = true
		switch len(hits[n]) {
		case 0:
			missing = append(missing, n)
		case 1:
		default:
			ambiguous = append(ambiguous, fmt.Sprintf("%s (%s)", n, strings.Join(hits[n], ", ")))
		}
	}
	var msgs []string
	if len(missing) > 0 {
		msgs = append(msgs, "no worktree matches "+strings.Join(missing, ", ")+" (a name is a worktree directory, a branch, or a path)")
	}
	if len(ambiguous) > 0 {
		msgs = append(msgs, "more than one worktree matches "+strings.Join(ambiguous, "; ")+", so name it by its path")
	}
	if len(msgs) == 0 {
		return nil
	}
	return fmt.Errorf("%s; nothing was cleaned", strings.Join(msgs, "; "))
}

// RerunHint is the line a listing-only clean ends with, or "" when it listed
// nothing as shipped. listed holds, for a named run, the names that picked out a
// shipped worktree. The old unconditional hint told the reader to "remove the
// shipped ones listed above" after listing none, and for a name that matched
// nothing it echoed the name back as a command that exits 1 (#169). Pure.
func RerunHint(named bool, listed []string) string {
	if len(listed) == 0 {
		return ""
	}
	if named {
		return fmt.Sprintf("re-run with `wt clean -y %s` to remove the shipped ones listed above", strings.Join(listed, " "))
	}
	return "re-run with `wt clean -y` to remove the shipped worktrees listed above"
}

// Clean lists (or, with apply, removes) the shipped secondary worktrees. names,
// when non-empty, limits the run to the worktrees they pick out (#169).
func Clean(c *config.Config, apply, staleIndex, allRoots bool, names []string) error {
	ui.Step("fetching origin/%s", c.Base)
	_ = gitx.Fetch("origin", c.Base)
	_ = gitx.WorktreePrune() // #42: drop stale metadata for manually-deleted dirs

	paths, err := gitx.WorktreePaths()
	if err != nil {
		return err
	}
	if len(paths) <= 1 {
		ui.Info("no secondary worktrees to clean")
		return CheckNames(names, nil)
	}
	// A name that is a path is compared by its real path, the form git reports.
	resolved := make([]string, len(names))
	for i, n := range names {
		if strings.ContainsRune(n, filepath.Separator) {
			resolved[i] = realPath(n)
		}
	}
	// #169: resolve every name before acting on any (see CheckNames).
	if len(names) > 0 {
		hits := map[string][]string{}
		for _, wt := range paths[1:] {
			b, _ := gitx.CurrentBranchIn(wt)
			for i, n := range names {
				if MatchedName(names[i:i+1], resolved[i:i+1], realPath(wt), b) != "" && !slices.Contains(hits[n], wt) {
					hits[n] = append(hits[n], wt)
				}
			}
		}
		if err := CheckNames(names, hits); err != nil {
			return err
		}
	}

	now := time.Now()
	removed := 0
	var listed []string // what a listing-only run showed as shipped, for RerunHint
	skippedOutOfRoot := 0
	for _, wt := range paths[1:] { // skip primary (index 0)
		name := ""
		if len(names) > 0 {
			b, _ := gitx.CurrentBranchIn(wt)
			name = MatchedName(names, resolved, realPath(wt), b)
			if name == "" {
				continue // not named: say nothing about it
			}
		}
		if !ManagedByClean(wt, c.WorktreeRoot, allRoots) {
			skippedOutOfRoot++
			// Say what to DO about it (#101): the old wording ("never offered for
			// cleanup") read as a safety refusal, so nobody realised these are
			// exactly the worktrees that go on blocking pushes forever.
			fmt.Printf("# %s — outside worktree root; `wt clean --all-roots` evaluates it  (%s)\n", filepath.Base(wt), wt)
			continue
		}
		if !isDir(wt) {
			continue
		}
		br, err := gitx.CurrentBranchIn(wt)
		if err != nil {
			continue
		}
		if !ReapableBranch(br, c.Base) {
			if br == c.Base {
				ui.Info("%s — checked out on the base branch, never reaped", filepath.Base(wt))
			}
			continue
		}
		// #61 safety: freshly-created worktrees are protected by a grace window so
		// another window's clean can't reap them mid-work; a branch that was never
		// pushed holds new/unshared work, not stale work. "Never pushed" includes a
		// `wt new` branch whose only upstream is the base it was cut from (#175),
		// and a dirty tree is live work whatever its branch says (#174).
		age, ageOK := worktreeAge(wt, now)
		withinGrace := ageOK && age < cleanGraceWindow
		pushed := PushedUpstream(gitx.HasUpstream(wt), gitx.UpstreamMergeRef(wt, br), c.Base)
		dirty := !gitx.IsClean(wt) // fails closed: unreadable status counts as dirty
		n, cerr := gitx.CountUnshipped("origin/"+c.Base, "refs/heads/"+br)
		if cerr != nil {
			n, cerr = gitx.CountUnshipped(c.Base, "refs/heads/"+br)
		}
		cherryFailed := cerr != nil
		// One PR-state read (#88) shared with collide/status/check via ghx —
		// merged ⇒ shipped (#37 squash). Feeds both the normal reap and --stale-index.
		// #168: when no PR has this branch's name, the branch's tip commit is looked
		// up instead, so work pushed under another name and squash-merged there is
		// recognised as shipped. A dirty worktree is still never removed.
		prNum, prState, viaTip, prOK := ghx.PRForBranchOrTip(br, gitx.ResolveRemoteBase(c.Base))
		prMerged := prOK && prState == "MERGED"

		// #88 --stale-index: REPORT (never auto-remove) a MERGED-PR worktree that
		// holds a leftover uncommitted index a plain clean silently leaves forever
		// (#79). wt won't force-discard it — a MERGED PR only proves the committed
		// work shipped, and the dirty index could be fresh post-merge work (the #88
		// review). Surface it + the manual command; the operator inspects + decides.
		if StaleIndexReportable(prState, prOK, dirty, staleIndex, withinGrace) {
			if viaTip {
				// #168: a follow-up branch started at the PR's head looks the same,
				// so "leftover" is a guess here, not the likely reading.
				ui.Warn("%s — PR #%s merged (found by the branch's tip commit), but this worktree has uncommitted changes (%d dirty file(s)) that may be a follow-up rather than leftovers, so plain clean leaves it. INSPECT it, then if it's stale leftovers (not fresh work) remove it by hand:", br, prNum, dirtyCount(wt))
			} else {
				ui.Warn("%s — PR #%s merged, but this worktree has a leftover uncommitted index (%d dirty file(s)) so plain clean leaves it. INSPECT it, then if it's stale leftovers (not fresh work) remove it by hand:", br, prNum, dirtyCount(wt))
			}
			fmt.Printf("  git -C %s status              # confirm what's uncommitted FIRST\n", wt)
			fmt.Printf("  git worktree remove --force %s && git branch -D %s\n", wt, br)
			continue
		}

		if !ReapVerdict(n, cherryFailed, prMerged, pushed, withinGrace, dirty) {
			switch {
			case withinGrace:
				ui.Info("%s — created %s ago, within grace window, leave alone", br, age.Round(time.Second))
			case dirty && prMerged:
				ui.Info("%s — PR #%s merged, but has uncommitted changes (%d file(s)), leave alone; `wt clean --stale-index` reports it with the manual remove", br, prNum, dirtyCount(wt))
			case dirty:
				ui.Info("%s — has uncommitted changes (%d file(s)), leave alone", br, dirtyCount(wt))
			case !pushed:
				ui.Info("%s — never pushed (no upstream of its own), leave alone", br)
			case cherryFailed:
				// can't tell (no cherry base) and no merged PR → leave alone silently
			default:
				ui.Info("%s — %d commit(s) not on %s, leave alone", br, n, c.Base)
			}
			continue
		}
		reason := fmt.Sprintf("patch-equivalent on %s", c.Base)
		if prMerged {
			reason = "PR merged"
			if viaTip {
				reason = fmt.Sprintf("PR #%s merged, found by the branch's tip commit", prNum)
			}
		}
		if !apply {
			ui.OK("%s — shipped (%s), safe to remove", br, reason)
			fmt.Printf("  git worktree remove %s && git branch -D %s\n", wt, br)
			listed = append(listed, name)
			continue
		}
		// apply: remove it (never force — refuse to discard uncommitted work).
		// allRoots relaxes ONLY the under-root guard; every data-loss guard above
		// (grace window, upstream, cherry/PR-merged proof, clean tree) still ran.
		if err := remove(c, wt, br, false, allRoots); err != nil {
			ui.Warn("skipped %s: %v", br, err)
			continue
		}
		removed++
	}
	if apply {
		if removed == 0 {
			ui.Info("nothing removed — no fully-shipped worktrees clean enough to delete")
		} else {
			ui.OK("removed %d shipped worktree(s)", removed)
		}
	} else if hint := RerunHint(len(names) > 0, listed); hint != "" {
		ui.Info("%s", hint)
	}
	if skippedOutOfRoot > 0 {
		// The whole point of #101: these still collide, so "clean says nothing to
		// do" must not read as "nothing can be blocking you".
		ui.Info("%d worktree(s) live outside %s and were NOT evaluated — the collision engine still scans them, so they can block a push that clean won't clear. Re-run with --all-roots to include them.", skippedOutOfRoot, c.WorktreeRoot)
	}
	return nil
}

// Remove deletes the worktree at wtPath and (unless detached) its local branch.
// Guards, in order:
//   - refuses any path NOT under c.WorktreeRoot (never touches the primary
//     checkout or a foreign/harness worktree),
//   - refuses a worktree with uncommitted changes unless force (so we never
//     silently discard in-flight work; force is for known-junk like a stray
//     extracted binary),
//   - a failed branch delete is a warning, not an error (the worktree is
//     already gone; a lingering local branch is harmless).
func Remove(c *config.Config, wtPath, branch string, force bool) error {
	return remove(c, wtPath, branch, force, false)
}

// remove is Remove with the under-root guard optionally relaxed for `wt clean
// --all-roots` (#101). allowOutsideRoot ONLY widens which directories are in
// scope — it never relaxes the uncommitted-work guard, and callers must still
// have proven the branch shipped. Kept unexported so the safe Remove stays the
// only entry point everything else can reach.
func remove(c *config.Config, wtPath, branch string, force, allowOutsideRoot bool) error {
	if !ManagedByClean(wtPath, c.WorktreeRoot, allowOutsideRoot) {
		return fmt.Errorf("not under worktree root %s — refusing to remove", c.WorktreeRoot)
	}
	if !force && !gitx.IsClean(wtPath) {
		return fmt.Errorf("has uncommitted changes (commit/stash, or force to discard)")
	}
	if err := gitx.WorktreeRemove(wtPath, force); err != nil {
		return fmt.Errorf("git worktree remove: %w", err)
	}
	ui.OK("removed worktree %s", filepath.Base(wtPath))
	if branch == c.Base {
		// Defense in depth alongside Clean's own guard: removing a base checkout
		// is fine, deleting the base branch is not. `wt release --clean` reaches
		// here too, so the refusal belongs at the delete, not only at the caller.
		ui.Info("kept branch %s — it is the base branch", branch)
		return nil
	}
	if branch != "" && branch != "HEAD" {
		if err := gitx.BranchDelete(branch); err != nil {
			ui.Warn("branch %s not deleted (harmless): %v", branch, err)
		} else {
			ui.Step("deleted local branch %s", branch)
		}
	}
	return nil
}

// resolveBaseRef prefers origin/<base>, falls back to local <base>, then HEAD.
func resolveBaseRef(base string) string {
	for _, ref := range []string{"origin/" + base, base} {
		if _, err := gitx.Run("rev-parse", "--verify", "--quiet", ref+"^{commit}"); err == nil {
			return ref
		}
	}
	return "HEAD"
}

func isDir(p string) bool {
	fi, err := os.Stat(p)
	return err == nil && fi.IsDir()
}

func fileExists(p string) bool {
	fi, err := os.Stat(p)
	return err == nil && !fi.IsDir()
}

func under(path, root string) bool {
	ap := realPath(path)
	ar := realPath(root)
	rel, err := filepath.Rel(ar, ap)
	return err == nil && rel != ".." && !strings.HasPrefix(rel, ".."+string(filepath.Separator))
}

// realPath resolves symlinks when possible (e.g. macOS /var → /private/var,
// which otherwise makes the env-supplied worktree root mismatch git's resolved
// paths), falling back to an absolute path.
func realPath(p string) string {
	if r, err := filepath.EvalSymlinks(p); err == nil {
		return r
	}
	if a, err := filepath.Abs(p); err == nil {
		return a
	}
	return p
}
