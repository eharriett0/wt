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

`wt check` reads a path relative to your current directory and compares it as
that exact repo path: `wt check README.md` at the root asks about the root
`README.md`, never about `pkg/svc/README.md`. A bare name that is no path in the
repo is a search instead: `wt check foo.go` matches a touched file of that name
in any directory. A file another window moved counts by both its old and its
new path. Names are matched exactly as git stores them, non-ASCII ones
(`café.md`) and names with spaces, quotes, newlines or glob characters
(`a[1].md`) included.

In `wt check --json`, each entry's `path` is the repo-relative path that
collides, not the argument as you typed it: `wt check ./svc/README.md` run in
`pkg/` reports `pkg/svc/README.md`, and a directory argument lists one entry per
file under it. Only a search keeps the argument as typed (`foo.go`).

### Hunk-level, not just file-level

Append-heavy files — an image-inventory YAML, a kustomize `resources:` list, a
changelog — get edited by many windows at once where every edit is a disjoint
append; the real conflict risk is ~zero, yet a file-level check lights up a `💥`
wall on exactly the files touched most. `wt` diffs the **pending hunks** of each
window (`git diff -U0`, uncommitted ∪ committed since its merge base with
base) and grades the overlap:

```
config.yaml   — overlapping L88-95  → HIGH   (exit 3, blocks)
inventory.yaml — 6 windows, 0 overlapping hunks → low (FYI, exit 0)
```

Only **overlapping line ranges** (or an indeterminate case where your side has
no edits yet — kept blocking, to be safe) count as HIGH and drive exit 3.
Provably-disjoint hunks are downgraded to a non-blocking FYI. A branch that has
fallen behind base is graded on its own edits only: a line base added or changed
after it forked is never counted as that branch's edit, unless the branch's own
edit touches it (no unchanged line between), where git would conflict. Two
escape hatches
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

