// Package cli is the wt command router.
package cli

import (
	"bufio"
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"runtime/debug"
	"strings"
	"time"

	"github.com/eharriett0/wt/internal/activework"
	"github.com/eharriett0/wt/internal/claim"
	"github.com/eharriett0/wt/internal/collide"
	"github.com/eharriett0/wt/internal/config"
	"github.com/eharriett0/wt/internal/doctor"
	"github.com/eharriett0/wt/internal/ghx"
	"github.com/eharriett0/wt/internal/gitx"
	"github.com/eharriett0/wt/internal/hooks"
	"github.com/eharriett0/wt/internal/merge"
	"github.com/eharriett0/wt/internal/todos"
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
		return cmdClean(rest)
	case "claim":
		return cmdClaim(rest)
	case "release":
		return cmdRelease(rest)
	case "merge-pr":
		return cmdMergePR(rest)
	case "status":
		return cmdStatus(rest)
	case "todos":
		return cmdTodos(rest)
	case "check":
		return cmdCheck(rest)
	case "install-hooks":
		return cmdInstallHooks(rest)
	case "announce":
		return cmdAnnounce(rest)
	case "inbox":
		return cmdInbox(rest)
	case "ack":
		return cmdAck(rest)
	case "all-clear":
		return cmdAllClear(rest)
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
		peerHoldBanner(c)
		if _, err := worktree.New(c, args[0]); err != nil {
			ui.Err("%v", err)
			return 1
		}
		return 0
	})
}

func cmdClean(args []string) int {
	fs := flag.NewFlagSet("clean", flag.ContinueOnError)
	apply := fs.Bool("y", false, "actually remove the shipped worktrees (default: list only)")
	fs.BoolVar(apply, "yes", false, "alias for -y")
	if err := fs.Parse(args); err != nil {
		return 64
	}
	return withConfig(func(c *config.Config) int {
		if err := worktree.Clean(c, *apply); err != nil {
			ui.Err("%v", err)
			return 1
		}
		return 0
	})
}

// parseInterspersed parses a FlagSet allowing flags to appear BEFORE or AFTER
// positional args. Go's flag package stops parsing at the first positional and
// silently drops any flag that follows it — a real footgun: `wt merge-pr 968
// --confirm-deploy` left --confirm-deploy unparsed (so the prod gate wasn't
// acknowledged) AND leaked "--confirm-deploy" downstream as a gh arg. We first
// split off a trailing `-- passthrough` (everything after the first standalone
// "--"), then loop fs.Parse, peeling one positional at a time. The loop handles
// value-taking flags correctly (e.g. `claim 913 --epic 43`): each Parse consumes
// the flag+its value before stopping at the next positional. On an undefined flag
// Parse errors (ContinueOnError) and we surface it — undefined flags no longer
// leak through as positionals. Use `--` for genuine downstream passthrough.
func parseInterspersed(fs *flag.FlagSet, args []string) (positionals, passthrough []string, err error) {
	for i, a := range args {
		if a == "--" {
			passthrough = append(passthrough, args[i+1:]...)
			args = args[:i]
			break
		}
	}
	for {
		if err = fs.Parse(args); err != nil {
			return nil, nil, err
		}
		if fs.NArg() == 0 {
			break
		}
		positionals = append(positionals, fs.Arg(0))
		args = fs.Args()[1:]
	}
	return positionals, passthrough, nil
}

