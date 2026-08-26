// Package gitx wraps the git CLI via os/exec — the same shell-out approach the
// original bash scripts use, keeping behavior identical and dependencies zero.
package gitx

import (
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"time"
)

// gitScopeEnvVars are the environment variables git sets for a running hook
// that PIN a git subprocess to a specific repo/worktree/index — overriding the
// cwd/`-C dir` discovery every per-worktree command in wt relies on. Under a
// pre-push/pre-commit hook these are set to the INVOKING worktree, so a
// collision scan's `git -C otherworktree status` would read the invoking
// worktree's index instead — making every window look like it holds the
// pusher's changes (self-reference + phantom dirty-state, #92). We strip them so
// every git command does clean discovery from its own dir.
var gitScopeEnvVars = []string{
	"GIT_DIR", "GIT_WORK_TREE", "GIT_INDEX_FILE", "GIT_PREFIX",
	"GIT_COMMON_DIR", "GIT_OBJECT_DIRECTORY", "GIT_NAMESPACE",
}

// scopedEnv returns the current environment with the repo/worktree-pinning git
// vars removed, so a git subprocess discovers its repo from cwd / -C dir.
func scopedEnv() []string {
	env := os.Environ()
	out := env[:0:0]
	for _, kv := range env {
		drop := false
		for _, v := range gitScopeEnvVars {
			if strings.HasPrefix(kv, v+"=") {
				drop = true
				break
			}
		}
		if !drop {
			out = append(out, kv)
		}
	}
	return out
}

// run executes git with args in dir ("" = current dir) and returns trimmed
// stdout. stderr is discarded; callers branch on err.
func run(dir string, args ...string) (string, error) {
	cmd := exec.Command("git", args...)
	if dir != "" {
		cmd.Dir = dir
	}
	cmd.Env = scopedEnv()
	out, err := cmd.Output()
	return strings.TrimSpace(string(out)), err
}

// StagedFiles returns the paths staged for the IN-PROGRESS commit. It PRESERVES
// git's ambient environment (unlike run(), which strips GIT_INDEX_FILE) because
// git points GIT_INDEX_FILE at a TEMPORARY index for a partial commit
// (`git commit -a` / `-p` / `--only` / `-- <paths>`) — the on-disk index does
// NOT yet reflect what's being committed. Stripping it makes `git diff --cached`
// read the stale index and mis-report the staged set, so the pre-commit
// collision notice would miss (or over-report) collisions (#92 review). This is
// the invoking worktree's OWN read, so the ambient env is correct here — only
// the cross-worktree `-C dir` scans need scopedEnv.
func StagedFiles() ([]string, error) {
	out, err := exec.Command("git", "diff", "--cached", "--name-only").Output()
	if err != nil {
		return nil, err
	}
	var files []string
	for _, ln := range strings.Split(strings.TrimSpace(string(out)), "\n") {
		if s := strings.TrimSpace(ln); s != "" {
			files = append(files, s)
		}
	}
	return files, nil
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
	cmd.Env = scopedEnv()
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
	// If the branch already exists (its previous worktree was removed out-of-band
	// but the branch — and its commits — survived), re-attach it to a fresh
	// worktree instead of `-b` (which errors "branch already exists") (#62).
	if LocalBranchExists(branch) {
		_, err := Run("worktree", "add", path, branch)
		return err
	}
	_, err := Run("worktree", "add", path, "-b", branch, base)
	return err
}

