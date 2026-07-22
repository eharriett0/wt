// Package gitx wraps the git CLI via os/exec — the same shell-out approach the
// original bash scripts use, keeping behavior identical and dependencies zero.
package gitx

import (
	"os/exec"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"time"
)

// run executes git with args in dir ("" = current dir) and returns trimmed
// stdout. stderr is discarded; callers branch on err.
func run(dir string, args ...string) (string, error) {
	cmd := exec.Command("git", args...)
	if dir != "" {
		cmd.Dir = dir
	}
	out, err := cmd.Output()
	return strings.TrimSpace(string(out)), err
}

// Run executes git in the current directory.
func Run(args ...string) (string, error) { return run("", args...) }

// RunDir executes git in dir.
func RunDir(dir string, args ...string) (string, error) { return run(dir, args...) }

// runRaw is like run but does NOT trim — required for `status --porcelain`,
// whose leading status-column space is load-bearing (trimming the blob shifts
// the first line's path by one byte).
func runRaw(dir string, args ...string) (string, error) {
	cmd := exec.Command("git", args...)
	if dir != "" {
		cmd.Dir = dir
	}
	out, err := cmd.Output()
	return string(out), err
}

// Present reports whether the git binary is on PATH.
func Present() bool {
	_, err := exec.LookPath("git")
	return err == nil
}

// RepoRoot returns the top-level dir of the repo containing cwd.
func RepoRoot() (string, error) { return Run("rev-parse", "--show-toplevel") }

// CommonDir returns the absolute $GIT_COMMON_DIR (shared across all worktrees).
func CommonDir() (string, error) {
	return Run("rev-parse", "--path-format=absolute", "--git-common-dir")
}

// CommonDirIn returns the absolute $GIT_COMMON_DIR for the repo at dir (or an
// error if dir isn't a git repo). Used to resolve a sibling repo's shared state.
func CommonDirIn(dir string) (string, error) {
	return RunDir(dir, "rev-parse", "--path-format=absolute", "--git-common-dir")
}

// CurrentBranch returns the abbreviated current branch (or "HEAD" if detached).
func CurrentBranch() (string, error) { return Run("rev-parse", "--abbrev-ref", "HEAD") }

// CurrentBranchIn returns the current branch of the worktree at dir.
func CurrentBranchIn(dir string) (string, error) {
	return RunDir(dir, "rev-parse", "--abbrev-ref", "HEAD")
}

// DefaultBranchFromRef extracts the branch name from an origin/HEAD symbolic
// ref like "origin/main" → "main". Returns "" when the ref is empty/unexpected.
func DefaultBranchFromRef(ref string) string {
	ref = strings.TrimSpace(ref)
	if ref == "" {
		return ""
	}
	if i := strings.Index(ref, "/"); i >= 0 {
		return ref[i+1:]
	}
	return ref
}

// DefaultBranch derives the repo's default branch: origin/HEAD symbolic ref,
// then a main→master fallback by checking which ref exists.
func DefaultBranch() string {
	if ref, err := Run("rev-parse", "--abbrev-ref", "origin/HEAD"); err == nil {
		if b := DefaultBranchFromRef(ref); b != "" {
			return b
		}
	}
	for _, candidate := range []string{"main", "master"} {
		if _, err := Run("rev-parse", "--verify", "--quiet", "refs/remotes/origin/"+candidate); err == nil {
			return candidate
		}
		if _, err := Run("rev-parse", "--verify", "--quiet", "refs/heads/"+candidate); err == nil {
			return candidate
		}
	}
	return "main"
}

// Fetch updates remote/branch quietly (best-effort; error returned for caller).
func Fetch(remote, branch string) error {
	_, err := Run("fetch", remote, branch)
	return err
}

// WorktreeAdd creates a new worktree at path on a new branch from base.
func WorktreeAdd(path, branch, base string) error {
	_, err := Run("worktree", "add", path, "-b", branch, base)
	return err
}

// WorktreePaths lists every worktree path (primary first), via porcelain.
func WorktreePaths() ([]string, error) {
	out, err := Run("worktree", "list", "--porcelain")
	if err != nil {
		return nil, err
	}
	var paths []string
	for _, ln := range strings.Split(out, "\n") {
		if strings.HasPrefix(ln, "worktree ") {
			paths = append(paths, strings.TrimSpace(strings.TrimPrefix(ln, "worktree ")))
		}
	}
	return paths, nil
}

