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
- **A window's ranges are its OWN edits, in BASE line numbers (#29/#142/#184).**
  Diffed straight from base, a branch BEHIND base reads every line base inserted
  or modified after it forked as its own edit (it still holds the old text), so
  any window editing those lines "overlapped" it: a false HIGH that blocked
  pushes. `ChangedRanges` therefore diffs a behind branch from its merge base and
  moves its hunks into base numbering through base's own hunks
  (`mapHunksToBase`). ⚠ A base hunk that CONFLICTS with a branch hunk by git's
  3-way rule (`hunksConflict`: they overlap, or touch with no unchanged line
  between; checked against `git merge-file` on every 1-2 line shape) adds its
  replacement to the branch's range: git merges the two as ONE conflict region,
  so a window editing base's text there collides even on lines the branch never
  had (branch edits l11, base rewrites l12-13, another window edits l13: merge-tree
  conflicts). Mapping only the branch's own lines dropped exactly that case, and a
  push main had blocked went through. An edit identical to one base also made
  (a cherry-pick) stays the branch's: if the other window lands first, git
  conflicts. `ChangedRangesNew` diffs from the merge base too.
  ⚠ **A failed measurement never reads as "no edits":** the merge-base diffs
  failing fall back to the plain base diff (over-reports); git failing outright
  → `ChangedRangesChecked` ok=false, `ChangedRanges` nil (graders: indeterminate
  = HIGH), `ChangedRangesNew` the whole file. `gitOutput` is the test seam.
- **The pre-edit hooks grade what `wt check` will say once the edit is made
  (#108/#184).** `regradePending`: every hunk-graded entry (HIGH *or* FYI: a
  window whose earlier edits were disjoint can be about to overlap) is HIGH iff
  this window's own ranges ∪ the pending edit overlap the other window's. The
  pending edit is located in the on-disk file and moved into base numbering
  through this worktree's own diff (`gitx.LinesToBase`, the same mapping with the
  sides swapped), so a window that is behind base or already edited the file is
  graded exactly, not file-level. Only when that can't be computed (a Write, a
  non-unique `old_string`, a binary file, a git error) does an entry stay as a
  file-level heads-up, and then it is never dropped.
