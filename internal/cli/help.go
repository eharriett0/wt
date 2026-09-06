package cli

import (
	"fmt"

	"github.com/eharriett0/wt/internal/ui"
)

func printHelp() {
	b, c, d, g, y, m := ui.Bold, ui.Cyan, ui.Dim, ui.Green, ui.Yellow, ui.Magenta

	fmt.Println()
	fmt.Println("  " + m(b("wt")) + b(" — multi-window git coordination") + "  " + d("("+version()+")"))
	fmt.Println("  " + d("Run 3-4 windows on different things without stepping on each other."))
	fmt.Println()

	fmt.Println(b(y("  ✦ COLLISION AWARENESS")) + d("  — the core: who's touching what, right now"))
	fmt.Printf("    %s       %s\n", c("wt status [--json]"), "all windows + which files each is touching + graded overlaps  "+d("[--blocking] [--max-age D]"))
	fmt.Printf("    %s             %s\n", c("wt todos"), "what every window is working on (mirrors each window's TODO list)")
	fmt.Println("    " + d("fed by the ") + c("install-claude-hook") + d(" TodoWrite mirror — empty until that hook is wired AND the agent uses TodoWrite."))
	fmt.Printf("    %s   %s\n", c("wt check <paths…>"), "before you edit: is anyone in these files? "+d("[--show-diff] [--json] [--blocking] [--max-age D]"))
	fmt.Printf("    %s      %s\n", c("wt where <issue|branch>"), "print that window's worktree path — "+d("cd $(wt where 42)"))
	fmt.Println("    " + d("HUNK-LEVEL: overlapping line ranges = ") + y("HIGH") + d(" (exit 3); same file but"))
	fmt.Println("    " + d("disjoint hunks = low/FYI (exit 0) — no more crying wolf on parallel appends."))
	fmt.Println("    " + d("Suppressed by default: stale branches (merged / no PR) and — with ") + c("max_age") + d(" —"))
	fmt.Println("    " + d("dormant ones (unmerged but idle). ") + c("--include-stale") + d(" shows them."))
	fmt.Println("    " + d("shared docs (CLAUDE.md, MEMORY.md) + ") + c("append_only_paths") + d(" globs are advisory-only."))
	fmt.Println()

	fmt.Println(b(g("  ✦ WORKTREES")) + d("  — one isolated checkout per window"))
	fmt.Printf("    %s        %s\n", c("wt new <branch>"), "create a worktree on a new branch from the base")
	fmt.Printf("    %s              %s\n", c("wt init"), "scaffold a commented .wt.conf for this repo (derived defaults)  "+d("[--force]"))
	fmt.Printf("    %s              %s\n", c("wt clean"), "list worktrees whose branch already shipped  "+d("(-y to remove; --stale-index reports merged leftover-index ones; --all-roots also evaluates worktrees outside worktree_root, which still block pushes)"))
	fmt.Println()

	fmt.Println(b(g("  ✦ CLAIM A UNIT OF WORK")) + d("  — assign issue + worktree + draft PR + record"))
	fmt.Printf("    %s   %s\n", c("wt claim <issue>"), "claim a GitHub issue for this window  "+d("[--force] [--no-pr] [--epic <id>]"))
	fmt.Printf("    %s %s\n", c("wt adopt <branch|pr>"), "worktree on an EXISTING branch/PR (don't fork a new one) + record  "+d("[--epic <id>]"))
	fmt.Printf("    %s %s\n", c("wt release <issue>"), "drop the claim  "+d("[--clean = also remove an abandoned worktree]"))
	fmt.Println("    " + d("claim refuses (won't duplicate) when an OPEN PR already references the issue —"))
	fmt.Println("    " + d("including a plain ") + c("Refs #N") + d(" (never a linked/closing ref); it names that PR and"))
	fmt.Println("    " + d("points at ") + c("wt adopt") + d(". ") + c("--force") + d(" opens another anyway."))
	fmt.Printf("    %s %s\n", c("wt status --epic <id>"), "aggregate an epic's claims + PR states across sibling repos")
	fmt.Println()

	fmt.Println(b(g("  ✦ MERGE")) + d("  — guarded squash that refuses empty/placeholder-only PRs"))
	fmt.Printf("    %s     %s\n", c("wt merge-pr <pr>"), "guarded squash (surfaces head branch) + auto-remove worktree  "+d("[--dry-run] [--bypass] [--merge-foreign] [--keep] [--confirm-deploy] [--admin] [--close-ok] [--no-close-check]"))
	fmt.Println("    " + d("set ") + c("merge_is_deploy") + d(" for GitOps repos (merge auto-applies to prod): refuses"))
	fmt.Println("    " + d("a draft PR, banners the deploy, and requires a typed confirm / --confirm-deploy."))
	fmt.Println("    " + d("scope it with ") + c("merge_is_deploy_paths") + d(" (globs, ** ok) so the gate skips docs/CI/scripts-only PRs."))
	fmt.Println("    " + d("prefix a glob with ") + c("!") + d(" to carve out a class that can't deploy inside one that can (e.g. a README beside the code it documents)."))
	fmt.Println("    " + d("--admin forwards to gh pr merge to bypass a required-review branch (keeps the guard)."))
	fmt.Println("    " + d("--no-close-check skips the close-keyword lint + post-merge issue-state verify (#77) for a PR that intentionally closes nothing."))
	fmt.Println()

	fmt.Println(b(y("  ✦ CROSS-WINDOW COORDINATION")) + d("  — hand off disruptive changes (incidents, rolls, deploys)"))
	fmt.Printf("    %s  %s\n", c("wt announce \"<msg>\""), "tell other windows a change is starting  "+d("[--hold \"merge-main,…\"] [--issue N] [--file <path>]"))
	fmt.Printf("    %s                %s\n", c("wt inbox"), "un-acked announcements from other windows  "+d("[--json] [--issue N = read the cross-machine mirror]"))
	fmt.Printf("    %s           %s\n", c("wt ack <id>"), "acknowledge one  "+d("[--state \"what this window is touching\"] [--file <path>]"))
	fmt.Printf("    %s     %s\n", c("wt all-clear <id>"), "release your hold  "+d("(also: wt announce --clear <id>)"))
	fmt.Printf("    %s               %s\n", c("wt holds"), "YOUR outstanding announcements/holds + block reservations (with all-clear lines)")
	fmt.Printf("    %s          %s\n", c("wt prune-coord"), "GC the coordination log (drop resolved handshakes + aged block reservations)")
	fmt.Printf("    %s  %s\n", c("wt block-id <file>"), "reserve the next append-log id so two windows never grab the same NEWEST-N  "+d("[--pattern \"NEWEST-{n}\"] [--format] [--written N]"))
	fmt.Printf("    %s  %s\n", c("wt append <doc> --section H \"txt\""), "locked append under a section — parallel gotcha-adds can't clobber  "+d("[--file <path>]"))
	fmt.Println("    " + d("Prose with backticks / $ / ! ? Pass ") + c("--file <path>") + d(" (or ") + c("--file -") + d(" for stdin) — a shell-quoted"))
	fmt.Println("    " + d("argument expands those BEFORE wt sees it, silently deleting a `...` span (#75)."))
	fmt.Println("    " + d("Structured docs (") + c("structured_doc.<name>") + d(" = <section regexp> in .wt.conf) grade by"))
	fmt.Println("    " + d("SECTION: two windows editing the SAME section is ") + y("HIGH") + d(", disjoint sections stay advisory."))
	fmt.Println("    " + d("A ") + y("--hold") + d(" surfaces as a banner on other windows' next ") + c("status/new/check/claim") + d(","))
	fmt.Println("    " + d("and ") + c("merge-pr") + d(" REFUSES a held ") + y("merge-main") + d(" until you ack (or --bypass). Shared local"))
	fmt.Println("    " + d("log ~/.wt/coordination/<repo>.jsonl; ") + c("--issue") + d(" mirrors to a GitHub issue (cross-machine)."))
	fmt.Println("    " + d("Window identity = the worktree (stable across branch switches); export ") + c("WT_WINDOW"))
	fmt.Println("    " + d("to pin it across checkouts so a hold's creator is never blocked by its own hold (#18)."))
	fmt.Println()

	fmt.Println(b("  ✦ SETUP"))
	fmt.Printf("    %s        %s\n", c("wt install-hooks"), "install pre-push (base-branch guard) + pre-commit (collision notice)")
	fmt.Printf("    %s %s\n", c("wt install-claude-hook"), "wire Claude Code hooks (per-edit collision-check + TodoWrite mirror for `wt todos` + per-turn overlaps/coordination)  "+d("[--write]"))
	fmt.Printf("    %s  %s\n", c("wt install-codex-hook"), "wire Codex hooks (per-turn + per-edit apply_patch) for multi-window collision awareness  "+d("[--write]"))
	fmt.Printf("    %s                 %s\n", c("wt mcp"), "stdio MCP server exposing read-only tools (wt_status/check/todos/where) to any MCP client")
	fmt.Printf("    %s              %s\n", c("wt doctor"), "check git/gh + all resolved config + structured-doc regex + coord-log health + preflight  "+d("[--json]"))
	fmt.Printf("    %s                %s\n", c("wt help"), "this screen")
	fmt.Println()

	fmt.Println(d("  Typical multi-window flow:"))
	fmt.Println(d("    window A:  ") + c("wt claim 42") + d("   window B:  ") + c("wt claim 51") + d("   window C:  ") + c("wt new spike/x"))
	fmt.Println(d("    anytime:   ") + c("wt status") + d("   →  see overlaps before they become merge conflicts"))
	fmt.Println()

	fmt.Println(d("  Config: derived defaults → repo-root .wt.conf → env. Keys/vars: base, prefix,"))
	fmt.Println(d("  worktree_root, active_work, link_files, claim_open_pr, shared_docs, append_only_paths,"))
	fmt.Println(d("  structured_doc.<name>, max_age (e.g. 4d/2w), hold_max_age, coord_issue,"))
	fmt.Println(d("  merge_is_deploy[_paths] (! excludes)  (env = WT_<UPPER>)."))
	fmt.Println(d("  Color off: NO_COLOR=1.   More: https://github.com/eharriett0/wt"))
	fmt.Println()
}