When no PR has the branch's own name, the branch's **tip commit** is looked up
instead, so work pushed under another name (an automation's PR branch, say) reads
as its real PR: `merged #N` when that commit is in a merged PR's final commits,
`open PR #N` when an open PR holds it. One difference from a name match: a tip
match does **not** outrank uncommitted edits. A follow-up branch started at a
merged PR's head looks the same as the merged work, so a dirty worktree stays
active, shown as `[uncommitted edits · its commits merged in #N]`. Commit or
discard the edits to clear it.

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
| `wt status [--json]` | All windows + files each touches + severity-graded overlaps. An overlap lists only live windows: merged, closed-PR and (with `max_age`) dormant ones are left out of its `windows`, and one left with fewer than two counts in `benign_count`. A file is HIGH when some pair of its windows would each block in `wt check`; a window whose copy is already on base, whose change already landed, or whose copy is untracked contests nothing, so two windows creating the same new file read advisory until one commits it. `[--blocking]` = only HIGH, exit 3 (a gate): it exits 0 when those were the only HIGHs. `[--max-age D]` |
| `wt status --epic <id>` | Aggregate an epic's claims + live PR states across sibling repos |
| `wt check <paths…>` | Is another window touching these paths? `[--show-diff] [--json] [--blocking] [--include-stale] [--allow-missing] [--max-age D]` (exit 3 = HIGH). `--blocking` prints only HIGH (a scriptable gate). Refuses a path that doesn't exist, isn't tracked, and no window is touching — a typo must never falsely report "clear" (`--allow-missing` opts into a deleted/other-branch/about-to-create path) |
| `wt where <issue\|branch>` | Print that window's worktree path — `cd $(wt where 42)` |
| `wt new <branch>` | Create a worktree on a new branch from the base branch |
| `wt clean [-y] [<name>...]` | List worktrees whose branch already shipped (incl. squash-merged PRs); `-y` removes them. Name worktrees (a directory, a branch, or a path) to limit the run to those, so one session can remove its own without touching the others the list shows. Every name must pick out exactly one worktree: one that matches nothing (a typo) or more than one (a directory name that is also another worktree's branch; use the path) stops the run with nothing cleaned. A branch whose commits were pushed under **another** name (e.g. onto an automation's PR branch) is recognised by its tip commit: shipped when that commit is in a merged PR's final commits. Never reaps, or lists as removable, a just-created worktree (grace window), a never-pushed branch (no upstream of its own: the `origin/<base>` upstream a `wt new` branch starts with is where it was cut from, not a push), or one with uncommitted changes, which it names with a file count. `[--stale-index]` also **reports** (never auto-removes) a merged-PR worktree holding a leftover uncommitted index a plain clean can't touch, and prints the manual remove command. `[--all-roots]` additionally evaluates worktrees **outside** `worktree_root` — the collision engine scans those, so a legacy worktree root can hard-block pushes that a default clean never clears (all the data-loss guards still apply) |
| `wt claim <issue>` | Assign a GitHub issue, make a worktree, open a draft PR, record the claim `[--force] [--no-pr] [--epic <id>]`. **Refuses (won't duplicate) when an open PR already references the issue** — including a plain `Refs #N` (which GitHub never treats as a linked/closing reference, so it's invisible to `closingIssuesReferences`); it names that PR and points at `wt adopt`. `--force` opens another anyway |
| `wt adopt <branch\|pr>` | Put a worktree on an **existing** branch (a colleague's or a previous session's PR branch) instead of forking a new one, and record it like `claim` — resolves a PR number to its head branch. This is the actionable half of `claim`'s refusal above, and the only command that lands a registered worktree on a branch you didn't just create `[--epic <id>]`. **Adopting by PR, `origin/<branch>` must carry the PR head first:** a PR from a fork (its head is on another repository, out of `git fetch origin`'s reach) or a failed fetch is refused before anything is checked out, created or moved, with a `gh pr checkout` recipe for the fork case. A local branch of that name is then compared with `origin/<branch>`: one that is only behind is fast-forwarded, one with unpushed commits on top is attached as it is with a note, and one that diverged (typically left over from an earlier PR that reused the name) is refused with both SHAs, never silently checked out. A re-run that finds the worktree already there is compared the same way but never moved: behind or ahead is handed back with a note, diverged is refused |
| `wt release <issue>` | Drop the claim. `[--clean]` also removes the worktree when the branch is abandoned (clean tree, no live PR, WIP-only commits) |
| `wt merge-pr <pr>` | Guarded squash-merge (PR-state precheck, strips a `WIP:` subject unless you forward `-- --subject`, refuses an empty/placeholder-only PR), then auto-removes the worktree + claim (also when `gh pr merge` fails after merging, as `-- -d` does when a worktree has the branch checked out). Lints the closing keywords the squash will fire: the PR body, plus the squash commit's **subject and body as GitHub will write them** (see [Merging](#merging-auto-cleanup-and-merge--deploy)), and verifies issue state after (skip both with `--no-close-check` for a PR that closes nothing) `[--dry-run] [--bypass] [--merge-foreign] [--keep] [--confirm-deploy] [--admin] [--close-ok] [--no-close-check]` |
| `wt todos` | What every window is working on (mirrors each window's TODO list) |
| **— cross-window coordination —** | |
| `wt announce "<msg>"` | Tell other windows a change is starting `[--hold "merge-main,…"] [--issue N]` |
| `wt inbox` | Un-acked announcements from other windows, and from another session working in this same checkout (labelled; see below). `[--issue N]` also reads back the cross-machine mirror `[--json]` |
| `wt ack <id>` | Acknowledge one `[--state "what this window is touching"]` |
| `wt all-clear <id>` | Release your hold |
| `wt holds` | YOUR outstanding announcements/holds + block reservations, with copy-pasteable all-clear lines |
| `wt prune-coord` | GC the coordination log — drop resolved handshakes + aged block reservations `[--block-max-age D]` |
| `wt block-id <file>` | Atomically reserve the next append-log id so two windows never grab the same `NEWEST-N`. `--written N` marks a reservation done (clears the banner; frees it if never written) `[--pattern] [--format]` |
| `wt append <doc> --section H "txt"` | Locked, section-scoped append to a structured shared doc (parallel adds can't clobber) |
| `wt install-hooks` | Install pre-push (base guard + collision check + base-conflict warning) + pre-commit (collision notice) `[--force]` |
| `wt install-claude-hook` | Wire Claude Code hooks (**PreToolUse** per-edit + **UserPromptSubmit** per-turn overlaps + coordination) `[--write]` (see [Agents](#agents-claude-code--codex)) |
| `wt install-codex-hook` | Wire Codex hooks (**UserPromptSubmit** per-turn awareness + coordination, and a **PreToolUse** `apply_patch` check that can deny a colliding edit) `[--write]` (see [Agents](#agents-claude-code--codex)) |
| `wt doctor` | Check git/gh + all resolved config + structured-doc regex + coordination-log health + preflight, and flag worktrees that track the base branch, a stale far-behind base checkout, and whether the hooks are installed `[--json]` |
| `wt version` | Print the version |
| `wt help` | Colorful overview |

**Window and session identity.** A *window* is a checkout: its worktree path,
stable across branch switches (`WT_WINDOW` pins it across checkouts). Inside one
checkout, each *session* is its own party: the Claude Code session
(`CLAUDE_CODE_SESSION_ID`), the Codex session (`CODEX_SESSION_ID`), or the value
of `WT_SESSION` (the same value in several shells makes them one session; a
distinct value per agent splits agents that export no session id of their own).
So two agents started in the same checkout see each other's announcements in
`inbox`, `holds` lists only your own, `merge-pr` honours the other session's
`merge-main` hold, and `doctor` / `status` warn that the two share ONE working
tree (where only `wt new` separates their edits). Claude Code keeps its session
id across `--resume` / `--continue`; `--fork-session`, `/clear` and a fresh
session get a new one, so a hold you placed before a `/clear` gates you after it
like another session's: `wt ack <id>` waives it for you only (`wt all-clear <id>`
would release it for every window). Terminal tabs are not sessions: a person's
tabs, and anything started in them, stay one party. A shell with no session id is
never silently merged with one that has: it sees that session's announcements,
and `inbox` hedges rather than reporting a confident "clear". Records written before
sessions existed keep the old window-only behaviour.

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
worktrees the same way. If `gh pr merge` fails *after* merging (`-- -d` does
when a worktree has the branch checked out), merge-pr sees the PR is MERGED and
finishes the job: the close verify and the auto-clean still run.

Before the squash, merge-pr lists every issue the merge will close and refuses
(`--close-ok` proceeds) when one is closed by text the PR's own closing
references don't show, or by phrasing that says it isn't meant ("does not fix
#N"). It reads the PR body plus the squash commit **as GitHub will write it**:

| | taken from |
|---|---|
| subject | a forwarded `--subject`/`-t` (or the `WIP:`-stripped PR title), else the repo's `squash_merge_commit_title`: the PR title, or with `COMMIT_OR_PR_TITLE` (GitHub's default) the commit's headline when the PR has one commit, merge commits not counted |
| body | a forwarded `--body`/`--body-file`/`-F -`, else `squash_merge_commit_message`: the PR body, the commit messages (one commit under the default: its body only), or nothing |

So `wt merge-pr <pr> -- --subject "…" --body "…"` drops a stray keyword from
both halves without force-pushing. When the settings can't be read, it reads
every subject and body the squash could carry: a false refusal costs a
`--close-ok`, a missed close shuts an issue silently. After the merge it
re-reads every issue a keyword in the PR title, body or commits would close,
shipped or not, and says which changed state.

In a **GitOps repo where merging to base auto-applies to prod** (Flux/Argo
reconcile on push), that squash is far higher-stakes than normal. Set
`merge_is_deploy = true` and `wt merge-pr`:

- refuses to merge a **draft** PR,
- prints a `⚠ merging … AUTO-APPLIES to prod` banner,
- requires a deliberate confirm — a typed `deploy` at an interactive prompt, or
  `--confirm-deploy` for non-interactive/agent use (never a silent default).

`wt merge-pr <pr> --dry-run` evaluates the same gate without prompting and says
what a real merge would do: refuse a draft, stop for the confirm, or proceed
because `--confirm-deploy` was passed. With `merge_is_deploy_paths` set, a PR
that touches no deploy path says it would skip the gate.

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

`wt install-claude-hook` closes that. It wires **two** Claude Code hooks:

- **`PreToolUse`** (matcher `Edit|Write|MultiEdit`) — runs the same collision
  grading as `wt check` on the file an agent is about to touch.
- **`UserPromptSubmit`** (`wt _hook claude-context`) — each turn, injects a
  snapshot of what other live windows are doing: cross-window file overlaps
  **plus** un-acked coordination signals — a `merge-main` hold another window
  placed, or an announcement you haven't acked (each with a `wt ack <id>`).
  A file you are editing lists the windows `wt check <file>` would (merged,
  closed-PR and, with `max_age`, dormant branches are left out) and reads HIGH
  only where `wt check` would block. One difference is deliberate: when your own
  copy of the file is already on base, or your change to it already landed, it
  reads "same file" while `wt check` still flags it as a pre-edit heads-up,
  because you hold nothing that can collide. A new file another window has not
  committed yet shows as advisory (untracked there) until it is committed, in
  `wt status` and every other window's banner; `wt check` and the banner in the
  window holding the untracked copy still flag it.

```
wt install-claude-hook            # prints the .claude/settings.json snippet (both hooks)
wt install-claude-hook --write    # merges both in (never clobbers existing hooks)
```

- **Advisory by default** — a HIGH cross-worktree overlap is returned to the
  agent as context (`additionalContext`), so it *sees* the collision and can
  coordinate, without being halted.
- **`WT_CLAUDE_HOOK_BLOCK=1`** turns a HIGH (per-edit) into a hard `deny`.
- **Cheap** — a repo with ≤1 worktree and no coordination log is skipped
  instantly (solo repos pay nothing); with a log, the per-turn hook still
  delivers another same-checkout session's announcements and every other
  window's hold. Bypass with `WT_SKIP_COLLISION=1`; fail-open on any error
  (a coordination nicety must never disrupt the session).

So when the agent in worktree A goes to edit the exact hunk of `foo.go` that
worktree B is live-editing, it's told — instead of both landing competing PRs;
and if B has announced a hold on `merge-main`, A sees it before it merges.

### Codex

Codex is already a first-class window with **zero** extra setup: the collision
engine enumerates `git worktree list`, so a worktree Codex is working in shows in
`wt status`, and Codex's `git commit` / `git push` (run through its shell tool)
trip `wt`'s pre-commit / pre-push guards like anyone else's. On top of that,
`wt install-codex-hook` wires **two** hooks so Codex gets the same collision
awareness a Claude window does:

- **`UserPromptSubmit`** (`wt _hook codex-context`) — injects `additionalContext`
  each turn: which files other live windows are editing, plus un-acked
  coordination signals (a `merge-main` hold or an announcement from another
  window). Same builder as the Claude `claude-context` hook.
- **`PreToolUse`** on `apply_patch` (`wt _hook codex-edit`) — grades the pending
  patch's target files with the same engine as `wt check`, and injects
  `additionalContext` when a hunk overlaps another live window. Codex's
  `PreToolUse` now fires on `apply_patch` and supports both advisory context and
  `permissionDecision: "deny"`.

```
wt install-codex-hook             # prints the .codex/hooks.json snippet (both hooks)
wt install-codex-hook --write     # merges both into .codex/hooks.json (idempotent)
```

Codex hooks are **enabled by default** (hook *definitions* still require
trust/review on first run). To turn them off entirely, set in
`~/.codex/config.toml`:

```toml
[features]
hooks = false
```

- Both hooks are injected **only when** another live window overlaps a file
  (silent otherwise), with the current window excluded and a `wt check <file>`
  reminder.
- The edit hook re-grades against the patch's actual hunks, moved into base line
  numbers through this worktree's own diff (so it stays exact when the worktree is
  behind base or already edited the file), so a **disjoint** patch to a shared
  file stays silent — no crying wolf on parallel appends.
- Advisory by default. Set `WT_CODEX_HOOK_BLOCK=1` to have the edit hook `deny`
  a **confirmed** HIGH overlap (a heads-up-only file-level match never denies).
- Fail-open; `WT_SKIP_COLLISION=1` to silence; ≤1-worktree repos skipped.

The awareness is proactive-but-coarse (per prompt, not per edit) because that's
what Codex exposes — but the pre-push guard is the hard gate for both agents, so
nothing lands a competing push regardless of which agent is driving.

### Any MCP client — `wt mcp`

For chat-style agents that speak [MCP](https://modelcontextprotocol.io) (Claude
Desktop, Cursor, and others) rather than shell hooks, `wt mcp` runs a stdio
server exposing wt's **read-only** surface as tools, so the agent can ask "who
else is in this file?" mid-conversation:

- **`wt_status`** — every active window + graded cross-window overlaps
- **`wt_check`** — before editing given paths: is another live window in them?
  Relative paths are read from the repo root, wherever the client started the
  server.
- **`wt_todos`** — what each window is working on
- **`wt_where`** — resolve an issue/branch to its worktree path

Each tool single-sources the SAME `collide.Scan` / `buildCheckReport` /
`gradeStatusOverlaps` pipeline the CLI uses, so the data never drifts from
`wt status --json`. Point your client at the command (run in the repo directory):

```jsonc
// e.g. an MCP client config
{ "mcpServers": { "wt": { "command": "wt", "args": ["mcp"] } } }
```

v1 is read-only by design — no tool mutates state; the write surface (claim,
announce, merge) stays in the CLI + git hooks.

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