- **Coordination ownership is ONE predicate, and it includes the session (#163).**
  Two agent sessions started in one checkout resolve to the same window id, so
  each treated the other's announcements as its own: `inbox clear`, `wt holds`
  listing them as "yours", the merge-pr gate exempting their holds. `coord.Self.Owns`
  = same window AND (same session OR either side session-less OR the record's
  session is `MirrorSession` of self's token) is what inbox, acks, `holds` and the
  gate all use, and every "same checkout, another session" label comes from
  `Self.SharesCheckout` (derived from `Owns`); don't add a raw `r.Window == self`
  compare. The window id itself stays path-based (#18 self-hold exemption, #156
  claims across restarts). The session is `WT_SESSION` → `CLAUDE_CODE_SESSION_ID`
  → `CODEX_SESSION_ID`, else `coord.SessionNone`. Claude Code keeps its id across
  `--resume`/`--continue`; `--fork-session`, **`/clear`** (the conversation reset
  rewrites the env var; verified in the 2.1.292 binary) and a fresh session mint a
  new one, so a hold placed before a /clear gates the session after it like
  another session's (the gate says so). Subagents share their parent's id. Codex
  exports `CODEX_SESSION_ID` (the root thread's id, openai/codex#37848) to every
  shell command.
  ⚠ **Terminal ids (`TERM_SESSION_ID`/`ITERM_SESSION_ID`) are deliberately NOT
  sessions**: every tmux pane or editor started in the tab inherits them (false
  confidence), and they split one person's tabs into parties (their own hold
  blocks them from another tab, and #18's WT_WINDOW pinning across terminals stops
  exempting it). ⚠ **`""` means ONLY a pre-#163 record** (the back-compat
  wildcard). A session with no token stamps `coord.SessionNone`, so a token-less
  shell and a Claude session in one checkout stay two parties; "simplifying" it
  back to `""` silently re-merges them. Every writer stamps through
  `stampRecord` (pinned). ⚠ **The per-turn hook reads the log as the session the
  agent's own `wt` commands stamp** (`agentHookSession`): its inherited env first.
  Claude Code sets `CLAUDE_CODE_SESSION_ID` in hook processes too, so its payload
  is never consulted. Codex does NOT put `CODEX_SESSION_ID` in a hook's env (a
  hook runs with the Codex process's own env snapshot,
  `codex-rs/hooks/src/registry.rs`), but its payload `session_id` comes from the
  same `Session::session_id()` (`core/src/hook_runtime.rs`), so codex-context
  ONLY falls back to the payload id. ⚠ **The GitHub mirror carries
  `coord.MirrorSession` (a hash), never the raw id**, and `Owns` matches a
  session's own hash, or #18 breaks across clones that share only the mirror
  (separate local logs). ⚠ **Hold advice:** `wt ack <id>` (waives the hold for
  the reader only) always leads; `wt all-clear <id>` releases it for EVERY window
  and is suggested only for a hold `coord.HoldLooksOrphaned` (12h old, or its
  session silent for 4h), always saying so. In a single-worktree repo the per-turn
  hook shows another session's entries plus every other window's HOLD (the gate
  enforces those). Block-id reservations are still keyed by window only.
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
  itself and dropped a real pair. **So do conflicts (#193):** `collide.Conflict`
  carries the other window's `Worktree` (set in `CheckPaths`), and
  `checkEntries`, `hooks.gradeConflicts` and the edit hooks' #122 re-check read
  THAT worktree (`collide.WorktreeOf`), never the label's: the label lookup kept
  the last namesake, so a real overlap was graded against its twin's disjoint
  hunks and pre-push let it through. Liveness stays per label (least-suppressed,
  so a shared label can only surface more).
- **A real path matches EXACTLY; only a search term is fuzzy (#181).** Paths
  from git or a hook payload (pre-push outgoing, pre-commit staged,
  Claude/Codex edit targets) go through `collide.ExactQueries`. A `wt check` /
  `wt_check` argument goes through `collide.QueryFor`: exact when it names a
  real path (on disk or tracked relative to the cwd, or touched at that exact
  path by a window), fuzzy (suffix / basename / dir-suffix) only when it names
  nothing. A root file has no `/` to anchor a suffix on, so fuzzy-matching every
  path made the root `README.md` collide with another branch's
  `pkg/svc/README.md` and blocked the push. The zero `MatchMode` is exact, so a
  query whose mode was never decided can't invent a collision. A `wt check`
  argument is read from the cwd (`git rev-parse --show-prefix`, `cwdArgBase`),
  a `wt_check` one from the repo ROOT (`rootArgBase`: its schema says
  repo-relative, and the server may run in a subdirectory); absolute ones go
  through `resolveExisting`, which handles `/var`→`/private/var` even for a file
  that doesn't exist yet. ⚠ So the #92 equality holds for every path that
  names something; for a name that names nothing yet (not on disk, not
  tracked, not touched at that path), `wt check` runs a fuzzy SEARCH and is a
  superset of the edit hooks, which ask about the exact file being created and
  stay silent about namesakes. That direction is safe; don't "fix" it by making
  the hooks fuzzy. CheckPaths lets an exact query claim a file before a fuzzy
  one (`exactFirst`), so an entry names the real path whatever the argument
  order. ⚠ **Every `git diff --name-only` feeding the engine passes
  `--no-renames`** (TouchedFiles, RangeChangedPaths, StagedFiles): with rename
  detection a COMMITTED move listed only its new path, so an edit of the old
  path in another window went unmatched. The fuzzy suffix tier used to hide that
  (README.md is a suffix of pkg/README.md); exact matching needs the old path
  listed, as the porcelain read already does for a staged move (#28).
- **Paths from git are read NUL-separated, never quoted (#200).** Read line by
  line, git C-quotes a path holding a byte it calls unusual: under the default
  `core.quotePath` every non-ASCII byte, plus `"`, `\` and control characters
  (porcelain status quotes a space too). `café.md` came back `"caf\303\251.md"`,
  was stored as the window's touched path, and `wt check café.md`, pre-push and
  both edit hooks never matched it: a real collision on any non-ASCII name went
  unreported. Every path lister passes `-z` (`gitx.nulPaths`: TouchedFiles'
  status and diff, StagedFiles, RangeChangedPaths, and MergeTreeConflicts for
  the base-drift warning's names) and is read through `runRaw`, never the
  trimming `run`/`RunDir` (a name can begin or end with a space), by the pure
  `splitNUL` / `parsePorcelainZ`. ⚠ `status --porcelain -z` writes a rename or
  copy as `XY <new>\0<orig>\0`, new FIRST: the reverse of the line format's
  `orig -> new`. ⚠ So a real path is never trimmed downstream either:
  `Query.cmpPath` trims only a fuzzy search term, `resolveCheckArgs` keeps an
  argument's spaces when it names a real path as typed, and the Claude payload
  path is used as sent. Line-based on purpose: `IsUntracked`, `IsClean` and the
  dirty counts read only status columns (one line per entry, a newline in a
  name is quoted), and `git worktree list --porcelain` prints paths unquoted
  (its `-z` needs git 2.36; Ubuntu 22.04 ships 2.34). gh has no `-z`: `gh pr
  diff --name-only` relays GitHub's quoted names, so `ghx.PRChangedFiles`
  unquotes them (`unquoteGitPath`) before the deploy-path globs see them.
- **A path handed to git means that one file, never a pattern (#204).** After
  `--` git reads a path as a pathspec: `a[1].md` also matched an untracked
  `a1.md`, so `IsUntracked` read a committed `a[1].md` as untracked and #113
  downgraded a real collision; `*.md` folded every .md file's hunks into one
  file's ranges; a leading `:` is magic (`:colon.md` measured `colon.md`). Every
  `-- <path>` call passes `gitx.literalPath(p)` (`:(literal)<p>`): IsTracked*,
  IsUntracked, ChangedRangesChecked/ChangedRangesNew, LinesToBase,
  uncommittedRangesNew. Prefixing the path, not `git --literal-pathspecs`, keeps
  the subcommand at `args[0]`, where the `gitOutput` failure-injection tests
  match it (a per-call env var would need the seam itself changed). ⚠
  `scopedEnv` strips `GIT_LITERAL_PATHSPECS` (and the glob/noglob/icase
  switches): git exports it to the hooks of `git --literal-pathspecs …`, and
  under it git reads `:(literal)` as part of the name, which then matches
  nothing. Already literal, not pathspecs: `hash-object -- <file>` and
  `<rev>:<path>` lookups. But `RefBlob`'s staged form is `:0:<path>`: in the
  short `:<path>` form, `1:x.md` reads as stage 1 of `x.md`.
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
- **`wt adopt` never checks out, creates or moves a branch onto anything but the
  target (#167).** `git worktree add <path> <branch>` takes `refs/heads/<branch>`
  whenever it exists, so a stale branch left by an earlier PR that reused the
  name was adopted in place of the PR head (ahead 6, behind 285). The target is
  `origin/<branch>` as just fetched. ⚠ **Adopting by PR it must first carry
  gh's `headRefOid`** (`GatePRHead`: equal, or contains it after a SUCCESSFUL
  fetch, since gh can lag a push): a fork PR's head is not on origin, so
  `origin/<branch>` is another branch that shares the name (and DWIM checked it
  out), or the PR head is in the clone via `gh pr checkout` and the local branch
  of that name (base `main`, for a PR from a contributor's main) got
  fast-forwarded onto the fork's commits. A failed fetch leaves an old copy.
  Gate first, before the existing-worktree short-circuit; don't key on
  `isCrossRepository` (an origin that IS the user's fork is legitimate). Then
  `prepareLocalBranch` (pure `ClassifyTips`/`DecideAdopt`): equal or only ahead
  (unpushed) is attached as is, only-behind is fast-forwarded
  (`gitx.FastForwardBranch`), diverged is refused with both SHAs. ⚠ Only a
  branch NO worktree is using is moved (`BranchInUse`; a failed `git worktree
  list` counts as in use): moving one underneath its worktree leaves its files at
  the old commit, and its next commit reverts the move. `git worktree list`
  cannot see a worktree mid-rebase of the branch (detached HEAD), so the move
  goes through `git branch -f`, whose own in-use check refuses it (`update-ref`
  moved it, measured). ⚠ **With `core.ignorecase` a branch differing only in
  case is the same loose ref file**, and git's in-use check compares names
  exactly (it moved `Feat` under its worktree for `wt adopt` of `feat`), so a
  case-only twin is refused outright. ⚠ After the add, the worktree must be on
  `refs/heads/<branch>` at the intended commit (`AdoptedOnTarget`, read with
  `symbolic-ref`, not `--abbrev-ref`): a tag of the same name wins over
  `origin/<branch>` in worktree-add's DWIM and leaves a detached HEAD; that
  worktree, and only it, is removed. A re-run is checked too (`DecideExisting`:
  behind or ahead is handed back with a note, diverged is refused, and wt never
  moves an existing worktree's branch).
- **`wt new` / `wt claim` re-attach a same-named local branch only after #167's
  check (#198).** #62 re-attaches an existing local branch so a worktree whose
  directory went away keeps its work; unchecked, that resumed a branch left by an
  earlier attempt that reused the name, or one behind or diverged from what was
  pushed. `PlanNew` fetches `origin/<branch>` with an explicit refspec (so a
  single-branch or shallow clone still gets the tracking ref, read back exactly
  with `for-each-ref`, never a case-only twin's loose ref; none: attach unverified,
  the #62 never-pushed case) and decides through adopt's own
  `DecideAdopt`/`planAttach`: equal or only-ahead attached (ahead names the unpushed
  commits), only-behind fast-forwarded with #167's guards (in use, case twin,
  `branch -f`), diverged or uncomparable refused. An existing worktree is checked
  like adopt's re-run and never moved (`DecideExistingFor`: for claim, only-behind
  is refused too, its placeholder could not be pushed). ⚠ Claim calls `PlanNew`
  BEFORE assigning the issue and `Create` after, so a refusal leaves no partial
  claim; `Create` re-checks the planned tip (gh and a prompt run in between).
  ⚠ **The #159 rollback removes only what the claim made (`rollbackFor`)**: it
  used to `git branch -D` a branch the claim had merely re-attached, never-pushed
  work and all. Now that branch, or a worktree claim was handed back, gets the
  placeholder undone (`gitx.UndoCommit`) and stays.
  ⚠ **Edge cases closed in review (#198):** `isValidWorktree` requires the dir to
  be its work tree's TOP (a leftover dir under a worktree_root inside the repo is
  not a worktree), and new/claim refuse an existing worktree that is off the
  branch, never printing `git -C <dir>` advice for one. `DecideNew` refuses a
  branch checked out in another worktree for EVERY relation, before claim's
  assign. A failed fetch asks `ls-remote`: a branch gone from origin that was
  pushed under its name (`PushedUnderItsName`) was deleted, not offline (new
  warns, claim refuses). The placeholder is `commit --allow-empty --only`, so
  staged work stays staged and a mid-merge claim fails instead of concluding the
  merge; `UndoCommit` resets only an empty single-parent commit. Any failure
  after the assign unassigns, but only an assignment this claim made.
- **`merge-pr` auto-cleans only a PR that reads MERGED afterwards (#185).** `gh
  pr merge` exits 0 WITHOUT merging for `--help`/`-h`, `--auto` (armed),
  `--disable-auto`, a merge queue (queued) and `-R` (another repo's PR). Taking
  that 0 as a merge removed the lane and `git branch -D`'d unpushed commits.
  `merge.ConfirmMerged` re-reads the state (2.5s at most, for API lag); anything
  but MERGED (OPEN, CLOSED, no answer) keeps the worktree, branch and claim for
  `wt clean` to reap once the PR ships. ⚠ **The converse holds too (#196): a
  non-zero exit is not "not merged".** `-- -d` merges, then fails to delete a
  local branch a wt worktree has checked out, and exits 1, so the close verify
  and the auto-clean were skipped for a merged PR. `merge.Run` wraps gh's own
  failure in `ErrMergeCommand` (a guard's refusal is not), and only that error
  reads the state (`mergedDespiteFailure`, the same `ConfirmMerged`): MERGED
  goes on to verify + auto-clean, anything else exits 1. ⚠ **Even then, only a lane whose local
  tip shipped (#187):** a squash leaves the branch unmerged in git's eyes, so the
  auto-clean's `git branch -D` would drop commits made after the push.
  `merge.LocalTipVerdict` needs the tip to BE the PR's `headRefOid` or an
  ancestor of it; otherwise, or when it can't tell (no head, not fetched here),
  the worktree and branch stay and the warning counts the commits not in the PR.
- **merge-pr's checks gate never reads a failed read, or `--admin`, as green
  (#179).** It reads the head commit's statusCheckRollup (one GraphQL query,
  paged: `ghx.PRChecks`) and the base branch's required checks from REST
  `branches/{base}` (`protection.required_status_checks`, which any reader
  gets, unlike the protection endpoint) and `rules/branches/{base}`, then
  decides in pure `merge.DecideChecks`: pending, failed (CANCELLED and STALE
  included), a state wt does not know, a required check with no run on the
  head, fewer non-SKIPPED checks than `merge_min_checks` (or a value that is not
  a count), and an unreadable read of either half all block; no check that ran,
  nothing required and no floor is `none`: merge, with a note. ⚠ **Not `gh pr
  checks`:** it exits 1 with a stderr sentence for "no checks" (a failure's exit
  code), and the raw rollup keeps every re-run (cli/cli#14044: 21 runs, 9
  checks), so `latestChecks` keeps the newest per name/workflow/event by
  `databaseId` (a status per context by `createdAt`). ⚠
  `{"data":{"resource":null}}` (no such PR) and `{"resource":{}}` (an issue's
  URL) come back with exit 0; `parsePRChecks` requires the head commit, so they
  are errors, never "no checks" (#168). ⚠ A 404 or the "Upgrade to GitHub Pro"
  403 from the rules endpoint means the server has no rulesets for the repo
  (`ErrRulesetsUnavailable`): an answer, not a failed read. A rate-limit or SSO
  403 is a failed read. ⚠ **Order:** precheck → checks → deploy confirm → coord
  hold → close check → Run's guards. Before the deploy confirm, so nobody types
  "deploy" for a red PR. ⚠ **Only `--checks-ok` gets past it:** `--bypass` is
  for wt's structural guards, and `--admin` bypasses GitHub's own checks, which
  is why the gate exists. ⚠ The merge is pinned with `--match-head-commit <the
  head it read>` (`merge.WithMatchHead`, in front of the passthrough like
  `--admin`), so a push after the read fails the merge instead of shipping an
  unchecked head. ⚠ Zero checks in a repo WITH workflows still merges, with the
  note: a workflow's triggers decide whether it runs on a PR (awesome-o's one
  workflow is path-filtered, so its PRs legitimately carry none), so the
  deterministic "CI never started" signals are a required check and
  `merge_min_checks`.
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
  (the same grader as `wt check`), then RE-grades each hunk-graded entry (HIGH or
  FYI) against this window's own ranges plus the patch's actual hunks — localized
  in the current file via `locateRange` (`parseCodexPatch` → per-hunk pre-image of
  context+removed lines) and moved into base numbering by `gitx.LinesToBase` —
  with the same `regradePending` as the Claude hook (#108/#184). A disjoint patch
  to a shared file therefore stays silent. Emits `additionalContext` on overlap;
  `WT_CODEX_HOOK_BLOCK=1` upgrades a **confirmed** HIGH (a computed hunk overlap) to
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
  ⚠ **That bare check's exit code is an aggregate (#183):** one unreachable
  Enterprise host fails it for a github.com login that is fine, and outside a
  repo there is never a host to scope to. So `doctor` reads it per host
  (`AuthStatusFor` → pure `parseAuthStatus`, from the SAME memoized run; the
  active account decides a host). gh writes every host section to **stderr**
  once any account fails, so `authCheck` captures BOTH streams; stdout alone
  reads as unparseable. A timeout or unreadable output proves nothing about the
  login → "could not be verified", never "NOT authenticated". `Authed()` stays
  the exit code: it only gates gh calls that need the repo's host anyway.
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
  ⚠ **A body forwarded after `--` replaces the commit bodies in the squash (#180)**,
  so the lint judges it instead (the subject stays, and is judged as #196 below
  says). `merge.ParseForwardedBody` reads the passthrough the way gh's pflag
  does — last flag wins, a value flag eats a `-`-led next token, `--body` with
  `--body-file` is gh's own error. A file or `-F -` body is read ONCE and handed
  to gh on stdin with the flag re-pointed at `-`: `-F <(…)` is a pipe, and gh
  re-opening it after wt read it would merge an EMPTY body. That handoff is
  pinned by fake-`gh`-on-PATH tests (`ghx.MergePRSquash`, `merge.Run`), which put
  the shim alone on PATH and refuse to run unless `gh` resolves to it.
  ⚠ **wt's own gh flags go IN FRONT of the passthrough** (`--admin`, the WIP
  `--subject`: `gh pr merge N --squash [wt flags] <passthrough>`). Appended, a
  passthrough ending in a value flag (`-- --subject`) took `--admin` as its value
  and gh merged the subject "--admin" with no admin; in front, gh fails on the
  dangling flag, and an operator's own `--subject` beats the WIP strip (gh keeps
  the last one).
  ⚠ **The gate judges the squash commit GitHub will WRITE, subject included
  (#196).** The subject was never read, so a PR title "Fixes #N" on a two-commit
  PR closed #N silently, and a closing headline that a forwarded `--subject`
  and `--body` kept out still refused. `merge.ShippedSquash` (pure) models it:
  subject = a forwarded `--subject`/`-t` (`SubjectOverride`: the operator's, the
  last one, else the WIP strip's; `--subject ""` makes gh send none), else
  `squash_merge_commit_title` (PR_TITLE → title; COMMIT_OR_PR_TITLE → the one
  commit's headline, else the title); body = a forwarded body, else
  `squash_merge_commit_message` (PR_BODY / BLANK / COMMIT_MESSAGES: every
  message, but ONE commit gives its body alone unless its headline differs from
  the default subject, i.e. PR_TITLE with another title). The gate is the PR
  body + that subject + body; the verify watches the title and every commit
  message too. Measured against GitHub's own default text, GraphQL
  `viewerMergeHeadlineText`/`viewerMergeBodyText(mergeType:SQUASH)`, on 785
  merged PRs across all four setting pairs: subject, body (up to bullets,
  wrapping and gathered trailers) and close set all matched. Merge commits are
  neither counted nor listed; a headline is the WHOLE first line, while GraphQL
  `messageHeadline` cuts it at 69 chars with "…" (rest into `messageBody`), so
  `ghx.PRCommits` reads `message` + `parents{totalCount}`; gh sends no subject
  unless `--subject` is non-empty. ⚠ **The settings come from GraphQL**
  (`ghx.RepoSquashSettings`): REST `gh api repos/{owner}/{repo}` returns them as
  null to a non-admin viewer. Unreadable → **over-scan** (the title AND the one
  commit's headline; every commit message): a missed close is the #77 trap, a
  false refusal costs a `--close-ok`.
- macOS is the dev floor: bash 3.2 (no `mapfile`/`declare -A`), BSD `sed`/`stat`,
  `/var`→`/private/var` symlinks (resolve with `EvalSymlinks` before path
  compares).
