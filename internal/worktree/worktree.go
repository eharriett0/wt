// Package worktree creates and prunes per-window git worktrees.
package worktree

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/eharriett0/wt/internal/config"
	"github.com/eharriett0/wt/internal/gitx"
	"github.com/eharriett0/wt/internal/ui"
)

// New creates a worktree for branch under c.WorktreeRoot, based on the repo's
// base branch. Idempotent: if the worktree already exists, prints the cd hint
// and returns its path. Returns the worktree path.
func New(c *config.Config, branch string) (string, error) {
	slug := strings.ReplaceAll(branch, "/", "-")
	wtDir := filepath.Join(c.WorktreeRoot, slug)

	if isDir(wtDir) {
		ui.OK("worktree already exists at %s", wtDir)
		ui.Step("cd %s", wtDir)
		return wtDir, nil
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

	paths, err := gitx.WorktreePaths()
	if err != nil {
		return err
	}
	if len(paths) <= 1 {
		ui.Info("no secondary worktrees to clean")
		return nil
	}

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
		n, err := gitx.CountUnshipped("origin/"+c.Base, "refs/heads/"+br)
		if err != nil {
			if n, err = gitx.CountUnshipped(c.Base, "refs/heads/"+br); err != nil {
				continue
			}
		}
		if n != 0 {
			ui.Info("%s — %d commit(s) not on %s, leave alone", br, n, c.Base)
			continue
		}
		if !apply {
			ui.OK("%s — fully shipped (patch-equivalent on %s), safe to remove", br, c.Base)
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
