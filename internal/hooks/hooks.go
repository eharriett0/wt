// Package hooks installs and implements the git hooks: a pre-push guard against
// pushing to the base branch, and a collision-aware pre-commit notice. The
// installed hooks are thin shims that exec `wt _hook <name>`, so all logic
// lives in the binary (no embedded bash).
package hooks

import (
	"bufio"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"regexp"
	"strings"

	"github.com/eharriett0/wt/internal/activework"
	"github.com/eharriett0/wt/internal/collide"
	"github.com/eharriett0/wt/internal/config"
	"github.com/eharriett0/wt/internal/gitx"
	"github.com/eharriett0/wt/internal/ui"
)

// sentinel is the marker every wt-managed shim carries in its comment line. It
// is matched to decide "is this hook ours?" for idempotent overwrite + doctor
// detection. It MUST be a literal substring of the shim body — the previous
// value "wt _hook" was NOT (the shim's exec line is `exec "…/wt" _hook …`, i.e.
// `wt" _hook`, so the quote broke the substring), which made doctor report wt's
// own hooks as "not installed" and made re-install treat them as foreign (#91).
const sentinel = "wt-managed hook"

// shimStatus is the outcome of installing one hook, so Install can report
// honestly instead of printing a green ✓ after skipping everything (#91).
type shimStatus int

const (
	shimInstalled shimStatus = iota // no prior hook — wrote fresh
	shimRefreshed                   // prior hook was ours — overwrote (idempotent)
	shimReplaced                    // prior foreign hook backed up + replaced (--force)
	shimSkipped                     // prior foreign hook left in place (no --force)
)

// PrePushInstalled reports whether a wt-managed pre-push hook is present in the
// repo's shared hooks dir — used by `wt doctor` to flag an unguarded repo where
// the base-branch push guard never ran (#76).
func PrePushInstalled(commonDir string) bool {
	b, err := os.ReadFile(filepath.Join(commonDir, "hooks", "pre-push"))
	return err == nil && strings.Contains(string(b), sentinel)
}

// Install writes the pre-push + pre-commit shims into the shared hooks dir
// (covers every worktree of the repo). force overwrites foreign hooks.
func Install(c *config.Config, force bool) error {
	common, err := gitx.CommonDir()
	if err != nil {
		return fmt.Errorf("resolve git common dir: %w", err)
	}
	hooksDir := filepath.Join(common, "hooks")
	if err := os.MkdirAll(hooksDir, 0o755); err != nil {
		return err
	}
	self, err := os.Executable()
	if err != nil || self == "" {
		self = "wt"
	}

	var installed, refreshed, skipped int
	tally := func(st shimStatus) {
		switch st {
		case shimInstalled:
			installed++
		case shimRefreshed, shimReplaced:
			refreshed++
		case shimSkipped:
			skipped++
		}
	}

	st, err := writeShim(filepath.Join(hooksDir, "pre-push"), self, "pre-push", force)
	if err != nil {
		return err
	}
	tally(st)

	if frameworkPresent(c.Root, hooksDir) {
		ui.Warn("pre-commit framework detected — not clobbering pre-commit hook")
		ui.Info("add this to .pre-commit-config.yaml instead:")
		fmt.Print(precommitSnippet(self))
	} else {
		st, err := writeShim(filepath.Join(hooksDir, "pre-commit"), self, "pre-commit", force)
		if err != nil {
			return err
		}
		tally(st)
	}

	// Honest outcome (#91): only claim ✓ when something was actually written, and
	// never a green ✓ after skipping every hook.
	switch {
	case installed+refreshed == 0 && skipped > 0:
		ui.Warn("nothing installed — %d foreign hook(s) already present in %s (use --force to back up + replace)", skipped, hooksDir)
		return nil
	case installed == 0 && refreshed > 0 && skipped == 0:
		ui.OK("hooks already wt-managed + refreshed in %s", hooksDir)
	default:
		ui.OK("hooks installed in %s", hooksDir)
		if skipped > 0 {
			ui.Warn("...but skipped %d foreign hook(s) (use --force to back up + replace)", skipped)
		}
	}
	ui.Info("shared across all worktrees of this repo")
	ui.Info("pre-push also warns on base-conflict (no-CI PR, #78) + BLOCKS on a HIGH file collision (#74)")
	ui.Info("bypass: HOOK_DISABLE_MAIN_PUSH=1 (base-push) · WT_SKIP_COLLISION=1 (push collision) · HOOK_DISABLE_MULTIWINDOW_CHECK=1 (commit+push collision)")
	return nil
}

