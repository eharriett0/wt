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
	fmt.Printf("    %s            %s\n", c("wt status"), "all windows + which files each is touching + overlaps")
	fmt.Printf("    %s             %s\n", c("wt todos"), "what every window is working on (mirrors each window's TODO list)")
	fmt.Printf("    %s   %s\n", c("wt check <paths…>"), "before you edit: is anyone else in these files? "+d("(exit 3 = collision)"))
	fmt.Println("    " + d("collisions on stale branches (merged / no open PR) are suppressed by"))
	fmt.Println("    " + d("default — only live windows count. ") + c("--include-stale") + d(" shows them all."))
	fmt.Println("    " + d("shared docs (CLAUDE.md, MEMORY.md) are advisory-only, never a block —"))
	fmt.Println("    " + d("set ") + c("WT_SHARED_DOCS") + d(" (CSV, empty to disable) to change the list."))
	fmt.Println()

	fmt.Println(b(g("  ✦ WORKTREES")) + d("  — one isolated checkout per window"))
	fmt.Printf("    %s        %s\n", c("wt new <branch>"), "create a worktree on a new branch from the base")
	fmt.Printf("    %s              %s\n", c("wt clean"), "list worktrees whose branch already shipped  "+d("(-y to remove them)"))
	fmt.Println()

	fmt.Println(b(g("  ✦ CLAIM A UNIT OF WORK")) + d("  — assign issue + worktree + draft PR + record"))
	fmt.Printf("    %s   %s\n", c("wt claim <issue>"), "claim a GitHub issue for this window  "+d("[--force] [--no-pr]"))
	fmt.Printf("    %s %s\n", c("wt release <issue>"), "drop the claim (leaves worktree + PR in place)")
	fmt.Println()

	fmt.Println(b(g("  ✦ MERGE")) + d("  — guarded squash that refuses empty/placeholder-only PRs"))
	fmt.Printf("    %s     %s\n", c("wt merge-pr <pr>"), "guarded squash-merge, then auto-removes the worktree  "+d("[--dry-run] [--bypass] [--keep]"))
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

	fmt.Println(d("  Config: derived defaults → repo-root .wt.conf → env (WT_BASE, WT_PREFIX,"))
	fmt.Println(d("  WT_WORKTREE_ROOT, WT_ACTIVE_WORK, WT_LINK_FILES, WT_CLAIM_OPEN_PR, WT_SHARED_DOCS)."))
	fmt.Println(d("  Color off: NO_COLOR=1.   More: https://github.com/eharriett0/wt"))
	fmt.Println()
}
