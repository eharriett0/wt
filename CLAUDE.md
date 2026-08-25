# CLAUDE.md — wt

`wt` is a single-binary Go CLI for **multi-window git coordination**: per-window
worktrees, issue claims, and — the differentiated core — **file/hunk-level
collision detection across live worktrees** with liveness-aware suppression.
Repo-agnostic, zero third-party deps (shells out to `git` + `gh`). User-facing
docs live in [README.md](README.md); this file is for working *on* wt.

## Layout

`main.go` → `internal/cli.Main`. Packages under `internal/`:

- **collide** — the core: `Scan` (enumerate worktrees + touched files),
  `CheckPaths`/`Overlaps`, the liveness classifier (`LiveFacts` → `ClassifyFacts`
  → `Liveness`), `ConflictSeverity` (hunk grading).
- **cli** — command dispatch + `report.go` (`buildCheckReport` = the grader) +
  the hook drivers (`runHook`, `claude_hook.go`).
- **gitx** / **ghx** — thin `git` / `gh` shell-outs.
- **hooks** — git hook shims + `HookPrePush`/`HookPreCommit`.
- **worktree** — `wt new`/`clean` (data-loss-critical; see below).
- **merge** — `merge-pr` guard + closing-keyword lint. **doctor**, **coord**,
  **activework**, **todos**, **config**, **section**, **ui**, **selfupdate**,
  **lock**.

## Discipline (non-negotiable)

- **Pure-function tests.** There is no live-git/network test harness. Split I/O
  from the decision and unit-test the pure core; smoke-verify the I/O path.
  Canonical pairs: `ClassifyFacts` (pure) vs `Classify` (I/O); `ReapVerdict` /
  `StaleIndexReportable` / `claudeDecision` / `classifyUpstream` — all pure.
  When you add a decision, extract it pure and table-test it.
- **Ship gate:** `go build ./... && go vet ./... && go test ./...` all green,
  plus an e2e smoke of the actual command against a scratch repo.
- **Adversarial review for anything safety-relevant.** Every review run this
  project has done caught a real bug — suppression *hiding* a collision (#87), a
  force-remove *data-loss* path (#88), a `GIT_INDEX_FILE` strip that broke the
  pre-commit hook's own staged read (#92). Reviews earn their keep here.
- **Commits reference an issue** (`Fixes #N` / `Refs #N`).

## Load-bearing invariants — do not regress

- **Never hide a real collision on ambiguity.** `Liveness.IsSuppressed()` covers
  only *definitively*-inert states (merged / dormant / closed-PR). A **dirty**
  worktree is NEVER suppressed — even a far-behind base checkout stays HIGH and
  is only *labelled* "likely stale" (#87). Uncomputable/offline → surfaced, not
  hidden.
- **`ClassifyFacts` precedence:** open PR > merged PR > closed PR > dirty >
  unmerged > merged-by-ancestry. PR state outranks a dirty index (a
  merged/closed branch with leftover staged cruft is stale, not a permanent
  HIGH — #79).
- **The hook block predicate MUST equal `wt check`.** Advisory means advisory in
  both; disjoint hunks don't block in either. `pushCollisionBlocks` mirrors
  `buildCheckReport`'s hunk grading on purpose (#92). If you change the grading,
  change both (or they'll disagree and get bypassed).
- **`wt clean` is data-loss-critical.** `ReapVerdict` only reaps a *provably
  shipped* worktree (grace window, upstream, merged PR / cherry). Never
  force-remove a dirty worktree automatically — `--stale-index` is
  **report-only** because a MERGED PR proves only the *committed* work shipped;
  the dirty index could be fresh post-merge work (#88).
- **gitx env-scoping (#92).** `run`/`runRaw` strip `GIT_DIR`/`GIT_INDEX_FILE`/
  `GIT_WORK_TREE`/… (git sets these for a running hook, pinned to the invoking
  worktree) so per-worktree `git -C dir` commands discover from their own dir.
  **But** the invoking worktree's own staged read must honor the ambient index
  (git uses a *temp* index for `git commit -a`/`-p`/`--only`) — that's why
  `gitx.StagedFiles()` deliberately does NOT strip. Don't route a hook's own
  index-dependent read through the scoped `run`.

## Hooks

Installed hooks are thin shims (`exec "…/wt" _hook <name>`) — all logic is in the
binary. The **sentinel** that marks a wt-managed shim is the comment `wt-managed
hook` (NOT `wt _hook` — the quoted path renders `wt" _hook`, which broke
detection in v0.1.8, #91). `runHook` dispatches: git hooks (`pre-push`,
`pre-commit`) and Claude Code hooks (`todo-write` PostToolUse, `claude-edit`
PreToolUse). Claude Code hooks derive the repo from the payload's `cwd` and
**always exit 0** (advisory; a coordination nicety must never break the session).

## Release

`wt` is installed via the Homebrew tap `eharriett0/homebrew-tap` (formula
`Formula/wt.rb`, which `go build`s from the release tarball and injects the
version via `-ldflags -X …cli.Version=`). To ship:

1. `git tag -a vX.Y.Z -m "…" && git push origin vX.Y.Z`
2. `gh release create vX.Y.Z …`
3. `SHA=$(curl -sL <archive/refs/tags/vX.Y.Z.tar.gz> | shasum -a 256)`; update
   the formula's `url` + `sha256` in the tap; commit (the tap has no issue-ref
   hook — bypass the me-repo's with `HOOK_DISABLE_ISSUE_REF=1` if it fires) +
   push.
4. `brew update && brew upgrade wt`; verify `wt version`.

The formula supports `head "…", branch: "main"` for `--HEAD` builds.

## gh / tooling gotchas

- `gh pr edit --body` can silently no-op (GraphQL Projects-classic deprecation)
  — use `gh api repos/OWNER/REPO/pulls/N -X PATCH -F body=@file`.
- `gh issue create` has **no `--json`** — capture the printed URL, parse the
  number.
- `closingIssuesReferences` is **GraphQL-only** (not a `gh pr view --json` field)
  — query via `gh api graphql … resource(url:){… on PullRequest{…}}`. It reads
  the PR body only, NOT the squash commit body (the `merge-pr` close-lint scans
  commit messages too — #77).
- macOS is the dev floor: bash 3.2 (no `mapfile`/`declare -A`), BSD `sed`/`stat`,
  `/var`→`/private/var` symlinks (resolve with `EvalSymlinks` before path
  compares).