// LocalBranchExists reports whether refs/heads/<branch> exists.
func LocalBranchExists(branch string) bool {
	_, err := Run("rev-parse", "--verify", "--quiet", "refs/heads/"+branch)
	return err == nil
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

// WorktreeRef is a worktree's path + its checked-out branch ("" if detached).
type WorktreeRef struct {
	Path   string
	Branch string
}

// WorktreeList returns every worktree of this repo with its checked-out branch.
// Detached worktrees have Branch == "". (Companion to WorktreePaths /
// WorktreeBranchesUnder — this one pairs path↔branch, which `wt doctor`'s
// upstream check needs per-worktree, #76.)
func WorktreeList() ([]WorktreeRef, error) {
	out, err := Run("worktree", "list", "--porcelain")
	if err != nil {
		return nil, err
	}
	var refs []WorktreeRef
	var cur WorktreeRef
	flush := func() {
		if cur.Path != "" {
			refs = append(refs, cur)
		}
		cur = WorktreeRef{}
	}
	for _, ln := range strings.Split(out, "\n") {
		switch {
		case strings.HasPrefix(ln, "worktree "):
			flush()
			cur.Path = strings.TrimSpace(strings.TrimPrefix(ln, "worktree "))
		case strings.HasPrefix(ln, "branch "):
			cur.Branch = strings.TrimPrefix(strings.TrimSpace(strings.TrimPrefix(ln, "branch ")), "refs/heads/")
		}
	}
	flush()
	return refs, nil
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

// CommitSubjects returns the one-line subjects of commits on branchRef that are
// not on base (git log --format=%s base..branchRef), newest first. Used to tell
// whether a branch carries only WIP placeholder commits (#42).
func CommitSubjects(base, branchRef string) ([]string, error) {
	out, err := Run("log", "--format=%s", base+".."+branchRef)
	if err != nil {
		return nil, err
	}
	var subjects []string
	for _, ln := range strings.Split(out, "\n") {
		if ln = strings.TrimSpace(ln); ln != "" {
			subjects = append(subjects, ln)
		}
	}
	return subjects, nil
}

// WorktreePrune runs `git worktree prune`, dropping administrative metadata for
// worktrees whose directories were removed out-of-band (#42). Best-effort.
func WorktreePrune() error {
	_, err := Run("worktree", "prune")
	return err
}

// HasUpstream reports whether the checkout at dir has a configured upstream
// (@{u}) — i.e. the branch has been pushed. A branch with no upstream was never
// shared, so it can't be "merged" and must not be reaped by `wt clean` (#61).
func HasUpstream(dir string) bool {
	_, err := RunDir(dir, "rev-parse", "--abbrev-ref", "--symbolic-full-name", "@{u}")
	return err == nil
}

// IsInsideWorktree reports whether dir is a live git worktree (its .git resolves
// and rev-parse succeeds). Distinguishes a real worktree from a leftover empty
// directory whose worktree was removed out-of-band (#62).
func IsInsideWorktree(dir string) bool {
	out, err := RunDir(dir, "rev-parse", "--is-inside-work-tree")
	return err == nil && strings.TrimSpace(out) == "true"
}

// IsTracked reports whether path is known to git — tracked in the index (so a
// path deleted in the working tree but still in git returns true). Used by
// `wt check` to distinguish a deleted/renamed path from a typo (#93).
func IsTracked(path string) bool {
	_, err := Run("ls-files", "--error-unmatch", "--", path)
	return err == nil
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

// AllZeroSHA reports whether ref is git's all-zero object id — the sentinel a
// pre-push line carries for the remote side of a brand-new branch (nothing on
// the remote yet) or the local side of a branch deletion.
func AllZeroSHA(ref string) bool {
	ref = strings.TrimSpace(ref)
	return ref != "" && strings.Trim(ref, "0") == ""
}

// IsAncestor reports whether maybeAncestor is an ancestor of descendant
// (`git merge-base --is-ancestor`). False on any error — including an unknown
// ref, which is the right default for the caller (#106): if we cannot prove the
// remote head is still in this branch's history, treat the push as non-ff and
// measure from the base rather than walking through unrelated commits.
func IsAncestor(maybeAncestor, descendant string) bool {
	if strings.TrimSpace(maybeAncestor) == "" || strings.TrimSpace(descendant) == "" {
		return false
	}
	_, err := Run("merge-base", "--is-ancestor", maybeAncestor, descendant)
	return err == nil
}

// RangeChangedPaths returns the repo-relative paths changed between two commits
// (`git diff --name-only from..to`). Used by the pre-push collision check (#74)
// to scope the check to the OUTGOING commits, not the whole worktree.
func RangeChangedPaths(from, to string) ([]string, error) {
	out, err := Run("diff", "--name-only", from+".."+to)
	if err != nil {
		return nil, err
	}
	var paths []string
	for _, ln := range strings.Split(out, "\n") {
		if s := strings.TrimSpace(ln); s != "" {
			paths = append(paths, s)
		}
	}
	return paths, nil
}

// IsUntracked reports whether path exists in worktree but is NOT tracked by git
// ("?? path" in porcelain) — a new file never added/staged/committed there. Such
// a file has no diff and cannot be pushed, so it can't cause a merge collision
// until it's actually committed (#113). Scoped to the one path; "" worktree or
// any git error → false (fail-safe: don't claim untracked when we can't tell).
func IsUntracked(worktree, path string) bool {
	if worktree == "" {
		return false
	}
	out, err := runRaw(worktree, "status", "--porcelain", "--untracked-files=all", "--", path)
	if err != nil {
		return false
	}
	for _, ln := range strings.Split(out, "\n") {
		if strings.HasPrefix(ln, "?? ") {
			return true
		}
	}
	return false
}

// WorktreeBlob returns the git blob hash of the WORKING-TREE file at path inside
// worktree (`git hash-object`), and whether it could be hashed. Lets a caller
// compare a window's on-disk content against a ref without a diff (#109).
func WorktreeBlob(worktree, path string) (string, bool) {
	out, err := RunDir(worktree, "hash-object", "--", path)
	if err != nil || out == "" {
		return "", false
	}
	return out, true
}

// RefBlob returns the git blob hash of path at ref inside worktree (`ref:path`),
// and whether it resolved. ref "" reads the STAGED blob (`:path`). Absent ref or
// path → ("", false) via --verify --quiet, so the caller fails safe (#109).
func RefBlob(worktree, ref, path string) (string, bool) {
	out, err := RunDir(worktree, "rev-parse", "--verify", "--quiet", ref+":"+path)
	if err != nil || out == "" {
		return "", false
	}
	return out, true
}

// ResolveRemoteBase returns the best last-known base ref for offline base-drift
// checks (#78): the remote-tracking `origin/<base>` if it exists (what the PR
// will actually merge into), else the local `<base>`, else "" when neither
// resolves. No network — reads only refs already in the object store.
func ResolveRemoteBase(base string) string {
	if base == "" {
		return ""
	}
	for _, ref := range []string{"refs/remotes/origin/" + base, "refs/heads/" + base} {
		if _, err := Run("rev-parse", "--verify", "--quiet", ref); err == nil {
			return ref
		}
	}
	return ""
}

// RemoteURL returns the configured URL for a remote ("" when the remote does
// not exist). Used to work out which forge host this repo actually talks to,
// so gh-auth checks can be scoped to it rather than assuming github.com (#100).
func RemoteURL(remote string) string {
	if remote == "" {
		remote = "origin"
	}
	out, err := Run("remote", "get-url", remote)
	if err != nil {
		return ""
	}
	return strings.TrimSpace(out)
}

// BehindCount returns how many commits base is ahead of head (`git rev-list
// --count head..base`) — the "behind main by N" signal (#78). -1 when it can't
// be computed (bad refs), so the caller can suppress the line rather than lie.
func BehindCount(head, base string) int {
	out, err := Run("rev-list", "--count", head+".."+base)
	if err != nil {
		return -1
	}
	n, err := strconv.Atoi(strings.TrimSpace(out))
	if err != nil {
		return -1
	}
	return n
}

// MergeTreeConflicts performs an in-memory 3-way merge of head into base via
// `git merge-tree --write-tree --name-only` — NO network, NO worktree mutation,
// NO index touch (git >= 2.38). It returns the conflicting repo-relative paths
// (empty when clean), whether the merge conflicted, and an error ONLY when
// merge-tree itself could not run (bad refs / ancient git) so the caller fails
// open rather than blocking a push on a tooling gap. Backs the #78 "this PR
// will get NO CI until rebased" warning.
func MergeTreeConflicts(base, head string) (paths []string, conflicted bool, err error) {
	cmd := exec.Command("git", "merge-tree", "--write-tree", "--name-only", base, head)
	cmd.Env = scopedEnv()
	out, runErr := cmd.Output()
	if runErr == nil {
		return nil, false, nil // exit 0 → clean merge
	}
	ee, ok := runErr.(*exec.ExitError)
	if !ok {
		return nil, false, runErr // couldn't exec git at all
	}
	if ee.ExitCode() != 1 {
		return nil, false, fmt.Errorf("git merge-tree exit %d", ee.ExitCode())
	}
	// Exit 1 = conflicts.
	return parseMergeTreeConflictPaths(string(out)), true, nil
}

// parseMergeTreeConflictPaths pulls the conflicted paths out of `git merge-tree
// --write-tree --name-only` stdout. The format is:
//
//	<toplevel-tree-oid>
//	<conflicted path>...        (one per line)
//	                            (blank line)
//	<informational messages>...
//
// so we skip line 0 (the tree OID) and take lines up to the first blank line
// (the separator before informational text). Pure — unit-tested.
func parseMergeTreeConflictPaths(out string) []string {
	lines := strings.Split(strings.TrimSpace(out), "\n")
	if len(lines) <= 1 {
		return nil
	}
	var paths []string
	for _, ln := range lines[1:] {
		if strings.TrimSpace(ln) == "" {
			break
		}
		paths = append(paths, strings.TrimSpace(ln))
	}
	return paths
}

// TouchedFiles returns the union of (a) uncommitted changes in the worktree at
// dir and (b) files this branch changed vs base (merge-base diff). This is the
// "what is this window working on right now" set used for collision detection.
func TouchedFiles(dir, base string) []string {
	set := map[string]struct{}{}

	// (a) uncommitted (staged + unstaged + untracked) via porcelain. Use the
	// raw runner: the 2-char status code + space prefix is positional, so the
	// path begins at byte 3 of every line — trimming the blob would corrupt it.
	//
	// --untracked-files=all (#27): git's DEFAULT untracked mode collapses a
	// fully-untracked directory to a single "dir/" entry, which never
	// string-matches another window's specific "dir/foo.go" in Overlaps — so a
	// collision under a freshly-created dir goes silently undetected. -uall
	// lists each new file at its full path. Gitignored files stay excluded, so
	// the cost is bounded to genuinely-new files.
	if out, err := runRaw(dir, "status", "--porcelain", "--untracked-files=all"); err == nil {
		for _, ln := range strings.Split(out, "\n") {
			if len(ln) < 4 {
				continue
			}
			path := strings.TrimSpace(ln[3:])
			// Rename/copy "old -> new" (#28): record BOTH sides. Keeping only
			// the new path misses a rename/modify clash — window A renames
			// x.go, window B edits x.go — a real 3-way conflict that would
			// otherwise show no overlap. Recording old can only add a flag,
			// never hide one (correct for a safety tool). Each side may be
			// individually quoted when it contains special chars.
			if i := strings.Index(path, " -> "); i >= 0 {
				oldp := strings.Trim(strings.TrimSpace(path[:i]), "\"")
				newp := strings.Trim(strings.TrimSpace(path[i+len(" -> "):]), "\"")
				if oldp != "" {
					set[oldp] = struct{}{}
				}
				if newp != "" {
					set[newp] = struct{}{}
				}
				continue
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

// LineRange is a 1-based inclusive span of changed lines. ChangedRanges emits
// these in BASE coordinates (the OLD side of a base-vs-worktree diff, #29) so
// two windows that fork from the same base can be compared in one frame. A pure
// insertion/deletion (count 0) is recorded spanning the gap boundary (the
// surviving line before + after) so it still overlaps an edit of the same region
// in another window — biasing toward flagging (a missed conflict is worse than
// an extra heads-up for a safety tool).
type LineRange struct{ Start, End int }

// Overlaps reports whether two line ranges intersect.
func (r LineRange) Overlaps(o LineRange) bool {
	return r.Start <= o.End && o.Start <= r.End
}

var hunkRe = regexp.MustCompile(`^@@ -\d+(?:,\d+)? \+(\d+)(?:,(\d+))? @@`)

// oldHunkRe captures the OLD-side (base) start+count of a -U0 hunk header (#29).
var oldHunkRe = regexp.MustCompile(`^@@ -(\d+)(?:,(\d+))? \+\d+(?:,\d+)? @@`)

// parseHunkRanges extracts NEW-side line ranges from unified-diff (-U0) output.
func parseHunkRanges(diff string) []LineRange {
	return parseHunkRangesWith(diff, hunkRe)
}

// parseHunkRangesOld extracts BASE-side (OLD) line ranges from -U0 output (#29).
// Grading in base coordinates is what lets two windows' ranges be compared in a
// single shared frame: once either has an insert/delete before a shared region,
// their NEW-side numbers diverge and a same-base-line conflict grades disjoint.
func parseHunkRangesOld(diff string) []LineRange {
	return parseHunkRangesWith(diff, oldHunkRe)
}

// parseHunkRangesWith is the shared parser; re captures (start, optional count)
// on whichever side. A zero count is a pure insertion/deletion on that side —
// git reports the surviving line before the gap, so span [start, start+1] to
// cover both neighbors (biases toward flagging, correct for a safety tool).
func parseHunkRangesWith(diff string, re *regexp.Regexp) []LineRange {
	var out []LineRange
	for _, ln := range strings.Split(diff, "\n") {
		m := re.FindStringSubmatch(ln)
		if m == nil {
			continue
		}
		start, _ := strconv.Atoi(m[1])
		count := 1
		if m[2] != "" {
			count, _ = strconv.Atoi(m[2])
		}
		if count == 0 {
			out = append(out, LineRange{start, start + 1})
		} else {
			out = append(out, LineRange{start, start + count - 1})
		}
	}
	return out
}

// ChangedRanges returns the line ranges file was changed on in the worktree at
// dir, expressed in BASE coordinates (#29). It runs ONE `git diff -U0 <base> --
// <file>` — base commit vs the working tree, which folds committed + staged +
// unstaged changes into a single diff — and parses the OLD (base) side of each
// hunk. Because every window diffs against the SAME base ref, their ranges live
// in one shared frame, so OverlappingSpans compares like-for-like even after one
// window has inserts/deletes before a shared edit region (the pre-#29 latent
// false-negative: NEW-side numbers from three separate diffs in three frames).
//
// Fallback: when neither origin/<base> nor <base> resolves (a base-less repo,
// where cross-window base comparison is meaningless anyway), degrade to the
// NEW-side uncommitted hunks so a single window still self-reports.
func ChangedRanges(dir, base, file string) []LineRange {
	for _, ref := range []string{"origin/" + base, base} {
		if _, err := RunDir(dir, "rev-parse", "--verify", "--quiet", ref+"^{commit}"); err != nil {
			continue
		}
		if out, err := runRaw(dir, "diff", "-U0", ref, "--", file); err == nil {
			return parseHunkRangesOld(out)
		}
		return nil
	}
	// No base ref — degraded NEW-side self-report (index frame).
	var ranges []LineRange
	if out, err := runRaw(dir, "diff", "-U0", "--", file); err == nil {
		ranges = append(ranges, parseHunkRanges(out)...)
	}
	if out, err := runRaw(dir, "diff", "-U0", "--cached", "--", file); err == nil {
		ranges = append(ranges, parseHunkRanges(out)...)
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
