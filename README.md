# wt — multi-window git coordination

Run **3 or 4 windows on different things in the same repo** without stepping on
each other. `wt` gives each window its own worktree, lets a window *claim* a
unit of work so the others can see it, and — the core — tells you when two
windows are **editing the same lines**, before that becomes a merge conflict.
It distinguishes real conflicts (overlapping hunks) from parallel appends to the
same file, quiets down dormant and merged branches, cleans up worktrees when
work ships, and can gate merges in repos where merge == deploy-to-prod. It works
whether the windows are humans or AI agents — and Claude Code / Codex hooks can
surface collisions to the **agents doing the editing**, not just to you (see
[Agents](#agents-claude-code--codex)).

Repo-agnostic, single static binary, zero third-party dependencies (it shells
out to `git` and `gh`). Works in any git repo on any machine.

```
go install github.com/eharriett0/wt@latest
```

Or via Homebrew. Note the tap is `eharriett0/homebrew-tap` but `brew trust`
takes the short form `eharriett0/tap` — Homebrew requires trusting a third-party
tap before it will load its formula, so a fresh machine needs the `trust` line:

```
brew tap eharriett0/homebrew-tap
brew trust eharriett0/tap    # required for third-party taps; without it `brew install` refuses to load the formula
brew install wt              # later: brew upgrade wt
wt version
```

The formula builds from source, so it pulls the `go` formula and will **upgrade
an existing Homebrew Go** as a build dependency — harmless for `wt`, but worth
knowing if you have other Go work on the machine.

## The core: collision awareness

The headline problem with several windows in one repo isn't "did two of us grab
the same ticket" — it's "are we editing the same files right now?" `wt` answers
that at the **file level**, derived from each worktree's live git state, so it
works even when windows are on different branches and even when a window never
claimed an issue.

```
wt status            # every window, the files each is touching, and graded overlaps
wt check <paths…>    # before you edit: is anyone else in these files?
```

`wt status` ends with either `✓ no file collisions … all clear` or a `💥` list
of files touched by more than one window. `wt check` exits **3** when another
window is already in one of the given paths (so a script — or an agent in one
window — can branch on "collision found"), **0** when clear.

### Hunk-level, not just file-level

Append-heavy files — an image-inventory YAML, a kustomize `resources:` list, a
changelog — get edited by many windows at once where every edit is a disjoint
append; the real conflict risk is ~zero, yet a file-level check lights up a `💥`
wall on exactly the files touched most. `wt` diffs the **pending hunks** of each
window (`git diff -U0`, uncommitted ∪ committed-vs-base) and grades the overlap:

```
config.yaml   — overlapping L88-95  → HIGH   (exit 3, blocks)
inventory.yaml — 6 windows, 0 overlapping hunks → low (FYI, exit 0)
```

Only **overlapping line ranges** (or an indeterminate case where your side has
no edits yet — kept blocking, to be safe) count as HIGH and drive exit 3.
Provably-disjoint hunks are downgraded to a non-blocking FYI. Two escape hatches
make files always-advisory regardless of hunks: `shared_docs` (basename match,
default `CLAUDE.md,MEMORY.md`) and `append_only_paths` (globs — changelogs,
inventory lists).

`wt check --show-diff` previews the *other* window's hunk ranges inline so you
can eyeball disjoint-ness; `wt check --json` / `wt status --json` emit the same
data structured (with a `severity` field and `blocking` flag) for tooling and
pre-push hooks.

### Stale branches don't count

A file "collision" only matters if the other window can still *change* that
file. A branch whose work is already merged (squash-safe, detected via
`git cherry`) sitting on a clean worktree with no open PR cannot — so by default
`wt check` and `wt status` **suppress collisions against stale branches** and
report only the live ones. Each colliding window is classified:

| Signal | Treated as | Shown |
|---|---|---|
| Open PR for the branch | **active** | `[open PR #123]` |
| Uncommitted changes in the worktree | **active** | `[uncommitted edits]` |
| Commits not yet on base, never opened a PR | **active** (latent) | `[commits, no PR opened, last commit 4d ago]` |
| Merged PR (incl. squash-merged + deleted branch) | **stale** → suppressed | `[merged #84]` |
| Closed-unmerged PR (kept on purpose) | **stale** → suppressed | `[PR #1654 closed]` |
| Commits, no PR, idle past `max_age` | **dormant** → suppressed | `[dormant, last commit 12d ago]` |
| Clean worktree, no PR, nothing unshipped | **stale** → suppressed | `[stale: merged / no PR]` |

PR state is resolved with a single `gh pr list --state all`, so a **squash-merged
branch** (which `git cherry` can't detect as shipped, and whose branch is often
deleted) is correctly suppressed as `merged #N` rather than lighting up as a
false HIGH. A **closed-but-unmerged** PR's branch is kept on purpose (recoverable
diff), so it's suppressed too and labelled `PR #N closed`. PR state also
**outranks a leftover dirty index**: a merged/closed branch that still carries
staged cruft `wt clean` won't remove reads stale (with a `· leftover uncommitted
edits` note), not a permanent HIGH.

The last-commit age is always shown on unmerged/dormant windows. **Dormancy** is
opt-in: set `max_age` (e.g. `4d`, `2w`, `36h`) and an unmerged-but-idle branch —
one you'd otherwise have to confirm out-of-band was abandoned — is suppressed
just like a merged one. A dirty or open-PR branch is never dormant (it's active
by definition).

A **dirty base-branch checkout far behind `origin/base`** — the shared `main`
checkout everyone forgot to pull, whose stale edits then "overlap" almost
everything — stays HIGH (its uncommitted edits *could* be real work, so it's
never hidden) but is labelled `[uncommitted edits · N behind base — likely
stale]` so you can dismiss it at a glance, and `wt doctor` names it so you fix
the root cause.

Without this, hot shared files (a top-level `CLAUDE.md`, a central policy file)
light up against every long-dead branch that ever touched them, training you to
ignore the warning. Now `wt check CLAUDE.md` shows the one window with an open PR
and notes `+N more on stale branch(es) … ignored`. Pass `--include-stale` to see
everything (and count stale as a collision for exit 3). Classification is
conservative: only the *definitively* merged-and-clean case is suppressed —
anything ambiguous (e.g. gh offline) is surfaced.

A `pre-commit` hook (installed by `wt install-hooks`) runs the same check
automatically: when files you're committing overlap another window's working
set, it prints a loud, non-blocking notice naming the files and the window.

## Commands

| Command | What it does |
|---|---|
| `wt status [--json]` | All windows + files each touches + severity-graded overlaps. `[--blocking]` = only HIGH, exit 3 (a gate). `[--max-age D]` |
| `wt status --epic <id>` | Aggregate an epic's claims + live PR states across sibling repos |
| `wt check <paths…>` | Is another window touching these paths? `[--show-diff] [--json] [--blocking] [--include-stale] [--allow-missing] [--max-age D]` (exit 3 = HIGH). `--blocking` prints only HIGH (a scriptable gate). Refuses a path that doesn't exist, isn't tracked, and no window is touching — a typo must never falsely report "clear" (`--allow-missing` opts into a deleted/other-branch/about-to-create path) |
| `wt where <issue\|branch>` | Print that window's worktree path — `cd $(wt where 42)` |
| `wt new <branch>` | Create a worktree on a new branch from the base branch |
| `wt clean [-y]` | List worktrees whose branch already shipped (incl. squash-merged PRs); `-y` removes them. Never reaps a just-created worktree (grace window), a never-pushed branch (no upstream = unshared work), or a dirty one. `[--stale-index]` also **reports** (never auto-removes) a merged-PR worktree holding a leftover uncommitted index a plain clean can't touch, and prints the manual remove command. `[--all-roots]` additionally evaluates worktrees **outside** `worktree_root` — the collision engine scans those, so a legacy worktree root can hard-block pushes that a default clean never clears (all the data-loss guards still apply) |
| `wt claim <issue>` | Assign a GitHub issue, make a worktree, open a draft PR, record the claim `[--force] [--no-pr] [--epic <id>]` |
| `wt release <issue>` | Drop the claim. `[--clean]` also removes the worktree when the branch is abandoned (clean tree, no live PR, WIP-only commits) |
| `wt merge-pr <pr>` | Guarded squash-merge (PR-state precheck, strips a `WIP:` subject, refuses an empty/placeholder-only PR), then auto-removes the worktree + claim. Lints the closing keywords the squash will fire (PR body **and** commit bodies) and verifies issue state after `[--dry-run] [--bypass] [--merge-foreign] [--keep] [--confirm-deploy] [--admin] [--close-ok]` |
| `wt todos` | What every window is working on (mirrors each window's TODO list) |
| **— cross-window coordination —** | |
| `wt announce "<msg>"` | Tell other windows a change is starting `[--hold "merge-main,…"] [--issue N]` |
| `wt inbox` | Un-acked announcements from other windows. `[--issue N]` also reads back the cross-machine mirror `[--json]` |
| `wt ack <id>` | Acknowledge one `[--state "what this window is touching"]` |
| `wt all-clear <id>` | Release your hold |
| `wt holds` | YOUR outstanding announcements/holds + block reservations, with copy-pasteable all-clear lines |
| `wt prune-coord` | GC the coordination log — drop resolved handshakes + aged block reservations `[--block-max-age D]` |
| `wt block-id <file>` | Atomically reserve the next append-log id so two windows never grab the same `NEWEST-N`. `--written N` marks a reservation done (clears the banner; frees it if never written) `[--pattern] [--format]` |
| `wt append <doc> --section H "txt"` | Locked, section-scoped append to a structured shared doc (parallel adds can't clobber) |
| `wt install-hooks` | Install pre-push (base guard + collision check + base-conflict warning) + pre-commit (collision notice) `[--force]` |
| `wt install-claude-hook` | Wire a Claude Code **PreToolUse** hook so the AI agents doing the editing get collision-checked per edit `[--write]` (see [Agents](#agents-claude-code--codex)) |
| `wt install-codex-hook` | Wire a Codex **UserPromptSubmit** hook so it gets multi-window collision awareness each turn `[--write]` (see [Agents](#agents-claude-code--codex)) |
| `wt doctor` | Check git/gh + all resolved config + structured-doc regex + coordination-log health + preflight, and flag worktrees that track the base branch, a stale far-behind base checkout, and whether the hooks are installed `[--json]` |
| `wt version` | Print the version |
| `wt help` | Colorful overview |

Structured shared docs (`structured_doc.<name>` in config) upgrade the blanket
"shared doc — advisory" to **section-aware** grading: two windows editing the
**same** section is HIGH, disjoint sections stay advisory. The installed binary
also nudges (once/day, best-effort) when a newer `wt` is available on the remote
— silence with `WT_NO_UPDATE_CHECK=1`.

## Typical multi-window flow

```
window A   wt claim 42          window B   wt claim 51          window C   wt new spike/x
anytime    wt status            # see overlaps before they become merge conflicts
before a   wt check internal/foo.go   # "is anyone else in here?"  exit 3 = yes
big edit
done       gh pr ready … && wt merge-pr 60
```

Each `claim`/`new` creates an isolated worktree (default
`<repo-parent>/<repo>-worktrees/<slug>`), so a `git checkout` in one window can
never poach another window's HEAD. Claims are recorded in a shared file inside
`$GIT_COMMON_DIR` (visible to every worktree of the repo, never committed) — no
external service, no Claude Code dependency.

## Merging, auto-cleanup, and merge == deploy

`wt merge-pr <pr>` does the guarded squash (refusing an empty or
placeholder-only PR) and then, since the work has shipped, **auto-removes that
PR's worktree and local branch**. Guarded: it only removes a worktree under the
configured root (never your primary or a foreign checkout) and never one with
uncommitted changes. `--keep` opts out; if you were sitting inside the removed
worktree it prints a `cd` hint back. `wt clean -y` sweeps any already-shipped
worktrees the same way.

In a **GitOps repo where merging to base auto-applies to prod** (Flux/Argo
reconcile on push), that squash is far higher-stakes than normal. Set
`merge_is_deploy = true` and `wt merge-pr`:

- refuses to merge a **draft** PR,
- prints a `⚠ merging … AUTO-APPLIES to prod` banner,
- requires a deliberate confirm — a typed `deploy` at an interactive prompt, or
  `--confirm-deploy` for non-interactive/agent use (never a silent default).

## Cross-repo epics

A logical change often spans repos (a build PR in repo A gates a deploy PR in
repo B). Tag related claims with `wt claim <issue> --epic <id>`, then:

```
wt status --epic <id>       # (add --json for structured output)
```

aggregates every claim carrying that tag across the current repo **and its
sibling repos** (git repos under the shared parent dir), showing each unit's
repo, branch, and live PR state (`OPEN` / `DRAFT` / `MERGED` / `CLOSED`,
resolved via the recorded PR URL) — so you can see `A #NNN MERGED → B #MMM DRAFT`
in one view.

## Hooks

`wt install-hooks` writes two thin shims into the repo's shared hooks dir
(covers all worktrees):

- **pre-push** — three checks, in cost order:
  - rejects a direct push to the base branch (`main`/`master`/…) — bypass
    `HOOK_DISABLE_MAIN_PUSH=1 git push`;
  - warns (offline, via `git merge-tree`) when the branch **conflicts with
    base** — a conflicting PR gets *zero* CI runs (GitHub can't build
    `refs/pull/N/merge`), which looks identical to a cold runner pool; a
    clean-but-behind branch gets a one-line "behind by N";
  - **blocks** (last moment before a duplicate PR) when the *outgoing* paths
    overlap another active window's live hunks — same HIGH grading as
    `wt check`. Bypass: `WT_SKIP_COLLISION=1 git push`.
- **pre-commit** — non-blocking collision notice (staged files overlapping other
  windows). Bypass: `HOOK_DISABLE_MULTIWINDOW_CHECK=1`.

Both hooks strip git's ambient `GIT_DIR`/`GIT_INDEX_FILE`/`GIT_WORK_TREE` before
the cross-worktree scan (git sets those for a running hook, pinned to the
invoking worktree — without stripping them every window looks like it holds the
invoking worktree's changes), while preserving them for the invoking worktree's
own staged read so a partial commit (`git commit -a`/`-p`/`--only`) is graded
correctly.

If your repo uses the [pre-commit](https://pre-commit.com) framework, `wt`
detects it and prints a `repo: local` snippet to add instead of clobbering the
framework's managed hook.

## Agents (Claude Code + Codex)

The whole point of the collision engine is worktree-creator-agnostic: `wt
status`/`check` enumerate `git worktree list`, so a worktree an agent spawns
(Claude Code's native `--worktree`, say) is visible to `wt` for free. But the
usual failure mode is that the *agents doing the editing* never run `wt check`.

`wt install-claude-hook` closes that. It wires a Claude Code **PreToolUse** hook
(matcher `Edit|Write|MultiEdit`) that runs the same collision grading as
`wt check` on the file an agent is about to touch:

```
wt install-claude-hook            # prints the .claude/settings.json snippet
wt install-claude-hook --write    # merges it in (never clobbers existing hooks)
```

- **Advisory by default** — a HIGH cross-worktree overlap is returned to the
  agent as context (`additionalContext`), so it *sees* the collision and can
  coordinate, without being halted.
- **`WT_CLAUDE_HOOK_BLOCK=1`** turns a HIGH into a hard `deny`.
- **Cheap** — a repo with ≤1 worktree is skipped instantly (solo repos pay
  nothing per edit). Bypass with `WT_SKIP_COLLISION=1`; fail-open on any error
  (a coordination nicety must never disrupt the session).

So when the agent in worktree A goes to edit the exact hunk of `foo.go` that
worktree B is live-editing, it's told — instead of both landing competing PRs.

### Codex

Codex is already a first-class window with **zero** extra setup: the collision
engine enumerates `git worktree list`, so a worktree Codex is working in shows in
`wt status`, and Codex's `git commit` / `git push` (run through its shell tool)
trip `wt`'s pre-commit / pre-push guards like anyone else's. What Codex *can't* do
is a Claude-style per-edit hook — its `PreToolUse` fires on the **shell tool
only** (`apply_patch` edits don't fire it) and only acts on `deny`, not advisory
context ([openai/codex#19385](https://github.com/openai/codex/issues/19385)). So
`wt install-codex-hook` uses the surface Codex *does* have —
**`UserPromptSubmit`**, which injects `additionalContext` — to tell Codex, each
turn, which files other live windows are editing:

```
wt install-codex-hook             # prints the .codex/hooks.json snippet
wt install-codex-hook --write     # merges it into .codex/hooks.json
```

Codex hooks are **opt-in** — enable them once in `~/.codex/config.toml`:

```toml
[features]
hooks = true
```

- Injected **only when** another live window overlaps a file (silent otherwise),
  with the current window excluded and a `wt check <file>` reminder.
- Fail-open; `WT_SKIP_COLLISION=1` to silence; ≤1-worktree repos skipped.

The awareness is proactive-but-coarse (per prompt, not per edit) because that's
what Codex exposes — but the pre-push guard is the hard gate for both agents, so
nothing lands a competing push regardless of which agent is driving.

## Configuration

Zero-config works by derivation. Override via a repo-root `.wt.conf`
(`key=value`) or environment variables (env wins):

| `.wt.conf` key | env | default |
|---|---|---|
| `base` | `WT_BASE` | derived from `origin/HEAD` (main → master fallback) |
| `worktree_root` | `WT_WORKTREE_ROOT` | `<repo-parent>/<repo>-worktrees` |
| `active_work` | `WT_ACTIVE_WORK` | `$GIT_COMMON_DIR/wt-active-work.md` |
| `prefix` | `WT_PREFIX` | `feat-` (claim branch prefix) |
| `link_files` | `WT_LINK_FILES` | `.env` (gitignored files symlinked into new worktrees) |
| `claim_open_pr` | `WT_CLAIM_OPEN_PR` | `true` |
| `shared_docs` | `WT_SHARED_DOCS` | `CLAUDE.md,MEMORY.md` (advisory-only basenames; empty disables) |
| `structured_doc.<basename>` | *(config-file only)* | *(none)* — section-delimiter regex; the doc grades by SECTION (same section = HIGH). e.g. `structured_doc.CLAUDE.md = ^##\s` |
| `append_only_paths` | `WT_APPEND_ONLY_PATHS` | *(none)* — globs whose overlaps are always FYI (`**` matches any depth) |
| `max_age` | `WT_MAX_AGE` | *(off)* — dormancy threshold, e.g. `4d`, `2w`, `36h`, or a bare int (days) |
| `hold_max_age` | `WT_HOLD_MAX_AGE` | `24h` — a `--hold` older than this stops hard-blocking `merge-pr` (warns instead); `0`/`off` = never expire |
| `coord_issue` | `WT_COORD_ISSUE` | *(off)* — a pinned GitHub issue as the **cross-machine** mirror: announce/ack/all-clear auto-mirror to it, and `inbox` + the `merge-pr` gate read it back, so a hold on one machine blocks/warns on another |
| `merge_is_deploy` | `WT_MERGE_IS_DEPLOY` | `false` — enable the prod-deploy gate on `merge-pr` |

Color is auto-disabled when stdout isn't a TTY; force off with `NO_COLOR=1`.

## Requirements

- `git`
- `gh` (GitHub CLI), authenticated — only for `claim` / `release` / `merge-pr`.
  `new` / `clean` / `status` / `check` / `install-hooks` need only `git`.

Run `wt doctor` to check.

## License

MIT — see [LICENSE](LICENSE).
