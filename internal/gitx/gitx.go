// Package gitx wraps the git CLI via os/exec — the same shell-out approach the
// original bash scripts use, keeping behavior identical and dependencies zero.
package gitx

import (
	"fmt"
	"math"
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

// gitOutput runs git with args in dir ("" = current dir), the repo-pinning env
// stripped (scopedEnv), and returns its stdout. It is a var for exactly one
// reason: the fail-safe tests inject a git failure (a lost object, a crash) that
// a scratch repo can't produce on demand, to pin that a failed measurement never
// reads as "no edits" or "frame-safe" (#184). Production never reassigns it.
var gitOutput = func(dir string, args ...string) ([]byte, error) {
	cmd := exec.Command("git", args...)
	if dir != "" {
		cmd.Dir = dir
	}
	cmd.Env = scopedEnv()
	return cmd.Output()
}

// run executes git with args in dir ("" = current dir) and returns trimmed
// stdout. stderr is discarded; callers branch on err.
func run(dir string, args ...string) (string, error) {
	out, err := gitOutput(dir, args...)
	return strings.TrimSpace(string(out)), err
}

// noRenames is the flag every `git diff --name-only` feeding the collision
// engine passes: a move is listed by BOTH its old and new path, never just the
// new one (#181 review). With rename detection on (git's default), a window that
// committed README.md → pkg/README.md listed only pkg/README.md, so an edit of
// README.md in another window went unmatched. Fuzzy suffix matching used to
// catch that by accident ("README.md" is a suffix of "pkg/README.md"); exact
// matching depends on the old path being listed. It can only add paths, the
// direction a collision check can afford, and matches what the porcelain read
// in TouchedFiles already does for a staged move (#28).
const noRenames = "--no-renames"

// StagedFiles returns the paths staged for the IN-PROGRESS commit. It PRESERVES
// git's ambient environment (unlike run(), which strips GIT_INDEX_FILE) because
// git points GIT_INDEX_FILE at a TEMPORARY index for a partial commit
// (`git commit -a` / `-p` / `--only` / `-- <paths>`) — the on-disk index does
// NOT yet reflect what's being committed. Stripping it makes `git diff --cached`
// read the stale index and mis-report the staged set, so the pre-commit
// collision notice would miss (or over-report) collisions (#92 review). This is
// the invoking worktree's OWN read, so the ambient env is correct here — only
// the cross-worktree `-C dir` scans need scopedEnv.
//
// --no-renames: a staged move is listed by BOTH paths (#181 review), as the
// collision engine's other name sources are (noRenames).
func StagedFiles() ([]string, error) {
	out, err := exec.Command("git", "diff", "--cached", "--name-only", noRenames).Output()
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
	out, err := gitOutput(dir, args...)
	return string(out), err
}

// runRawReadOnly is runRaw for a read-only query in ANOTHER window's worktree,
// with readOnlyEnv. Use it only for a command that must not write there.
func runRawReadOnly(dir string, args ...string) (string, error) {
	cmd := exec.Command("git", args...)
	if dir != "" {
		cmd.Dir = dir
	}
	cmd.Env = readOnlyEnv(scopedEnv())
	out, err := cmd.Output()
	return string(out), err
}

// readOnlyEnv adds GIT_OPTIONAL_LOCKS=0 to env: without it `git status`
// opportunistically refreshes the worktree's index, which takes index.lock, so a
// probe of another window can make that window's own `git add` or `git commit`
// fail with "index.lock: File exists". The per-turn banner asks once per
// overlapping window on every prompt (#182). Set on the one command, never
// process-wide: the #92 lesson is that git env leaking into the wrong call breaks
// it. Pure.
func readOnlyEnv(env []string) []string {
	return append(env[:len(env):len(env)], "GIT_OPTIONAL_LOCKS=0")
}

// Present reports whether the git binary is on PATH.
func Present() bool {
	_, err := exec.LookPath("git")
	return err == nil
}

// RepoRoot returns the top-level dir of the repo containing cwd.
func RepoRoot() (string, error) { return Run("rev-parse", "--show-toplevel") }

// ShowPrefix returns cwd's path relative to the top of its worktree, as git
// sees it: "pkg/svc/" in a subdirectory, "" at the root. Used to read a path the
// operator typed relative to where they are as a repo-relative one (#181).
// Asking git avoids comparing os.Getwd, which can return the logical /var/…
// path on macOS, with --show-toplevel, which is always the physical
// /private/var/… one.
func ShowPrefix() (string, error) { return Run("rev-parse", "--show-prefix") }

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

// WorktreeAdopt attaches a worktree at path to an EXISTING branch — a local
// refs/heads/<branch> or, via git worktree-add's DWIM, a lone remote
// origin/<branch> (which materializes a local tracking branch). Unlike
// WorktreeAdd it NEVER creates a branch from base: adopting someone else's or a
// previous session's PR branch must land on that exact branch, not a fresh fork
// of it. On failure — the branch is absent, OR (common for adopt) already
// checked out in another worktree — the error carries git's own stderr so the
// caller can surface the real reason instead of a bare "exit status 128". (#134)
func WorktreeAdopt(path, branch string) error {
	cmd := exec.Command("git", "worktree", "add", path, branch)
	cmd.Env = scopedEnv()
	if out, err := cmd.CombinedOutput(); err != nil {
		if msg := strings.TrimSpace(string(out)); msg != "" {
			return fmt.Errorf("%w: %s", err, msg)
		}
		return err
	}
	return nil
}

// LocalBranchExists reports whether refs/heads/<branch> exists.
func LocalBranchExists(branch string) bool {
	_, err := Run("rev-parse", "--verify", "--quiet", "refs/heads/"+branch)
	return err == nil
}

// RemoteTrackingTip returns the commit refs/remotes/origin/<branch> points at, as
// last fetched, or "" when there is no such ref. No network. `wt adopt <branch>`
// checks a local branch of the same name against it (#167).
func RemoteTrackingTip(branch string) string {
	if branch == "" {
		return ""
	}
	out, err := Run("rev-parse", "--verify", "--quiet", "refs/remotes/origin/"+branch+"^{commit}")
	if err != nil {
		return ""
	}
	return strings.TrimSpace(out)
}

// HeadCommit returns the commit HEAD points at in the worktree at dir, or "" when
// it does not resolve (#167).
func HeadCommit(dir string) string {
	out, err := RunDir(dir, "rev-parse", "--verify", "--quiet", "HEAD^{commit}")
	if err != nil {
		return ""
	}
	return strings.TrimSpace(out)
}

// SymbolicHead returns the full ref HEAD points at in the worktree at dir
// ("refs/heads/<branch>"), or "" when HEAD is detached or unreadable (#167).
// Unlike CurrentBranchIn it never abbreviates, so a tag of the same name cannot
// turn the answer into "heads/<branch>".
func SymbolicHead(dir string) string {
	out, err := RunDir(dir, "symbolic-ref", "-q", "HEAD")
	if err != nil {
		return ""
	}
	return strings.TrimSpace(out)
}

// IgnoreCase reports core.ignorecase: git sets it when it creates a repo on a
// case-insensitive filesystem (macOS by default), where two branch names that
// differ only in case are one loose ref file. Unset or unreadable is false, as
// git itself reads it (#167).
func IgnoreCase() bool {
	out, err := Run("config", "--type=bool", "--get", "core.ignorecase")
	return err == nil && strings.TrimSpace(out) == "true"
}

// LocalBranches lists every local branch name (refs/heads/*, without the
// prefix) (#167).
func LocalBranches() ([]string, error) {
	out, err := Run("for-each-ref", "--format=%(refname)", "refs/heads")
	if err != nil {
		return nil, err
	}
	var names []string
	for _, ln := range strings.Split(out, "\n") {
		if ln = strings.TrimSpace(ln); strings.HasPrefix(ln, "refs/heads/") {
			names = append(names, strings.TrimPrefix(ln, "refs/heads/"))
		}
	}
	return names, nil
}

// AheadBehind counts the commits a has that b lacks (ahead) and the commits b
// has that a lacks (behind): `git rev-list --left-right --count a...b` (#167).
func AheadBehind(a, b string) (ahead, behind int, err error) {
	out, err := Run("rev-list", "--left-right", "--count", a+"..."+b)
	if err != nil {
		return 0, 0, err
	}
	return parseLeftRight(out)
}

// parseLeftRight reads `rev-list --left-right --count`'s "<left>\t<right>". Pure.
func parseLeftRight(out string) (left, right int, err error) {
	f := strings.Fields(out)
	if len(f) != 2 {
		return 0, 0, fmt.Errorf("unexpected rev-list --left-right --count output %q", out)
	}
	if left, err = strconv.Atoi(f[0]); err != nil {
		return 0, 0, err
	}
	if right, err = strconv.Atoi(f[1]); err != nil {
		return 0, 0, err
	}
	return left, right, nil
}

// OnelineLog returns up to max "<short sha> <subject>" lines for the commits in
// to that are not in from (`git log from..to`), newest first (#167).
func OnelineLog(from, to string, max int) ([]string, error) {
	out, err := Run("log", "--no-decorate", "--format=%h %s", "-n", strconv.Itoa(max), from+".."+to)
	if err != nil {
		return nil, err
	}
	var lines []string
	for _, ln := range strings.Split(out, "\n") {
		if ln = strings.TrimSpace(ln); ln != "" {
			lines = append(lines, ln)
		}
	}
	return lines, nil
}

// FastForwardBranch moves refs/heads/<branch> from `from` to `to`, and only as a
// fast-forward: it refuses unless from is an ancestor of to and the branch still
// points at from (#167).
//
// It moves the ref with `git branch -f`, not `update-ref`, for git's own in-use
// check: branch -f refuses a branch that any worktree has checked out OR is in
// the middle of rebasing or bisecting. Moving such a branch leaves that
// worktree's files at the old commit (its next commit reverts the move) or
// breaks the rebase's final ref update. A worktree mid-rebase has a detached
// HEAD, so `git worktree list` does not report the branch, and update-ref moved
// it anyway (measured, git 2.39). Callers should still rule out a checked-out
// branch first, for a clearer message.
func FastForwardBranch(branch, from, to string) error {
	if branch == "" || from == "" || to == "" {
		return fmt.Errorf("fast-forward needs a branch and two commits, got %q %q %q", branch, from, to)
	}
	ok, err := IsAncestor(from, to)
	if err != nil {
		return fmt.Errorf("fast-forward %s: %w", branch, err)
	}
	if !ok {
		return fmt.Errorf("fast-forward %s: %s is not an ancestor of %s", branch, from, to)
	}
	// branch -f has no old-value check, so confirm the branch has not moved since
	// the caller read it, as late as possible.
	if cur := BranchTip(branch); cur != from {
		return fmt.Errorf("fast-forward %s: it is at %s now, not %s", branch, cur, from)
	}
	cmd := exec.Command("git", "branch", "-f", branch, to)
	cmd.Env = scopedEnv()
	if out, err := cmd.CombinedOutput(); err != nil {
		if msg := strings.TrimSpace(string(out)); msg != "" {
			return fmt.Errorf("%w: %s", err, msg)
		}
		return err
	}
	if cur := BranchTip(branch); cur != to {
		return fmt.Errorf("fast-forward %s: it is at %s after the move, not %s", branch, cur, to)
	}
	return nil
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
// (@{u}). ⚠ That is NOT "has been pushed": a branch cut from origin/<base> (every
// `wt new` branch) gets origin/<base> as its upstream from birth, via git's
// default branch.autoSetupMerge. Pair it with UpstreamMergeRef and
// worktree.PushedUpstream to ask "was it pushed" (#175).
func HasUpstream(dir string) bool {
	_, err := RunDir(dir, "rev-parse", "--abbrev-ref", "--symbolic-full-name", "@{u}")
	return err == nil
}

// UpstreamMergeRef returns branch's configured merge ref (branch.<b>.merge, e.g.
// "refs/heads/main" for a `wt new` branch, "refs/heads/<b>" once pushed with -u),
// read in dir, or "" when unset or unreadable (#175).
func UpstreamMergeRef(dir, branch string) string {
	out, err := RunDir(dir, "config", "--get", "branch."+branch+".merge")
	if err != nil {
		return ""
	}
	return strings.TrimSpace(out)
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

// IsTrackedIn is IsTracked with path read relative to dir instead of the
// current directory. MCP wt_check reads its paths from the repo root, whatever
// directory the server was started in (#181 review).
func IsTrackedIn(dir, path string) bool {
	_, err := RunDir(dir, "ls-files", "--error-unmatch", "--", path)
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

// RangeChangedPaths returns the repo-relative paths this branch CONTRIBUTES over
// `from` — `git diff --name-only from...to` (THREE-dot: the diff since the
// merge-base, i.e. what `to` added, ignoring whatever `from` advanced by). Used
// by the pre-push collision check (#74) to scope the check to the OUTGOING
// commits, not the whole worktree.
//
// Three-dot is load-bearing. With two-dot (`from..to`), a branch BEHIND `from`
// (you branched, base moved on, you did NOT rebase) diffs base's tree against
// yours and reports every file base gained as an outgoing change (a reversal),
// so the guard blocked on files the pusher never touched — the more behind, the
// more it invented — contradicting `wt check`/`status`, which scope with
// three-dot (TouchedFiles, "committed-on-branch vs base since merge-base"). This
// is the #106 family for the not-rebased case: #106 moved the `from` ref to base,
// but two-dot still diverged whenever base wasn't already an ancestor of `to`.
// For a fast-forward (from is an ancestor of to) three-dot == two-dot, so the FF
// case is unchanged. A move is listed by both paths (noRenames). Runs in dir
// (empty → cwd). Best-effort.
func RangeChangedPaths(dir, from, to string) ([]string, error) {
	out, err := RunDir(dir, "diff", "--name-only", noRenames, from+"..."+to)
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
	// Read-only: this asks about ANOTHER window's worktree, once per prompt from
	// the agent banner, so it must not take that window's index.lock (#182).
	out, err := runRawReadOnly(worktree, "status", "--porcelain", "--untracked-files=all", "--", path)
	if err != nil {
		return false
	}
	// PURELY untracked only: every porcelain line for the path must be "?? ". A
	// coexisting index-side line means a pushable staged change is ALSO present —
	// e.g. `git rm --cached foo` (file kept on disk) emits BOTH "D  foo" (a staged,
	// committable deletion) and "?? foo". That deletion can collide (delete/modify)
	// with another window's edit, so it must NOT be downgraded (mirrors the #109
	// staged-deletion lesson). Any non-"?? " line → not purely untracked → false.
	sawUntracked := false
	for _, ln := range strings.Split(out, "\n") {
		if ln == "" {
			continue
		}
		if !strings.HasPrefix(ln, "?? ") {
			return false
		}
		sawUntracked = true
	}
	return sawUntracked
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

	// (b) committed-on-branch vs base (three-dot = since merge-base). A COMMITTED
	// move records both paths too (noRenames), like the staged one above.
	for _, ref := range []string{"origin/" + base, base} {
		if out, err := RunDir(dir, "diff", "--name-only", noRenames, ref+"...HEAD"); err == nil {
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
// these in BASE coordinates (the base ref's line numbers, #29) so two windows
// that fork from the same base can be compared in one frame. A pure
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
// dir, expressed in BASE coordinates (#29): line numbers of the base ref's copy
// of the file. Every window is reported in that one shared frame, so
// OverlappingSpans compares like-for-like even after one window has
// inserts/deletes before a shared edit region (the pre-#29 latent false
// negative: NEW-side numbers from separate diffs, in separate frames).
//
// The ranges are the window's OWN edits (committed + staged + unstaged), never
// the base's. For a branch up to date with base that is the old side of one
// `git diff -U0 <base> -- <file>` (base commit vs the working tree). A branch
// BEHIND base still holds the old text of everything base changed after it
// forked, so that same diff reads base's later edits as the branch's: a block
// base inserted (#142), a line base modified (#184). Another window editing
// those lines then "overlapped" a branch that never touched them, a false HIGH
// that blocked the pre-push hook. So a behind branch is measured from its merge
// base instead: `git diff -U0 <merge-base> -- <file>` holds only the branch's
// own edits, in merge-base line numbers, and they are moved into base line
// numbers through base's own hunks since that merge base (mapHunksToBase).
// Where a branch edit and a base edit meet (they overlap, or touch with no
// unchanged line between: git's 3-way conflict rule), the branch's range also
// covers base's replacement text there, so a window editing that text still
// overlaps it: a real conflict is never mapped away. A file absent at the merge
// base (each side added its own: an add/add) keeps the plain base diff, which
// compares the two versions directly (#142 review).
//
// nil when git could not measure the file at all (ChangedRangesChecked has the
// explicit ok): the graders read an empty side as indeterminate, so that grades
// HIGH, never disjoint.
//
// Fallback: when neither origin/<base> nor <base> resolves (a base-less repo,
// where cross-window base comparison is meaningless anyway), degrade to the
// NEW-side uncommitted hunks so a single window still self-reports.
func ChangedRanges(dir, base, file string) []LineRange {
	r, _ := ChangedRangesChecked(dir, base, file)
	return r
}

// ChangedRangesChecked is ChangedRanges with a failed measurement made explicit:
// ok=false when git could not diff the file at all. A caller that UNIONS these
// ranges with others (the pre-edit hooks add an agent's pending edit to them)
// must then fall back to a conservative grade: there an empty set would read as
// "no edits", where the graders read it as indeterminate.
func ChangedRangesChecked(dir, base, file string) (ranges []LineRange, ok bool) {
	ref, sha, hasBase := resolveBaseRef(dir, base)
	if !hasBase {
		return uncommittedRangesNew(dir, file)
	}
	if mb := behindMergeBase(dir, ref, sha, file); mb != "" {
		own, ownErr := runRaw(dir, "diff", "-U0", mb, "--", file)
		moved, movedErr := runRaw(dir, "diff", "-U0", mb, ref, "--", file)
		if ownErr == nil && movedErr == nil {
			return mapHunksToBase(parseHunks(own), parseHunks(moved)), true
		}
		// Can't measure from the merge base: fall through to the plain base diff,
		// which over-reports (base's own edits read as the branch's). A noisy
		// grade, never a hidden one.
	}
	out, err := runRaw(dir, "diff", "-U0", ref, "--", file)
	if err != nil {
		return nil, false
	}
	return parseHunkRangesOld(out), true
}

// wholeFile is what ChangedRangesNew reports when git can't measure the file: a
// span over every line, so a failed measurement grades as "edited every section"
// (HIGH against any window editing the doc), never as "edited nothing".
var wholeFile = []LineRange{{1, math.MaxInt32}}

// ChangedRangesNew is ChangedRanges but NEW-frame (current/`+` side). Use it to
// attribute a window's diff to its OWN current-content sections (#22 structured
// docs), where the section spans are parsed from the current file and so must
// share the current frame — base-frame ranges mis-attribute a section whenever an
// earlier edit shifts line counts (#123). NOT for cross-window line grading:
// that needs the base frame (ChangedRanges) so two windows' ranges are comparable
// (#108).
//
// It too reports only the window's OWN edits: a branch behind base is diffed from
// its merge base (#184), whose new side already IS the current file, so no
// mapping is needed. Diffed from base, the old text a behind branch still holds
// wherever base changed after it forked was attributed to the branch, so a
// section only base had touched graded "same section" HIGH against any window
// editing it (in both the #142 insert and the #184 modify shape). If that diff
// fails it falls back to the base diff (over-reports), and if git can't diff the
// file at all it reports wholeFile.
func ChangedRangesNew(dir, base, file string) []LineRange {
	ref, sha, hasBase := resolveBaseRef(dir, base)
	if !hasBase {
		if r, ok := uncommittedRangesNew(dir, file); ok {
			return r
		}
		return wholeFile
	}
	if mb := behindMergeBase(dir, ref, sha, file); mb != "" {
		if out, err := runRaw(dir, "diff", "-U0", mb, "--", file); err == nil {
			return parseHunkRanges(out)
		}
	}
	out, err := runRaw(dir, "diff", "-U0", ref, "--", file)
	if err != nil {
		return wholeFile
	}
	return parseHunkRanges(out)
}

// LinesToBase moves line spans of the worktree's CURRENT copy of file (an agent's
// pending edit, located in the on-disk file) into base line numbers, the frame
// every window's ChangedRanges are reported in. It maps through the worktree's
// own diff against base with the sides swapped (worktree → base), by the same
// rule as ChangedRanges: a span on a line the worktree holds differently from
// base (its own edit, or a base edit it is behind on) lands on base's text there,
// and a span touching such a region claims it too. So the result is where
// ChangedRanges will report the edit once it is made, whether the branch is up
// to date, behind, or already edited (#184; the #108 frame lesson without giving
// up on a worktree that differs from base). ok=false when that can't be measured
// (a git error, a binary file, or a base-less repo with uncommitted changes); the
// caller must then fall back to a conservative grade, never treat on-disk line
// numbers as base's. Pure mapping over one git call.
func LinesToBase(dir, base, file string, spans []LineRange) ([]LineRange, bool) {
	ref, _, hasBase := resolveBaseRef(dir, base)
	if !hasBase {
		// Base-less: every window self-reports its uncommitted NEW side, which is
		// the on-disk frame only while nothing uncommitted (not even a binary
		// change, which has no hunks) shifts it.
		for _, args := range [][]string{{"diff", "--", file}, {"diff", "--cached", "--", file}} {
			if out, err := runRaw(dir, args...); err != nil || out != "" {
				return nil, false
			}
		}
		return spans, true
	}
	out, err := runRaw(dir, "diff", "-U0", ref, "--", file)
	if err != nil {
		return nil, false
	}
	hunks := parseHunks(out)
	if len(hunks) == 0 && strings.Contains("\n"+out, "\nBinary files ") {
		return nil, false // differs from base, but not line by line
	}
	toBase := make([]diffHunk, len(hunks))
	for i, h := range hunks {
		toBase[i] = diffHunk{oldStart: h.newStart, oldCount: h.newCount, newStart: h.oldStart, newCount: h.oldCount}
	}
	own := make([]diffHunk, len(spans))
	for i, sp := range spans {
		own[i] = diffHunk{oldStart: sp.Start, oldCount: sp.End - sp.Start + 1}
	}
	return mapHunksToBase(own, toBase), true
}

// resolveBaseRef returns the ref every window's ranges are measured against —
// origin/<base>, else <base>, whichever first resolves to a commit in dir — and
// that commit's sha. ok=false when neither resolves (a base-less repo). One
// resolution for every caller keeps all windows in the same frame (#29/#108).
func resolveBaseRef(dir, base string) (ref, sha string, ok bool) {
	for _, r := range []string{"origin/" + base, base} {
		if s, err := RunDir(dir, "rev-parse", "--verify", "--quiet", r+"^{commit}"); err == nil && s != "" {
			return r, s, true
		}
	}
	return "", "", false
}

// behindMergeBase returns the merge base of ref (at sha) and the worktree's HEAD
// when the branch is BEHIND ref (the merge base is not ref itself) and file
// existed at that merge base. "" otherwise: up to date with base, no merge base
// (unrelated history, unborn HEAD), or a file new since the merge base, where
// "lines base changed under the branch" means nothing (each side added its own
// file, an add/add the plain base diff must keep seeing, #142 review).
func behindMergeBase(dir, ref, sha, file string) string {
	mb, err := RunDir(dir, "merge-base", ref, "HEAD")
	if err != nil || mb == "" || mb == sha {
		return ""
	}
	if _, err := RunDir(dir, "cat-file", "-e", mb+":"+file); err != nil {
		return ""
	}
	return mb
}

// diffHunk is one `git diff -U0` hunk header: the OLD side's start + line count
// and the NEW side's. A side with count 0 holds no lines (a pure insertion on the
// old side, a pure deletion on the new), and its start is then the line BEFORE
// the gap (0 = above line 1), as git reports it.
type diffHunk struct{ oldStart, oldCount, newStart, newCount int }

// fullHunkRe captures both sides of a -U0 hunk header (#184).
var fullHunkRe = regexp.MustCompile(`^@@ -(\d+)(?:,(\d+))? \+(\d+)(?:,(\d+))? @@`)

// parseHunks extracts every hunk header from -U0 output, in file order. A count
// git omits is 1. Pure (#184).
func parseHunks(diff string) []diffHunk {
	count := func(s string) int {
		if s == "" {
			return 1
		}
		n, _ := strconv.Atoi(s)
		return n
	}
	var out []diffHunk
	for _, ln := range strings.Split(diff, "\n") {
		m := fullHunkRe.FindStringSubmatch(ln)
		if m == nil {
			continue
		}
		oldStart, _ := strconv.Atoi(m[1])
		newStart, _ := strconv.Atoi(m[3])
		out = append(out, diffHunk{oldStart, count(m[2]), newStart, count(m[4])})
	}
	return out
}

// sideRange is one side of a hunk as a LineRange: [start, start+count-1], or for a
// zero-count side the gap's two neighbours [start, start+1] (parseHunkRangesWith's
// convention). Pure.
func sideRange(start, count int) LineRange {
	if count == 0 {
		return LineRange{start, start + 1}
	}
	return LineRange{start, start + count - 1}
}

// mapHunksToBase moves a branch's OWN hunks from MERGE-BASE line numbers (the old
// side of `git diff -U0 <merge-base> -- <file>` in its worktree) into BASE line
// numbers, given base's hunks since that merge base (old side = merge-base frame,
// new side = base frame), and returns them as LineRanges in parseHunkRangesWith's
// convention. Pure (#184).
//
// Each end of a hunk's range is mapped on its own (mapLine), so the range keeps
// covering everything between its ends. And every base hunk that CONFLICTS with
// the branch hunk by git's 3-way rule (hunksConflict: they overlap, or touch with
// no unchanged line between) adds its whole replacement to the range: in a merge
// the two are one conflict region, so a window editing base's text anywhere in it
// collides with this branch even on lines the branch never had. The chain shape:
// a stale branch edits line 11, base rewrites lines 12-13, another window edits
// line 13 — git merge-tree conflicts, so this must overlap.
func mapHunksToBase(own, base []diffHunk) []LineRange {
	out := make([]LineRange, 0, len(own))
	for _, o := range own {
		r := sideRange(o.oldStart, o.oldCount)
		m := LineRange{mapLine(r.Start, base).Start, mapLine(r.End, base).End}
		for _, h := range base {
			if !hunksConflict(o, h) {
				continue
			}
			n := sideRange(h.newStart, h.newCount)
			if n.Start < m.Start {
				m.Start = n.Start
			}
			if n.End > m.End {
				m.End = n.End
			}
		}
		out = append(out, m)
	}
	return out
}

// hunksConflict reports whether two hunks against the SAME old file are one
// conflict region in a 3-way merge. git (xdl_merge) keeps two changes apart only
// when an unchanged line separates them, so changed spans that overlap or are
// adjacent conflict, an insertion conflicts with a change that includes the line
// before or after its gap, and two insertions conflict at the same gap. (git
// also lets two IDENTICAL changes through; content isn't compared here, which
// only ever errs toward flagging.) Pure (#184).
func hunksConflict(a, b diffHunk) bool {
	switch {
	case a.oldCount == 0 && b.oldCount == 0:
		return a.oldStart == b.oldStart
	case a.oldCount == 0:
		return insertTouches(a.oldStart, b)
	case b.oldCount == 0:
		return insertTouches(b.oldStart, a)
	}
	return a.oldStart <= b.oldStart+b.oldCount && b.oldStart <= a.oldStart+a.oldCount
}

// insertTouches: an insertion after old line p sits in the gap between lines p
// and p+1, so it meets a change of old lines [start, start+count-1] that includes
// either of them. Pure.
func insertTouches(p int, h diffHunk) bool {
	return h.oldStart <= p+1 && p <= h.oldStart+h.oldCount-1
}

// mapLine maps ONE merge-base line into base line numbers (#184). Pure.
//
//   - A line base left alone moves by the net size change (newCount-oldCount)
//     of every base hunk above it. A pure insertion (oldCount 0) sits AFTER old
//     line oldStart, so it moves only the lines below that point.
//   - A line base modified or deleted lands on that hunk's whole new side. A
//     deletion has no new side, so it maps to the gap's two neighbours
//     [newStart, newStart+1], the same zero-count convention as
//     parseHunkRangesWith.
func mapLine(line int, hunks []diffHunk) LineRange {
	shift := 0
	for _, h := range hunks {
		if h.oldCount == 0 { // pure insertion below old line h.oldStart
			if line <= h.oldStart {
				break
			}
			shift += h.newCount
			continue
		}
		if line < h.oldStart {
			break
		}
		if line < h.oldStart+h.oldCount { // base modified or deleted this line
			return sideRange(h.newStart, h.newCount)
		}
		shift += h.newCount - h.oldCount
	}
	return LineRange{line + shift, line + shift}
}

// FileChangeSubsumed reports whether the worktree's branch contributes NOTHING
// new to base for path — its change to that file is already present on base, so
// the file is NOT contested even though the branch's commit history reads as
// unshipped (e.g. the branch's work landed via a DIFFERENT branch's squash and it
// never had its own PR, so neither PR-state nor `git cherry` can see it — #122).
//
// It 3-way merges the branch's CURRENT file (theirs — the worktree blob, so
// uncommitted edits count as live) into base (ours) using their merge-base as the
// ancestor: a CLEAN merge that reproduces base byte-for-byte means the branch adds
// nothing. Fail-safe: any unresolved ref/blob, a merge conflict, or a merge that
// changes base returns subsumed=false — an undeterminable case stays a collision
// (a missed collision is worse than a noisy one). known=false when it couldn't be
// evaluated at all.
func FileChangeSubsumed(worktree, base, path string) (subsumed, known bool) {
	if worktree == "" {
		return false, false // no worktree to evaluate — fail-safe (keep the collision)
	}
	baseRef := "origin/" + base
	if _, err := RunDir(worktree, "rev-parse", "--verify", "--quiet", baseRef+"^{commit}"); err != nil {
		baseRef = base
		if _, err := RunDir(worktree, "rev-parse", "--verify", "--quiet", baseRef+"^{commit}"); err != nil {
			return false, false
		}
	}
	mb, err := RunDir(worktree, "merge-base", baseRef, "HEAD")
	if err != nil || mb == "" {
		return false, false
	}
	// Untrimmed (runRaw): file content is newline-sensitive — trimming would break
	// both the merge and the byte comparison.
	ours, err := runRaw(worktree, "show", baseRef+":"+path)
	if err != nil {
		return false, false // base lacks the file — the branch adds a new one → contested
	}
	theirs, err := os.ReadFile(filepath.Join(worktree, path))
	if err != nil {
		return false, false // no worktree file (staged deletion etc.) → contested
	}
	ancestor, aerr := runRaw(worktree, "show", mb+":"+path)
	if aerr != nil {
		ancestor = "" // file added since the merge-base → empty ancestor (add/add)
	}

	dir, err := os.MkdirTemp("", "wt-subsumed-")
	if err != nil {
		return false, false
	}
	defer os.RemoveAll(dir)
	write := func(name, content string) (string, bool) {
		p := filepath.Join(dir, name)
		if werr := os.WriteFile(p, []byte(content), 0o600); werr != nil {
			return "", false
		}
		return p, true
	}
	oursP, ok1 := write("ours", ours)
	ancP, ok2 := write("anc", ancestor)
	theirsP, ok3 := write("theirs", string(theirs))
	if !ok1 || !ok2 || !ok3 {
		return false, false
	}
	// git merge-file -p prints the merged result; a non-zero exit (returned as
	// err) means a conflict → not subsumed. A clean merge that equals ours means
	// the branch's change is already on base.
	merged, err := runRaw(worktree, "merge-file", "-p", "-q", oursP, ancP, theirsP)
	if err != nil {
		return false, true // conflict → contested, but determinably so
	}
	return merged == ours, true
}

// uncommittedRangesNew is the base-less fallback of ChangedRanges,
// ChangedRangesNew and LinesToBase: the NEW-side hunks of the unstaged and the
// staged diff (index frame). With no base to be old-relative to, a single window
// still self-reports. ok=false when either diff fails.
func uncommittedRangesNew(dir, file string) (ranges []LineRange, ok bool) {
	ok = true
	for _, args := range [][]string{{"diff", "-U0", "--", file}, {"diff", "-U0", "--cached", "--", file}} {
		out, err := runRaw(dir, args...)
		if err != nil {
			ok = false
			continue
		}
		ranges = append(ranges, parseHunkRanges(out)...)
	}
	return ranges, ok
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

// DeleteRemoteBranch deletes branch on origin, from dir. `release --clean` uses it
// to drop the abandoned placeholder branch that claim pushed, so re-claiming the
// same issue pushes a fresh branch cleanly instead of hitting a non-fast-forward
// rejection (#159). Errors (e.g. the remote ref is already gone) are the caller's
// to treat as best-effort.
func DeleteRemoteBranch(dir, branch string) error {
	_, err := RunDir(dir, "push", "origin", "--delete", branch)
	return err
}

// BranchTip returns the full sha refs/heads/<branch> points at, or "" when it does
// not resolve (no such branch, detached HEAD). Branches are shared by every
// worktree, so this reads from the current repo. Used by the #168 tip lookup.
func BranchTip(branch string) string {
	if branch == "" || branch == "HEAD" {
		return ""
	}
	out, err := Run("rev-parse", "--verify", "--quiet", "refs/heads/"+branch+"^{commit}")
	if err != nil {
		return ""
	}
	return strings.TrimSpace(out)
}

// IsAncestor reports whether commit a is reachable from ref b (`git merge-base
// --is-ancestor`). Exit 0 is true and exit 1 is false; any other outcome (a bad or
// missing ref) is an error, so a caller cannot read "could not tell" as "no".
func IsAncestor(a, b string) (bool, error) {
	if a == "" || b == "" {
		return false, fmt.Errorf("is-ancestor needs two refs, got %q and %q", a, b)
	}
	_, err := Run("merge-base", "--is-ancestor", a, b)
	if err == nil {
		return true, nil
	}
	if ee, ok := err.(*exec.ExitError); ok && ee.ExitCode() == 1 {
		return false, nil
	}
	return false, err
}

// RemoteBranchTip returns the sha origin/branch points at (via ls-remote), or ""
// when the remote branch doesn't exist. `release --clean` compares it to the local
// placeholder tip before deleting, so a remote that diverged with real commits is
// never force-deleted (#159 review).
func RemoteBranchTip(dir, branch string) (string, error) {
	out, err := RunDir(dir, "ls-remote", "origin", "refs/heads/"+branch)
	if err != nil {
		return "", err
	}
	if out = strings.TrimSpace(out); out == "" {
		return "", nil
	}
	return strings.Fields(out)[0], nil // "<sha>\trefs/heads/<branch>"
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
