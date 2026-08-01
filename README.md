# wt — multi-window git coordination

Run **3 or 4 windows on different things in the same repo** without stepping on
each other. `wt` gives each window its own worktree, lets a window *claim* a
unit of work so the others can see it, and — the core — tells you when two
windows are **editing the same lines**, before that becomes a merge conflict.
It distinguishes real conflicts (overlapping hunks) from parallel appends to the
same file, quiets down dormant branches, cleans up worktrees when work ships,
and can gate merges in repos where merge == deploy-to-prod.

Repo-agnostic, single static binary, zero third-party dependencies (it shells
out to `git` and `gh`). Works in any git repo on any machine.

```
go install github.com/eharriett0/wt@latest
```

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
| Commits not yet on base, no PR | **active** (latent) | `[commits, no PR, last commit 4d ago]` |
| Commits, no PR, idle past `max_age` | **dormant** → suppressed | `[dormant, last commit 12d ago]` |
| Clean worktree, no PR, nothing unshipped | **stale** → suppressed | `[stale: merged / no PR]` |

The last-commit age is always shown on unmerged/dormant windows. **Dormancy** is
opt-in: set `max_age` (e.g. `4d`, `2w`, `36h`) and an unmerged-but-idle branch —
one you'd otherwise have to confirm out-of-band was abandoned — is suppressed
just like a merged one. A dirty or open-PR branch is never dormant (it's active
by definition).

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
| `wt check <paths…>` | Is another window touching these paths? `[--show-diff] [--json] [--include-stale] [--max-age D]` (exit 3 = HIGH) |
| `wt where <issue\|branch>` | Print that window's worktree path — `cd $(wt where 42)` |
| `wt new <branch>` | Create a worktree on a new branch from the base branch |
| `wt clean [-y]` | List worktrees whose branch already shipped (incl. squash-merged PRs); `-y` removes them, skipping dirty ones |
| `wt claim <issue>` | Assign a GitHub issue, make a worktree, open a draft PR, record the claim `[--force] [--no-pr] [--epic <id>]` |
| `wt release <issue>` | Drop the claim. `[--clean]` also removes the worktree when the branch is abandoned (clean tree, no live PR, WIP-only commits) |
| `wt merge-pr <pr>` | Guarded squash-merge (PR-state precheck, strips a `WIP:` subject), then auto-removes the worktree + claim `[--dry-run] [--bypass] [--merge-foreign] [--keep] [--confirm-deploy] [--admin]` |
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
| `wt install-hooks` | Install pre-push (base-branch guard) + pre-commit (collision notice) `[--force]` |
| `wt doctor` | Check git/gh and show the resolved config |
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

- **pre-push** — rejects a direct push to the base branch (`main`/`master`/…).
  Bypass: `HOOK_DISABLE_MAIN_PUSH=1 git push`.
- **pre-commit** — non-blocking collision notice (file overlaps with other
  windows). Bypass: `HOOK_DISABLE_MULTIWINDOW_CHECK=1`.

If your repo uses the [pre-commit](https://pre-commit.com) framework, `wt`
detects it and prints a `repo: local` snippet to add instead of clobbering the
framework's managed hook.

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
