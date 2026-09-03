// Package worktree creates and prunes per-window git worktrees.
package worktree

import (
	"fmt"
	"os"
	"path/filepath"
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
//   - a MERGED PR                                  → reap (pushed + merged)
//   - no upstream (never pushed)                   → keep (new / unshared work)
//   - pushed AND patch-equivalent on base (cherry) → reap
//
// The old ShippedVerdict classified a commitless worktree (cherry reports 0)
// as shipped — reaping a brand-new `wt new` checkout. Requiring an upstream
// closes that: `wt new` never pushes, so its worktree is never swept. Pure.
func ReapVerdict(unshipped int, cherryFailed, prMerged, hasUpstream, withinGrace bool) bool {
	if withinGrace {
		return false
	}
	if prMerged {
		return true
	}
	if !hasUpstream {
		return false
	}
	return !cherryFailed && unshipped == 0
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
// and returns its path. Returns the worktree path.
func New(c *config.Config, branch string) (string, error) {
	slug := strings.ReplaceAll(branch, "/", "-")
	wtDir := filepath.Join(c.WorktreeRoot, slug)

	// Short-circuit only when it's a LIVE worktree (#62) — not a stale/empty dir
	// left by another window's clean or an out-of-band `git worktree remove`.
	if isValidWorktree(wtDir) {
		ui.OK("worktree already exists at %s", wtDir)
		ui.Step("cd %s", wtDir)
		return wtDir, nil
	}
	if err := reconcileWorktreeDir(wtDir); err != nil {
		return "", err
	}

	ui.Step("fetching origin/%s", c.Base)
	if err := gitx.Fetch("origin", c.Base); err != nil {
		ui.Warn("git fetch failed (continuing with local refs): %v", err)
	}

	if err := os.MkdirAll(c.WorktreeRoot, 0o755); err != nil {
		return "", fmt.Errorf("mkdir worktree root: %w", err)
	}

	base := resolveBaseRef(c.Base)
	ui.Step("creating worktree at %s on %s (from %s)", wtDir, branch, base)
	if err := gitx.WorktreeAdd(wtDir, branch, base); err != nil {
		return "", fmt.Errorf("git worktree add: %w", err)
	}

	linkSharedFiles(c, wtDir)
	ui.OK("worktree ready")
	ui.Step("cd %s", wtDir)
	return wtDir, nil
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
func Adopt(c *config.Config, branch string) (string, error) {
	slug := strings.ReplaceAll(branch, "/", "-")
	wtDir := filepath.Join(c.WorktreeRoot, slug)

	if isValidWorktree(wtDir) {
		ui.OK("worktree already exists at %s", wtDir)
		ui.Step("cd %s", wtDir)
		return wtDir, nil
	}
	if err := reconcileWorktreeDir(wtDir); err != nil {
		return "", err
	}

	ui.Step("fetching origin/%s", branch)
	if err := gitx.Fetch("origin", branch); err != nil {
		// Not fatal: the branch may be purely local, or origin may be offline —
		// WorktreeAdopt still succeeds on a local ref and errors clearly otherwise.
		ui.Warn("git fetch origin %s failed (trying local refs): %v", branch, err)
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

func Clean(c *config.Config, apply, staleIndex, allRoots bool) error {
	ui.Step("fetching origin/%s", c.Base)
	_ = gitx.Fetch("origin", c.Base)
	_ = gitx.WorktreePrune() // #42: drop stale metadata for manually-deleted dirs

	paths, err := gitx.WorktreePaths()
	if err != nil {
		return err
	}
	if len(paths) <= 1 {
		ui.Info("no secondary worktrees to clean")
		return nil
	}

	now := time.Now()
	removed := 0
	skippedOutOfRoot := 0
	for _, wt := range paths[1:] { // skip primary (index 0)
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
		// another window's clean can't reap them mid-work; a branch with no
		// upstream was never pushed, so it holds new/unshared work, not stale work.
		age, ageOK := worktreeAge(wt, now)
		withinGrace := ageOK && age < cleanGraceWindow
		hasUpstream := gitx.HasUpstream(wt)
		n, cerr := gitx.CountUnshipped("origin/"+c.Base, "refs/heads/"+br)
		if cerr != nil {
			n, cerr = gitx.CountUnshipped(c.Base, "refs/heads/"+br)
		}
		cherryFailed := cerr != nil
		// One PR-state read (#88) shared with collide/status/check via ghx —
		// merged ⇒ shipped (#37 squash). Feeds both the normal reap and --stale-index.
		prNum, prState, prOK := ghx.PRForBranch(br)
		prMerged := prOK && prState == "MERGED"

		// #88 --stale-index: REPORT (never auto-remove) a MERGED-PR worktree that
		// holds a leftover uncommitted index a plain clean silently leaves forever
		// (#79). wt won't force-discard it — a MERGED PR only proves the committed
		// work shipped, and the dirty index could be fresh post-merge work (the #88
		// review). Surface it + the manual command; the operator inspects + decides.
		if StaleIndexReportable(prState, prOK, !gitx.IsClean(wt), staleIndex, withinGrace) {
			ui.Warn("%s — PR #%s merged, but this worktree has a leftover uncommitted index (%d dirty file(s)) so plain clean leaves it. INSPECT it, then if it's stale leftovers (not fresh work) remove it by hand:", br, prNum, dirtyCount(wt))
			fmt.Printf("  git -C %s status              # confirm what's uncommitted FIRST\n", wt)
			fmt.Printf("  git worktree remove --force %s && git branch -D %s\n", wt, br)
			continue
		}

		if !ReapVerdict(n, cherryFailed, prMerged, hasUpstream, withinGrace) {
			switch {
			case withinGrace:
				ui.Info("%s — created %s ago, within grace window, leave alone", br, age.Round(time.Second))
			case !hasUpstream:
				ui.Info("%s — never pushed (no upstream), holds unshared work, leave alone", br)
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
		}
		if !apply {
			ui.OK("%s — shipped (%s), safe to remove", br, reason)
			fmt.Printf("  git worktree remove %s && git branch -D %s\n", wt, br)
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
	} else if removed == 0 {
		ui.Info("re-run with `wt clean -y` to remove the shipped worktrees listed above")
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