func cmdClaim(args []string) int {
	fs := flag.NewFlagSet("claim", flag.ContinueOnError)
	force := fs.Bool("force", false, "claim even if the issue is already assigned")
	noPR := fs.Bool("no-pr", false, "skip opening a draft PR")
	epic := fs.String("epic", "", "tag this claim with a cross-repo epic id (wt status --epic)")
	pos, _, err := parseInterspersed(fs, args)
	if err != nil {
		return 64
	}
	if len(pos) < 1 {
		ui.Err("usage: wt claim <issue> [--force] [--no-pr] [--epic <id>]")
		return 64
	}
	return withConfig(func(c *config.Config) int {
		peerHoldBanner(c)
		openPR := c.ClaimOpenPR && !*noPR
		if err := claim.Claim(c, pos[0], *force, openPR, *epic); err != nil {
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
	mergeForeign := fs.Bool("merge-foreign", false, "merge a PR whose head branch has no wt worktree here (wt#15)")
	keep := fs.Bool("keep", false, "keep the worktree after merge (default: auto-remove it)")
	confirmDeploy := fs.Bool("confirm-deploy", false, "acknowledge merge auto-applies to prod (merge_is_deploy repos)")
	admin := fs.Bool("admin", false, "forward --admin to gh pr merge — maintainer bypass of a required-review branch (wt#20)")
	pos, ghArgs, err := parseInterspersed(fs, args)
	if err != nil {
		return 64
	}
	if len(pos) < 1 {
		ui.Err("usage: wt merge-pr <pr> [--dry-run] [--bypass] [--merge-foreign] [--keep] [--confirm-deploy] [--admin] [-- extra gh args]")
		return 64
	}
	pr := pos[0]
	// merge==deploy prod gate: in a repo where merging auto-applies (Flux/Argo
	// reconcile on push to base), require a deliberate ack before the squash.
	// Fail CLOSED — if we can't resolve config we can't verify merge_is_deploy,
	// so abort rather than risk an ungated prod deploy.
	c, err := config.Load()
	if err != nil {
		ui.Err("cannot resolve repo config (%v) — refusing merge; can't verify merge_is_deploy", err)
		return 1
	}
	if c.MergeIsDeploy && !*dryRun {
		if code := deployGate(pr, *confirmDeploy); code != 0 {
			return code
		}
	}
	// Cross-window coordination interlock (#13): refuse if another window holds
	// `merge-main` (a disruptive change in flight) and this window hasn't acked
	// it. --bypass overrides, matching the collision guard's escape hatch.
	if !*dryRun && !*bypass {
		if code := mergeCoordGate(c); code != 0 {
			return code
		}
	}
	// Foreign-branch guard input (wt#15): the wt-managed worktree branches for
	// this repo. Best-effort — if we can't enumerate them, merge.Run fails open
	// (surfaces the head branch but doesn't block).
	var wtBranches []string
	if c.WorktreeRoot != "" {
		wtBranches, _ = gitx.WorktreeBranchesUnder(c.WorktreeRoot)
	}
	// --admin forwards through to `gh pr merge` for the required-review-branch
	// maintainer bypass (wt#20). Appended only here — AFTER the deploy-gate +
	// coord + guard checks above — so it bypasses GitHub branch protection, not
	// wt's own safety checks (which is exactly the value the raw `gh` fallback
	// lost).
	ghArgs = merge.WithAdmin(*admin, ghArgs)
	if err := merge.Run(pr, *dryRun, *bypass, *mergeForeign, wtBranches, ghArgs); err != nil {
		return 1
	}
	// Auto-clean: the PR just shipped, so its worktree is done. Only on a real
	// merge (not dry-run) and unless -keep. Best-effort — a cleanup miss must
	// not fail the merge that already succeeded, so we warn and return 0.
	if !*dryRun && !*keep {
		autoCleanMergedWorktree(pr)
	}
	return 0
}

// autoCleanMergedWorktree resolves the merged PR's head branch, finds the
// matching worktree under the configured root, and removes it (+ local branch).
// Every failure path is a soft warning: the merge is already done.
func autoCleanMergedWorktree(pr string) {
	c, err := config.Load()
	if err != nil {
		return
	}
	branch, err := ghx.PRHeadBranch(pr)
	if err != nil || branch == "" {
		ui.Info("merged; couldn't resolve PR head branch to auto-clean (use `wt clean`)")
		return
	}
	paths, err := gitx.WorktreePaths()
	if err != nil {
		return
	}
	for _, wt := range paths {
		br, _ := gitx.CurrentBranchIn(wt)
		if br != branch {
			continue
		}
		if err := worktree.Remove(c, wt, branch, false); err != nil {
			ui.Warn("worktree for %s not auto-removed: %v", branch, err)
			ui.Info("remove it manually once clean, or `wt clean -y`")
		} else if cwdUnder(wt) {
			ui.Step("cd %s   (you were inside the removed worktree)", c.Root)
		}
		return
	}
	// No local worktree for the branch (e.g. merged from the primary checkout).
}

// cwdUnder reports whether the current dir is inside dir (so we can hint a cd
// back to the primary checkout after removing the worktree we were sitting in).
func cwdUnder(dir string) bool {
	cwd, err := os.Getwd()
	if err != nil {
		return false
	}
	rel, err := filepath.Rel(dir, cwd)
	return err == nil && rel != ".." && !strings.HasPrefix(rel, ".."+string(filepath.Separator))
}

// deployGate enforces the merge==deploy prod safety check. Returns 0 to proceed,
// non-zero to abort. Refuses a draft PR outright, prints a prod banner, and
// requires either --confirm-deploy or a typed "deploy" at an interactive prompt.
func deployGate(pr string, confirmed bool) int {
	if draft, err := ghx.PRIsDraft(pr); err == nil && draft {
		ui.Err("PR #%s is a DRAFT — refusing to merge in a merge==deploy repo (would auto-apply to prod).", pr)
		ui.Info("mark it ready first: gh pr ready %s", pr)
		return 1
	}
	ui.Banner("⚠ merge_is_deploy — merging PR #" + pr + " AUTO-APPLIES to prod")
	if confirmed {
		ui.Info("--confirm-deploy set — proceeding with the prod deploy.")
		return 0
	}
	if !stdinIsTTY() {
		ui.Err("prod deploy not confirmed. Re-run with --confirm-deploy to merge PR #%s.", pr)
		return 1
	}
	fmt.Fprintf(os.Stderr, "%s Type %s to merge PR #%s to prod (anything else aborts): ",
		ui.Yellow("→"), ui.Bold("deploy"), pr)
	line, _ := bufio.NewReader(os.Stdin).ReadString('\n')
	if strings.TrimSpace(line) == "deploy" {
		return 0
	}
	ui.Err("aborted — deploy not confirmed.")
	return 1
}

// stdinIsTTY reports whether stdin is an interactive terminal (so we only
// prompt a human; an agent/pipe must pass --confirm-deploy explicitly).
func stdinIsTTY() bool {
	fi, err := os.Stdin.Stat()
	return err == nil && fi.Mode()&os.ModeCharDevice != 0
}

func cmdStatus(args []string) int {
	fs := flag.NewFlagSet("status", flag.ContinueOnError)
	asJSON := fs.Bool("json", false, "structured JSON output")
	epic := fs.String("epic", "", "aggregate claims tagged with this epic id across sibling repos")
	if err := fs.Parse(args); err != nil {
		return 64
	}
	if *epic != "" {
		return withConfig(func(c *config.Config) int { return cmdStatusEpic(c, *epic, *asJSON) })
	}
	return withConfig(func(c *config.Config) int { return statusReport(c, *asJSON) })
}

func statusReport(c *config.Config, asJSON bool) int {
	if !asJSON {
		peerHoldBanner(c)
	}
	ws, err := collide.Scan(c)
	if err != nil {
		ui.Err("scan failed: %v", err)
		return 1
	}
	ov := collide.Overlaps(ws)
	live := collide.ClassifyWindows(ws, c.Base, collide.OverlapWindowSet(ov), c.MaxAge)
	active, benign := collide.PartitionOverlaps(ov, live)
	graded := gradeStatusOverlaps(c, ws, active)

	if asJSON {
		return renderStatusJSON(ws, graded, len(benign))
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
		// Task-level awareness: each window's current focus, mirrored from its
		// Claude Code TODO list by the `wt _hook todo-write` PostToolUse hook.
		if rec, _ := todos.ForWorktree(w.Worktree); rec != nil {
			if a, ok := rec.Active(); ok {
				fmt.Println("   " + ui.Yellow("▶ ") + ui.Dim(a.ActiveForm))
			}
		}
		fmt.Println()
	}

	if len(ov) == 0 {
		ui.OK("no file collisions across %d window(s) — all clear", len(ws))
		return 0
	}
	if len(graded) == 0 {
		ui.OK("no active collisions — %d file-overlap(s) are all on stale/dormant branches", len(benign))
		return 0
	}

	// Bucket graded overlaps: HIGH (overlapping hunks) blocks attention; the
	// rest — shared docs and disjoint-hunk / append-only overlaps — are advisory.
	var high, low []StatusOverlap
	for _, o := range graded {
		if o.Category == CatBlocking {
			high = append(high, o)
		} else {
			low = append(low, o)
		}
	}
	if len(high) > 0 {
		ui.Collision("%d file(s) with a HIGH-risk collision (overlapping hunks):", len(high))
		for _, o := range high {
			detail := ui.Yellow("indeterminate (untracked/binary — can't prove disjoint)")
			if s := spansString(o.OverlapSpans); s != "" {
				detail = ui.Yellow("overlap " + s)
			}
			fmt.Fprintf(os.Stderr, "   %s  %s %s  %s\n", ui.Bold(o.File), ui.Dim("←"),
				strings.Join(taggedWindows(o.Windows, live), ", "), detail)
		}
	} else {
		ui.OK("no HIGH-risk (overlapping-hunk) collisions across %d window(s)", len(ws))
	}
	for _, o := range low {
		reason := fmt.Sprintf("%d windows, 0 overlapping hunks → low", len(o.Windows))
		if o.Category == CatAdvisory {
			reason = "shared doc → advisory, coordinate sections"
		}
		fmt.Fprintln(os.Stderr, "   "+ui.Dim(fmt.Sprintf("%s — %s (%s)", o.File, reason, strings.Join(o.Windows, ", "))))
	}
	if len(benign) > 0 {
		fmt.Fprintln(os.Stderr, ui.Dim(fmt.Sprintf("   +%d file-overlap(s) on stale/dormant branches only — not active collisions", len(benign))))
	}
	if len(high) > 0 {
		fmt.Fprintln(os.Stderr, ui.Yellow("   Coordinate on the HIGH ones before committing."))
	}
	return 0
}

// cmdStatusEpic aggregates every claim tagged with the given epic id across the
// current repo and its sibling repos (git repos under the shared parent dir),
// reading each repo's active-work file. PR state is resolved live via the
// recorded PR URL when gh is available (works cross-repo).
func cmdStatusEpic(c *config.Config, epic string, asJSON bool) int {
	type unit struct {
		Repo     string `json:"repo"`
		Issue    string `json:"issue"`
		Title    string `json:"title,omitempty"`
		Branch   string `json:"branch"`
		PRURL    string `json:"pr_url,omitempty"`
		PRState  string `json:"pr_state,omitempty"`
		Worktree string `json:"worktree"`
	}

	// c.Root is the CURRENT worktree, which may be a linked worktree whose .git
	// is a file. Resolve the true primary repo root + shared parent from the git
	// common dir so we scan sibling REPOS, not sibling worktrees.
	primaryRoot := c.Root
	if cd, err := gitx.CommonDir(); err == nil && cd != "" {
		primaryRoot = filepath.Dir(cd)
	}
	parent := filepath.Dir(primaryRoot)

	// (repoName, active-work path). Self uses c.ActiveWork (already resolved via
	// the common dir + honoring active_work overrides); siblings resolve their
	// own common dir.
	type repoAW struct{ name, aw string }
	repos := []repoAW{{filepath.Base(primaryRoot), c.ActiveWork}}
	seen := map[string]bool{primaryRoot: true}
	if entries, err := os.ReadDir(parent); err == nil {
		for _, e := range entries {
			if !e.IsDir() {
				continue
			}
			p := filepath.Join(parent, e.Name())
			if seen[p] {
				continue
			}
			cd, err := gitx.CommonDirIn(p) // fails for non-repos → skipped
			if err != nil || cd == "" {
				continue
			}
			seen[p] = true
			repos = append(repos, repoAW{e.Name(), filepath.Join(cd, "wt-active-work.md")})
		}
	}

	units := make([]unit, 0)
	for _, r := range repos {
		content := activework.Read(r.aw)
		if content == "" {
			continue
		}
		for _, e := range activework.Parse(content) {
			if e.Epic != epic {
				continue
			}
			units = append(units, unit{
				Repo: r.name, Issue: e.Issue, Title: e.Title,
				Branch: e.Branch, PRURL: e.PRURL, PRState: ghx.PRStateByURL(e.PRURL),
				Worktree: e.Worktree,
			})
		}
	}

	if asJSON {
		b, _ := json.MarshalIndent(struct {
			Epic  string `json:"epic"`
			Units []unit `json:"units"`
		}{epic, units}, "", "  ")
		fmt.Println(string(b))
		return 0
	}
	if len(units) == 0 {
		ui.Info("no claims tagged epic %q across %d repo(s)", epic, len(repos))
		return 0
	}
	ui.Banner(fmt.Sprintf("epic %s — %d unit(s) across %d repo(s)", epic, len(units), len(repos)))
	for _, u := range units {
		state := u.PRState
		if state == "" {
			state = "no PR / offline"
		}
		fmt.Printf("  %s  #%s  %s  %s\n", ui.Bold(u.Repo), u.Issue, ui.Cyan(u.Branch), ui.Dim("["+state+"]"))
		if u.Title != "" {
			fmt.Println("     " + ui.Dim(u.Title))
		}
		if u.PRURL != "" {
			fmt.Println("     " + ui.Dim(u.PRURL))
		}
	}
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
	// Manual scan (not flag.FlagSet): paths are positional and flags may appear
	// anywhere — flag.Parse would stop at the first positional.
	includeStale, showDiff, asJSON := false, false, false
	var paths []string
	for _, a := range args {
		switch a {
		case "--include-stale", "-include-stale":
			includeStale = true
		case "--show-diff", "-show-diff":
			showDiff = true
		case "--json", "-json":
			asJSON = true
		default:
			paths = append(paths, a)
		}
	}
	if len(paths) == 0 {
		ui.Err("usage: wt check [--include-stale] [--show-diff] [--json] <path> [path...]")
		return 64
	}
	return withConfig(func(c *config.Config) int {
		if !asJSON {
			peerHoldBanner(c)
		}
		ws, err := collide.Scan(c)
		if err != nil {
			ui.Err("scan failed: %v", err)
			return 1
		}
		root, _ := gitx.RepoRoot()
		entries := buildCheckReport(c, ws, root, paths, includeStale)
		if asJSON {
			return renderCheckJSON(entries, includeStale)
		}
		return renderCheckText(entries, paths, includeStale, showDiff)
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
	// todo-write is a Claude Code PostToolUse hook (not a git hook): it derives
	// the repo from the cwd in its stdin payload, so it works without a loaded
	// config and must always exit 0 (never disrupt the editing session).
	if args[0] == "todo-write" {
		return hookTodoWrite(os.Stdin)
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

// hookTodoWrite mirrors a Claude Code TodoWrite tool call into the wt todo
// store. The PostToolUse payload carries the window's cwd and the tool input
// (the full todo list); we resolve the worktree root + branch from cwd and
// persist them keyed by worktree. Any failure is swallowed (return 0) — a
// coordination nicety must never break the user's session.
func hookTodoWrite(r io.Reader) int {
	var in struct {
		CWD       string `json:"cwd"`
		ToolInput struct {
			Todos []todos.Todo `json:"todos"`
		} `json:"tool_input"`
	}
	b, err := io.ReadAll(r)
	if err != nil {
		return 0
	}
	if err := json.Unmarshal(b, &in); err != nil {
		return 0
	}
	cwd := in.CWD
	if cwd == "" {
		cwd, _ = os.Getwd()
	}
	root, err := gitx.RunDir(cwd, "rev-parse", "--show-toplevel")
	if err != nil || root == "" {
		return 0 // not in a git repo — nothing to record
	}
	branch, _ := gitx.CurrentBranchIn(cwd)
	_ = todos.Write(root, branch, time.Now().UTC().Format(time.RFC3339), in.ToolInput.Todos)
	return 0
}

// cmdTodos shows what every window is currently working on, by joining each
// live worktree (collide.Scan) with its mirrored Claude Code TODO list.
func cmdTodos(args []string) int {
	fs := flag.NewFlagSet("todos", flag.ContinueOnError)
	prune := fs.Bool("prune", false, "remove stored todos for worktrees that no longer exist")
	all := fs.Bool("all", false, "list every window, including those with no recorded TODO list")
	if err := fs.Parse(args); err != nil {
		return 64
	}
	return withConfig(func(c *config.Config) int {
		ws, err := collide.Scan(c)
		if err != nil {
			ui.Err("scan failed: %v", err)
			return 1
		}
		liveKeys := map[string]bool{}
		for _, w := range ws {
			liveKeys[todos.Key(w.Worktree)] = true
		}

		if *prune {
			all, _ := todos.All()
			n := 0
			for _, r := range all {
				if !liveKeys[todos.Key(r.Worktree)] {
					if todos.Remove(r.Worktree) == nil {
						n++
					}
				}
			}
			ui.OK("pruned %d orphaned todo record(s)", n)
			return 0
		}

		ui.Banner("wt todos — " + filepath.Base(c.Root) + " (" + fmt.Sprintf("%d window(s)", len(ws)) + ")")
		recorded, idle := 0, 0
		for _, w := range ws {
			rec, _ := todos.ForWorktree(w.Worktree)
			if rec == nil || len(rec.Todos) == 0 {
				idle++
				if *all {
					fmt.Println(ui.Bold(w.Label()) + "  " + ui.Cyan(w.Branch))
					fmt.Println("   " + ui.Dim("(no todos recorded — hook not installed, or no TODO list yet)"))
					fmt.Println()
				}
				continue
			}
			recorded++
			line := ui.Bold(w.Label()) + "  " + ui.Cyan(w.Branch)
			if w.Title != "" {
				line += "  " + ui.Dim(w.Title)
			}
			fmt.Println(line)
			pending, inProgress, done := rec.Counts()
			if a, ok := rec.Active(); ok {
				fmt.Println("   " + ui.Yellow("▶ ") + a.ActiveForm)
			}
			shown := 0
			for _, t := range rec.Todos {
				if t.Status == "pending" {
					fmt.Println("   " + ui.Dim("• "+t.Content))
					if shown++; shown >= 5 {
						break
					}
				}
			}
			fmt.Println("   " + ui.Dim(fmt.Sprintf("%d done · %d in-progress · %d pending · updated %s",
				done, inProgress, pending, humanAgo(rec.Updated))))
			fmt.Println()
		}

		allRecs, _ := todos.All()
		orphans := 0
		for _, r := range allRecs {
			if !liveKeys[todos.Key(r.Worktree)] {
				orphans++
			}
		}
		if recorded == 0 {
			ui.Info("no todos recorded yet — install the Claude Code PostToolUse hook (`wt help`) so each window mirrors its TODO list.")
		} else if idle > 0 && !*all {
			ui.Info("+%d window(s) with no recorded TODO list (hidden) — `wt todos --all` to show them", idle)
		}
		if orphans > 0 {
			ui.Info("%d todo record(s) for worktrees no longer present — `wt todos --prune` to clean up", orphans)
		}
		return 0
	})
}

// humanAgo renders an RFC3339 timestamp as a coarse relative age.
func humanAgo(rfc string) string {
	t, err := time.Parse(time.RFC3339, rfc)
	if err != nil {
		return rfc
	}
	d := time.Since(t)
	switch {
	case d < time.Minute:
		return "just now"
	case d < time.Hour:
		return fmt.Sprintf("%dm ago", int(d.Minutes()))
	case d < 24*time.Hour:
		return fmt.Sprintf("%dh ago", int(d.Hours()))
	default:
		return fmt.Sprintf("%dd ago", int(d.Hours()/24))
	}
}

func capList(items []string, n int) []string {
	if len(items) <= n {
		return items
	}
	out := append([]string{}, items[:n]...)
	return append(out, fmt.Sprintf("…+%d more", len(items)-n))
}