func writeShim(path, self, hook string, force bool) (shimStatus, error) {
	body := fmt.Sprintf("#!/bin/sh\n# wt-managed hook — multi-window coordination (see `wt help`).\nexec %q _hook %s \"$@\"\n", self, hook)

	status := shimInstalled
	if existing, err := os.ReadFile(path); err == nil {
		if strings.Contains(string(existing), sentinel) {
			status = shimRefreshed // ours — idempotent overwrite (refresh the binary path)
		} else if force {
			_ = os.WriteFile(path+".bak", existing, 0o755)
			ui.Warn("backed up existing %s hook to %s.bak", hook, filepath.Base(path))
			status = shimReplaced
		} else {
			ui.Warn("a non-wt %s hook already exists at %s — skipping (use --force to back up + replace)", hook, path)
			return shimSkipped, nil
		}
	}
	if err := os.WriteFile(path, []byte(body), 0o755); err != nil {
		return status, err
	}
	return status, nil
}

func frameworkPresent(root, hooksDir string) bool {
	for _, f := range []string{".pre-commit-config.yaml", ".pre-commit-config.yml"} {
		if _, err := os.Stat(filepath.Join(root, f)); err == nil {
			return true
		}
	}
	if b, err := os.ReadFile(filepath.Join(hooksDir, "pre-commit")); err == nil {
		s := string(b)
		if strings.Contains(s, "File generated by pre-commit") || strings.Contains(s, "INSTALL_PYTHON") {
			return true
		}
	}
	return false
}

func precommitSnippet(self string) string {
	return fmt.Sprintf(`
  - repo: local
    hooks:
      - id: wt-collision-check
        name: wt multi-window collision check
        entry: %s _hook pre-commit
        language: system
        pass_filenames: false
        always_run: true
`, self)
}

// HookPrePush implements `wt _hook pre-push`. Three checks, in order of cost:
//
//  1. base-branch guard — reject a direct push to the base branch (blocking;
//     bypass HOOK_DISABLE_MAIN_PUSH=1).
//  2. base-drift warning (#78) — an offline `git merge-tree` against the
//     last-known base; a conflict means the PR gets ZERO CI until rebased
//     (loud WARN, non-blocking), and a clean-but-behind branch gets a one-liner.
//  3. collision check (#74) — the outgoing paths vs every other active window;
//     a HIGH overlap is the last moment to avoid a duplicate PR, so it BLOCKS
//     (bypass WT_SKIP_COLLISION=1).
//
// Reads either the pre-commit-framework env vars (base-guard only — no local
// sha available there) or the raw pre-push stdin protocol (the wt-installed
// shim, and the plain-`git push`-from-a-worktree case #74/#78 actually target).
func HookPrePush(c *config.Config, stdin io.Reader) int {
	base := c.Base

	if rb := os.Getenv("PRE_COMMIT_REMOTE_BRANCH"); rb != "" {
		if os.Getenv("HOOK_DISABLE_MAIN_PUSH") != "1" && (rb == "refs/heads/"+base || rb == base) {
			rejectPush(base)
			return 1
		}
		return 0
	}

	code := 0
	// The window scan (for the collision check) is expensive; load it lazily and
	// once, only when a non-base line actually needs it.
	var (
		ws        []collide.Window
		root      string
		scanTried bool
		scanOK    bool
	)
	sc := bufio.NewScanner(stdin)
	for sc.Scan() {
		fields := strings.Fields(sc.Text())
		// <local_ref> <local_sha> <remote_ref> <remote_sha>
		if len(fields) < 4 {
			continue
		}
		localSHA, remoteRef, remoteSHA := fields[1], fields[2], fields[3]

		// (1) base-branch guard.
		if remoteRef == "refs/heads/"+base {
			if os.Getenv("HOOK_DISABLE_MAIN_PUSH") != "1" {
				rejectPush(base)
				code = 1
			}
			continue
		}
		// A branch deletion (local sha all-zero) pushes nothing — skip 2 & 3.
		if gitx.AllZeroSHA(localSHA) {
			continue
		}

		// (2) base-drift / conflict warning — offline, never blocks.
		warnBaseDrift(base, localSHA)

		// (3) collision check over the outgoing paths.
		if collisionSkipped() {
			continue
		}
		if !scanTried {
			scanTried = true
			if w, err := collide.Scan(c); err == nil {
				ws, root, scanOK = w, repoRootOrEmpty(), true
			}
		}
		if !scanOK {
			continue
		}
		if pushCollisionBlocks(c, ws, root, outgoingPaths(base, localSHA, remoteSHA)) {
			code = 1
		}
	}
	return code
}

