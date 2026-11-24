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

A path means what your shell would open: `wt check svclink/README.md` through a
symlinked directory checks the file it reaches, and a path that is itself a
symlink is checked as both the link and its target. A path inside another
worktree of the same repository is checked as the same repo path, and
`wt check .` at the top level checks the whole repository. On a
case-insensitive filesystem (macOS by default), `readme.md` is checked as the
`README.md` git tracks (only when both spellings are the same file there, and
`core.ignorecase` alone never folds a name that is on no disk). A
path outside every worktree of the repository is refused (exit 64) rather than
reported clear, and that includes a path through a symlink in the repository
that leads outside it (`ext/x.md` with `ext` linking to another directory),
which used to be checked as if it were a repo path.

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

Hunks are graded by git's own 3-way merge rule: only edits git would merge as
**one conflict region** count as HIGH and drive exit 3 — edits that overlap, or
that touch with no unchanged line between them (one window editing line 10 and
another line 11 conflict in git too). A pure insertion meets only an edit of a
line on either side of it, or another insertion at the same spot, so insertions
after lines 10 and 11 stay apart. Edits are placed where git's merge places them
(histogram diff, no indent heuristic, whatever your diff config says): an
inserted line that could sit in more than one spot, beside an equal one, is
graded at the spot the merge will use. An indeterminate case where your side has no
edits yet is kept blocking too, to be safe. Hunks with an unchanged line between
them are downgraded to a non-blocking FYI. A branch that has fallen behind base
is graded on its own edits only: a line base added or changed after it forked is
never counted as that branch's edit, unless the branch's own edit touches it (no
unchanged line between), where git would conflict. Two escape hatches make files
always-advisory regardless of hunks: `shared_docs` (basename match, default
`CLAUDE.md,MEMORY.md`) and `append_only_paths` (globs — changelogs, inventory
lists).