// WorktreeBranchesUnder returns the branch names of every worktree whose path
// is under root (the wt-managed worktree root). Detached worktrees are skipped.
// These are the "wt-managed worktree branches for this repo" used by merge-pr's
// foreign-branch guard (wt#15) — a PR head branch that is NOT one of these has
// no local wt lane and may be another window's branch merged by mistake.
func WorktreeBranchesUnder(root string) ([]string, error) {
	out, err := Run("worktree", "list", "--porcelain")
	if err != nil {
		return nil, err
	}
	var branches []string
	var curPath string
	for _, ln := range strings.Split(out, "\n") {
		switch {
		case strings.HasPrefix(ln, "worktree "):
			curPath = strings.TrimSpace(strings.TrimPrefix(ln, "worktree "))
		case strings.HasPrefix(ln, "branch "):
			br := strings.TrimPrefix(strings.TrimSpace(strings.TrimPrefix(ln, "branch ")), "refs/heads/")
			if br != "" && pathUnder(curPath, root) {
				branches = append(branches, br)
			}
		}
	}
	return branches, nil
}

// pathUnder reports whether path is root itself or nested under it.
func pathUnder(path, root string) bool {
	if path == "" || root == "" {
		return false
	}
	sep := string(filepath.Separator)
	path = strings.TrimSuffix(path, sep)
	root = strings.TrimSuffix(root, sep)
	return path == root || strings.HasPrefix(path, root+sep)
}

// WorktreeRemove removes the worktree at path. force discards untracked/dirty
// files (git refuses otherwise).
func WorktreeRemove(path string, force bool) error {
	args := []string{"worktree", "remove"}
	if force {
		args = append(args, "--force")
	}
	args = append(args, path)
	_, err := Run(args...)
	return err
}

// BranchDelete force-deletes local branch (git branch -D). Safe to call after a
// squash-merge, where the branch is not fast-forward-merged into base.
func BranchDelete(branch string) error {
	_, err := Run("branch", "-D", branch)
	return err
}

// Cherry returns `git cherry <base> <branchRef>` output (+/- lines).
func Cherry(base, branchRef string) (string, error) {
	return Run("cherry", base, branchRef)
}

// CountUnshipped counts cherry "+" lines (commits with no patch-equivalent on
// base). Zero means the branch is fully shipped (squash-merge safe).
func CountUnshipped(base, branchRef string) (int, error) {
	out, err := Cherry(base, branchRef)
	if err != nil {
		return -1, err
	}
	n := 0
	for _, ln := range strings.Split(out, "\n") {
		if strings.HasPrefix(ln, "+") {
			n++
		}
	}
	return n, nil
}

// TouchedFiles returns the union of (a) uncommitted changes in the worktree at
// dir and (b) files this branch changed vs base (merge-base diff). This is the
// "what is this window working on right now" set used for collision detection.
func TouchedFiles(dir, base string) []string {
	set := map[string]struct{}{}

	// (a) uncommitted (staged + unstaged + untracked) via porcelain. Use the
	// raw runner: the 2-char status code + space prefix is positional, so the
	// path begins at byte 3 of every line — trimming the blob would corrupt it.
	if out, err := runRaw(dir, "status", "--porcelain"); err == nil {
		for _, ln := range strings.Split(out, "\n") {
			if len(ln) < 4 {
				continue
			}
			path := strings.TrimSpace(ln[3:])
			// Rename/copy: "old -> new" — take the new path.
			if i := strings.Index(path, " -> "); i >= 0 {
				path = path[i+4:]
			}
			path = strings.Trim(path, "\"")
			if path != "" {
				set[path] = struct{}{}
			}
		}
	}

	// (b) committed-on-branch vs base (three-dot = since merge-base).
	for _, ref := range []string{"origin/" + base, base} {
		if out, err := RunDir(dir, "diff", "--name-only", ref+"...HEAD"); err == nil {
			for _, ln := range strings.Split(out, "\n") {
				if p := strings.TrimSpace(ln); p != "" {
					set[p] = struct{}{}
				}
			}
			break // first ref that resolves wins
		}
	}

	files := make([]string, 0, len(set))
	for f := range set {
		files = append(files, f)
	}
	return files
}