func rejectPush(base string) {
	ui.Err("REJECTED: direct push to %q is blocked by the wt pre-push guard.", base)
	fmt.Fprintln(os.Stderr, "  Use a branch + PR:  wt new <slug>  →  commit/push  →  gh pr create  →  wt merge-pr <N>")
	fmt.Fprintln(os.Stderr, "  Bypass (genuine intent — rollback/recovery):  HOOK_DISABLE_MAIN_PUSH=1 git push")
}

// collisionSkipped reports whether the #74 push collision check is disabled —
// its own WT_SKIP_COLLISION escape hatch OR the shared HOOK_DISABLE_MULTIWINDOW_CHECK
// that already silences the pre-commit collision notice (so one silence covers both).
func collisionSkipped() bool {
	return os.Getenv("WT_SKIP_COLLISION") == "1" || os.Getenv("HOOK_DISABLE_MULTIWINDOW_CHECK") == "1"
}

func repoRootOrEmpty() string {
	r, _ := gitx.RepoRoot()
	return r
}

// outgoingPaths returns the repo-relative paths in the commits being pushed. For
// an update it diffs the remote sha the push is fast-forwarding from; for a
// brand-new branch (remote sha all-zero) it diffs the last-known base, so the
// full branch is checked. Best-effort — empty on any git error.
func outgoingPaths(base, localSHA, remoteSHA string) []string {
	remoteIsAncestor := !gitx.AllZeroSHA(remoteSHA) && gitx.IsAncestor(remoteSHA, localSHA)
	from := outgoingFrom(base, remoteSHA, remoteIsAncestor, gitx.ResolveRemoteBase(base))
	paths, _ := gitx.RangeChangedPaths(from, localSHA)
	return paths
}

// outgoingFrom decides which ref the outgoing range is measured FROM. Pure, so
// the three cases are table-testable without a live repo.
//
//   - fast-forward update — the remote head IS an ancestor, so remote..local is
//     exactly the new commits. Use it; it is the most precise answer.
//   - brand-new branch — remote sha is all-zero; nothing on the remote to diff
//     against, so measure the whole branch against the base.
//   - NON-fast-forward (#106) — the remote head is no longer an ancestor, which
//     is what `git rebase origin/main && git push --force-with-lease` produces.
//     remote..local would walk THROUGH the base's commits and report every file
//     the base gained as "outgoing", so the hook blocked on files the pusher had
//     never touched — and the further behind the branch was, the more it invented.
//     That contradicted `wt check` on identical input (the equality #92 restored),
//     and it fired precisely when someone did the recommended thing and rebased.
//     Measure against the base instead: what this branch contributes over it.
func outgoingFrom(base, remoteSHA string, remoteIsAncestor bool, resolvedBase string) string {
	if !gitx.AllZeroSHA(remoteSHA) && remoteIsAncestor {
		return remoteSHA
	}
	if resolvedBase != "" {
		return resolvedBase
	}
	return base
}

