// Package cli is the wt command router.
package cli

import (
	"flag"
	"fmt"
	"os"
	"path/filepath"
	"runtime/debug"
	"strings"

	"github.com/eharriett0/wt/internal/claim"
	"github.com/eharriett0/wt/internal/collide"
	"github.com/eharriett0/wt/internal/config"
	"github.com/eharriett0/wt/internal/doctor"
	"github.com/eharriett0/wt/internal/gitx"
	"github.com/eharriett0/wt/internal/hooks"
	"github.com/eharriett0/wt/internal/merge"
	"github.com/eharriett0/wt/internal/ui"
	"github.com/eharriett0/wt/internal/worktree"
)

// Version may be set via -ldflags at build time; otherwise it's resolved from
// the module version embedded by `go install module@vX.Y.Z`.
var Version = "dev"

func version() string {
	if Version != "dev" && Version != "" {
		return Version
	}
	if bi, ok := debug.ReadBuildInfo(); ok {
		if v := bi.Main.Version; v != "" && v != "(devel)" {
			return v
		}
	}
	return "dev"
}

// Main dispatches a subcommand and returns a process exit code.
func Main(args []string) int {
	if len(args) == 0 {
		printHelp()
		return 0
	}
	cmd, rest := args[0], args[1:]

	switch cmd {
	case "help", "-h", "--help":
		printHelp()
		return 0
	case "version", "-v", "--version":
		fmt.Printf("wt %s\n", version())
		return 0
	case "doctor":
		return doctor.Run(loadConfigOrNil())
	case "_hook":
		return runHook(rest)
	case "new":
		return cmdNew(rest)
	case "clean":
		return withConfig(func(c *config.Config) int {
			if err := worktree.Clean(c); err != nil {
				ui.Err("%v", err)
				return 1
			}
			return 0
		})
	case "claim":
		return cmdClaim(rest)
	case "release":
		return cmdRelease(rest)
	case "merge-pr":
		return cmdMergePR(rest)
	case "status":
		return withConfig(cmdStatus)
	case "check":
		return cmdCheck(rest)
	case "install-hooks":
		return cmdInstallHooks(rest)
	default:
		ui.Err("unknown command %q", cmd)
		fmt.Fprintln(os.Stderr, "run `wt help` for usage")
		return 64
	}
}

func loadConfigOrNil() *config.Config {
	c, err := config.Load()
	if err != nil {
		return nil
	}
	return c
}

func withConfig(fn func(*config.Config) int) int {
	c, err := config.Load()
	if err != nil {
		ui.Err("not inside a git repository (%v)", err)
		return 1
	}
	return fn(c)
}

func cmdNew(args []string) int {
	if len(args) < 1 || args[0] == "" {
		ui.Err("usage: wt new <branch>")
		return 64
	}
	return withConfig(func(c *config.Config) int {
		if _, err := worktree.New(c, args[0]); err != nil {
			ui.Err("%v", err)
			return 1
		}
		return 0
	})
}

func cmdClaim(args []string) int {
	fs := flag.NewFlagSet("claim", flag.ContinueOnError)
	force := fs.Bool("force", false, "claim even if the issue is already assigned")
	noPR := fs.Bool("no-pr", false, "skip opening a draft PR")
	if err := fs.Parse(args); err != nil {
		return 64
	}
	if fs.NArg() < 1 {
		ui.Err("usage: wt claim <issue> [--force] [--no-pr]")
		return 64
	}
	return withConfig(func(c *config.Config) int {
		openPR := c.ClaimOpenPR && !*noPR
		if err := claim.Claim(c, fs.Arg(0), *force, openPR); err != nil {
			ui.Err("%v", err)
			return 1
		}
		return 0
	})
}

func cmdRelease(args []string) int {
	if len(args) < 1 {
		ui.Err("usage: wt release <issue>")
		return 64
	}
	return withConfig(func(c *config.Config) int {
		if err := claim.Release(c, args[0]); err != nil {
			ui.Err("%v", err)
			return 1
		}
		return 0
	})
}

func cmdMergePR(args []string) int {
	fs := flag.NewFlagSet("merge-pr", flag.ContinueOnError)
	dryRun := fs.Bool("dry-run", false, "print the guard verdict without merging")
	bypass := fs.Bool("bypass", false, "merge despite a block verdict (rare)")
	if err := fs.Parse(args); err != nil {
		return 64
	}
	if fs.NArg() < 1 {
		ui.Err("usage: wt merge-pr <pr> [--dry-run] [--bypass] [-- extra gh args]")
		return 64
	}
	if err := merge.Run(fs.Arg(0), *dryRun, *bypass, fs.Args()[1:]); err != nil {
		return 1
	}
	return 0
}

