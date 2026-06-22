# wt — multi-window git coordination

Run **3 or 4 windows on different things in the same repo** without stepping on
each other. `wt` gives each window its own worktree, lets a window *claim* a
unit of work so the others can see it, and — the core — tells you when two
windows are **touching the same files**, before that becomes a merge conflict.

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
wt status            # every window, the files each is touching, and overlaps
wt check <paths…>    # before you edit: is anyone else in these files?
```

`wt status` ends with either `✓ no file collisions … all clear` or a `💥` list
of files touched by more than one window. `wt check` exits **3** when another
window is already in one of the given paths (so a script — or an agent in one
window — can branch on "collision found"), **0** when clear.

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
| Commits not yet on base, no PR | **active** (latent) | `[commits, no PR]` |
| Clean worktree, no PR, nothing unshipped | **stale** → suppressed | `[stale: merged / no PR]` |

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
| `wt status` | All windows + the files each is touching + cross-window overlaps |
| `wt check <paths…>` | Is another window touching these paths? (exit 3 = collision) |
| `wt new <branch>` | Create a worktree on a new branch from the base branch |
| `wt clean` | List worktrees whose branch already shipped (prints safe `remove` cmds; never auto-deletes) |
| `wt claim <issue>` | Assign a GitHub issue, make a worktree, open a draft PR, record the claim `[--force] [--no-pr]` |
| `wt release <issue>` | Drop the claim (leaves the worktree + PR in place) |
| `wt merge-pr <pr>` | Guarded squash-merge — refuses empty or placeholder-only PRs `[--dry-run] [--bypass]` |
| `wt install-hooks` | Install pre-push (base-branch guard) + pre-commit (collision notice) `[--force]` |
| `wt doctor` | Check git/gh and show the resolved config |
| `wt help` | Colorful overview |

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

Color is auto-disabled when stdout isn't a TTY; force off with `NO_COLOR=1`.

## Requirements

- `git`
- `gh` (GitHub CLI), authenticated — only for `claim` / `release` / `merge-pr`.
  `new` / `clean` / `status` / `check` / `install-hooks` need only `git`.

Run `wt doctor` to check.

## License

MIT — see [LICENSE](LICENSE).
