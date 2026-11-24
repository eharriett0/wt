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
- **worktree** — `wt new`/`clean`/`discard` (data-loss-critical; see below).
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
  hidden. ⚠ **A window git can't read is named, never "no changes" (#253).**
  `TouchedFiles` returns git status's failure (a branch holding a missing
  object, an unreadable index; an `index.lock` does NOT fail it, measured on
  git 2.39), `Scan` sets `Window.Unread`, and every surface names it through
  `collide.UnreadWindows`/`UnreadNote`: `wt status` (`UNREAD`, `unread` in
  JSON and MCP), `wt check` text/`--blocking`/JSON/MCP (`unread_windows`, the
  clear qualified by `clearScope`), both edit hooks (`withUnreadNote`), the
  per-turn context, pre-push (once a push) and pre-commit. Being unread NEVER
  blocks (decided in #253): no path can be tied to it, so blocking would stop
  every edit in every window until the worktree is repaired. A path the
  window COMMITTED is still graded when git runs there (a broken branch or
  index), and its unreadable working copy makes that grade indeterminate, so
  HIGH (the fail-safe; never hidden); its tag reads `working tree unread`
  (`WindowLiveness.Unread`), not `uncommitted edits`, and status explains the
  indeterminate overlap by it (`indeterminateDetail`) (#254 review). When git
  can't run in the directory at all (`gitx.ErrNoDir`), none of its edits are
  compared, committed or not, and `collide.UnreadScope` says so. The #93 unknown-path refusal names unread windows too: one
  may be creating that path. The committed diff
  failing stays silent: it fails for an unborn or unrelated branch too. An
  entry with no work tree is NOT unread: one git lists as prunable whose
  directory is really gone (`WorktreeRef.Prunable` plus a stat in `Scan`:
  git also says prunable for a directory it can't enter) and a bare repository
  (`WorktreeRef.Bare`; `gitx.statusError` asks `--git-dir`, since a directory
  merely INSIDE a bare repo walks up into it); naming either repeated a false
  warning on every agent turn (measured in review). A LOCKED worktree whose directory is gone is
  unread: its edits sit on an unmounted volume. A `--separate-git-dir`
  repo's main entry is its git dir (`gitx.ErrGitDirOnly`), and a submodule
  with linked worktrees lists its checkout as `.git/modules/<name>`: `Scan`
  reads such an entry by its work tree from inside it (`readOwnEntry`, keyed
  on `gitx.GitDir`), and other windows see it as listed. Worktree paths are
  taken verbatim from git, and so are `RepoRoot` and `WorktreeToplevel`
  (`runPath`): trimming a path that ends in a space read a directory that
  doesn't exist, and a window there then graded its own edit. When git never ran,
  the reason is the directory's own error, not Go's failed exec of git.
  Pinned by `TestUnreadWindow_namedEverywhere` (every surface, with a healthy
  control), `TestScan_worktreeStates`, `TestWorktreeList_PrunableLockedVerbatim`
  and `TestTouchedFiles_UnreadIsAnError`.
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
- **Two windows' edits are graded by git's 3-way rule, which needs each edit's
  SHAPE (#199).** git merges two edits as ONE conflict region unless an
  unchanged line separates them, so windows editing lines 10 and 11 conflict,
  yet every grader called them disjoint (line OVERLAP): 32 of 170 real
  conflicts in an 800-scenario fuzz vs `git merge-tree`, 24 with both windows
  current. All of them (`check`, status, banner, pre-push/pre-commit,
  `regradePending`) go through `collide.ConflictSeverity`/`ConflictSpans`, which
  compare edits with `LineRange.Conflicts` (= `hunksConflict` on their old
  sides): changes conflict when they overlap or touch; an insertion only with a
  change that includes a line on either side of its gap, or another insertion at
  that gap. ⚠ **A span can't say that**: it encodes an insertion as both
  neighbours of its gap, so a touch rule over spans claims a line too many next
  to every insertion, and plain overlap flagged insertions at neighbouring gaps.
  So a base-frame `LineRange` carries `Gap` (an insertion between `Start` and
  `End`=`Start+1`): `parseHunkRangesOld` sets it; `mapHunk` keeps an
  unconflicted behind-branch hunk's shape and makes a conflicting one claim
  exactly its surviving lines plus base's replacement (a base deletion is its
  deletion point, a Gap, never its neighbours as lines). NEW-frame ranges never
  set Gap (a span there only errs toward flagging); `Overlaps` stays the span
  intersection, for sections. A touching pair's display span is the seam's two
  lines (`overlap L10-11`).
  ⚠ **The exact gap is only right if the diff puts an edit WHERE git's merge
  does (#199 review).** An inserted `}` beside an equal one, a blank line, a
  rewrite among repeated lines can sit in more than one gap, and a plain `git
  diff` (myers, the indent heuristic, the user's `diff.algorithm`) picks another
  gap than merge-ort (histogram, no indent heuristic). Measured that way, two
  insertions graded a gap apart (clean) were one gap in git and conflicted, and
  check, status and pre-push all let them through where main's looser spans had
  blocked. So every range-measuring diff passes `gitx.gradeDiffFlags`
  (`--diff-algorithm=histogram --no-indent-heuristic --inter-hunk-context=0
  --no-color --no-ext-diff --no-textconv`, after the revs, so a failure-injection
  matcher still finds the first rev at `args[2]`), and `scopedEnv` strips
  `GIT_DIFF_OPTS` (it beats `-U0`). A flag beats config, `GIT_CONFIG_PARAMETERS`
  included, so that one stays (a caller's `git -c` may matter to other calls).
  The truth assumed is merge-ort's (the default since git 2.34; `merge-tree
  --write-tree`, rebase, the forge's merge button); a `-s recursive` merge aligns
  like myers. Pinned by `TestChangedRanges_MatchMergeTree` (775 single-edit pairs
  on a slider-heavy file vs `git merge-tree`) and `TestGrade_FollowsMergeAlignment`
  (check, status and pre-push from both sides); `TestLineRangeConflicts_MatchesGitMergeFile`
  (unique lines vs `git merge-file`) pins only the rule.
- **The pre-edit hooks grade what `wt check` will say once the edit is made, by
  construction (#108/#184/#199).** A hook predicts the FILE the edit will produce
  and measures it with `wt check`'s own diffs and base mapping
  (`gitx.ChangedRangesWith`: the copy goes in as a blob, through the file's clean
  filters with `hash-object --path`, into a throwaway object directory, so the
  repo's objects are only read). `regradePending`: every hunk-graded entry (HIGH
  *or* FYI: a window whose earlier edits were disjoint can be about to overlap)
  is HIGH iff those ranges conflict with the other window's
  (`collide.ConflictSeverity`); an edit that puts the file back to base's copy
  reads HIGH like `wt check`'s empty side, as a heads-up, not a confirmed
  overlap. A STRUCTURED shared doc is graded by section, not hunk (#22), so
  `regradeSections` re-grades its entries first (#226): the same
  `SharedSections` call `gradeEntry` makes, with this window's copy as the edit
  leaves it (`collide.PendingDoc`) and its ranges in that copy's OWN line
  numbers (`gitx.ChangedDocEditsWith`; the base frame put an edit past an
  insert in the wrong section). Advisory or section-HIGH: HIGH, naming the
  section, iff the copy now shares one with the other window, else dropped.
  Before #226 an advisory was never re-graded, so an edit moving INTO the other
  window's section was silent while `wt check` then said HIGH, and one moving
  OUT kept a heads-up (a deny under block). `ChangedRangesNewWith` IS
  `ChangedRangesNew` once the copy is written, fallbacks included (no base:
  one diff from HEAD, #247, or the uncommitted ranges on an unborn HEAD; a
  side without the file is empty and an untracked
  copy is not the working tree's, as `git diff <rev> -- file` reads them; a
  failed diff: the whole file); each fallback it lacked read as
  "unmeasurable" (#226 review). ⚠ A copy the hook can't predict or put in as
  a blob may still read HIGH in `wt check`, so every section-graded entry
  stays a file-level heads-up, an advisory included; only an ungradable doc
  (a bad delimiter, where `wt check` is the blanket advisory too) drops one.
  ⚠ **Deleted lines count too (#237).** New-frame ranges can't show a
  deletion (a deleted line has no line in the window's copy), so deleting
  another window's section, or emptying or deleting the doc, read disjoint
  in `wt check`, pre-push and both hooks while git conflicts. Every section
  grade measures `gitx.DocEdits` (`ChangedDocEdits`; `ChangedDocEditsWith`
  for a pending copy, the same measurement once written): the one diff's new
  side as before, plus the lines it removed or replaced, numbered in the copy
  it diffed from (the merge base's for a behind branch, else base's), read as
  stored (`cat-file blob`) only when something was deleted.
  `collide.SharedSectionsAcrossWith` counts both: a window's sections are the
  ones its edits touch in its own copy AND the ones its old-frame claims
  touch in that old copy (`collide.oldClaims`): every line it deleted or
  replaced; for a changed HEADING line, the line above it too (git conflicts
  a rename of "## Beta" with an edit of the last Alpha line); and for an
  insertion (`DocEdits.Gaps`), the line it lands after (the first line, at
  the top). ⚠ #237 review: a new section's own lines have no section in the
  old copy, so a window whose only edit was a new section claimed nothing
  there, and deleting the section it was added to, or emptying or deleting
  the doc, read disjoint (and so did two sections appended at the end).
  ⚠ Not the line AFTER an insertion's gap, on purpose: appending a section
  at the end of your lane would then claim the next lane, the case
  structured docs exist for; an edit of the next heading line itself is
  caught from its side by the heading rule. Measured on the review's
  enumeration (124 single edits of a 3-section doc, 7,750 pairs vs `git
  merge-tree`): 0 conflicts missed (438 before), 1,862 of 4,565 clean pairs
  over-reported (1,400 before; claiming both neighbours, 2,288); round 2
  widened it (two sections or plain lines inserted at once, a line replaced
  by a heading, moves) over six doc shapes (a first-line heading, blank lines
  between sections, a long section, duplicate headings, no headings): 0
  misses for windows up to date with base. ⚠ Accepted over-report: a section
  appended at the very END claims the last section (the line it lands
  after), so appending a new lane at the bottom is HIGH against any body edit
  in the last lane while git merges it; without that claim, deleting the last
  section against the append (S3) read disjoint. A window whose doc is gone
  counts its deletions; a doc the measured revision LACKS is added whole and
  is `DocEdits.All` (git conflicts two windows' adds of one path; deliberate
  in both measurements, `docEditsFrom`'s oldMissing: the pending one names
  that side by the empty blob, which `cat-file` finds in any repo holding an
  empty file, and the hook disagreed with `wt check`, #244 review); a
  measurement that failed (the diff, or the old copy unreadable) is All too:
  every section, never fewer. Heading text
  stays the identity (by position, a deletion renumbers a duplicate heading
  in one window's copy and a conflict goes missing), so a rename counts the
  old heading's section and deleting one "## Notes" counts every "## Notes":
  coarser than git, on purpose. ⚠ A pure deletion's new-frame gap still
  claims the lines on both sides, so deleting a section's LAST lines counts
  the next section too (git conflicts with an edit of that next heading line;
  the heading rule now catches that from its side as well, and dropping the
  line-after claim measured 0 misses and 3-14% fewer over-reports in #244's
  review: left as is, a possible refinement). The hooks' stopgap
  (`headingGone`) is gone. ⚠ **A window BEHIND base is read in base's copy
  too (#247).** Its own copy and its merge base's still carry the headings
  it forked with, so where base RENAMED, ADDED or REMOVED a heading since,
  its edits counted under the merge base's sections and a current window's
  edits of the same lines under base's: "disjoint" while git conflicts
  (#244's review measured 76 and 197 misses in a behind-window enumeration
  for an added and a removed heading). `ChangedDocEdits` (and the pending
  `ChangedDocEditsWith`) add `DocEdits.Base`, the window's own hunks moved
  into base's numbering exactly as `ChangedRanges` moves them
  (`withBaseClaims`: `mapHunksToBase` over base's diff since the merge
  base), and `BaseCopy`; `collide.baseClaims` reads their sections in base's
  copy by `oldClaims`' rules (a change its lines, plus the line above when
  it starts at a heading; an insertion the line it lands after), and they
  are added to the rest. Base without the doc adds nothing (read from its
  tree, `ls-tree` with a literal pathspec: `cat-file` can't tell a missing
  path from a failure, and a plain `:PLAN.md` is pathspec magic); a
  listing, copy or diff of base's that can't be read is All (#252 review:
  an unreadable copy read as "no such doc", the old miss), and so is a
  base entry that is no regular file (symlink, directory, submodule).
  ⚠ **No base (#247):** the section grade reads ONE diff
  from HEAD, both sides, so a base-less repo's uncommitted deletions and
  gaps count too (its old fallback, the uncommitted ranges' new side, still
  reads an unborn HEAD, `unbornHEAD`: HEAD names a branch, `symbolic-ref`
  succeeds, that doesn't exist, `rev-parse --verify HEAD` exits 1. A
  branch holding a missing object, a tree or garbage, or a bad symref,
  also fails `HEAD^{commit}` with exit 1; those and any other failure are
  All). Committed changes there have no frame to compare,
  and `TouchedFiles` doesn't list them, so `wt check` and pre-push warn
  that no base resolves (`collide.NoBaseNote`; `base_unresolved` in
  `--json` and MCP `wt_check`) rather than a bare clear (#252 review:
  pre-push, which ships the commits, passed a real conflict silently).
  Pinned by `TestSectionDeletions_MatchMergeTree` (deleting a section,
  emptying or deleting the doc, a heading deleted or renamed, duplicate
  headings, the two-step history, a behind branch, a new section against
  each deletion, two sections appended at the end, the lanes case that
  stays advisory, and #247's R1, R2, SPLIT and MERGE with a control: each
  held against `git merge-tree` in `wt check` and pre-push from both
  windows), `TestSectionGrade_NoBase` (B2 HIGH; B1 said by check, JSON,
  MCP and pre-push), `TestOldClaims`,
  `TestBaseClaims`, `TestPreEditHooks_StructuredDocDeletionsGradeHIGH`,
  `TestSharedSectionsAcross_CountsDeletions`,
  `TestSharedSectionsAcross_BehindCountsBasesSections` and
  `TestChangedDocEdits*`.
  A delimiter that doesn't compile leaves the entries as they were (`wt
  check` can't section-grade that doc either). The empty side of a
  measurement is hashed `--no-filters` (git's /dev/null never sees a clean
  filter; one adding a header moved every range of a file measured against it:
  one new since the measured revision, or an untracked copy). Pinned by
  `TestPreEditHooks_StructuredDocSections` (into, staying
  out of, past an insert into, and out of the other window's section, each held
  to `wt check` after the edit, both hooks, both block modes) and the
  `StructuredDoc*` cases beside it. Claude (`claudePendingContent`): Claude Code's Edit replayed (refused
  for an absent `old_string`, or a repeated one without `replace_all`; an empty
  `new_string` takes the newline after it), MultiEdit's edits in turn, Write's
  content; a CRLF file is edited as LF (the edit's own CRLFs too) and written
  back CRLF (measured with Claude Code's own tools). Codex (`codex_apply.go`): apply_patch replayed, held
  to the app's bundled CLI by `TestApplyPatchOps_MatchesCodex`: each chunk sought
  from where the last ended, fuzzy whitespace and punctuation tiers, context
  lines written as the patch spells them, `@@ <line>` and End-of-File anchors, a
  context-less chunk appended, and ⚠ every file it writes ends with a newline, so
  a file without one also changes its last line. ⚠ Don't go back to guessing the
  changed lines from the hunk (the `old_string`'s lines, the union of every gap a
  slide could take): a diff can place an inserted or deleted line in several
  gaps, a guess misses where git puts it, and the union denied edits `wt check`
  grades low. Only when the file can't be predicted (an edit the tool refuses, a
  file mixing line endings, a patch apply_patch rejects, a new file, a git error)
  does an entry stay as a file-level heads-up, and then it is never dropped.
  ⚠ **A hook reads a file only through `readForPrediction`** (#201 re-review):
  opened non-blocking and read only if the OPENED file is regular.
  `os.ReadFile` on a FIFO waited for a writer forever (a patch moving one in,
  or any FIFO in the repo, hung both edit hooks, on main too) and on /dev/zero
  read without end; a hook must always return promptly. Anything not read is a
  file-level heads-up. ⚠ No size cap: a regular file ends, and an 8 MiB cap
  (tried in review) cost an 11.7 MB tracked file its grade: a lost Codex deny
  and a false Claude deny for a disjoint edit.
- **Coordination ownership is ONE decision, and only a real session token
  PROVES a record is yours (#163, #211).** Two agent sessions started in one
  checkout resolve to the same window id, so each treated the other's
  announcements as its own (#163). `coord.Self.Attribute` classifies every
  record: **mine** (self's real token, or its `MirrorSession` hash: proven),
  **directory fallback** (a token-less reader and a token-less record from the
  same window: counted as yours, NOT proven), **another session** (a different
  session: tagged vs another token, or tagged vs token-less in either direction),
  **unattributable** (no session at all: written by a wt without session
  support), or another window. `coord.View` (`ViewOf(recs, self)`) splits
  unattributable records by the log's **session epoch** (`SessionEpoch`: the
  first datable record carrying a session, ANY window of the log as read):
  before it, **pre-session** history; at or after it, or undatable, a
  **still-running older wt**. `Owns` (mine + fallback), `ProvablyWrote` (own
  token or hash, any window), `SharesCheckout`, `Unattributed`, `PreSession` and
  `HoldLimit` are views of these; don't add a raw `r.Window == self` compare.
  Per operation, always in the safe direction (#211):

  | | mine (proven) | directory fallback | another session | no session, pre-session | no session, later (older wt running) |
  |---|---|---|---|---|---|
  | `wt holds` | "yours", with all-clear lines | "yours", all-clear line + caveat | counted, not listed as yours | own section, NO all-clear lines: holds that still gate always listed, others newest 10; plain ones <24h (older counted) | same section, labelled "written by a wt without session support"; plain ones always listed |
  | `all-clear` / `announce --clear` | posts (from another checkout too: "yours, placed from checkout X") | TTY asks; non-TTY refuses without `--not-mine`, naming WT_SESSION | TTY asks; non-TTY refuses without `--not-mine` | same | same |
  | merge-pr gate | exempt (#18) | exempt (#18) | gates (hold_max_age) | gates; stale at min(hold_max_age, 24h) | gates (hold_max_age) |
  | inbox / banner / per-turn hook | hidden | hidden | shown | shown <24h (`coord.UnattributedHorizon`); older counted in `wt inbox` | shown |
  | acks (waive for self) | count | count | don't | count only for a signal older than 24h | never |
  | `wt prune-coord --legacy` | — | — | — | dropped (with -y) once nothing needs it: plain >24h, a hold past both 24h and hold_max_age (never with hold_max_age off) | kept |
  | block-id reservation: `--written`/`--abandon`, `wt holds` (#197) | marks (its own token from another checkout too); listed as yours | `--written` marks; `--abandon` asks, or needs `--not-mine`; listed with the caveat | refused, owner named; in the status banner | refused ("no session can mark it"); in the banner | same |

  Another window's records gate with hold_max_age and their acks never count, as
  before; a record carrying this session's token from another checkout still
  GATES (one session runs several lanes: subagents share their parent's id), it
  is only labelled "yours, placed from checkout X" and released without asking.
  ⚠ **`""` is NOT a wildcard (#211).** #163 kept pre-#163 records owned by every
  session of their window, so `wt holds` listed a month of them as the current
  session's own and offered `wt all-clear` beside each; one all-clear would have
  released another live session's hold. The 24h horizon and cap keep the month of
  pre-upgrade records every upgraded log carries from flooding every inbox and
  per-turn context, and from gating forever under hold_max_age=off. ⚠ **The epoch
  is the whole log's, not one window's** (#211 review): a session-less ack
  written a minute earlier, in a window no session-aware wt had written from yet,
  waived another window's live hold for everyone there. An ack is written after
  its announcement, so a session-less ack of a session-stamped hold is never
  history. A log with NO session-stamped record (the upgrade, before any
  session-aware write) reads every datable session-less record as history; a
  `View` not built by `ViewOf` reads none as history (the safe zero value). ⚠
  **Accepted trade-off: two token-less shells in one checkout are ONE party for
  the gate and the inbox** (the directory fallback #211 itself suggests): a
  terminal's own hold never blocks it and #18's WT_WINDOW pinning works for
  terminals, at the cost that two token-less agents in one checkout can't tell
  each other apart (the #163 inbox hedge still fires when tagged sessions post
  there). Releasing is where it would hurt, so `all-clear` treats the fallback as
  unproven. `WT_SESSION` per shell separates them. ⚠ **Rollout:** a window still
  on v0.1.33 (no sessions: writes `""`, exempts itself from every hold in its own
  checkout) or v0.1.34 (#163: `""` is a wildcard every session owns; all-clear
  never asks) behaves as before until upgraded. What a v0.1.33 window writes
  after the log first carried a session reads as a still-running older wt's:
  its holds gate for hold_max_age, its acks waive nothing.
  The window id itself stays path-based (#18 self-hold exemption, #156
  claims across restarts). The session is `WT_SESSION` → `CLAUDE_CODE_SESSION_ID`
  → `CODEX_SESSION_ID`, else `coord.SessionNone`. Claude Code keeps its id across
  `--resume`/`--continue`; `--fork-session`, **`/clear`** (the conversation reset
  rewrites the env var; verified in the 2.1.292 binary) and a fresh session mint a
  new one, so a hold placed before a /clear gates the session after it like
  another session's (the gate says so). Subagents share their parent's id. Codex
  exports `CODEX_SESSION_ID` (the root thread's id, openai/codex#37848) to every
  shell command, from rust-v0.148.0 on (measured with GitHub's compare: 0.148.0
  contains the merge commit, 0.147.0 doesn't). ⚠ An older Codex stamps
  `coord.SessionNone` from its shells while its hook reads the payload id, so
  the per-turn banner shows that session's own records as another session's
  (advisory only; #197 item 5). wt can't tell the Codex version, so it is
  documented (README, Codex section), with `WT_SESSION` as the workaround.
  ⚠ **Terminal ids (`TERM_SESSION_ID`/`ITERM_SESSION_ID`) are deliberately NOT
  sessions**: every tmux pane or editor started in the tab inherits them (false
  confidence). ⚠ A session with no token stamps `coord.SessionNone`, never `""`,
  so a token-less shell and a Claude session in one checkout stay two parties.
  Every writer stamps through `stampRecord` (pinned). ⚠ **The per-turn hook reads
  the log as the session the agent's own `wt` commands stamp**
  (`agentHookSession`): its inherited env first. Claude Code sets
  `CLAUDE_CODE_SESSION_ID` in hook processes too, so its payload is never
  consulted. Codex does NOT put `CODEX_SESSION_ID` in a hook's env (a hook runs
  with the Codex process's own env snapshot, `codex-rs/hooks/src/registry.rs`),
  but its payload `session_id` comes from the same `Session::session_id()`
  (`core/src/hook_runtime.rs`), so codex-context ONLY falls back to the payload
  id. ⚠ **The GitHub mirror carries `coord.MirrorSession` (a hash), never the raw
  id**, and a session owns its own hash, or #18 breaks across clones that share
  only the mirror (separate local logs). ⚠ **Hold advice:** `wt ack <id>` (waives
  the hold for the reader only) always leads; `wt all-clear <id>` releases it for
  EVERY window and is suggested only for a hold `coord.HoldLooksOrphaned` (12h old,
  or its session silent for 4h), always saying so. In a single-worktree repo the
  per-turn hook shows this checkout's non-mine entries plus every other window's
  HOLD (the gate enforces those). ⚠ **Block-id reservations are owned the same
  way (#197)**: `OwnBlockReservations`, `FindOwnReservation` and
  `RecentBlockReservations` take a `Self` and use `Self.ReservationIsMine`
  (`Owns`, or `ProvablyWrote`: its own token from another checkout, since a
  subagent in its own worktree shares its parent's id). Keyed by window, a
  second session in the checkout listed the first's reservation as its own in
  `wt holds` and could `--abandon` it, freeing an id the first was about to
  write; the next reservation then handed that id out twice. Now `--written` /
  `--abandon` refuse anything self doesn't own and say whose it is
  (`notYourReservation`: another session, with a /clear hint; no session, "no
  session can mark it"; another window). ⚠ Freeing is where a wrong owner
  hurts, so `--abandon` of a directory-fallback reservation (both token-less)
  asks on a terminal and otherwise needs `--not-mine`, as `all-clear` does;
  `--written` needs block N in the file, so it doesn't. An unwritten
  reservation still frees its id on its own after `DefaultBlockReserveTTL`, so a
  crashed session's, or an older wt's (no session), never strands. ⚠ **Only an
  OPEN reservation is a handle** (`coord.openReservation`, #197 review): a
  block-written or block-abandoned marker closes the reservation its `AckOf`
  names AND whose file and block it carries (an older wt's written marker
  without `AckOf` still closes its (file, block)), and FindOwnReservation
  skips closed ones. Keyed by (file, block), a dead reservation's owner could
  mark, and prune could drop, another session's live reservation of the reused
  number. A retried `--written` of a reservation already marked exits 0
  ("already marked", `coord.OwnClosedReservation`), and one after your own
  abandon is refused for that reason; neither advises reserving a new id for a
  block already in the file. `--not-mine` goes with `--abandon` only (exit 64
  otherwise). `wt holds` flags an own reservation past the TTL as expired (its
  id may have been handed out and written by someone else). The status banner
  shows every open reservation self doesn't `Own`, its own token's from another
  checkout included and labelled (a subagent's prepend is imminent too). The
  /clear hint appears only between two tagged sessions; a token-less shell is
  pointed at WT_SESSION. Messages say "this window": a pinned WT_WINDOW makes
  one window of several checkouts. ⚠ Rollout: an older wt (window-keyed) still
  running in the checkout can mark or abandon a new session's reservation
  until upgraded.
  `wt status --json` and MCP `wt_status` carry `session`, `other_sessions`
  (`statusSessions`: `coord.OtherSessions` within `SharedCheckoutWindow`, the
  banner's reading; doctor's field names) and `sessions_unread` (the log could
  not be read: unknown, not empty). ⚠ **It reads the block ledgers too**
  (`coord.CheckoutActivity`, #197 item 2): a reservation lives in its file's
  ledger, not the log, so a session whose only records were reservations was
  invisible to `other_sessions`, the status banner and doctor (text and
  JSON, even before the log exists). ⚠ Only THIS repository's ledger
  records count (`Repo`, the log's own scope): the ledgers are machine-wide,
  and a pinned WT_WINDOW names one window in several repositories, so
  another repo's reservation read as this checkout's (#197 review). The
  read is advisory (`coord.RecentBlockLedgers`): a ledger untouched for 24h
  is skipped, and a locked one is read without waiting (`Load` waited 5s per
  stuck ledger on every `status --json`). ⚠ **A session that never runs a
  coordination command marks its presence** (#197 item 2, step two): the
  per-turn hooks (`hookAgentContext` → `markPresence`) write a
  `coord.KindPresence` record for a session with a real token when
  `coord.PresenceDue` (nothing of its own, this window AND session, any kind,
  in the last `PresenceEvery`, an hour): at most 24 a day per window and
  session. `coord.AppendIfFree` never waits (one lock try; a held lock skips
  the turn) and never prints. In a single-worktree repo with no log, a turn
  with a token no longer returns before `config.Load`: its mark creates the
  log (the #163 case, two agents in one checkout), and later turns read the
  coordination block there as any repo with a log does. ⚠ So the hook reads
  the log with `coord.LoadForHook`, which waits a second for a writer, not
  Load's 5s (review: far more repos now have a log, and a prune stopped
  mid-rewrite stalled every prompt there; what it reads is advisory). Inbox,
  holds, acks and the gate select announcements by kind, so a presence
  record never reads as one (an older wt's included); `PruneRecords` drops
  it after `SharedCheckoutWindow`, or undatable (`presenceExpired`). Nothing
  prunes on its own, so doctor lists expired marks
  (`coord.ExpiredPresence`, `presence_expired`) apart from the `prunable`
  records it warns about: some expire every day wherever agents prompt, and
  a daily warning gets ignored (measured: 50k marks, 14 MB, cost the hook
  114 ms against 66 ms). ⚠ Under a Codex older than 0.148.0 the mark carries
  the payload id while the session's shells stamp none, so its own `wt
  status` warns that another session shares the checkout (README, Codex
  section). It does count as the session's
  activity elsewhere, on purpose: `OtherSessions`, the token-less inbox
  hedge (`InboxClearAmbiguous`) and `HoldLooksOrphaned`'s "silent for 4h"
  (a prompting session's hold is not orphaned). ⚠ **But a mark alone shows
  its session at work only for `PresenceLive`** (two `PresenceEvery`;
  `presenceStale`, round-2 review): counted for the whole day, every hooked
  session's marks told the session after its /clear (a new id) that another
  shared its checkout, for a day. A session with any other record keeps the
  day's window, and its marks still date it; the warning adds "or it was
  you, before a /clear" between two tagged sessions ("one may be you" for
  several). ⚠ Marks come only from the per-turn hooks (UserPromptSubmit),
  at most hourly, so a session drops out 1 to 2h after its last prompt (2h
  after its last mark), as in one long autonomous turn, until it prompts
  again (round-3 and round-4 reviews, accepted: main had no signal for
  it at all, and any other record keeps a session for the day). An older
  wt, and a `wt mcp` started before the upgrade, still list marks-only
  sessions for 24h. ⚠ A mark also sets the
  log's session epoch (#211 `SessionEpoch`, any kind), so a session-less
  hold written after it reads as a still-running older wt's, not history:
  the safe direction, and prune-coord's drop of the mark puts it back. A
  session with no wt hooks that never runs a coordination command is still
  invisible. Pinned by
  `TestHookPresence_marksASilentSession`, `TestPresenceDue`,
  `TestPruneRecords_presence`, `TestAppendIfFree`,
  `TestPresence_countsAsTheSessionsActivity`,
  `TestOtherSessions_presenceAloneCountsWhileLive`,
  `TestHookPresence_neverStallsOnAHeldLog`,
  `TestDoctor_listsExpiredPresenceWithoutWarning`, and
  `TestOtherSessions_countBlockReservations`, `TestCheckoutActivity` and
  `TestRecentBlockLedgers`.
  ⚠ `wt mcp` reads its session from its env ONCE, at spawn: after
  `/clear` its `other_sessions` counts the new session's records as another
  session's until the client restarts it, and whether a client passes its
  session id to a stdio MCP server at all is not verified. The payload's
  `session` says which one it answered as.
- **wt detects, Claude talks: a live Claude Code session is named, never
  presumed gone (#250).** Claude Code's cross-session messaging (2.1.224+)
  lets an agent pick and message another session itself (current versions
  show it each session's working directory), so wt sends nothing; what an
  agent can't see is a collision, or which of two sessions in one checkout
  placed a hold. So wt names the session to talk to where it reports one:
  (1) **a collision** (`wt check` text, both edit hooks advisory and deny,
  the per-turn banner): the live sessions working in the other window's
  worktree (`CheckEntry.liveNote` via `annotateLive`, or the banner's
  label map `labelSessionNotes`; `worktreeSessionNotes` reads the listing
  and this repository's log (`coord.LoadForHook`) once, on first use).
  ⚠ **A session is credited by its records** in the log from the last
  `SharedCheckoutWindow` (`sessionsByWindow`): the window of its latest
  record from a wt COMMAND (announce, ack, all-clear, a block id: run where
  the agent works), else of its latest presence mark. ⚠ #251 round 2: a
  presence mark follows the per-turn hook's payload cwd, which is where the
  session was started unless it cd'd, so a session started in a main
  checkout and working in a worktree by path marked main hourly, and
  "latest record" credited it there. A session started outside any
  repository writes no mark at all (the hook returns before it), so it is
  named only through its wt commands' records. ⚠ Accepted (rounds 3-5):
  a session that moved after its last wt command stays credited to that
  command's window until its next one, for up to a day; at worst a wrong
  name, the collision still shows. Two rules that let a later mark win
  were tried and dropped: marks are hourly per window, and `PresenceDue`
  skips one where the session wrote anything in the last hour. "Any mark
  in the command's window that day" sent a session started in /r/b that
  cd'd to main and works in /r/b by path to main hourly; "its last mark
  before the command was there" sent one that cd'd to main, ran `wt ack`
  there (its main mark skipped), then announced in /r/b by path, to main.
  A later mark can't be told from by-path work elsewhere, or from a mark
  skipped for an hour. A shorter horizon for records would credit path
  workers to main again. **Never by the listing's
  `cwd`** either: that is where the session was STARTED. Measured on the
  owner's machine: 5 of 6 live sessions started in a parent folder outside
  every repository and work in worktrees by path, so the first version
  (deepest worktree holding the cwd) named nobody, or the wrong window. A
  session with no recent record here is named on no collision, self never;
  `realPath` resolves a window's deepest existing ancestor, so a window id
  and a worktree path compare however they were spelled. Checked against
  the real listing and log in another repository: a real collision named
  the session whose latest record is in that worktree, and a window with no
  recent live session stayed unnamed. Known cost: with another process
  holding the log's lock, the per-turn hook waits for it twice (its own
  coordination block, then the note), ~1s more. (2) **an
  announcement or hold** (`postedByLive`: inbox, the per-turn hook through
  `labelForContext`, merge-pr's hold gate and its stale-hold warning, the
  status banner), by the record's raw session id ("live as `api-worker`,
  idle: ask it with SendMessage"): the one case a directory can't settle,
  two sessions in one checkout; (3) **`other_sessions`** (`coord.NameLive`:
  `status --json`, doctor, MCP; the `wt status` banner too, round 4)
  carries `name`/`status`, and `SharedCheckoutWarning` names a live latest
  session as live `name` in place of its id, cut to what line 1's 160 runes
  leave (8 to 24; round 4: a token-less self and a 24-rune name ran it to
  181); its /clear hint needs a tagged session not listed live (round 3:
  an older one keeps it when the latest is live). An unnamed latest with a
  token-less self can still reach 163, or 170 with a mirrored id (round 6;
  not new). The push
  phase first planned (wt posting into sessions' inbox sockets) was
  dropped: it duplicated `SendMessage`, woke idle sessions, and relied on
  an undocumented line format. `internal/peers` reads `claude agents
  --json`, which Claude Code documents as the supported way to read
  session state from outside (`peers.Live`: `claude` on PATH, else
  `CLAUDE_CODE_EXECPATH`, the binary running the session, since the VS Code
  extension puts none on PATH; `Timeout` 2s; memoized 10s, so a command reads
  it once; an entry counts only with a `pid`, which the listing gives "while
  the process is alive"). ⚠ **Annotation only.** A session missing from the
  listing is never treated as gone (the agent-view docs say interactive
  sessions in other terminals appear only once backgrounded; 2.1.322 lists
  them), and a listing wt can't read (no binary, an error, the timeout, not
  the documented array) is unknown and changes nothing. The one decision it
  changes is in the safe direction: `holdAdvice` never offers a global `wt
  all-clear` for a hold whose session is listed live (`otherPeer`), and says
  to ask it; the stale-hold warning likewise. Only another session or window
  is looked up (`otherPeer`: never a record this session provably wrote, never
  `SessionNone`). `livePeerOf` runs the listing only when a record is shown,
  so a turn with nothing of another party's runs no `claude` (measured: a
  turn showing one costs ~195 ms against ~58 ms; a listing is 135-297 ms).
  ⚠ #251 review: names go into another agent's context, so `peers.Parse`
  drops control characters and backticks and caps a name at 64 runes; the
  read is capped at 1 MiB (`maxListing`) and its whole process group goes at
  the deadline (`gitx.StopGroupOnCancel`); this session is excluded by its wt
  token AND by `CLAUDE_CODE_SESSION_ID`, the id the listing keys it by (with
  `WT_SESSION` set it was named on its own old window). `WT_PEERS=0`
  disables it, and the cli and doctor packages' `TestMain`s set it, since a
  test run inside a Claude Code session would otherwise reach the real binary
  through `CLAUDE_CODE_EXECPATH`; a test that wants live sessions sets
  `livePeers`.
  Pinned by `TestCollisions_nameTheLiveSessionInTheOtherWindow` (real git,
  a fake `claude`: check, both edit hooks in both modes and the per-turn
  hook name the other window's session and never self or an outside one;
  read once a command; never for a file no window contests; nothing under
  `WT_PEERS=0`; the listing's cwds point elsewhere, and a session started
  inside wb is not named), `TestSessionsByWindow`,
  `TestSessionsByWindow_recordWins` (rounds 3-5), `TestLabelSessionNotes`,
  `TestCollisionNote`, `TestHoldAdvice_liveHolderIsAskedNotCleared`,
  `TestPostedByLive`, `TestWarnStaleHolds_liveHolderIsAsked`,
  `TestLiveHolderNamed_hookAndGate` (a fake `claude`: named in the hook, the
  gate's listing and advice, inbox and the banner; read once, never for a
  turn with only its own records) and
  `internal/peers`' tests (every failure is unknown; the timeout; the
  execpath fallback).
- **Every write to a coordination log or block ledger holds its flock (#229).**
  `wt prune-coord` (`withLockedLog`) reads, truncates and rewrites the file
  under an exclusive flock, and `coord.Append` used to write with a bare
  `O_APPEND`, so a record appended in between (a hold included) was deleted by
  the truncate or overwritten by the rewrite. `Append` now takes the same lock
  for its write. ⚠ flock locks are per open file, so a caller that already
  holds the lock through another descriptor must not call `Append` (it would
  wait on itself): `ReserveBlock` writes through its locked handle with
  `appendLocked`. Both handles are `O_APPEND`, so a writer that doesn't lock
  (an older wt mid-rollout, a no-lock build) can't be overwritten by these
  writes; a prune can still drop its record, which is #229 itself.
  ⚠ **Readers take the SHARED lock (#234):** unlocked, a `Load` between the
  prune's truncate and its rewrite read an empty log, so merge-pr's hold gate
  (and inbox, holds, the hooks, MCP, doctor) missed a hold that never left the
  file. `Load` waits while a prune or an append holds the log, reads the bytes
  under the lock and parses after releasing it; the two callers that hold the
  exclusive lock read through their OWN handle (`readHeld`), so they can't
  wait on themselves. ⚠ **The wait is bounded** (`loadLockWait`, 5s; #234
  review: a prune stopped with Ctrl-Z blocked every reader, the per-turn hooks
  included): then `Load` reads unlocked, as before #234, and `LoadForGate`
  returns `ErrLogLocked`, which merge-pr's hold gate refuses on (an unlocked
  read could be a log stopped between its truncate and its rewrite). A lock
  that fails (no flock) reads unlocked, as before. ⚠ The refusal says to
  RESUME the holder (`kill -CONT`), never to kill it: a prune killed after its
  truncate leaves the log empty for good. ⚠ **So is the writers' wait
  (#235):** every writer (`Append`, `ReserveBlock`, `withLockedLog`) locks
  through `lockForWrite` (`lock.ExclusiveWithin`, polling `LOCK_EX|LOCK_NB`):
  after `writeLockNotice` (1s) it prints "waiting for the lock on <path>"
  once (not sooner: another append holds it for microseconds), and at
  `writeLockWait` (30s) it fails with an error naming the log and the same
  resume advice. Before, a stopped prune hung every announce and ack
  silently, an agent's tool call until its own timeout. ⚠ **A filesystem
  that can't lock at all** (`lock.Unsupported`: ENOTSUP, EOPNOTSUPP, ENOSYS;
  macOS NFS `nolocks`, some FUSE/SMB mounts) made `Append` refuse after
  #229, which stopped announce, ack, all-clear and block-id
  `--written`/`--abandon`. `Append` alone writes unlocked there, `O_APPEND`
  as before #229, warning once per process: every locker ON THIS MACHINE
  fails the same way there, so no prune here can rewrite under it (machines
  sharing `~/.wt` where only some can lock were never protected).
  `ReserveBlock` (an id needs the lock across its read and write) and
  `withLockedLog` (a rewrite without it is #229) still fail. ⚠ **ENOLCK is
  NOT Unsupported** (#235 review; the owner chose failing closed): "no locks
  available" is a lock service failing (NFS lockd unreachable) on a mount
  that does lock, and it can start while a prune holding the log is stopped.
  Read as "can't lock", a retry wrote unlocked into the prune's rewrite and
  lost a hold (measured with a stubbed flock). So announce and ack fail
  until lockd is back, as on main before #235. ⚠ **Nor is any failure after
  a try found the file held**: `within` returns it as `lock.ErrSeenHeld`.
  EINTR (a signal during an NFS lock call) is retried, never read as a
  holder. `LoadForGate` refuses on every lock error
  but Unsupported, not only on the timeout: a holder (`ErrLogLocked`, the
  resume advice) or any other failure (`ErrLogUnlockable`, EIO say: retry).
  prune-coord prunes past a locked block ledger and lists the locked ones,
  one a line, with the advice once (`PruneBlockLedgers` returns them, and
  the ledgers share ONE writers' deadline: each waiting its own cost 30s per
  held ledger); it used to stop at the first and drop the error and the
  count. `wt append`'s doc lock is bounded the same way (`appendLockWait`)
  and still fails on a filesystem that can't lock. ⚠ Polling `LOCK_NB` gives
  a writer no place in a queue: a near-continuous stream of readers (two in a
  tight loop) can hold it off to the deadline, where a blocking `LOCK_EX`
  mostly got in (measured on macOS; at per-turn hook rates neither waits).
  Not covered: a prune longer than the readers' wait (measured 8.9s for a
  1M-record log on Linux; a real log is ~10k) lets a non-gate reader read
  unlocked mid-rewrite.
  Pinned by
  `TestAppend_waitsForAPruneRewrite` (a prune paused between its read and its
  rewrite; `lockedLogPause` is the seam),
  `TestLoad_waitsForAPruneRewrite` (paused right after the truncate,
  `lockedLogTruncated`), `TestLockHolders_readWithoutSelfDeadlock`,
  `TestLoad_givesUpOnAStuckHolder`,
  `TestMergeCoordGate_RefusesALogLockedByAnotherProcess`,
  `TestReserveBlock_noSelfDeadlockAndAppendsLand`, and for #235
  `TestAppend_givesUpOnAStuckHolder`,
  `TestAppend_writesUnlockedWhereTheFilesystemCantLock` (`exclusiveWithin`
  is the seam), `TestLockedWriters_sameDeadlineAndNoUnlockedFallback`,
  `TestExclusiveWithin_unsupportedAfterHeldFailsClosed` (`lock.flock` is the
  seam), `TestLoadForGate_refusesOnLockErrorsButUnsupported`,
  `TestPruneBlockLedgers_prunesPastAHeldLedger`,
  `TestPruneBlockLedgers_reportsEveryFailure`,
  `TestPruneBlockLedgers_waitsWithinTheSharedDeadline`, `TestWriteLockDefaults`,
  `TestExclusiveWithin_retriesEINTR`, `TestAppend_givesUpOnAStuckHolder`
  and `TestAppend_lockDefaultsAndNoFallback` (cli) and
  `TestPruneCoord_reportsALedgerItCouldntPrune`. Off unix the lock is a
  no-op, as before.
- **`ClassifyFacts` precedence:** open PR > merged PR > closed PR > dirty >
  unmerged > merged-by-ancestry. PR state outranks a dirty index (a
  merged/closed branch with leftover staged cruft is stale, not a permanent
  HIGH — #79), except a MERGED PR found only by the tip commit (#168, below).
- **The hook block predicate MUST equal `wt check`.** Advisory means advisory in
  both; disjoint hunks (an unchanged line between) don't block in either, and
  touching ones block in both (#199). `pushCollisionBlocks` mirrors
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
  repo-relative, and the server may run in a subdirectory); then every one, not
  only an absolute one (#201, below), goes through `resolveExisting`, which
  handles `/var`→`/private/var` even for a file that doesn't exist yet. ⚠ So the
  #92 equality holds for every path that names something; for a name that
  names nothing yet (not on disk, not
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
  ⚠ **An argument names the path the filesystem opens, in whichever worktree
  holds it (#201).** Only an absolute argument used to be symlink-resolved, so
  `wt check svclink/README.md` (svclink → pkg/svc) asked about a path no window
  can touch (git never tracks a path beyond a symlink) and read clear while
  `$PWD/svclink/README.md` was HIGH. Every argument and hook path is now joined
  onto the directory it is read from WITHOUT cleaning (`joinRaw`) and read by
  `resolvePath` as the OS reads it: each symlink replaced where it occurs, a
  `..` stepping up from where the path has got to (`svclink/../x` is pkg/x, not
  x), the first missing component and the rest kept as given. `placePath` then
  places it in the worktree that holds it (`locateIn`: the DEEPEST of the
  repository's worktrees, Scan's `git worktree list`, since one can sit inside
  another, whether it matched as spelled or, confirmed by `os.SameFile`, equal
  but for case; stopping at the first as-spelled match let the main checkout
  claim `.wt/feat/x` from a nested worktree `.wt/Feat`, #201 review). A bare
  entry (`git clone --bare` with worktrees inside it) holds no repo paths and is
  never a root (`gitx.BareWorktreesIn`). So a path into ANOTHER worktree is that
  repo path (they share the layout), for `wt check` and wt_check; the edit
  hooks, given only their own root, stay silent about a file in another
  window's tree, as before. A path no worktree holds (`/tmp/x`, `..` at the
  top, `../HEAD` beside a worktree inside a bare repo, and ⚠ any path THROUGH an
  in-repo symlink that leads out of the repository, read lexically as a repo
  path before #201) is refused (`outsideCheckPaths`: exit 64 that `--allow-missing`
  can't override; wt_check: isError), never a search that matches nothing and
  reads clear. ⚠ **The Codex hook replays apply_patch over outside paths too**
  (`predictCodexPatch`, #201 reviews): an outside path is keyed by where it
  resolves (`patchKey`: `outsideKey`, a NUL no repo path holds, then the
  resolved absolute path) and only repo paths are graded. Read lexically before
  #201, a path through an in-repo link (wt's own `link_files` `.env` is a link
  into the main checkout) was a repo path; once it resolved outside, giving up
  on the whole patch cost the deny for a repo file beside it, giving up on a
  move across the edge only warned, and reading a moved-in source as it lay on
  disk ignored what the same patch had done to it (an outside file added,
  moved in, moved again; one rewritten, then moved in), so a real HIGH read
  SILENT. Now an outside Add/Update/Delete is tracked, a move out of the repo
  removes its source (a modify/delete with a window editing it), and a move in
  writes what the patch itself made of the source. ⚠ **What can't be read is
  unknown, and unknown is carried** (`applyPatchOpsKnown`, the same replay
  `applyPatchOps` is, plus a `fileUnread` state): an Update or move of an
  unread file leaves its result, and anything later made from it, unpredicted
  (a file-level heads-up, never dropped, never a deny); a missing file still
  fails the patch, as apply_patch does.
  ⚠ **A symlink FILE is two paths.** `wt check link.md` asks about the link
  itself (`resolvePath(p, false)`, the entry: another window can retarget it)
  AND its target (whose content an edit through it changes), so it never reads
  clear for either. The edit hooks ask about the one the tool changes: the
  target for a write (Claude's Edit/Write, apply_patch Update/Add, which write
  through a link), the link for apply_patch's Delete and a move's source
  (`repoRelativeEntry`, `codexPatchFile.removed`: apply_patch unlinks the link).
  For a symlink argument `wt check` is therefore a superset of the hooks, the
  same safe direction as the fuzzy search above.
  ⚠ **`.` at a worktree's top level is the whole repository** (`./`, `$PWD`,
  `..` from a subdirectory, another worktree's top): `collide.RootQuery`, Path
  `.`, exact only, expands to every other window's touched file as any directory
  does (#154). It used to match nothing and read clear while live HIGHs existed;
  `matchTouchedDir` still answers nothing for a blank path, so the zero Query
  can't become the whole repo, and `unknownCheckPaths` never calls it a typo.
  ⚠ **Case is folded only where names are case-insensitive** (the filesystem
  folds case AT THE WORKTREE, probed on its own `.git` vs `.GIT` with
  `os.SameFile` (`foldsCase`/`caseProbe`), or `core.ignorecase`). ⚠ #201
  review: the first probe walked up to the nearest ancestor with a letter in its
  name, which for a case-sensitive volume mounted at a letterless path (`/2024/1`)
  is the PARENT filesystem: it read "folds", a new readme.md was checked as the
  tracked README.md, and an add/add conflict (another window created readme.md)
  read clear in `wt check` and both hooks. There `placePath` spells each existing
  component as its directory lists it (`pickListed`: only when the exact name is
  NOT listed, i.e. the filesystem folded it to open it) and then as the index
  spells the path (`gitx.TrackedFolded`, `:(literal,icase)`, used only when the
  index has exactly one spelling: a case-only `mv` on disk leaves the index's,
  and that is the name status, diff and every other window report; case twins
  keep the given one). ⚠ **Either folded spelling replaces the typed one only
  when `acceptFold` says they are one file** (both exist and `os.SameFile`), or
  when neither exists AND the worktree's filesystem itself folds case
  (`foldsCase`): one existing without the other, or two different files, are
  two names, so neither a wrong probe nor a hand-set `core.ignorecase` on a
  case-sensitive volume can merge two files (pure, table-tested with injected
  facts; `foldFacts` gathers them). ⚠ **`core.ignorecase` alone never folds a
  spelling that is on no disk** (#201 re-review): set by hand on a
  case-sensitive volume, with the tracked README.md deleted in this window, a
  readme.md about to be created was folded into the index's README.md and an
  add/add with another window's new readme.md read clear in `wt check` and the
  Claude hook. Shared with the hooks, so `wt check readme.md`, a Claude edit of
  `$PWD/readme.md` and a Codex patch of `readme.md` all ask about README.md.
  `wt check` alone, and only where the filesystem folds case (`argBase.fold` =
  `foldsCase`, the same rule), also matches a path that is nowhere under its
  own spelling against the spellings windows touch (`collide.TouchedSpellings`,
  from `checkArgQueries`): a file only another window created, the hooks'
  superset again. Under `core.ignorecase` alone such a name stays a search
  (`TestCheck_CaseSensitiveVolumeKeepsTheSearch`). ⚠ **Where it applies, that
  REPLACES the fuzzy search for that argument, on purpose**
  (#181's rule: an argument that names a real place, here one touched under
  another case, is a question about THAT place). On macOS, with window B
  creating NOTES.md and window C editing docs/notes.md, `wt check notes.md` was
  HIGH on main through C's basename and is now exact on NOTES.md: B only. The
  dropped hit was a different file, exactly the root-vs-nested confusion #181
  removed. A case-sensitive repository is never folded, and an exact match never
  becomes case-blind. Not folded (left as gaps): the fuzzy basename search, and
  Unicode normalization (APFS also opens an NFD name for its NFC spelling).
  Measured on git 2.39: `--show-toplevel` and `--show-prefix` come back in the
  on-disk case even from a cwd entered under another case, and `git worktree
  list` records physical paths.
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
  diff --name-only` relays GitHub's quoted names, so `ghx.PRChangedFileCount`
  unquotes them (`unquoteGitPath`, `diffNames`); the deploy gate's list
  (`ghx.PRChangedFiles`) comes from REST JSON since #221, verbatim.
- **A path handed to git means that one file, never a pattern (#204).** After
  `--` git reads a path as a pathspec: `a[1].md` also matched an untracked
  `a1.md`, so `IsUntracked` read a committed `a[1].md` as untracked and #113
  downgraded a real collision; `*.md` folded every .md file's hunks into one
  file's ranges; a leading `:` is magic (`:colon.md` measured `colon.md`). Every
  `-- <path>` call passes `gitx.literalPath(p)` (`:(literal)<p>`): IsTracked*,
  IsUntracked, ChangedRangesChecked/ChangedRangesNew, uncommittedRangesNew.
  Prefixing the path, not `git --literal-pathspecs`, keeps the subcommand at
  `args[0]`, where the `gitOutput` failure-injection tests match it (the seam
  also takes a per-call env and stdin since #199, for ChangedRangesWith). ⚠
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
  ⚠ **A merged PR reaps only a tip that shipped in it (#230 review).** It
  proves only the commits it had: clean reaped a merged PR's lane without
  asking, and a commit made there after the merge went with the branch.
  `merge.UnshippedLane` (merge-pr's #187 check, one function for both) must
  find the tip at the PR's `headRefOid` or an ancestor of it; otherwise the
  merged PR proves nothing and only the cherry proof can reap. Every proof is
  about the tip read at decision time, and the branch is deleted only there.
  `prLookup` is the test seam (gh's auth answer is memoized process-wide).
  ⚠ **Untracked files count whatever `status.showUntrackedFiles` says (#208).**
  A repo set to `no` hid them from a plain porcelain status AND from `git
  worktree remove`'s own check, so `clean -y`, merge-pr's auto-clean and
  `release --clean` deleted a worktree's untracked work (measured, git 2.39).
  `IsClean` and every dirty count read `gitx.StatusEntries`
  (`--untracked-files=normal`); `WorktreeRemove` carries a scoped `-c
  status.showUntrackedFiles=normal`, one command's argument, never the process
  env (#92). Ignored files are unchanged: never dirty, deleted with the worktree.
  ⚠ **So does an edit git status never looks at (#210).** A tracked file
  flagged `--assume-unchanged` or `--skip-worktree`, or every file a checkout
  writes under `core.ignoreStat=true`, is skipped by git status and by `git
  worktree remove`'s check, which deleted the edit. `StatusEntries` reads the
  flags from `git ls-files -z -s -v` (lower-case tag = assume-unchanged, `S`/`s`
  = skip-worktree) and lists each flagged file that exists and differs from its
  index blob: regular files hashed by git (`hash-object --stdin-paths`, so clean
  and eol filters apply as for `git add`), symlinks by their target. A file the
  entry no longer has on disk loses nothing and is skipped; anything that can't
  be compared counts as changed (fail closed).
  ⚠ **Liveness reads the same `StatusEntries`** (`IsClean` → `LiveFacts.Dirty`,
  doctor's counts): untracked files a `showUntrackedFiles=no` config hides,
  hidden tracked edits, and submodule changes (`--ignore-submodules=none`, as
  git's own removal check reads them, overriding `submodule.<name>.ignore`) now
  make a window dirty, and **dirty is never suppressed** (#87). A merged-by-
  ancestry, tip-merged or dormant window holding such junk therefore stays
  surfaced: exactly what the same window reads in a default-config repo, and
  untracked files were already its touched paths (`TouchedFiles` lists them
  with `-uall`). Silence junk with ignore rules, not by hiding it from status.
  ⚠ **What git status never shows but the removal deletes or git refuses
  blocks a clean worktree too** (`removalBlocked`, in `remove()` and in clean's
  listing, #174): a git repository below an ignored path (`gitx.NestedRepos`:
  `ls-files -o -i --exclude-standard --directory`, then a walk for `.git`
  entries, symlinks not followed; `git worktree remove` deleted it, unpushed
  commits and all) and a checked-out submodule (`gitx.PopulatedSubmodules`, git's
  `validate_no_submodules`). Not part of `IsClean`: the walk is too slow for
  liveness, and a nested clone is no collision. Any read error blocks.
  `release --clean` keeps origin's placeholder too when the removal is refused
  (it used to delete it and keep the worktree).
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
- **`wt discard` is the ONLY command that drops unpushed commits (#177), and only
  for an exact name with `--drop-commits`.** It removes ONE worktree its owner
  declared throwaway (a canary pushed, used and deleted on origin), its local
  branch and the claim sections recorded ON THAT BRANCH
  (`activework.RemoveBranchSections`; auto-clean's removal by issue would also
  drop another worktree's claim on the same issue). Never a sweep: one name,
  resolved by `DiscardTarget` (clean's `MatchedName` + `CheckNames` wording, with
  the main checkout a candidate, so a name that also picks it out is ambiguous).
  `DecideDiscard` is pure; no flag unlocks anything but its own guard (an
  exhaustive test pins it): the main checkout, a detached HEAD (mid-rebase), the
  base branch, the window it runs in, a dirty tree, a locked worktree, a git
  repository in its ignored files and a checked-out submodule are refused
  whatever the flags; out-of-root needs `--all-roots`, which keeps discard's
  reach equal to clean's (#101). ⚠ **"Unique" is read from origin itself**
  (`ls-remote --heads`), never `rev-list --not --remotes`: a branch deleted on
  origin from the web or another clone leaves a stale `origin/<branch>` here that
  hid the canary's own commit, and a later prune drops it. ⚠ **Offline, only
  `origin/<base>` as last fetched is trusted** (every other commit counts, and
  the plan says origin couldn't be asked): trusting the tracking refs minus
  `origin/<branch>` let a canary pushed under ANOTHER name, or to another remote,
  and deleted there, go without `--drop-commits` (#177 review). The plan also
  says which listed commits another ref here keeps (`gitx.RefTipsExcept`: other
  refs, the stash, other worktrees' HEADs) and which become unreachable. The
  fetch is `--no-prune`: a
  prune deletes other branches' stale refs, which can be the only copy of
  someone's commits. ⚠ `CommitsOnlyOn` verifies the tip first: under
  `--ignore-missing` a missing tip read as "no commits". ⚠ Measured on git 2.39:
  `git worktree remove` deletes a worktree nested (ignored) inside the one
  removed, so a worktree with another inside is refused; and
  `status.showUntrackedFiles=no` blinds a plain porcelain status AND worktree
  remove's own check, which deleted the untracked files, so `StatusEntries`
  passes `--untracked-files=normal` and `WorktreeRemove` a scoped `-c
  status.showUntrackedFiles=normal` (every removal, #208). ⚠ With
  `core.ignorecase` a case twin is one loose ref file and git's in-use check
  compares names exactly (#167), so the
  base and branch-in-use checks fold case (`sameBranch`): `git branch -D Main`
  deletes main. A worktree whose directory is gone has nothing on disk to lose
  (git removes just its record, measured); one that is no longer a work tree's
  top (replaced since git recorded it, #198 review), or any other unreadable
  status, counts as dirty. ⚠ **"The window it runs in" compares directories,
  not path text** (`holdsCwd`: walk up from "." via ".." with `os.SameFile`): a
  cwd reached by a differently-cased path (macOS) or a symlink was not "under"
  the worktree, which was removed from under the command; an unreadable cwd
  counts as held. Once it removes, discard's git runs in the main checkout
  (`WorktreeRemoveIn`, `DeleteBranchAt(dir, …)`), never in the deleted
  worktree. ⚠ **The branch is deleted only at the tip that was listed, in ONE
  step**: `git update-ref -d refs/heads/<b> <tip>` (`git branch -D` deletes
  whatever is there by then, so a commit landing between a separate check and it
  was dropped), after `branchBusy` makes `branch -D`'s own in-use check
  (checked out, case twins folded, or being rebased/bisected: the
  `rebase-*/head-name` and `BISECT_START` files of every worktree's git dir), and
  then its `branch.<b>` config section goes. Never forced; origin is never
  touched. `wt release <issue>` is suggested only when no other claim on the
  issue is left (`claimNotes`): release removes claims by issue.
  Known gaps, left as follow-ups: a commit only a reflog reaches (the removed
  worktree's HEAD reflog, the branch's own) is neither listed nor kept;
  `--drop-commits` drops what the run lists, with no pin to a tip reviewed
  earlier; another session working in the target is seen only through a dirty
  tree; a directory on an unmounted volume that is not `git worktree lock`ed
  reads as gone.
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
  `--disable-auto` and a merge queue (queued); a forwarded `-R` read the same
  way until #228 (merge-pr read this clone's PR of that number). Taking
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
  ⚠ **The removal's git runs from the main checkout (#230).** merge-pr usually
  runs INSIDE the worktree it removes; `git branch -D` then ran in the removed
  directory, exited 128, and the merged branch stayed, while the hint said to
  cd into that same directory (`c.Root` is the cwd's worktree). `worktree.remove`
  resolves `gitx.MainCheckout()` (the shared `.git`'s parent; in a bare
  repository, the repository itself) before removing, runs the removal and the
  delete there, and prints the cd hint itself (`holdsCwd`, read before, so a
  symlinked or differently-cased cwd still gets it). ⚠⚠ **That fix made the
  delete WORK where it used to fail, which armed guards that failure had been
  hiding (#230 review; "scope-widening" below):** every remover now
  (1) makes the path absolute from the cwd first, since the guards read it
  there and git, run from the main checkout, read a relative one there (`release
  --clean` from a subdirectory checked one lane and removed another);
  (2) deletes the branch only at the tip its caller CHECKED
  (`gitx.DeleteBranchAt`, as discard does; `Remove(c, path, branch, tip,
  force)`, no tip keeps the branch), since `git branch -D` deleted a commit that
  landed after the check;
  (3) keeps a case twin of the base (`sameBranch`, `Main` under
  `core.ignorecase` is main's loose ref).
  `release --clean` reads its placeholder tip first and origin from the main
  checkout (read from the removed lane it failed, read as "gone", and origin's
  placeholder stayed), removes only a lane still ON the claim's branch, and
  deletes origin's placeholder with a lease on that tip
  (`DeleteRemoteBranch(dir, b, tip)`: `--force-with-lease`, which git applies
  to a delete). `wt clean` removes the cwd's lane last (`cwdLast`). ⚠ The #187
  check fetches `refs/pull/<n>/head` (from each candidate in turn, below)
  when the PR's head is not in this clone (updated on GitHub, then deleted with the merge: no plain fetch brings
  it, and the lane was kept forever), and an UNKNOWN verdict never advises
  "push them" (that recreates the deleted branch at a stale tip). A PR given
  as a URL is fetched by its number (`merge.PRNumber`). ⚠ **`wt clean` hands
  the check another repository's PR by its URL** (`prURL` +
  `merge.LaneSelector`, #243): its number is gh's resolved repository's PR,
  and in a fork clone whose gh default is upstream, `refs/pull/<n>/head` on
  origin is another PR, so the head never arrived and the lane was kept on
  every run, its hint naming the wrong PR. origin's own PR, or one gh gives no
  URL for, keeps the number (and "#5" in the message). ⚠ **Where another
  repository's PR head is fetched from** (`merge.prRemotes`, #243 reviews),
  in turn until the head gh named is in the clone (`fetchHead`): configured
  remotes whose URL names that owner/name, those on the PR URL's host first
  (`git remote` lists alphabetically, and a same-path mirror elsewhere, a GHE
  copy or Codeberg, was picked before `upstream`), then the others (an ssh
  alias names the host differently); then the repository's https URL; then
  origin. The bare https URL alone ignored the user's own transport (an
  ssh-only user has no https credentials), and an origin still on a renamed
  repository's old name, which GitHub's URL names by its new one, was kept
  where the number had reaped it. A fetch that WORKS but leaves the head
  missing (a mirror's own PR n, origin's other PR) goes on to the next; any
  fetch's commits are harmless, since the check compares with gh's head. A
  fetch that hits `fetchTimeout` (`gitx.FetchTimeoutError`) ends the walk,
  so a hung remote costs one timeout per lane, not one per candidate (an ssh
  remote that never answers fails at `ConnectTimeout=10` with an ordinary
  error and the walk goes on: ~10s per matching ssh remote, on a dead
  network, and the lane is only kept). A remote named like an option
  (`--upload-pack=<cmd>`, which git accepts) is never a candidate, and
  every no-prompt fetch passes `--end-of-options`. The
  hint names the first candidate (`prRemote`).
  Pinned end to end with real git by
  `TestClean_ForkCloneFetchesThePRHeadFromThePRsRepository` (remotes through
  a fake ssh, https through `insteadOf` with a catch-all so nothing reaches
  the network; origin can't use `insteadOf`: git expands it in `remote
  get-url`, so wt would read a local path), and `TestPRRemote`,
  `TestPRRemotes_order`, `TestFetchNoPrompt_remoteIsNeverAnOption`,
  `TestLaneSelector` and `TestUnshippedLane_FetchesThePRHeadByNumber`.
  ⚠ **A no-prompt
  fetch has a deadline** (`fetchTimeout` 30s for the PR head,
  `baseFetchTimeout` 120s for #223's base fetch; ssh `ConnectTimeout=10`,
  HTTPS `lowSpeedTime=15`): an origin that took the connection and never
  answered hung merge-pr's cleanup. ⚠ The fetch runs in its own process group
  and the deadline SIGTERMs the GROUP (`stopGroupOnCancel`, unix): Apple's git
  runs the fetch as a child of a wrapper, and killing only the started process
  left the real fetch and git-remote-http running (pinned: the test pgreps for
  them while the silent origin still holds the connection). `cmd.WaitDelay`
  then stops waiting on a helper holding the pipe and SIGKILLs the leader
  (measured: Wait waited 15s past a 2s deadline without it). `release --clean` compares the lane's FULL HEAD ref
  (`gitx.SymbolicHead`): a tag of the branch's name made `--abbrev-ref` say
  `heads/<b>`. A lease refusal says origin moved. A relative
  `worktree_root` still means a different directory from each cwd (#238).
  Pinned by `remove_review_test.go`, `TestRelease_CleanFromInsideTheLane` and
  `TestCmdMergePR_keepsAnUnshippedLaneFromInsideIt`.
- **merge-pr's checks gate never reads a failed read, or `--admin`, as green
  (#179).** It reads the head commit's statusCheckRollup (one GraphQL query,
  paged: `ghx.PRChecks`) and what the base branch requires, from the PR's own
  `baseRepository` on the PR URL's host (never the cwd's `{owner}/{repo}`: a PR
  URL or a forwarded `-R` names another repo): REST `branches/{base}`
  (`protection.required_status_checks`, which any reader gets, unlike the
  protection endpoint) and `rules/branches/{base}`. It decides in pure
  `merge.DecideChecks`: pending, failed (CANCELLED and STALE included), a state
  wt does not know, a required check with no run on the head, a required check
  pinned to an app (`app_id`/`integration_id`) that only another app reported,
  a ruleset `workflows` rule with no run of that file in that repository
  (`workflowRun.file`; unverifiable when a run's file is unknown), fewer
  non-SKIPPED checks than `merge_min_checks` (or a value that is not a count),
  and an unreadable read of either half all block; no check that ran, nothing
  required and no floor is `none`: merge, with a note. GitHub's own per-context
  `isRequired(pullRequestNumber:)` decides an app-pinned check (it is the only
  way to verify a pinned commit STATUS) and costs nothing measurable (1
  rate-limit point a page either way). ⚠ **Not `gh pr checks`:** it exits 1
  with a stderr sentence for "no checks" (a failure's exit code), and the
  rollup keeps every workflow run a later event started on the same head
  (cli/cli#14044: label events, 21 runs of 9 checks), though it already drops a
  re-run job's earlier attempt (measured: same check suite, only attempt 2
  listed). ⚠⚠ **What still counts is `ghx.latestChecks` (#179 review), and
  newest-wins was a fail-open:** a label event's run that SKIPPED a job an
  earlier run FAILED read green. Runs group by app + workflow (name and file) +
  event + name (two apps posting one name are two checks); within ONE workflow
  run every run counts (two jobs of one name, pytorch); across workflow runs the
  newest decides, except a failure stands until a later run PASSES (a skip,
  neutral or cancelled run never clears it), a skip never replaces a pass, a
  running run or an unknown state, and CANCELLED/STALE are replaced by anything
  later. One state reading (`ghx.StateKind`) feeds both that and
  `merge.BucketOf`. ⚠ `{"data":{"resource":null}}` (no such PR) and
  `{"resource":{}}` (an issue's URL) come back with exit 0; `parsePRChecks`
  requires the head commit and the base repository, so they are errors, never
  "no checks" (#168). ⚠ A 404 or the "Upgrade to GitHub Pro" 403 from the rules
  endpoint means the server has no rulesets for the repo
  (`ErrRulesetsUnavailable`): an answer, not a failed read. A rate-limit or SSO
  403 is a failed read. ⚠ **Order:** precheck → checks → deploy confirm → coord
  hold → close check → Run's guards. Before the deploy confirm, so nobody types
  "deploy" for a red PR. ⚠ **Only `--checks-ok` gets past it:** `--bypass` is
  for wt's structural guards, and `--admin` bypasses GitHub's own checks, which
  is why the gate exists. `--auto` is not exempt either: gh merges at once when
  only checks no rule requires are pending or failing (UNSTABLE). A passthrough
  that merges nothing (`merge.NonMerging`: `--disable-auto`, `--help`/`-h`, read
  as gh's pflag reads it) skips the gate and the pin (`checks=skipped`). ⚠ The
  merge is pinned with `--match-head-commit <the head it read>`
  (`merge.WithMatchHead`, in front of the passthrough like `--admin`), so a push
  after the read fails the merge instead of shipping an unchecked head. ⚠ The
  terminal prompt reads its answer a byte at a time (`readLine`): a `-F -` body
  is read from the same stdin next (#180), and a buffered read took it along. ⚠
  A dry run's verdict line carries the gate's result (`verdict=ok
  checks=blocked`), never a bare `verdict=ok` for a merge the checks would
  stop; it still exits 0. ⚠ Zero checks in a repo WITH workflows still merges,
  with the note: a workflow's triggers decide whether it runs on a PR
  (awesome-o's one workflow is path-filtered, so its PRs legitimately carry
  none), so the deterministic "CI never started" signals are a required check
  and `merge_min_checks`.
- **A rate limit is named as one, and an unread diff is never EMPTY (#217).**
  The account's REST quota (5,000/h) is shared by every session and tool on it.
  Once it is used up, GitHub refuses every REST call with a 403 until it lifts,
  while GraphQL (its own quota) keeps answering. merge-pr misread that three
  ways: `gh auth status` validates over REST, so every gated read said "gh is
  not available or not authenticated"; `gh pr diff` failed and the count came
  back "0", so the guard called a 6-line diff EMPTY and pointed at `--bypass`;
  and the post-merge state read was gated off, so a merged PR's lane was kept.
  - `ghErrAt`/`apiErr` return a `*ghx.RateLimitError` (REST or GraphQL, primary
    or secondary: `rateLimitKind`) whenever gh's text says so, with when it lifts
    from a REFUSED call's headers (`gh api --include /`: Retry-After, else
    X-RateLimit-Reset when Remaining is 0; pure `rateReset`), probed once per
    host per authTTL (`probeRateReset` is the test seam). ⚠ Not the rate_limit
    endpoint: during the incident it reported 5,000 remaining while every call
    was refused. Only the REST primary quota is probed: a REST call's headers
    don't carry GraphQL's clock, and a probe during a secondary limit adds to
    the burst. ⚠ **Which API refused is read from the text first, then from the
    caller** (`ghErrAt(host, called, err)`): measured with real gh (#217
    review), `gh api graphql` refused for its quota prints only "gh: API rate
    limit already exceeded for user ID N." (RATE_LIMITED is in the stdout
    body), and a GraphQL 403 reads "… (https://…/graphql)". Text markers
    (`graphQLErr`: `GraphQL:`, RATE_LIMITED, `/graphql`, gh's GraphQL client's
    "non-200 OK status code") beat the caller's guess, since `gh pr diff` may
    itself resolve the PR over GraphQL; a bare `ghErr` assumes REST.
  - The auth check's 403/429 rate-limit refusal is `HostAuthRateLimited` (the
    `--json` form only: gh before 2.81 prints "token invalid" for it, and
    `authProblem` says it can't tell). GitHub refuses that way only after it took
    the token, so the login works. ⚠ **`AuthedFor` stays strict:** liveness
    trusts "no PR" only from it (`PRChecked`), and the tip lookup (#168) is REST,
    so a rate-limited host counted as authed would let dormancy suppress a window
    whose PR only that lookup finds. `SignedInFor`/`SignedIn` accept it too, and
    gate ONLY merge-pr's own reads (`PRState`, `PRChecks` and the required-check
    reads, the close check's `PRCommits`/`RepoSquashSettings`/
    `PRClosingIssueNumbers`), whose failures are reported, never read as
    "none". `signedIn` is a passed check, or a failed one that at least one
    rate-limited host explains (every other host OK): the human form's
    unexplained non-zero exit with every host it read OK stays a no. A refused
    gate says why (`notSignedIn`), per host. Which quota refused the CHECK is
    not known (gh checks a keyring token over REST, a GH_TOKEN over GraphQL),
    so neither doctor nor `HostAuth.Phrase` names one.
  - `PRChangedFileCount` is `(n, error)`: the REST diff, else GraphQL
    `changedFiles`. `merge.DiffRead` → `VerdictDiffUnread`: "could not be
    read", why, and NO `--bypass` pointer (a quota fault is no reason to spend
    a safety flag); an explicit `--bypass` still proceeds and says what it
    skipped. `PRChangedFiles` falls back to a paged GraphQL query
    (`prFilesQuery`; ⚠ `gh pr view --json files` stops at 100, measured 100 of
    285), taken only when it lists exactly `changedFiles` paths, below GitHub's
    3,000-file cap (`maxListedFiles`): a partial list is an error, so the
    deploy gate still fails closed. ⚠ It goes to the PR's own host
    (`--hostname`, from the PR URL via `parsePRRef`, as `PRChecks` does): bare
    `gh api graphql` asks github.com, which answers a GitHub Enterprise PR's URL
    with a null resource when gh is logged in to both. `bothFailed` wraps both
    errors (`%w`), so `ghx.IsRateLimit` sees either; the unread refusal's hint
    says "once the rate limit lifts" only when one did.
  - ⚠ **A rename is both of its paths to the deploy gate (#221).** `gh pr diff
    --name-only` prints only the new side (measured), so a PR moving
    `infra/x.yaml` to `docs/x.yaml`, which deletes a deployed object, never
    listed `infra/x.yaml` and merged without the typed confirm.
    `PRChangedFiles` now reads REST `pulls/{n}/files` (`prFilesREST`, at the
    PR's own host and repository: `resolvePR` + `prRepoPath`, 100 a page) and
    lists each file's `previous_filename` beside it (`parsePRFileList`: each
    path once; an error for a file without a path, a `renamed` file without
    the path it left, and a list at `maxListedFiles`). ⚠ The list must add up
    to the PR's `changedFiles` (read in `resolvePR`'s same `gh pr view` call):
    a page the pagination lost read as a PR without that file (#221 review,
    measured against real gh with the Link header dropped). The
    GraphQL fallback has no field for the old path (`PullRequestChangedFile`
    has only `changeType`), so any `RENAMED` file there is an error, and the
    gate gates (`parsePRFiles`). The empty-diff count keeps `gh pr diff`: a
    rename is one file either way. Pinned end to end by
    `TestCmdMergePR_renameOutOfADeployPathGates`, which merges without the
    confirm on the pre-#221 code.
  - The checks gate does not retry a read refused for a rate limit
    (`retryable`): it lifts in minutes, not in the 2 s retry.
  - doctor reads a rate-limited host as "logged in, but … refused the check for
    a rate limit", not "could not be verified".
  - `authNoJSON` is per gh binary (`ghPath`), not per process: a process whose
    PATH resolves `gh` to another binary (a test's fake) asks it for the JSON
    form afresh, instead of inheriting an earlier fake's "no JSON".
  Pinned end to end by `TestCmdMergePR_restQuotaUsedUp` (its own host, so the
  process-wide auth memo can't leak in), which fails on the pre-#217 code with
  every symptom above.
- **merge-pr's gate settings come from the base branch too, and the stricter
  applies (#223).** `config.Load` reads the `.wt.conf` of the checkout wt runs
  in, and merge-pr usually runs in the PR's own worktree, so the branch being
  merged decided its own gate: a PR setting `merge_is_deploy = false` (or
  narrowing `merge_is_deploy_paths`, lowering `merge_min_checks`, shortening
  `hold_max_age`, dropping `coord_issue`) merged under its own settings, and so
  did a branch cut before the base added a deploy glob. `mergeGates` reads two
  `config.Gate`s: this checkout's (`.wt.conf` + `WT_*`) and the base branch's
  (`ghx.ReadBaseConf`: `gh pr view --json number,url,baseRefName`, then one
  GraphQL query at the PR URL's host for `refs/heads/<base>`'s tip, its root
  tree's entry modes, and `object(expression: "refs/heads/<base>:.wt.conf")`;
  the full ref, since git resolves a bare name to a tag before a branch, per
  gitrevisions, not measured on GitHub). The stricter applies: the deploy gate
  fires if ANY source gates the PR (`deployDecision`; each source's `!`
  exclusions apply only to its own globs, so a checkout can't carve a file out
  of the base's gate; a bare `!` is an exclusion, so `merge_is_deploy_paths =
  !` fails closed), the checks floor is the highest (`strictestFloor`; a value
  that is not a count, from either, refuses and names where it was set), and
  holds use the longer `hold_max_age` (0 wins) and every `coord_issue`
  (`strictestHolds`, `mergeCoordGateWith`, whose stale-hold warning names the
  limit applied). `GateFromConf` starts from Load's defaults, so a base with no
  `.wt.conf` holds at hold_max_age 24h: hold_max_age is the one key whose
  default is not the loosest, so neither `WT_HOLD_MAX_AGE` nor an untracked
  local `.wt.conf` can shorten a hold below that any more (#223 re-review,
  accepted; `wt holds` and `prune-coord --legacy` still read only the
  checkout's value, so they can disagree with merge-pr).
  ⚠ Measured: a missing file is a null object with NO error (and no tree
  entry), a missing branch nulls the ref too, so no tip is an error, never "no
  file" (`parseBaseConf`); `Commit.file(path:)` was not used because GitHub
  answers a missing path with a NOT_FOUND error and gh exits 1. ⚠ **A symlink
  is a Blob whose text is its target** (#223 review: `object(expression:)` and
  `git cat-file blob` both hand it back), which parsed as a config with no
  keys while `config.Load` followed the link. Only the tree entry's mode
  (0o120000, decimal 40960) tells it apart: a symlink, a directory, a
  submodule, an entry and an object that disagree, or any mode but 100644 /
  100755 is an error. ⚠ **GitHub unread** (`baseGateSource`): the fallback is
  this clone's `origin/<base>` for the base GitHub NAMED for this PR, never
  `c.Base` (#223 review: the PR's own `base =` steered it, and a PR into
  `release` read `origin/main`), and only when the PR's repository is this
  clone's origin (owner and name, case-insensitive; the host is not compared,
  since an ssh alias names it differently), so `-R`, a PR URL, `GH_REPO` or a
  fork's default naming ANOTHER repository fail closed (a `-R` naming origin's
  own repository takes the fallback). The read is retried once first, as the
  checks gate's are (`retryable`: not for a rate limit). It fetches first
  (`fetchBase` = `gitx.FetchNoPrompt`, the git transport, which a GraphQL quota
  doesn't stop, with GIT_TERMINAL_PROMPT=0 against git's own HTTPS prompt and
  ssh in BatchMode unless the user set an ssh command; a GIT_ASKPASS helper is
  still the user's); a failed fetch reads the copy as last fetched, and says it
  may be stale. A retry that fails earlier than the first try keeps what the
  first resolved. ⚠ The tree entry and the object are resolved separately in
  one query, so both object ids are required and must match (a push between
  could pair a regular file's mode with a symlink's blob). ⚠ `ReadBaseConf` returns what it resolved WITH its error (base, owner,
  name, host), or the fallback can't read the right branch. ⚠
  `gitx.RefFileText` decides presence by `git ls-tree -z --full-tree` (exact
  path, from the top, whatever the cwd) and reads the blob by id: a failed
  `cat-file -t` used to read as "no file", so in an offline blob:none partial
  clone a `.wt.conf` the base added after the clone was absent (#223 review).
  Any read failure is an error. ⚠ **Fail closed** (`gateSource.unread`): the
  deploy gate gates the whole repository, its banner, draft refusal, confirm
  line and dry-run note say the merge MAY auto-apply (the reason, never
  "AUTO-APPLIES" as fact), the checks gate refuses, or asks at a terminal
  (`ChecksInput.FloorUnread`, with its own hint), until `--checks-ok`, and
  holds keep at least the default hold_max_age (#223 re-review: skipped, the
  PR's own hold_max_age decided while GitHub was down). A repo that never set
  merge_is_deploy can meet that when GitHub is unreachable; accepted. ⚠ A
  passthrough that merges nothing (`merge.NonMerging`) reads nothing from the
  base and lists no files, and passes neither the deploy nor the checks gate;
  the hold gate still applies to it, as before #223. ⚠ With `-R`,
  the deploy gate's file list and draft read are that repository's PR
  (`PRChangedFiles(pr, repo)`, `PRIsDraft(pr, repo)`); before, they read this
  clone's PR of the same number. ⚠⚠ **And so is every other read (#228):**
  the precheck, the guard, the close check and the post-merge reads went to
  this clone's PR of the same number, so a merged one here skipped the merge
  as "already merged" and auto-cleaned this clone's lane. merge-pr now
  resolves the PR in the forwarded repository once (`ghx.ResolvePRIn` →
  `ghx.PRAt`) and hands its URL to every read and to `gh pr merge` (gh reads a
  URL as that PR whatever `-R` or the cwd says, measured on gh 2.87); issues
  go by their URL there (`PRAt.IssueURL`) and the squash settings are that
  repository's, on its host (`RepoSquashSettingsOf`, `closeReadsAt`).
  Unresolvable: refuse. ⚠ **A PR given by its URL is resolved the same way**
  (`ghx.IsPRURL`; #228 review): the guard's own hints print that form under
  `-R`, and it read the issues and squash settings in the cwd's repository
  and auto-cleaned unchecked. `IsPRURL` is gh's own reading (its finder's
  `^/<owner>/<name>/pull/<n>`, a PREFIX match), so `…/pull/5.diff`, which gh
  merges as PR 5, is resolved too. ⚠ **Auto-clean keys on where the lane can be**
  (`laneMayBeHere`): the PR's repository OR its head's
  (`headRepository`) is origin. Keyed on the repository alone, a fork origin
  merging upstream's PR with `-R` kept its lane and said there was none; a
  PR whose both sides are elsewhere touches nothing here (a same-named lane
  is another lane). The host isn't compared (an ssh alias). The GraphQL
  reads that take the PR's URL (`PRClosingIssueNumbers`, `PRCommits`) pass
  its host (`withURLHost`), as `PRChecks` does: bare, gh asked its default
  host. Messages name the PR with `merge.PRLabel` (`#5`, or the URL, never
  `#<url>`). ⚠ Command hints (`prCommand`: the draft refusal's `gh pr
  ready`, the checks gate's `gh pr checks --watch`; merge.Run's `wt merge-pr
  … --bypass`) carry the forwarded `-R`, and spell the PR without a `#`
  (`merge.PRArg`): pasted, `gh pr ready #5 -R o/r` is a bare `gh pr ready`
  after the shell's comment, which marks the CURRENT branch's PR ready. With
  origin a fork and `-R` naming upstream, #187's PR-head fetch and its hint
  go to the PR's own repository (`merge.prRemotes` + `fetchHead`), where PR n on origin is
  another PR or none.
  Pinned by `TestCmdMergePR_forwardedRepoReadsOnlyThatPR` (`-R` and URL),
  whose fake gh answers only that PR and logs any other read, plus
  `…AlreadyMergedLeavesTheLane` (the precheck's auto-clean, with a lane here
  on the PR's head branch name), `…MergeFailureReadsThatPR` (#196's
  post-failure state read) and `…FromAForkOriginCleansOn`. The `WT_*` overlay is the checkout's side, so
  the environment can tighten but never loosen a gate the base sets;
  `--confirm-deploy` / `--checks-ok` are the ways past it. Pinned by
  `TestCmdMergePR_gateSettingsComeFromTheBaseToo` (merges ungated on the
  pre-#223 code), `TestCmdMergePR_holdSettingsComeFromTheBaseToo` and
  `TestCmdMergePR_forwardedRepoReadsThatPRsFiles`. Not covered: an LFS pointer
  or a clean-filtered `.wt.conf` reads as its stored text.
- **The deploy gate's GitHub status read only WARNS, and fails open (#178).**
  Inside the deploy confirm (draft refusal → prod banner → status → confirm),
  for a PR on github.com (`merge.GitHubStatusCovers`; the host is the checks
  gate's PR-URL read, so an ssh alias or a forwarded `-R` resolves, else
  origin's), merge-pr GETs githubstatus.com's `summary.json` (net/http, 3s for
  the whole read: wt's one direct network call) and warns when a component
  naming Actions is not `operational` or an unresolved incident names Actions
  (whole word, in its name, components or updates' affected components). Pure
  `merge.DecideActionsStatus`: a degraded signal outranks unknown, and a page
  that does not SAY operational (no Actions component, no incident list, not
  JSON, unreachable) is unknown: one "could not check" line, never silence.
  Never a refusal, `--confirm-deploy` still merges; a dry run prints the same
  lines; a draft, a non-deploy PR and a non-github.com host read nothing.
  ⚠ `WT_GITHUB_STATUS_URL` is the test seam, and the cli package's `TestMain`
  points it at a scheme that fails offline, so no test reaches the real page.
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
- **A relative `worktree_root` or `active_work` is read from the MAIN
  checkout (#238).** `config.Load` anchors either (`.wt.conf` or the env var)
  at `Config.MainRoot` (`anchorPath`), the same from every worktree:
  `mainWorkTree`, the shared git dir's parent when it is a `.git`, else the git
  dir itself (a bare repository; ⚠ a `--separate-git-dir`, which is not the
  work tree), which is what `git worktree list` gives first in every layout
  measured, without its ~10ms on every command. Kept relative, it meant a
  different directory from each cwd: a claim recorded a path only its own cwd
  could find, `release --clean` from elsewhere read it as "already gone", and
  `under()` matched from one window and refused from another. A claim recorded
  relative before #238 is found by its BRANCH in `git worktree list`, never
  the main worktree (`relativeRecord`): read from the main checkout, it missed
  a lane claimed from a subdirectory, and a decoy worktree at that path was
  removed. On a miss it is read from the main checkout, and `--clean`'s
  branch check (#230) refuses a worktree there on another branch. Release
  names the active-work file when there is none (an entry recorded under a
  cwd-relative `active_work` is not migrated).
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
PreToolUse). The per-turn hooks also mark the session's presence in the
coordination log (#197; never waiting on its lock). Agent hooks derive the repo from the payload's `cwd` and **always
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
  FYI) against this window's ranges as they will read once the patch is applied:
  `predictCodexPatch` replays apply_patch over the on-disk files
  (`codex_apply.go`) and `gitx.ChangedRangesWith` measures each result, with the
  same `regradePending` as the Claude hook (#108/#184/#199), after the same
  `regradeSections` for a structured doc (#226). A disjoint patch to a shared
  file therefore stays silent. Emits `additionalContext` on overlap;
  `WT_CODEX_HOOK_BLOCK=1` upgrades a **confirmed** HIGH (a computed hunk overlap) to
  `deny` — a file-level-only match never denies. Each file's line names EVERY
  window editing it, each with its own liveness and confirmed / not-computed tag
  (`codexEditDecision`, deduplicated by path + window: the worktree, else the
  label, #193). It used to keep only the first entry per path, so a second
  window on the file went unmentioned (#201). The Claude hook's message does
  the same (`claudeDecision`, #201 review): it deduplicated by label, so
  namesakes collapsed, and one file-level entry worded every window; now each
  window is named once by worktree with its own grade when the grades differ.
  Its deny predicate is unchanged (any kept entry under WT_CLAUDE_HOOK_BLOCK).
  ⚠ **Known gap: an `exec_command` workdir (#201).** The PreToolUse payload's
  `cwd` is the SESSION's cwd, and `codex-edit` reads a patch's relative paths
  from it. Codex can also run apply_patch through `exec_command` with a per-call
  `workdir` (openai/codex#32360, openai/codex#50271); if such a call ever reaches
  this hook, its relative paths resolve against the wrong directory (a file in
  another directory, or outside the repo: silent). No per-call working-directory
  field in the hook payload could be verified (no Codex binary to measure), so
  none is read: guessing a field name would read nothing, or the wrong thing,
  without a sign. Revisit once Codex documents one; `parseCodexEdit` is where it
  would go.

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
- **Ask gh about auth only through `ghx.AuthedFor`/`Authed`** (#172), or, for
  merge-pr's own reads only, `SignedInFor`/`SignedIn` (#217, above). The answer
  is memoized per gh binary and host for `authTTL` (a minute: one command asks
  once, and a long-lived `wt mcp` still notices a `gh auth login`); per binary
  since #197's review, when a test's real gh answer was reused for a later
  test's fake gh. In a repo with no forge
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
  login → "could not be verified", never "NOT authenticated".
  ⚠ **No exit code is the answer, scoped or not (#203).** `gh auth status
  --hostname H` exits 1 for an INACTIVE account's dead token on H too, so
  `Authed()` gated every gh call off while the active account worked, and a
  timeout on H read "NOT authenticated". `AuthedFor` is pure `authOK`: every
  host gh reported has a working ACTIVE account (human-form exit 0 is a yes;
  unreadable or no host is a no), and doctor reads a scoped check per host too.
  ⚠ **A yes is safety-relevant:** `Authed()` sets `PRChecked`, which dormancy
  needs (an idle branch is suppressed only when "no PR" was checkable), so a
  human-form exit 1 is a yes only when an inactive account's failure in a
  section wt READ explains it (`textAuthStatus`); a failing section wt can't
  read leaves it a no.
  ⚠ gh's human output prints "X Failed to log in … token invalid" for a host it
  never REACHED (DNS, refused); only `gh auth status --json hosts` (gh 2.81+,
  cli/cli#11544) keeps gh's error. `runAuthCheck` asks `--json hosts --active`
  first (stdout only: gh writes notes to stderr even then; it exits 0 whatever
  the login) and falls back to the human form IN THE SAME memoized check when gh
  refuses the flag ("unknown flag: --json", remembered per process in
  `authNoJSON`, so gh < 2.81 costs one flag error per process, no network) or
  answers without the JSON; any other failure falls back once, not remembered
  (`jsonAuthOutcome`, pure). Pure `classifyAuthError`: only a 401 is `failed`
  (REST "HTTP 401: …", GraphQL for a GH_TOKEN "non-200 OK status code: 401 …");
  no HTTP answer (`Get "<url>": …`) is `unreachable`; any other status (a 403
  rate limit, a 5xx) is `unknown`. A human-output `failed` stays ambiguous, so
  doctor's NOT-authenticated line says "or offline" for it (`failedUnconfirmed`:
  a failed host with no gh error). `wt doctor --json` (additive): `gh_hosts[]`
  gains state `unreachable` and `error` (gh's text, 2.81+), and a scoped check
  that did not pass now carries `gh_hosts`/`gh_auth_unknown` too.
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
