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
  HIGH — #79), except a MERGED PR found only by the tip commit (#168, below).
- **The hook block predicate MUST equal `wt check`.** Advisory means advisory in
  both; disjoint hunks don't block in either. `pushCollisionBlocks` mirrors
  `buildCheckReport`'s hunk grading on purpose (#92). If you change the grading,
  change both (or they'll disagree and get bypassed). The **structured-doc
  SECTION grade** is part of that equality (#98) and is single-sourced in
  `collide.SharedSectionsAcross` — the hooks used to stop at the blanket
  shared-doc advisory, so the one case `structured_doc` exists to catch (two
  windows in the same lane) blocked in `check` and sailed through pre-push.
  Inside `cli`, the per-entry decision is ONE function, `gradeEntry` (#182):
  `wt check`/wt_check/both edit hooks AND `wt status`/wt_status/the per-turn
  banner grade through it. The banner used to run its own all-windows grade, so
  merged/dormant/closed windows rode along and it said HIGH where `check` said
  low. For a file the current window edits, the banner lists what `check` lists
  and is **HIGH ⇒ `check` blocks** (`TestAgentOverlaps_MatchesCheck`). ⚠ NOT ⟺,
  on purpose: when this window's own copy is already on base (#109) or its change
  already landed (#122), `check` still blocks (its pre-edit heads-up: empty or
  phantom ranges can't be proven disjoint) but the banner reads "same file".
  Re-tighten it and a session left open after its PR merged is told HIGH on
  every turn, the #182 noise. An untracked copy (#113) stays HIGH in both.
  Window-neutral (`wt status`, banner lines for files this window isn't
  editing), a file is HIGH iff some pair blocks in BOTH directions: a window
  whose claim is already merged, landed or untracked contests nothing. ⇒ Two
  windows creating the same new file read advisory in status and in other
  windows' banners until one commits it (consistent with #113), while `check`
  from the untracked side still blocks. **A label is not an identity**: two
  worktrees that claimed one issue are both `#N`, and detached worktrees are
  named by their directory. Overlaps carry worktrees (`Overlap.Worktrees`,
  `collide.Self`) and pairs grade by worktree; `ClassifyWindows` keeps a shared
  label's least-suppressed answer. Grading by label compared one window with
  itself and dropped a real pair.
- **`wt clean` is data-loss-critical.** `ReapVerdict` only reaps a *provably
  shipped* worktree (grace window, upstream, merged PR / cherry). Never
  force-remove a dirty worktree automatically — `--stale-index` is
  **report-only** because a MERGED PR proves only the *committed* work shipped;
  the dirty index could be fresh post-merge work (#88). **Never reap a worktree
  on the BASE branch** (`ReapableBranch`): "patch-equivalent on base" is
  trivially true for the base itself, so every downstream verdict says shipped
  and the printed command becomes `git branch -D main` (#101).
  ⚠ **An upstream is not a push (#175).** `wt new` branches from
  `origin/<base>`, and git's default `branch.autoSetupMerge` records that as the
  new branch's upstream, so `HasUpstream` is true from birth. The "never pushed"
  guard read it as a push and never fired: a clean commitless checkout was swept
  as soon as its grace window passed. `PushedUpstream` excludes an upstream
  whose merge ref is the base itself. ⚠ **`dirty` is an input to `ReapVerdict`
  (#174)**, so the LISTING agrees with `remove()`, which already refused a dirty
  tree: the listing used to say "safe to remove" above a remove command for a
  worktree whose only content was uncommitted work. `remove()` keeps its own
  refusal as defense in depth.
- **The tip-commit PR lookup is data-loss-relevant (#168).** When no PR has a
  branch's own name, `PRForBranchOrTip` asks GitHub which PRs contain the
  branch's TIP commit. MERGED counts only when the tip is in that PR's FINAL
  commits (a force-pushed-away commit proves nothing), OPEN stays contention,
  and closed-unmerged is ignored. ⚠ It runs only for a tip that is NOT on base
  (`TipLookupApplies`): a tip already on base belongs to some merged PR on a
  merge-commit or rebase repo, so a fresh `wt new` worktree would read as
  shipped and `clean` would reap it (the #61 class). Unknown ancestry skips it.
  ⚠ **A tip-found MERGED does NOT outrank a dirty worktree** (`LiveFacts.ViaTip`,
  the one exception to #79's "PR state outranks dirty"). Found by name, the
  branch IS the merged PR's head, so a dirty index is leftover cruft. Found by
  tip, it only points into a merged PR, and so does a follow-up branch started
  at that PR's head, whose uncommitted edits are new work. wt cannot tell them
  apart, so the edits stay HIGH (as before #168) and the label names the PR.
- **`wt clean <name>...` resolves every name before acting (#169).** A name
  must pick out exactly ONE worktree (`CheckNames`): none is a typo, two is a
  directory name that is also another worktree's branch. Either stops the run
  with nothing cleaned, so a bad `-y` list never removes "the rest" of it.
- **"No PR" must be an `ok=false`, never a parsed placeholder (#168).**
  `PRForBranch`'s old `.[0] | …` query printed `null null` for a branch with
  no PR, which parsed as a PR in state `null`. Every caller then matched no
  state, so it worked by accident, until the first caller that asked "was there
  a PR at all?" (the tip fallback) silently never ran. Only the e2e smoke found
  it. `parsePRForBranch` now refuses anything that is not a number and a known
  state; keep new gh queries to `// empty` for "none".
- **Scope-widening re-opens what narrowness was hiding.** The base-branch footgun
  above was latent for as long as `clean` skipped everything outside
  `worktree_root` — an accident, not a guard. `--all-roots` (#101) armed it, and
  the e2e smoke is what caught it. When you widen a blast radius, re-derive which
  guards were load-bearing *by luck*.
- **`clean`'s reach and the collision engine's reach must not drift apart.** The
  engine scans EVERY worktree git knows about; if `clean` manages fewer, the
  difference is a set of worktrees that can hard-block a push with no in-tool way
  to clear them — they never age out either, since dormant suppression is gated
  on `max_age`, unset without a `.wt.conf` (#101). Suppressing them in the engine
  would be the wrong direction: an out-of-root worktree is still a live window,
  and a false negative there is worse than the noise.
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
`pre-commit`), Claude Code hooks (`todo-write` PostToolUse, `claude-edit`
PreToolUse), and the Codex hooks (`codex-context` UserPromptSubmit, `codex-edit`
PreToolUse). Agent hooks derive the repo from the payload's `cwd` and **always
exit 0** (advisory / fail-open; a coordination nicety must never break the session).

**Codex now gets BOTH a per-turn hook AND a per-edit hook (#117).** Codex's
`PreToolUse` fires on `apply_patch` and supports both `additionalContext` and
`permissionDecision:"deny"` — so `wt install-codex-hook` wires two hooks:
- `wt _hook codex-context` (**UserPromptSubmit**): each turn emits the cross-window
  overlap summary (`agentOverlaps`: `collide.PartitionOverlapsFor` + `gradeOverlaps`)
  from the CURRENT window's side, so a file this window edits lists the windows
  `wt check <file>` would there and reads HIGH only where it blocks (#182); the
  current window, identified by worktree (`collide.SelfFor`), is excluded. Same
  builder as `claude-context`.
- `wt _hook codex-edit` (**PreToolUse**, matcher `apply_patch`): parses the patch's
  `*** {Update|Add|Delete|Move} File:` targets, grades them via `buildCheckReport`
  (the same grader as `wt check`), then RE-grades each `CatBlocking` entry against
  the patch's actual hunks — localized in the current file via `locateRange`
  (`parseCodexPatch` → per-hunk pre-image of context+removed lines) — but **only
  when frame-safe** (this worktree's file is unchanged vs base, i.e.
  `ChangedRanges(root,base,path)` empty; the #108 lesson). A disjoint patch to a
  shared file therefore stays silent. Emits `additionalContext` on overlap;
  `WT_CODEX_HOOK_BLOCK=1` upgrades a **confirmed** HIGH (frame-safe hunk overlap) to
  `deny` — a file-level-only match never denies.

Key facts: `.codex/hooks.json` uses the **same nested shape** as Claude's
`.claude/settings.json` (`{hooks:{UserPromptSubmit:[{hooks:[…]}],PreToolUse:[{matcher:"apply_patch",hooks:[…]}]}}`);
`mergeCodexHook` installs both idempotently (`ensureCodexEntry` skips any event
whose inner hooks already run the command, preserving other tools). Codex hooks are
**enabled by default** now (hook *definitions* still require trust/review on first
run) — disable entirely via `[features] hooks = false` in `~/.codex/config.toml`
(the installer prints this; it does NOT edit config.toml). PreToolUse output JSON is
`{hookSpecificOutput:{hookEventName:"PreToolUse", additionalContext:…}}` (advisory)
or `{…permissionDecision:"deny", permissionDecisionReason:…}` (block). The
worktree-based engine + git `pre-push`/`pre-commit` guards are already
agent-agnostic, so a Codex window is first-class regardless of the hooks.

## MCP (`wt mcp`, #115)

`wt mcp` (`internal/cli/mcp.go`) is a **stdio MCP server** for chat-style agents
(Claude Desktop, Cursor, …) — the third agent-integration surface after Claude/Codex
hooks. It speaks newline-delimited JSON-RPC 2.0 on stdin/stdout (hand-rolled, zero
deps): `initialize` (echoes the client's `protocolVersion`, advertises `tools`),
`tools/list`, `tools/call`, `ping`; a request with **no `id`** is a notification →
no response (e.g. `notifications/initialized`). Responses are **compact** JSON + `\n`
(never `MarshalIndent` — an embedded newline would break the client's line reader; the
pretty JSON lives INSIDE the tool result's text string, where `\n` is escaped). Read
loop is `bufio.Scanner` with an 8MB buffer so one malformed line yields `-32700` and
the stream **continues** (doesn't desync). Four **read-only** tools —
`wt_status` / `wt_check` / `wt_todos` / `wt_where` — each single-source the SAME
`collide.Scan` + `buildStatusPayload`/`buildCheckReport`+`buildCheckPayload` +
`gradeStatusOverlaps` + `todos.ForWorktree` the CLI uses (payload builders extracted
in `report.go` so JSON can't drift from `wt status/check --json`). Tool-level failures
(not-in-repo, bad args, scan error) return `{isError:true}` results, NOT JSON-RPC
protocol errors, so the model sees them. **v1 is read-only** — no tool mutates state
(claim/announce/merge stay in the CLI + git hooks). The server runs in the client's
cwd (`config.Load()` derives the repo); it never chdirs.

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

**Fresh-machine install** (the person the install docs are actually for) needs a
`trust` step first — Homebrew refuses to load a formula from an untrusted
third-party tap: `brew tap eharriett0/homebrew-tap && brew trust eharriett0/tap
&& brew install wt`. Note the tap is `eharriett0/homebrew-tap` but `brew trust`
takes the short `eharriett0/tap`. The source build also pulls/upgrades the `go`
formula as a build dependency (#105).

The formula supports `head "…", branch: "main"` for `--HEAD` builds.

## gh / tooling gotchas

- `gh pr edit --body` can silently no-op (GraphQL Projects-classic deprecation)
  — use `gh api repos/OWNER/REPO/pulls/N -X PATCH -F body=@file`.
- `gh issue create` has **no `--json`** — capture the printed URL, parse the
  number.
- **Ask gh about auth only through `ghx.AuthedFor`/`Authed`** (#172). The answer
  is memoized per host for `authTTL` (a minute: one command asks once, and a
  long-lived `wt mcp` still notices a `gh auth login`). In a repo with no forge
  host (a local-path origin, which is what a scratch-repo smoke uses) the check
  is a bare `gh auth status`, deliberately (#100), and that validates EVERY
  configured host: about 6 s with two. Uncached, it ran once per PR lookup and
  made a two-worktree `wt clean` take 18 s instead of 6.
- **A backtick code span DOES suppress GitHub's linked-issue parser — measured, #164.**
  One PR, one already-closed issue, body varied and `closingIssuesReferences` sampled:

  | body | reading |
  |---|---|
  | keyword inside `` `…` `` | `[]` at T+0, 60, 120, 180s |
  | same keyword, bare | `[160]` **immediately** at T+0 |

  So quoting the phrase in a postmortem is safe, and `wt`'s own lint deliberately
  **over-reports** there (`test_a_code_span_is_still_matched` pins that): flagging a
  keyword GitHub would ignore is noise, missing one it would honour is a closed issue.
  ⚠ **Only the PR-BODY surface was measured.** A commit message is not markdown and
  may not honour code spans at all, so do NOT rely on backticks there.
- ⚠ **On an OPEN PR the field tracks the body immediately, in both directions.** Measured
  on the same PR: adding a bare keyword registered at T+0, and removing it cleared at T+0.
  So there is no general "closingIssuesReferences lags" rule.
  ⚠⚠ **The one observed lag was on a MERGED PR**, where stripping the keyword left the
  field still returning the issue across two consecutive reads before it cleared. That is
  the reading that produced the false claim "a merged PR's closing link is frozen and
  cannot be cleared". It is not frozen; it was read too early, twice.
  ⇒ Treat a post-merge read as the only one needing a pause, and never conclude "frozen"
  from two quick samples of the same artifact.
- `closingIssuesReferences` is **GraphQL-only** (not a `gh pr view --json` field)
  — query via `gh api graphql … resource(url:){… on PullRequest{…}}`. It reads
  the PR body only, NOT the squash commit body (the `merge-pr` close-lint scans
  commit messages too — #77).
- macOS is the dev floor: bash 3.2 (no `mapfile`/`declare -A`), BSD `sed`/`stat`,
  `/var`→`/private/var` symlinks (resolve with `EvalSymlinks` before path
  compares).