// warnBaseDrift runs the offline #78 base-drift check for one outgoing ref. A
// conflict against the last-known base means the resulting PR receives NO CI at
// all (GitHub can't build refs/pull/N/merge) — the part nobody knows — so it
// warns loudly and names the paths. A clean-but-behind branch (the state that
// becomes a conflict minutes later) gets a single informational line. Never
// blocks: the fix is a rebase, which blocking the push doesn't help.
func warnBaseDrift(base, localSHA string) {
	baseRef := gitx.ResolveRemoteBase(base)
	if baseRef == "" {
		return // no base ref locally — nothing to compare against
	}
	paths, conflicted, err := gitx.MergeTreeConflicts(baseRef, localSHA)
	if err != nil {
		return // merge-tree couldn't run (ancient git / bad ref) — fail open
	}
	if conflicted {
		ui.Warn("this branch CONFLICTS with %s — the resulting PR will receive NO CI at all until rebased.", base)
		fmt.Fprintln(os.Stderr, ui.Dim("   GitHub can't build refs/pull/N/merge on a conflicting PR, so no workflow ever dispatches"))
		fmt.Fprintln(os.Stderr, ui.Dim("   (absence of checks is the symptom — indistinguishable from a cold runner pool)."))
		if len(paths) > 0 {
			fmt.Fprintln(os.Stderr, ui.Yellow("   conflicting: "+strings.Join(paths, ", ")))
		}
		fmt.Fprintln(os.Stderr, ui.Dim("   Fix:  git fetch origin && git rebase origin/"+base))
		return
	}
	if n := gitx.BehindCount(localSHA, baseRef); n > 0 {
		fmt.Fprintln(os.Stderr, ui.Dim(fmt.Sprintf("ℹ️  behind %s by %d commit(s) — rebase soon; this is the state that becomes a conflict.", base, n)))
	}
}

// pushCollisionBlocks runs the #74 collision engine over the outgoing paths and
// reports whether the push should be BLOCKED — true only when an ACTIVE window
// (open PR / unmerged, not stale/merged) is editing the same non-shared,
// non-append-only file. Shared docs (CLAUDE.md/MEMORY.md) and append-only paths
// are downgraded to advisories so they never block. Prints the competing
// branch(es) + PR and the bypass hint on a hard block.
func pushCollisionBlocks(c *config.Config, ws []collide.Window, root string, paths []string) bool {
	if len(paths) == 0 {
		return false
	}
	conflicts := collide.CheckPaths(ws, root, paths)
	if len(conflicts) == 0 {
		return false
	}
	live := collide.ClassifyWindows(ws, c.Base, collide.ConflictWindowSet(conflicts), c.MaxAge)
	active, _ := collide.PartitionConflicts(conflicts, live)

	// Grade IDENTICALLY to `wt check` + pre-commit (#92, #97): shared-docs +
	// append-only are advisory; a code file blocks only when the two windows'
	// changed line-ranges OVERLAP (disjoint hunks stay advisory).
	hard, soft := gradeConflicts(c, active, root, ws, gitx.ChangedRanges)
	if len(hard) == 0 {
		if len(soft) > 0 {
			fmt.Fprintln(os.Stderr, ui.Dim(fmt.Sprintf("📝 %d advisory overlap(s) (shared docs / append-only / disjoint hunks) — not blocking", len(soft))))
		}
		return false
	}
	files := distinctPaths(hard)
	ui.Collision("%d outgoing file(s) have an OVERLAPPING edit by an active window — pushing now risks a duplicate PR:", len(files))
	for _, cf := range dedupConflicts(hard) {
		fmt.Fprintf(os.Stderr, "   %s  %s %s %s\n", ui.Bold(cf.Path), ui.Dim("← also"), cf.Window, live[cf.Window].Badge())
	}
	if len(soft) > 0 {
		fmt.Fprintln(os.Stderr, ui.Dim(fmt.Sprintf("   (+%d advisory overlap(s) — shared/append-only/disjoint)", len(soft))))
	}
	fmt.Fprintln(os.Stderr, ui.Yellow("   Coordinate with that window before pushing (run `wt check` for details)."))
	fmt.Fprintln(os.Stderr, ui.Dim("   Bypass (you've coordinated): WT_SKIP_COLLISION=1 git push"))
	return true
}

