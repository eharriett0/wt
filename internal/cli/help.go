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
	fmt.Printf("    %s       %s\n", c("wt status [--json]"), "all windows + which files each is touching + graded overlaps")
	fmt.Printf("    %s             %s\n", c("wt todos"), "what every window is working on (mirrors each window's TODO list)")
	fmt.Printf("    %s   %s\n", c("wt check <paths…>"), "before you edit: is anyone in these files? "+d("[--show-diff] [--json]"))
	fmt.Println("    " + d("HUNK-LEVEL: overlapping line ranges = ") + y("HIGH") + d(" (exit 3); same file but"))
	fmt.Println("    " + d("disjoint hunks = low/FYI (exit 0) — no more crying wolf on parallel appends."))
	fmt.Println("    " + d("Suppressed by default: stale branches (merged / no PR) and — with ") + c("max_age") + d(" —"))
	fmt.Println("    " + d("dormant ones (unmerged but idle). ") + c("--include-stale") + d(" shows them."))
	fmt.Println("    " + d("shared docs (CLAUDE.md, MEMORY.md) + ") + c("append_only_paths") + d(" globs are advisory-only."))
	fmt.Println()

	fmt.Println(b(g("  ✦ WORKTREES")) + d("  — one isolated checkout per window"))
	fmt.Printf("    %s        %s\n", c("wt new <branch>"), "create a worktree on a new branch from the base")
	fmt.Printf("    %s              %s\n", c("wt clean"), "list worktrees whose branch already shipped  "+d("(-y to remove them)"))
	fmt.Println()

	fmt.Println(b(g("  ✦ CLAIM A UNIT OF WORK")) + d("  — assign issue + worktree + draft PR + record"))
	fmt.Printf("    %s   %s\n", c("wt claim <issue>"), "claim a GitHub issue for this window  "+d("[--force] [--no-pr] [--epic <id>]"))
	fmt.Printf("    %s %s\n", c("wt release <issue>"), "drop the claim (leaves worktree + PR in place)")
	fmt.Printf("    %s %s\n", c("wt status --epic <id>"), "aggregate an epic's claims + PR states across sibling repos")
	fmt.Println()

	fmt.Println(b(g("  ✦ MERGE")) + d("  — guarded squash that refuses empty/placeholder-only PRs"))
	fmt.Printf("    %s     %s\n", c("wt merge-pr <pr>"), "guarded squash + auto-remove worktree  "+d("[--dry-run] [--bypass] [--keep] [--confirm-deploy]"))
	fmt.Println("    " + d("set ") + c("merge_is_deploy") + d(" for GitOps repos (merge auto-applies to prod): refuses"))
	fmt.Println("    " + d("a draft PR, banners the deploy, and requires a typed confirm / --confirm-deploy."))
	fmt.Println()

	fmt.Println(b(y("  ✦ CROSS-WINDOW COORDINATION")) + d("  — hand off disruptive changes (incidents, rolls, deploys)"))
	fmt.Printf("    %s  %s\n", c("wt announce \"<msg>\""), "tell other windows a change is starting  "+d("[--hold \"merge-main,…\"] [--issue N]"))
	fmt.Printf("    %s                %s\n", c("wt inbox"), "un-acked announcements from other windows  "+d("[--json]"))
	fmt.Printf("    %s           %s\n", c("wt ack <id>"), "acknowledge one  "+d("[--state \"what this window is touching\"]"))
	fmt.Printf("    %s     %s\n", c("wt all-clear <id>"), "release your hold  "+d("(also: wt announce --clear <id>)"))
	fmt.Println("    " + d("A ") + y("--hold") + d(" surfaces as a banner on other windows' next ") + c("status/new/check/claim") + d(","))
	fmt.Println("    " + d("and ") + c("merge-pr") + d(" REFUSES a held ") + y("merge-main") + d(" until you ack (or --bypass). Shared local"))
	fmt.Println("    " + d("log ~/.wt/coordination/<repo>.jsonl; ") + c("--issue") + d(" mirrors to a GitHub issue (cross-machine)."))
	fmt.Println()

	fmt.Println(b("  ✦ SETUP"))
	fmt.Printf("    %s        %s\n", c("wt install-hooks"), "install pre-push (base-branch guard) + pre-commit (collision notice)")
	fmt.Printf("    %s              %s\n", c("wt doctor"), "check git/gh + show resolved config")
	fmt.Printf("    %s                %s\n", c("wt help"), "this screen")
	fmt.Println()

	fmt.Println(d("  Typical multi-window flow:"))
	fmt.Println(d("    window A:  ") + c("wt claim 42") + d("   window B:  ") + c("wt claim 51") + d("   window C:  ") + c("wt new spike/x"))
	fmt.Println(d("    anytime:   ") + c("wt status") + d("   →  see overlaps before they become merge conflicts"))
	fmt.Println()

	fmt.Println(d("  Config: derived defaults → repo-root .wt.conf → env. Keys/vars: base, prefix,"))
	fmt.Println(d("  worktree_root, active_work, link_files, claim_open_pr, shared_docs,"))
	fmt.Println(d("  append_only_paths, max_age (e.g. 4d/2w), merge_is_deploy  (env = WT_<UPPER>)."))
	fmt.Println(d("  Color off: NO_COLOR=1.   More: https://github.com/eharriett0/wt"))
	fmt.Println()
}