// LineRange is a 1-based inclusive span of lines changed on the NEW side of a
// diff. A pure deletion (new-count 0) is recorded spanning the gap boundary
// (the surviving line before + after the removed content) so a delete in one
// window still overlaps an edit of the same region in another — biasing toward
// flagging (a missed conflict is worse than an extra heads-up for a safety tool).
type LineRange struct{ Start, End int }

// Overlaps reports whether two line ranges intersect.
func (r LineRange) Overlaps(o LineRange) bool {
	return r.Start <= o.End && o.Start <= r.End
}

var hunkRe = regexp.MustCompile(`^@@ -\d+(?:,\d+)? \+(\d+)(?:,(\d+))? @@`)

// parseHunkRanges extracts NEW-side line ranges from unified-diff (-U0) output.
func parseHunkRanges(diff string) []LineRange {
	var out []LineRange
	for _, ln := range strings.Split(diff, "\n") {
		m := hunkRe.FindStringSubmatch(ln)
		if m == nil {
			continue
		}
		start, _ := strconv.Atoi(m[1])
		count := 1
		if m[2] != "" {
			count, _ = strconv.Atoi(m[2])
		}
		if count == 0 {
			// Pure deletion: git reports the new-side line BEFORE the gap. Span
			// [start, start+1] to cover both surviving neighbors so an edit of
			// the removed region in another window overlaps.
			out = append(out, LineRange{start, start + 1})
		} else {
			out = append(out, LineRange{start, start + count - 1})
		}
	}
	return out
}

// ChangedRanges returns the NEW-side line ranges file was changed on in the
// worktree at dir — the union of uncommitted (staged + unstaged) hunks and
// committed-on-branch hunks vs base. -U0 so ranges are tight (no context
// bleed). Used for hunk-level overlap between windows.
func ChangedRanges(dir, base, file string) []LineRange {
	var ranges []LineRange
	add := func(args ...string) {
		if out, err := runRaw(dir, args...); err == nil {
			ranges = append(ranges, parseHunkRanges(out)...)
		}
	}
	add("diff", "-U0", "--", file)             // unstaged
	add("diff", "-U0", "--cached", "--", file) // staged
	for _, ref := range []string{"origin/" + base, base} {
		if _, err := RunDir(dir, "rev-parse", "--verify", "--quiet", ref+"^{commit}"); err == nil {
			add("diff", "-U0", ref+"...HEAD", "--", file)
			break
		}
	}
	return ranges
}

// LastCommitUnix returns the committer timestamp (unix seconds) of HEAD in dir.
func LastCommitUnix(dir string) (int64, error) {
	out, err := RunDir(dir, "log", "-1", "--format=%ct")
	if err != nil {
		return 0, err
	}
	return strconv.ParseInt(strings.TrimSpace(out), 10, 64)
}

// LastCommitAge returns how long ago HEAD in dir was committed, relative to now.
func LastCommitAge(dir string, now time.Time) (time.Duration, error) {
	ts, err := LastCommitUnix(dir)
	if err != nil {
		return 0, err
	}
	return now.Sub(time.Unix(ts, 0)), nil
}

// IsClean reports whether the worktree at dir has NO uncommitted changes
// (staged, unstaged, or untracked). A dirty worktree means the window is
// actively editing, which keeps it out of the "stale" collision bucket even
// when its branch has no open PR. On error (dir gone, not a worktree) it
// returns false — i.e. treat an unknowable worktree as potentially active.
func IsClean(dir string) bool {
	out, err := runRaw(dir, "status", "--porcelain")
	if err != nil {
		return false
	}
	return strings.TrimSpace(out) == ""
}

// CommitEmpty makes an empty commit in dir with the given message.
func CommitEmpty(dir, msg string) error {
	_, err := RunDir(dir, "commit", "--allow-empty", "-m", msg)
	return err
}

// PushSetUpstream pushes branch to origin and sets upstream, from dir.
func PushSetUpstream(dir, branch string) error {
	_, err := RunDir(dir, "push", "-u", "origin", branch)
	return err
}

// Abs resolves a possibly-relative path against the repo root.
func Abs(p string) string {
	if filepath.IsAbs(p) {
		return p
	}
	if root, err := RepoRoot(); err == nil {
		return filepath.Join(root, p)
	}
	return p
}