// distinctPaths returns the unique file paths across the conflicts.
func distinctPaths(cs []collide.Conflict) []string {
	seen := map[string]bool{}
	var out []string
	for _, cf := range cs {
		if !seen[cf.Path] {
			seen[cf.Path] = true
			out = append(out, cf.Path)
		}
	}
	return out
}

// dedupConflicts removes duplicate (path, window) rows so a file touched by the
// same window isn't printed twice (#92 — the report repeated one path N times).
func dedupConflicts(cs []collide.Conflict) []collide.Conflict {
	seen := map[string]bool{}
	var out []collide.Conflict
	for _, cf := range cs {
		key := cf.Path + "\x00" + cf.Window
		if !seen[key] {
			seen[key] = true
			out = append(out, cf)
		}
	}
	return out
}

// rangeFn fetches a worktree's changed line-ranges for a file vs base — injected
// so the pure grading decision is unit-testable without a live repo.
type rangeFn func(worktree, base, path string) []gitx.LineRange

// gradeConflicts splits active conflicts into hard (real overlap) vs soft
// (advisory) using the SAME rule as `wt check` + the pre-push guard: shared-docs
// and append-only paths are advisory, and a code file is HARD only when the two
// windows' changed line-ranges OVERLAP — disjoint hunks in the same file stay
// advisory. Single-source so pre-push and pre-commit can never disagree (#92, #97).
func gradeConflicts(c *config.Config, active []collide.Conflict, root string, ws []collide.Window, ranges rangeFn) (hard, soft []collide.Conflict) {
	wtByLabel := make(map[string]string, len(ws))
	for _, w := range ws {
		wtByLabel[w.Label()] = w.Worktree
	}
	for _, cf := range active {
		rangesPath := cf.MatchedFile
		if rangesPath == "" {
			rangesPath = cf.Path
		}
		if collide.IsSharedDoc(cf.Path, c.SharedDocs) {
			// #98: a STRUCTURED shared doc (configured section delimiter) grades
			// by SECTION, exactly as `wt check` does — both windows editing the
			// SAME section is HARD; disjoint sections stay advisory. Without this
			// the hooks stopped at the blanket shared-doc advisory, so the one
			// case a structured doc is configured to catch — two windows in the
			// same lane of a hand-merged doc — blocked in `wt check` and sailed
			// straight through the pre-push guard.
			if delim, isStructured := c.StructuredDocs[filepath.Base(cf.Path)]; isStructured {
				if shared, graded := collide.SharedSectionsAcross(
					c.Base, []string{root, wtByLabel[cf.Window]}, rangesPath, delim, collide.RangeFn(ranges),
				); graded && len(shared) > 0 {
					hard = append(hard, cf)
					continue
				}
			}
			soft = append(soft, cf)
			continue
		}
		if collide.IsAppendOnly(cf.Path, c.AppendOnlyPaths) {
			soft = append(soft, cf)
			continue
		}
		cur := ranges(root, c.Base, rangesPath)
		other := ranges(wtByLabel[cf.Window], c.Base, rangesPath)
		if collide.ConflictSeverity(cur, other, false) == collide.SevHigh {
			hard = append(hard, cf)
		} else {
			soft = append(soft, cf)
		}
	}
	return hard, soft
}