`wt check --show-diff` previews the *other* window's hunk ranges inline so you
can eyeball disjoint-ness; `wt check --json` / `wt status --json` emit the same
data structured (with a `severity` field and `blocking` flag) for tooling and
pre-push hooks. A range with `"Gap": true` is an insertion between lines `Start`
and `End`; `overlap_spans` gives the lines where two windows' edits meet (for
edits that only touch, the line on each side). `wt status --json` also carries
`session` (the reader's coordination session) and `other_sessions`: the other
sessions active in this checkout in the last 24h (coordination records, or
block ids reserved from this repository), as `wt doctor --json` reports them
(`sessions_unread` says the log could not be read, so the list is unknown
rather than empty). Two agents sharing one working tree look like one window
to the collision engine, so this is a sign of the second one. An agent whose
wt hooks are installed (`wt install-claude-hook`, `wt install-codex-hook`)
shows from its first prompt: its per-turn hook marks the session present at
most once an hour (a `presence` record, never shown as an announcement). A
session seen only through those marks is listed while its latest is under 2h
old, so the session after a `/clear` (a new id) isn't warned about its own
past for a day. Marks are written per prompt, at most hourly, so a session
drops out 1 to 2h after its last prompt (2h after its last mark), as in one
long turn, until it prompts again; a session with any other
record (an announcement, a reservation) stays listed for 24h. `wt
prune-coord` drops marks older than a day, which `wt doctor` lists without a
warning. One without the hooks shows only once it runs a coordination
command.

**A window wt can't read is named, never skipped.** When `git status` fails in
another worktree (its branch holds an object the repository lacks, or its
index can't be read; a `git add` running there does not), wt can't see that
window's uncommitted edits. `wt status` marks it `UNREAD` with git's reason
instead of `(no changes)`, and `wt check`, both edit hooks, the per-turn
context, pre-push and pre-commit name it, with any clear qualified as being
about the windows wt could read. Being unread never blocks, since no path can
be tied to it. A file the window committed is still graded, and as its
working copy can't be read, an overlap there is indeterminate and so HIGH,
like any grade wt can't compute; its tag then reads `working tree unread`, not
`uncommitted edits`. In JSON it is `unread` on the window (`wt status --json`, MCP
`wt_status`) and `unread_windows` (`window`, `worktree`, `error`) in `wt check
--json` and MCP `wt_check`. Run `git status` in that worktree to see what is
wrong. Two entries are not unread, since neither has a work tree to hold
edits: a worktree git lists as prunable (its directory deleted; `git worktree
prune` removes its record) and the bare repository of a bare-plus-worktrees
layout. A locked worktree whose directory is gone (an unmounted volume) is
unread: its edits still exist. In a `--separate-git-dir` repository git lists
the main work tree by its git directory; wt reads it from inside that work
tree, and other windows name it unread, since git does not say where it is.

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
| `wt new <branch>` | Create a worktree on a new branch from the base branch. A local branch of that name (say, one whose worktree directory went away) is re-attached instead, but only after it is compared with `origin/<branch>` as just fetched: equal is attached, one with unpushed commits on top is attached with a note naming them, one only behind is fast-forwarded (never while a worktree has it checked out), and one that diverged (typically left over from an earlier attempt that reused the name) is refused with both SHAs and the commits only it has, never silently resumed. A branch checked out in another worktree is refused, whatever its state. With no `origin/<branch>` (never pushed) it is attached as it is; one that was pushed before and that origin has since deleted (typically after its PR merged) is attached as it is with a warning, not compared with the stale `origin/<branch>`. A worktree already there for the branch is compared the same way but never moved: behind or ahead is handed back with a note, diverged is refused, and so is one that is not on the branch (another branch, or a detached HEAD). A directory at that path that is not a worktree of its own (say, a leftover one under a `worktree_root` inside the repo) is never taken for one |
| `wt clean [-y] [<name>...]` | List worktrees whose branch already shipped (incl. squash-merged PRs); `-y` removes them. Name worktrees (a directory, a branch, or a path) to limit the run to those, so one session can remove its own without touching the others the list shows. Every name must pick out exactly one worktree: one that matches nothing (a typo) or more than one (a directory name that is also another worktree's branch; use the path) stops the run with nothing cleaned. A branch whose commits were pushed under **another** name (e.g. onto an automation's PR branch) is recognised by its tip commit: shipped when that commit is in a merged PR's final commits. A merged PR reaps a worktree only when the branch's tip is the PR's head or an ancestor of it: a commit made in the worktree after the merge keeps it, and clean says how many commits the PR lacks. Never reaps, or lists as removable, a just-created worktree (grace window), a never-pushed branch (no upstream of its own: the `origin/<base>` upstream a `wt new` branch starts with is where it was cut from, not a push), or one with uncommitted changes, which it names with a file count (untracked files count, also in a repo set to `status.showUntrackedFiles=no`, and so does an edit git status skips: a file flagged `--assume-unchanged` or `--skip-worktree`, or checked out under `core.ignoreStat=true`), nor one whose ignored files hold a git repository (the removal would delete it, commits and all) or with a checked-out submodule (git refuses). `[--stale-index]` also **reports** (never auto-removes) a merged-PR worktree holding a leftover uncommitted index a plain clean can't touch, and prints the manual remove command. `[--all-roots]` additionally evaluates worktrees **outside** `worktree_root` — the collision engine scans those, so a legacy worktree root can hard-block pushes that a default clean never clears (all the data-loss guards still apply) |
| `wt discard <name> [--drop-commits] [--dry-run] [--all-roots]` | Drop **one** worktree you decided is throwaway (say a canary branch you pushed, used, then deleted on origin on purpose), with its local branch and the active-work claims recorded on that branch (only those): the exit `wt clean` rightly never takes for a branch whose work is on no branch origin has. Never a sweep: the name must pick out exactly one worktree, as for `wt clean <name>` (the main checkout counts too, so a name that also matches it is ambiguous). Refuses a dirty tree (uncommitted or untracked-but-not-ignored changes, an edit git status hides included, which it names), a git repository in its ignored files (git status never looks there, and the removal would delete it with all its commits), a checked-out submodule (git refuses to remove the worktree), the base branch, the main checkout, the window it runs in (by any path to it, a symlink or a differently-cased one), a detached or locked worktree, one with another worktree inside it, and one whose branch another worktree has checked out. A worktree whose directory is already gone (removed by hand) has nothing on disk to lose and goes through the same commit check. It fetches origin (never pruning) and lists the commits on the branch that no branch on origin has, asking origin itself, so an `origin/<branch>` left behind by a deletion on the web hides nothing; any such commit is dropped only with `--drop-commits`, and the plan says which of them another ref here (a local branch, a tag, the stash) still keeps and which become unreachable (the restore command is printed). When origin can't be asked, only `origin/<base>` as last fetched is trusted, so every other commit counts. The branch is deleted only at the tip that was listed, in one step (`git update-ref -d`). Never deletes the branch on origin: when origin still has it, it says so and prints `git push origin --delete <branch>`. `--dry-run` prints the plan and changes nothing but the fetch; `--all-roots` allows a worktree outside `worktree_root`, as for `wt clean`. Exit 64 on a usage error, 1 on a refusal (a dry run that a real run would refuse included) |
| `wt claim <issue>` | Assign a GitHub issue, make a worktree, open a draft PR, record the claim `[--force] [--no-pr] [--epic <id>]`. **Refuses (won't duplicate) when an open PR already references the issue** — including a plain `Refs #N` (which GitHub never treats as a linked/closing reference, so it's invisible to `closingIssuesReferences`); it names that PR and points at `wt adopt`. `--force` opens another anyway. Before it assigns anything, a local branch (or worktree) of the claim's name is checked against `origin/<branch>` exactly as `wt new` checks it, and a diverged one is refused; so is a worktree already there that is only behind, since a placeholder on it could not be pushed, a branch origin has deleted since it was pushed (the claim would push it back), and, with no local branch, a branch origin already has (pointing at `wt adopt`). The placeholder commit never takes work staged in a worktree it reuses, and refuses one in the middle of a merge. If anything fails after the assign, the claim rolls back only what it did: the issue is unassigned (unless it was yours before), and a branch it re-attached, or a worktree it reused, keeps its commits, minus the placeholder |
| `wt adopt <branch\|pr>` | Put a worktree on an **existing** branch (a colleague's or a previous session's PR branch) instead of forking a new one, and record it like `claim` — resolves a PR number to its head branch. This is the actionable half of `claim`'s refusal above, and the only command that lands a registered worktree on a branch you didn't just create `[--epic <id>]`. **Adopting by PR, `origin/<branch>` must carry the PR head first:** a PR from a fork (its head is on another repository, out of `git fetch origin`'s reach) or a failed fetch is refused before anything is checked out, created or moved, with a `gh pr checkout` recipe for the fork case. A local branch of that name is then compared with `origin/<branch>`: one that is only behind is fast-forwarded, one with unpushed commits on top is attached as it is with a note, and one that diverged (typically left over from an earlier PR that reused the name) is refused with both SHAs, never silently checked out. A re-run that finds the worktree already there is compared the same way but never moved: behind or ahead is handed back with a note, diverged is refused |
| `wt release <issue>` | Drop the claim. `[--clean]` also removes the worktree when the branch is abandoned (clean tree, no live PR, WIP-only commits) |
| `wt merge-pr <pr>` | Guarded squash-merge (PR-state precheck, refuses while the PR's checks are pending or failed, or a check or workflow its branch requires never ran — `--checks-ok` proceeds, see [Merging](#merging-auto-cleanup-and-merge--deploy) — strips a `WIP:` subject unless you forward `-- --subject`, refuses an empty/placeholder-only PR), then auto-removes the worktree + claim (also when `gh pr merge` fails after merging, as `-- -d` does when a worktree has the branch checked out). Lints the closing keywords the squash will fire: the PR body, plus the squash commit's **subject and body as GitHub will write them** (see [Merging](#merging-auto-cleanup-and-merge--deploy)), and verifies issue state after (skip both with `--no-close-check` for a PR that closes nothing) `[--dry-run] [--bypass] [--merge-foreign] [--keep] [--confirm-deploy] [--admin] [--close-ok] [--checks-ok] [--no-close-check]` |
| `wt todos` | What every window is working on (mirrors each window's TODO list) |
| **— cross-window coordination —** | |
| `wt announce "<msg>"` | Tell other windows a change is starting `[--hold "merge-main,…"] [--issue N]` |
| `wt inbox` | Un-acked announcements from other windows, and from another session working in this same checkout (labelled; see below). `[--issue N]` also reads back the cross-machine mirror `[--json]` |
| `wt ack <id>` | Acknowledge one `[--state "what this window is touching"]` |
| `wt all-clear <id>` | Release your hold — for EVERY window. A hold carrying this session's own token is released at once (also one placed from another checkout); anything else (another session's or window's, one with no session recorded, and a shell's own hold when it has no session token, which wt can't prove) asks first on a terminal, and otherwise refuses unless you pass `--not-mine` |
| `wt holds` | YOUR outstanding announcements/holds + block reservations, with copy-pasteable all-clear lines (a token-less shell's own are marked: wt can't prove them); this checkout's records wt can't attribute to any session are listed apart, without them, every hold that still gates included. Reads the mirror like `inbox` `[--issue N]` |
| `wt prune-coord` | GC the coordination log: drop resolved handshakes, aged block reservations `[--block-max-age D]` and presence marks older than a day. `--legacy` lists the open announcements written before wt recorded sessions that gate or show nothing for anyone any more; `--legacy -y` drops them with their acks (a one-time cleanup after upgrading) |
| `wt block-id <file>` | Atomically reserve the next append-log id so two windows never grab the same `NEWEST-N`. `--written N` marks a reservation done once block N is in the file (clears the banner), `--abandon N` frees one you won't write; a reservation never marked frees its id by itself after 30m `[--pattern] [--format]`. A reservation is its session's, from any checkout: another session can't mark or abandon it (wt names whose it is) and sees it in its status banner instead. One made with no session token (a plain shell) can be abandoned by any token-less shell in the checkout, so `--abandon` asks first, or needs `--not-mine`. After upgrading, a reservation made by an older wt has no session: no one can mark it, and it frees itself after 30m |
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
(`CLAUDE_CODE_SESSION_ID`), the Codex session (`CODEX_SESSION_ID`, Codex 0.148.0
or later; see the Codex section), or the value
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
and `inbox` hedges rather than reporting a confident "clear". Shells with no
session id fall back to the directory: in one checkout they are one party, so a
terminal is never blocked by its own hold, but wt can't prove such a record is
yours, so `wt all-clear` asks before releasing it (set `WT_SESSION` per shell to
tell shells apart and make their records provable). A record carrying your
session's token from another checkout reads "yours, placed from checkout X" and
`wt all-clear` releases it without asking; its hold still gates the other
checkouts' merges (subagents share their parent's session id, and each works its
own lane). A record wt can't attribute to any session (no session recorded) is
never "yours": `wt holds` lists it apart with no all-clear line, and `wt
all-clear` asks before releasing it. If it was written before this repo's log
first carried a session (pre-upgrade history), its hold gates `merge-pr` for up
to 24h and `inbox` shows it for 24h, then only counts it; an ack written that
way still keeps an older signal out of your inbox. If it was written after (a
wt without session support still running next to an upgraded one), it is
treated like another party's: its hold gates for `hold_max_age`, it stays in
`inbox`, and its acks waive nothing for anyone.

**wt detects, Claude talks.** Claude Code sessions on one machine can message
each other (cross-session messaging, Claude Code 2.1.224+), and pick the
session to message themselves. What an agent can't see is that another window
is editing the same lines, or which of two sessions in one checkout placed a
hold. So where wt reports one, it names the session to talk to, when that
session is running on this machine:

- **A collision** (`wt check`, both edit hooks, the per-turn hook) names the
  live sessions working in the other window: `notes.txt ← wb [active] (live:
  \`api-worker\`, idle; coordinate with SendMessage)`. A session counts as
  working in the window of its latest record from a wt command run there in
  the last day (announce, ack, all-clear, a block id), else of its latest
  presence mark. A presence mark comes from the per-turn hook and follows
  that hook's working directory, which is where the session was started
  unless it changed directory; a session started outside any repository
  writes none, and is named only through the wt commands it runs. A
  session that moved after its last wt command is named on that command's
  window until it runs another, for up to a day: a later mark can't be told
  from work elsewhere by path. Not the directory Claude Code lists either:
  that is where the session was started.
  This session is never named, and a session with no recent record in the
  repository is named on no collision.
- **An announcement or hold** (`inbox`, the per-turn hook, `merge-pr`'s hold
  gate, the status banner) names the live session that placed it, by its
  session id: `(live as \`api-worker\`, idle: ask it with SendMessage)`. A
  hold whose session is live is never called orphaned: wt says to ask that
  session, not to `wt all-clear` it.
- **`other_sessions`** (`status --json`, `doctor --json`, MCP `wt_status`)
  carries `name` and `status`, and the "checkout shared" warning names a live
  session in place of its id.

wt sends nothing itself. It reads `claude agents --json` (the documented
listing; the `claude` on PATH, else the one running the session), at most
once per command and only when it has another window's collision or record
to show, which costs about 140 ms. A session the listing doesn't show is never
treated as gone, and a listing wt can't read changes nothing. `WT_PEERS=0`
turns it off.

**Rollout.** Windows still running wt v0.1.33 or v0.1.34 behave as before until
they are upgraded: v0.1.33 records no session and exempts itself from every hold
placed in its own checkout; v0.1.34 treats a record with no session as every
session's own and releases any hold without asking. Upgrade every machine and
shell that shares a coordination log (or its mirror issue); after upgrading,
`wt prune-coord --legacy` lists the pre-upgrade backlog nothing needs any more.

**The log's lock.** Every write to a coordination log or block ledger takes the
file's lock, and so does `wt append` on its doc. If another wt process keeps it
(a `wt prune-coord` stopped with Ctrl-Z, a `wt block-id` stuck on a network
mount), the write says it is waiting after a second and gives up after 30s
with the file's path. A command that reads the log first (`ack`, `all-clear`,
`block-id --written`/`--abandon`) waits up to 5s more for that read, without a
word. Find the holder with `lsof <path>` and resume it (`fg`, or `kill -CONT
<pid>`); don't kill it, since a prune killed mid-rewrite leaves the log empty.
On a filesystem that can't lock at all (an NFS mount with `nolocks`, some FUSE
or SMB mounts), announce, ack, all-clear and `block-id --written`/`--abandon`
write without the lock and warn once; block reservations, `wt prune-coord` and
`wt append` need the lock and fail there. A lock service that is down (NFS
lockd unreachable: "no locks available") is not that: every write fails, and
`wt merge-pr`'s hold gate refuses, until it is back, since writing or reading
unlocked then could lose a hold. The lock guards processes on one machine:
machines sharing `~/.wt` over a network mount where only some of them can lock
are not protected from each other.

Structured shared docs (`structured_doc.<name>` in config) upgrade the blanket
"shared doc — advisory" to **section-aware** grading: two windows editing the
**same** section is HIGH, disjoint sections stay advisory. A window behind the
base branch is read in the base's copy too, so a heading the base renamed,
added or removed after the window forked can't hide a conflict. The
installed binary also nudges (once/day, best-effort) when a newer `wt` is available on the remote
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
uncommitted changes, or with a git repository in its ignored files. `--keep` opts out; if you were sitting inside the removed
worktree it prints a `cd` hint back. `wt clean -y` sweeps any already-shipped
worktrees the same way. Both keep a worktree whose branch has commits the
merged PR doesn't (made after the push, say), and delete the branch only at the
commit they checked, so a commit landing meanwhile is never deleted with it. A worktree that never ships because you threw it away
(a canary, a spike) is yours to drop by name with `wt discard <name>`, which
lists any commits that would go and drops them only with `--drop-commits`. If `gh pr merge` fails *after* merging (`-- -d` does
when a worktree has the branch checked out), merge-pr sees the PR is MERGED and
finishes the job: the close verify and the auto-clean still run.

Right after its PR-state precheck, merge-pr reads the checks on the PR's head
commit and what its base branch requires (branch protection and rulesets, read
from the PR's own base repository), and prints a summary line, `--dry-run`
included, so a clean dry run means the checks were read: `checks=green`, or
`checks=none` when no check ran:

```
merge-pr: PR #60 checks=green on 1a2b3c4d5e6f: 12 passed, 2 skipped; required: 3, all reported
```

It refuses (asks, at a terminal; a dry run says what a real merge would do)
when a check is pending; when one failed, errored, was cancelled, timed out or
needs action (`--admin` bypasses GitHub's required checks too, so nothing else
would stop a red one); when a required check never reported on the head (CI
never started), or, required from one app, only another app reported it; when
a workflow a ruleset requires never ran; when fewer checks ran than
`merge_min_checks`; and when the checks, or the required ones, can't be read.
A failure stays a failure until a later run of that check passes: a run a label
event started that skipped the job, or one that was cancelled, does not clear
it. `--checks-ok` merges anyway, and says so; `--bypass` and `--admin` never get
past it. A PR on which no check ran, in a repo that requires none and sets no
floor, merges with a "no check ran" note: whether a workflow should run on a PR
is in its triggers (paths, labels, `if:`), which wt can't evaluate, so a
required check or `merge_min_checks` is what makes "CI never started" refuse.
The merge is pinned to the commit whose checks were read
(`--match-head-commit`): push after the read and GitHub refuses it rather than
ship a head nobody checked. A forwarded `-R`/`--repo` names the PR's
repository, so that PR is the one read and pinned, and every other read too:
the state, the guard, the close check (its issues and squash settings are that
repository's) and the merge itself. A PR given by its URL is read the same
way. merge-pr refuses when it can't resolve the PR there, and auto-clean
leaves this clone alone unless the PR's repository or its branch's is this
clone's origin (a fork origin merging upstream's PR still cleans its lane).
A forwarded flag that merges
nothing (`--disable-auto`, `--help`) skips the gate. `--auto` gets no
exemption: when the only checks pending or failing are ones no branch rule
requires, gh merges at once instead of waiting for them. A dry run's verdict
line carries the result too (`verdict=ok checks=blocked`).

The gates run in this order: the PR-state precheck, the checks, the
merge==deploy confirm (below), another window's `merge-main` hold, the close
check, then the empty-diff, placeholder and foreign-lane guards.

When the account's GitHub REST quota is used up (it is shared by every session
and tool on the account, and GitHub then refuses REST calls until it lifts),
merge-pr says so and when it lifts, and reads what it can over GraphQL, which
has its own quota: the PR's state, its checks, and the diff's file count and
file list. The checks a branch requires can only be read over REST, so the
checks gate refuses them as unread, naming the rate limit (`--checks-ok`
merges anyway). A diff that can't be read at all is refused as unread, never
as empty, and `--bypass` is not the fix: run the merge again once the limit
lifts.

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
- for a PR on github.com, reads [GitHub's status page](https://www.githubstatus.com)
  and, right before the confirm, **warns** when the Actions component is not
  operational or an unresolved incident names Actions: each incident's name,
  status and last update, and the component's status. A deploy job started
  during an Actions incident can wait for a runner and be cancelled without
  running a step. It only warns: the confirm below is still the gate. The read
  gives up after 3 seconds, and a page it can't read (down, slow, not the
  expected JSON) costs one `could not check GitHub's status` line, never the
  merge. GitHub Enterprise hosts aren't on that page, so nothing is read for
  them. (`WT_GITHUB_STATUS_URL` points the read at another URL, for testing and
  diagnosis only.)
- requires a deliberate confirm — a typed `deploy` at an interactive prompt, or
  `--confirm-deploy` for non-interactive/agent use (never a silent default).

`wt merge-pr <pr> --dry-run` evaluates the same gate without prompting and says
what a real merge would do: refuse a draft, stop for the confirm, or proceed
because `--confirm-deploy` was passed; it prints the same status-page lines.
With `merge_is_deploy_paths` set, a PR that touches no deploy path says it
would skip the gate. A renamed file counts under both its old and its new
path, so moving a file out of a deploy path (which deletes it there) still
gates. If GitHub's REST API can't be read and its GraphQL file list shows a
rename, the old path is unknown, so the PR is gated.

**Where the gate settings come from.** `merge-pr` usually runs in the PR's own
worktree, whose `.wt.conf` is the branch being merged. So it reads its gate
settings (`merge_is_deploy`, `merge_is_deploy_paths`, `merge_min_checks`,
`hold_max_age`, `coord_issue`) from two places: this checkout (its `.wt.conf`
and the `WT_*` environment) and the `.wt.conf` on the PR's base branch, read
from GitHub at merge time. The stricter of the two applies:

- the deploy gate fires if either source turns `merge_is_deploy` on and gates
  the PR (each source's `!` exclusions apply to its own globs only);
- the higher checks floor holds;
- a hold lives for the longer `hold_max_age` (`0`, never expire, beats any
  age), and every `coord_issue` mirror named is read.

So a PR that turns the gate off, narrows the paths, lowers the floor or
shortens `hold_max_age` is still held to the base's settings, and a branch cut
before the base added a deploy glob is gated for it. A base branch without a
`.wt.conf` counts as the defaults, so `hold_max_age` stays at least 24h there
too. The environment can only tighten these settings, never loosen the base's:
`--confirm-deploy` and `--checks-ok` are the ways past them. (`wt holds` and
`wt prune-coord` still read only this checkout's `hold_max_age`.)

When GitHub can't be read (after one retry; a rate limit is not retried), `merge-pr` fetches the PR's base
branch into this clone and reads that copy (with a warning; if the fetch fails
too, the copy as last fetched, which may be stale), but only when the PR's
repository is this clone's `origin` (owner and name; the host is not compared,
since an ssh alias names it differently). In every other case (gh could not
name the PR's base, the PR is in another repository through `-R`, a URL,
`GH_REPO` or a fork's default, or this clone's copy can't be read, a symlinked
or unfetched `.wt.conf` included) it fails closed: the deploy gate gates,
saying the merge **may** auto-apply rather than that it does, the checks gate
refuses (at a terminal, asks) without `--checks-ok`, since the base's floor is
unknown, and holds keep at least the default `hold_max_age`. That can happen in
a repository that never set `merge_is_deploy`, when GitHub is unreachable. A
passthrough that merges nothing (`-- --disable-auto`, `-- --help`) reads
nothing from the base and passes neither the deploy nor the checks gate; the
hold gate still applies. `--dry-run` names both sources and what each one sets.

**Assert `merge_is_deploy_paths` in CI; wt can't derive it.** Which paths
deploy is derived state. Your reconciler's config (Flux Kustomizations, Argo
Applications) decides it, and so do the workflows that apply on a push to the
base branch. Both change as ordinary work, without touching `.wt.conf`. The
two ways the list can be wrong are not alike:

- a stale glob, matching something that no longer deploys, is loud: the gate
  stops a harmless PR, and someone fixes the list;
- a missing glob is silent: the gate reports that the PR changes no deploy
  path, and a change to a tree that started deploying merges without the
  typed confirm.

So derive the deploying set in CI from the same sources (each reconciler
entry that points into this repository; each workflow on a push to the base
that runs an apply), and fail the build when `merge_is_deploy_paths` does not
cover it. wt reads the list from the base branch, so a check that keeps the
base's list right keeps it right for every PR. A comment above the list
saying "keep this in step" is a note, not a control.

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
  grading as `wt check` on the file an agent is about to touch, graded on the
  file the edit will produce, so it says what `wt check` will say once the edit
  is made.
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
- The edit hook grades the file the patch will produce (apply_patch replayed,
  down to the newline it adds at the end of every file it writes), measured the
  way `wt check` measures it (so it stays exact when the worktree is behind base
  or already edited the file): a **disjoint** patch to a shared file stays
  silent — no crying wolf on parallel appends — and one that will collide is
  flagged before it is applied.
- Advisory by default. Set `WT_CODEX_HOOK_BLOCK=1` to have the edit hook `deny`
  a **confirmed** HIGH overlap (a heads-up-only file-level match never denies).
- Fail-open; `WT_SKIP_COLLISION=1` to silence; ≤1-worktree repos skipped.
- **Session identity needs Codex 0.148.0 or later**, the first release that
  exports `CODEX_SESSION_ID` to the commands Codex runs (openai/codex#37848).
  Under an older Codex your `wt` commands record no session, while the
  per-turn hook reads the session id Codex sends it, so the banner lists this
  session's own announcements as another session's (advisory, nothing
  blocks). The hook's presence mark carries that id too, so `wt status`
  and `wt doctor` in this session's shell warn that another session shares
  the checkout: it is this one. Upgrade Codex, or start it from a shell with
  its own `WT_SESSION` set: both your commands and the hooks read that first.

The awareness is proactive-but-coarse (per prompt, not per edit) because that's
what Codex exposes — but the pre-push guard is the hard gate for both agents, so
nothing lands a competing push regardless of which agent is driving.

### Any MCP client — `wt mcp`

For chat-style agents that speak [MCP](https://modelcontextprotocol.io) (Claude
Desktop, Cursor, and others) rather than shell hooks, `wt mcp` runs a stdio
server exposing wt's **read-only** surface as tools, so the agent can ask "who
else is in this file?" mid-conversation:

- **`wt_status`** — every active window + graded cross-window overlaps, and
  the other sessions active in this checkout (`other_sessions`). The server
  reads its session from its environment once, when the client starts it:
  after a `/clear` (a new session) it still answers as the old one, until the
  client restarts it, and a client that doesn't pass its session id to the
  server makes it answer as a token-less shell. `session` says which.
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
(`key=value`) or environment variables (env wins; for `merge-pr`'s gate settings the
base branch's `.wt.conf` counts too, and the stricter applies, see the merge section):

| `.wt.conf` key | env | default |
|---|---|---|
| `base` | `WT_BASE` | derived from `origin/HEAD` (main → master fallback). When neither `origin/<base>` nor `<base>` resolves, windows' committed changes can't be compared, only their uncommitted edits: `wt check` and pre-push say so (`base_unresolved` in `--json`) instead of a bare clear |
| `worktree_root` | `WT_WORKTREE_ROOT` | `<repo-parent>/<repo>-worktrees`. A relative value is read from the main checkout, so it names one directory from every window |
| `active_work` | `WT_ACTIVE_WORK` | `$GIT_COMMON_DIR/wt-active-work.md`. A relative value is read from the main checkout |
| `prefix` | `WT_PREFIX` | `feat-` (claim branch prefix) |
| `link_files` | `WT_LINK_FILES` | `.env` (gitignored files symlinked into new worktrees) |
| `claim_open_pr` | `WT_CLAIM_OPEN_PR` | `true` |
| `shared_docs` | `WT_SHARED_DOCS` | `CLAUDE.md,MEMORY.md` (advisory-only basenames; empty disables) |
| `structured_doc.<basename>` | *(config-file only)* | *(none)* — section-delimiter regex; the doc grades by SECTION (same section = HIGH). e.g. `structured_doc.CLAUDE.md = ^##\s` |
| `append_only_paths` | `WT_APPEND_ONLY_PATHS` | *(none)* — globs whose overlaps are always FYI (`**` matches any depth) |
| `max_age` | `WT_MAX_AGE` | *(off)* — dormancy threshold, e.g. `4d`, `2w`, `36h`, or a bare int (days) |
| `hold_max_age` | `WT_HOLD_MAX_AGE` | `24h` — a `--hold` older than this stops hard-blocking `merge-pr` (warns instead); `0`/`off` = never expire. `merge-pr` also reads it from the PR's base branch; the longer applies |
| `coord_issue` | `WT_COORD_ISSUE` | *(off)* — a pinned GitHub issue as the **cross-machine** mirror: announce/ack/all-clear auto-mirror to it, and `inbox` + the `merge-pr` gate read it back, so a hold on one machine blocks/warns on another. `merge-pr` also reads the base branch's, and both mirrors |
| `merge_is_deploy` | `WT_MERGE_IS_DEPLOY` | `false` — enable the prod-deploy gate on `merge-pr`. Also read from the PR's base branch at merge time; the stricter applies |
| `merge_is_deploy_paths` | `WT_MERGE_IS_DEPLOY_PATHS` | *(none: the whole repo deploys)*. Globs scoping the deploy gate; `!` excludes. Also read from the PR's base branch; a file gates if either source turns `merge_is_deploy` on and its list gates it |
| `merge_min_checks` | `WT_MERGE_MIN_CHECKS` | *(off)* — `merge-pr` refuses when fewer checks ran on the PR's head (skipped ones don't count): the "CI never started" floor for a repo that requires no check. A value that isn't a count makes `merge-pr` refuse until it's fixed. Also read from the PR's base branch; the higher floor holds |

Color is auto-disabled when stdout isn't a TTY; force off with `NO_COLOR=1`.

## Requirements

- `git`
- `gh` (GitHub CLI), authenticated — only for `claim` / `release` / `merge-pr`.
  `new` / `clean` / `status` / `check` / `install-hooks` need only `git`.

Run `wt doctor` to check. It judges each gh host by the account gh uses there,
and says "could not be verified", not "NOT authenticated", when a host timed out
or could not be reached. Telling an unreachable host from a rejected token needs
gh 2.81 or newer (`gh auth status --json`); older gh reports both as a failed
login, and doctor says so. With gh 2.81+ it also tells a host that refused the
check for a rate limit (logged in, but calls on that quota fail until it
lifts) from one whose login is broken.

## License

MIT — see [LICENSE](LICENSE).