func cmdStatus(c *config.Config) int {
	ws, err := collide.Scan(c)
	if err != nil {
		ui.Err("scan failed: %v", err)
		return 1
	}
	ui.Banner("wt status — " + filepath.Base(c.Root) + " (" + fmt.Sprintf("%d window(s)", len(ws)) + ")")
	for _, w := range ws {
		line := ui.Bold(w.Label()) + "  " + ui.Cyan(w.Branch)
		if w.Title != "" {
			line += "  " + ui.Dim(w.Title)
		}
		fmt.Println(line)
		fmt.Println("   " + ui.Dim(w.Worktree))
		if len(w.Touched) == 0 {
			fmt.Println("   " + ui.Dim("(no changes)"))
		} else {
			fmt.Printf("   %s %s\n", ui.Yellow(fmt.Sprintf("%d file(s):", len(w.Touched))), strings.Join(capList(w.Touched, 10), ", "))
		}
		fmt.Println()
	}

	ov := collide.Overlaps(ws)
	if len(ov) == 0 {
		ui.OK("no file collisions across %d window(s) — all clear", len(ws))
		return 0
	}

	// Classify the windows appearing in overlaps; an overlap is a real
	// collision only when ≥2 of its windows are non-stale.
	live := collide.ClassifyWindows(ws, c.Base, collide.OverlapWindowSet(ov))
	active, benign := collide.PartitionOverlaps(ov, live)

	if len(active) == 0 {
		ui.OK("no active collisions — %d file-overlap(s) are all on stale branches (merged / no open PR)", len(benign))
		return 0
	}
	ui.Collision("%d file(s) with an active multi-window collision:", len(active))
	for _, o := range active {
		fmt.Fprintf(os.Stderr, "   %s  %s %s\n", ui.Bold(o.File), ui.Dim("←"), strings.Join(taggedWindows(o.Windows, live), ", "))
	}
	if len(benign) > 0 {
		fmt.Fprintln(os.Stderr, ui.Dim(fmt.Sprintf("   +%d file-overlap(s) on stale branches only — not active collisions", len(benign))))
	}
	fmt.Fprintln(os.Stderr, ui.Yellow("   Coordinate on the active ones before committing."))
	return 0
}

// taggedWindows annotates each window label with a colored liveness badge,
// dimming stale ones so the live editors stand out.
func taggedWindows(windows []string, live map[string]collide.WindowLiveness) []string {
	out := make([]string, 0, len(windows))
	for _, w := range windows {
		wl := live[w]
		if wl.Level.IsStale() {
			out = append(out, ui.Dim(w+" "+wl.Badge()))
		} else {
			out = append(out, w+" "+wl.Badge())
		}
	}
	return out
}

func cmdCheck(args []string) int {
	// Manual scan (not flag.FlagSet): paths are positional and the flag may
	// appear anywhere — flag.Parse would stop at the first positional and miss
	// a trailing `--include-stale`.
	includeStale := false
	var paths []string
	for _, a := range args {
		switch a {
		case "--include-stale", "-include-stale":
			includeStale = true
		default:
			paths = append(paths, a)
		}
	}
	if len(paths) == 0 {
		ui.Err("usage: wt check [--include-stale] <path> [path...]")
		return 64
	}
	return withConfig(func(c *config.Config) int {
		ws, err := collide.Scan(c)
		if err != nil {
			ui.Err("scan failed: %v", err)
			return 1
		}
		root, _ := gitx.RepoRoot()
		conflicts := collide.CheckPaths(ws, root, paths)
		if len(conflicts) == 0 {
			ui.OK("clear — no other window is touching %s", strings.Join(paths, ", "))
			return 0
		}

		// Classify only the windows actually in a conflict, then split off the
		// stale ones (merged / no open PR — they can no longer change the file).
		live := collide.ClassifyWindows(ws, c.Base, collide.ConflictWindowSet(conflicts))
		active, stale := collide.PartitionConflicts(conflicts, live)
		if includeStale {
			active, stale = conflicts, nil
		}

		if len(active) == 0 {
			ui.OK("clear — %d path-overlap(s) are all on stale branches (merged / no open PR)", len(stale))
			for _, cf := range stale {
				fmt.Println("   " + ui.Dim(cf.Path+" ← "+cf.Window+" ["+live[cf.Window].Label()+"]"))
			}
			return 0
		}

		ui.Collision("%d path(s) being edited by an active window:", len(active))
		for _, cf := range active {
			fmt.Fprintf(os.Stderr, "   %s  %s %s %s\n",
				ui.Bold(cf.Path), ui.Dim("←"), cf.Window, live[cf.Window].Badge())
		}
		if len(stale) > 0 {
			fmt.Fprintln(os.Stderr, ui.Dim(fmt.Sprintf("   +%d more on stale branch(es) (merged / no open PR) — ignored; `--include-stale` to show", len(stale))))
		}
		return 3 // distinct exit code so a caller/script can branch on "collision found"
	})
}

func cmdInstallHooks(args []string) int {
	fs := flag.NewFlagSet("install-hooks", flag.ContinueOnError)
	force := fs.Bool("force", false, "back up + replace foreign hooks")
	if err := fs.Parse(args); err != nil {
		return 64
	}
	return withConfig(func(c *config.Config) int {
		if err := hooks.Install(c, *force); err != nil {
			ui.Err("%v", err)
			return 1
		}
		return 0
	})
}

func runHook(args []string) int {
	if len(args) < 1 {
		return 0 // unknown hook invocation — never block git
	}
	c, err := config.Load()
	if err != nil {
		// Outside a repo somehow — don't block git operations.
		return 0
	}
	switch args[0] {
	case "pre-push":
		return hooks.HookPrePush(c, os.Stdin)
	case "pre-commit":
		return hooks.HookPreCommit(c)
	default:
		return 0
	}
}

func capList(items []string, n int) []string {
	if len(items) <= n {
		return items
	}
	out := append([]string{}, items[:n]...)
	return append(out, fmt.Sprintf("…+%d more", len(items)-n))
}