// HookPreCommit implements `wt _hook pre-commit`. NEVER blocks (always exit 0).
// It surfaces FILE-LEVEL collisions: staged files that another window is also
// touching. Falls back to an informational "other claims exist" note.
func HookPreCommit(c *config.Config) int {
	if os.Getenv("HOOK_DISABLE_MULTIWINDOW_CHECK") == "1" {
		return 0
	}

	// StagedFiles honors git's hook-provided temporary index (GIT_INDEX_FILE) so
	// a partial commit (`git commit -a`/-p/--only/pathspec) reports the correct
	// staged set — a plain env-scoped read would strip it and miss collisions (#92).
	stagedFiles, _ := gitx.StagedFiles()

	ws, err := collide.Scan(c)
	if err != nil {
		return 0
	}
	root, _ := gitx.RepoRoot()

	if len(stagedFiles) > 0 {
		if conflicts := collide.CheckPaths(ws, root, stagedFiles); len(conflicts) > 0 {
			// Suppress collisions against stale branches (merged / no open PR) —
			// same liveness rule as `wt check`, so the hook doesn't cry wolf on
			// every commit against long-dead branches that touched the same file.
			live := collide.ClassifyWindows(ws, c.Base, collide.ConflictWindowSet(conflicts), c.MaxAge)
			active, stale := collide.PartitionConflicts(conflicts, live)
			// Grade IDENTICALLY to the pre-push guard + `wt check` (#97): shared-docs
			// and append-only paths are advisory, and a code file is "hard" only when
			// the two windows' changed line-ranges OVERLAP — disjoint hunks stay
			// advisory. Unlike pre-push this NEVER blocks; it's an awareness notice.
			hard, soft := gradeConflicts(c, active, root, ws, gitx.ChangedRanges)
			if len(hard) > 0 {
				files := distinctPaths(hard)
				ui.Collision("%d staged file(s) have an OVERLAPPING edit by an active window:", len(files))
				for _, cf := range dedupConflicts(hard) {
					fmt.Fprintf(os.Stderr, "   %s  %s %s %s\n", ui.Bold(cf.Path), ui.Dim("← also"), cf.Window, live[cf.Window].Badge())
				}
				if len(soft) > 0 {
					fmt.Fprintln(os.Stderr, ui.Dim(fmt.Sprintf("   (+%d advisory overlap(s) — shared / append-only / disjoint)", len(soft))))
				}
				if len(stale) > 0 {
					fmt.Fprintln(os.Stderr, ui.Dim(fmt.Sprintf("   (+%d on stale branch(es) — merged / no open PR — ignored)", len(stale))))
				}
				fmt.Fprintln(os.Stderr, ui.Yellow("   Coordinate before committing to avoid a merge collision."))
				fmt.Fprintln(os.Stderr, ui.Dim("   (informational — not blocking · HOOK_DISABLE_MULTIWINDOW_CHECK=1 to silence)"))
				return 0
			}
			if len(soft) > 0 {
				files := distinctPaths(soft)
				fmt.Fprintln(os.Stderr, ui.Dim(fmt.Sprintf("📝 %d file(s) also edited in another window (advisory — shared / append-only / disjoint hunks): %s", len(files), strings.Join(files, ", "))))
			}
		}
	}

	// No file overlap — cheap awareness of other active claims.
	others := activework.OtherClaims(activework.Read(c.ActiveWork), issueFromBranch(c))
	if len(others) > 0 {
		fmt.Fprintln(os.Stderr, ui.Dim(fmt.Sprintf("📋 %d other active window claim(s): %s", len(others), strings.Join(others, " "))))
	}
	return 0
}

func issueFromBranch(c *config.Config) string {
	br, err := gitx.CurrentBranch()
	if err != nil {
		return ""
	}
	re := regexp.MustCompile(`^` + regexp.QuoteMeta(c.Prefix) + `(\d+)-`)
	if m := re.FindStringSubmatch(br); m != nil {
		return m[1]
	}
	return ""
}

func nonEmptyLines(s string) []string {
	var out []string
	for _, ln := range strings.Split(s, "\n") {
		if ln = strings.TrimSpace(ln); ln != "" {
			out = append(out, ln)
		}
	}
	return out
}
