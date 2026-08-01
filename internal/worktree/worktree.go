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
	// Reconcile stale state: prune git's admin metadata, and clear a leftover
	// empty dir so `git worktree add` doesn't refuse with "already exists" (#62).
	_ = gitx.WorktreePrune()
	if isDir(wtDir) && !gitx.IsInsideWorktree(wtDir) {
		if err := os.Remove(wtDir); err != nil { // only succeeds if empty — never nukes files
			return "", fmt.Errorf("stale worktree dir %s exists but isn't a git worktree and isn't empty; remove it and retry: %w", wtDir, err)
		}
		ui.Step("cleared stale empty worktree dir %s", wtDir)
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

	for _, f := range c.LinkFiles {
		src := filepath.Join(c.Root, f)
		if fileExists(src) {
			if err := os.Symlink(src, filepath.Join(wtDir, f)); err == nil {
				ui.Step("symlinked %s from main checkout", f)
			}
		}
	}

	ui.OK("worktree ready")
	ui.Step("cd %s", wtDir)
	return wtDir, nil
}

// Clean finds worktrees whose branch is fully shipped (patch-equivalent on the
// base, via git cherry). With apply=false (default) it only LISTS them and
// prints the remove commands; with apply=true it removes each (worktree +
// local branch) via Remove, skipping any that still have uncommitted changes.
func Clean(c *config.Config, apply bool) error {
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
	for _, wt := range paths[1:] { // skip primary (index 0)
		if !under(wt, c.WorktreeRoot) {
			fmt.Printf("# %s — outside worktree root, never offered for cleanup\n", filepath.Base(wt))
			continue
		}
		if !isDir(wt) {
			continue
		}
		br, err := gitx.CurrentBranchIn(wt)
		if err != nil || br == "" || br == "HEAD" {
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
		prMerged := ghx.MergedPRForBranch(br) // #37: squash-merged branches read shipped here
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
		if err := Remove(c, wt, br, false); err != nil {
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
	if !under(wtPath, c.WorktreeRoot) {
		return fmt.Errorf("not under worktree root %s — refusing to remove", c.WorktreeRoot)
	}
	if !force && !gitx.IsClean(wtPath) {
		return fmt.Errorf("has uncommitted changes (commit/stash, or force to discard)")
	}
	if err := gitx.WorktreeRemove(wtPath, force); err != nil {
		return fmt.Errorf("git worktree remove: %w", err)
	}
	ui.OK("removed worktree %s", filepath.Base(wtPath))
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
