// Package cli is the wt command router.
package cli

import (
	"bufio"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"runtime/debug"
	"slices"
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
	"github.com/eharriett0/wt/internal/selfupdate"
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

// checkForUpdate prints a throttled "newer wt available" nudge to STDERR (#54).
// Skipped for meta / machine-consumed commands (their stdout AND their latency
// matter), and disabled by WT_NO_UPDATE_CHECK. The check itself only touches the
// network at most once per interval — see internal/selfupdate.
func checkForUpdate(cmd string) {
	switch cmd {
	case "version", "-v", "--version", "help", "-h", "--help", "_hook":
		return
	}
	if os.Getenv("WT_NO_UPDATE_CHECK") != "" {
		return
	}
	if nudge := selfupdate.Check(selfupdate.DefaultStampPath(), time.Now(), selfupdate.DefaultInterval); nudge != "" {
		fmt.Fprintln(os.Stderr, ui.Dim(nudge))
	}
}

// Main dispatches a subcommand and returns a process exit code.
func Main(args []string) int {
	if len(args) == 0 {
		printHelp()
		return 0
	}
	cmd, rest := args[0], args[1:]

	// Skip the "newer wt available" nudge on a help request (top-level OR
	// subcommand, e.g. `wt claim --help`) so `--help` output stays clean (#140).
	if !wantsHelp(rest) {
		checkForUpdate(cmd)
	}

	switch cmd {
	case "help", "-h", "--help":
		printHelp()
		return 0
	case "version", "-v", "--version":
		fmt.Printf("wt %s\n", version())
		return 0
	case "doctor":
		return cmdDoctor(rest)
	case "_hook":
		return runHook(rest)
	case "new":
		return cmdNew(rest)
	case "init":
		return cmdInit(rest)
	case "clean":
		return cmdClean(rest)
	case "discard":
		return cmdDiscard(rest)
	case "claim":
		return cmdClaim(rest)
	case "adopt":
		return cmdAdopt(rest)
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
	case "where":
		return cmdWhere(rest)
	case "install-hooks":
		return cmdInstallHooks(rest)
	case "install-claude-hook":
		return cmdInstallClaudeHook(rest)
	case "install-codex-hook":
		return cmdInstallCodexHook(rest)
	case "mcp":
		return cmdMCP(rest)
	case "announce":
		return cmdAnnounce(rest)
	case "inbox":
		return cmdInbox(rest)
	case "ack":
		return cmdAck(rest)
	case "all-clear":
		return cmdAllClear(rest)
	case "holds":
		return cmdHolds(rest)
	case "prune-coord":
		return cmdPruneCoord(rest)
	case "block-id":
		return cmdBlockID(rest)
	case "append":
		return cmdAppend(rest)
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
	if code, done := guardPositionalArg(args, "usage: wt new <branch>"); done {
		return code
	}
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
	staleIndex := fs.Bool("stale-index", false, "ALSO report merged-PR worktrees holding a leftover uncommitted index that a plain clean silently leaves (prints the manual remove command; never auto-discards) (#88)")
	allRoots := fs.Bool("all-roots", false, "ALSO evaluate worktrees outside worktree_root (e.g. a legacy worktree root) — the collision engine scans them and they can block a push that a default clean never clears (#101)")
	// #169: `wt clean [-y] <name>...` limits the run to the named worktrees (a
	// directory basename, a branch, or a path), so one session can remove its own
	// without touching the others the list shows. Every shipped-ness guard still
	// applies to a named worktree; naming one never forces it.
	names, _, err := parseInterspersed(fs, args)
	if err != nil {
		return 64
	}
	return withConfig(func(c *config.Config) int {
		if err := worktree.Clean(c, *apply, *staleIndex, *allRoots, names); err != nil {
			ui.Err("%v", err)
			return 1
		}
		return 0
	})
}

// discardUsage is `wt discard`'s usage line (#177).
const discardUsage = "usage: wt discard <name> [--drop-commits] [--dry-run] [--all-roots]"

// cmdDiscard is `wt discard <name>` (#177): drop ONE throwaway worktree, its
// local branch and its claim. Exactly one name, never a list: discard is never a
// sweep, and each run lists what that one worktree holds. The guards are
// worktree.DecideDiscard; exit 64 for usage, 1 for any refusal (a --dry-run that
// a real run would refuse included) or failure.
func cmdDiscard(args []string) int {
	if code, done := guardHelp(args, discardUsage); done {
		return code
	}
	fs := flag.NewFlagSet("discard", flag.ContinueOnError)
	dropCommits := fs.Bool("drop-commits", false, "also drop commits that no branch on origin has (they are listed first)")
	dryRun := fs.Bool("dry-run", false, "print what would happen and change nothing (origin is still fetched, as a real run fetches it)")
	allRoots := fs.Bool("all-roots", false, "allow a worktree outside worktree_root, as wt clean --all-roots evaluates them (#101)")
	pos, passthrough, err := parseInterspersed(fs, args)
	if err != nil {
		return 64
	}
	if len(pos) != 1 || pos[0] == "" || len(passthrough) > 0 {
		ui.Err("%s — exactly one worktree (a directory, a branch, or a path): discard is never a sweep", discardUsage)
		return 64
	}
	return withConfig(func(c *config.Config) int {
		if _, err := worktree.Discard(c, pos[0], worktree.DiscardOpts{DropCommits: *dropCommits, DryRun: *dryRun, AllRoots: *allRoots}); err != nil {
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

// isHelpFlag reports whether s is a help request.
func isHelpFlag(s string) bool { return s == "-h" || s == "--help" || s == "-help" }

// wantsHelp reports whether any arg BEFORE a "--" passthrough separator is a help
// request. Stopping at "--" keeps `wt merge-pr 5 -- --help` (a --help meant for
// the forwarded gh command) from being read as a request for wt's own usage.
func wantsHelp(args []string) bool {
	for _, a := range args {
		if a == "--" {
			return false
		}
		if isHelpFlag(a) {
			return true
		}
	}
	return false
}

// looksLikeFlag reports whether a positional arg is really a flag — it starts
// with '-' and isn't the "--" separator. Branch names, issue/PR numbers,
// coordination ids and paths never start with '-', so a FLAGLESS command that
// reads such an arg as its value silently acts on a mistyped or help flag (#140).
func looksLikeFlag(s string) bool { return len(s) > 1 && s[0] == '-' && s != "--" }

// guardHelp routes a -h/--help request to the command's own usage line (exit 0),
// uniformly across the CLI (#140). Used by EVERY subcommand so `wt <cmd> --help`
// always prints that command's usage and succeeds, instead of some printing the
// flag package's terser auto-usage (exit 64) and others silently treating
// "--help" as a value. Returns (0, true) when it printed usage.
func guardHelp(args []string, usage string) (int, bool) {
	if wantsHelp(args) {
		fmt.Println(usage)
		return 0, true
	}
	return 0, false
}

// guardPositionalArg is guardHelp plus a reject of a leading-'-' first arg — a
// mistyped or omitted flag — for a command that takes NO flags, so it never
// treats "--foo" as a value (a branch, id or path); that silent value-treatment
// was the #140 bug. A bare "--" is rejected too: a flagless command has no
// passthrough, so it's never a valid value (and `wt new -- --help` would
// otherwise reach git with branch "--" — the same opaque error #140 kills).
// Returns (code, true) when it handled the input. FLAGLESS commands only: a
// flagged command's own parser tells a valid flag (--force) from a typo, so it
// must not blanket-reject a leading '-'.
func guardPositionalArg(args []string, usage string) (int, bool) {
	if code, done := guardHelp(args, usage); done {
		return code, true
	}
	if len(args) > 0 && (looksLikeFlag(args[0]) || args[0] == "--") {
		ui.Err("%q looks like a flag, not a value — %s", args[0], usage)
		return 64, true
	}
	return 0, false
}

func cmdClaim(args []string) int {
	if code, done := guardHelp(args, "usage: wt claim <issue> [--force] [--yes] [--no-pr] [--epic <id>]"); done {
		return code
	}
	fs := flag.NewFlagSet("claim", flag.ContinueOnError)
	force := fs.Bool("force", false, "claim even if the issue is already assigned")
	yes := fs.Bool("yes", false, "skip the pre-claim confirmation (show-title prompt) — for scripted claims (#157)")
	fs.BoolVar(yes, "y", false, "alias for --yes")
	noPR := fs.Bool("no-pr", false, "skip opening a draft PR")
	epic := fs.String("epic", "", "tag this claim with a cross-repo epic id (wt status --epic)")
	pos, _, err := parseInterspersed(fs, args)
	if err != nil {
		return 64
	}
	if len(pos) < 1 {
		ui.Err("usage: wt claim <issue> [--force] [--yes] [--no-pr] [--epic <id>]")
		return 64
	}
	return withConfig(func(c *config.Config) int {
		peerHoldBanner(c)
		openPR := c.ClaimOpenPR && !*noPR
		if err := claim.Claim(c, pos[0], *force, *yes, openPR, *epic); err != nil {
			ui.Err("%v", err)
			return 1
		}
		return 0
	})
}

func cmdAdopt(args []string) int {
	if code, done := guardHelp(args, "usage: wt adopt <branch|pr#> [--epic <id>]"); done {
		return code
	}
	fs := flag.NewFlagSet("adopt", flag.ContinueOnError)
	epic := fs.String("epic", "", "tag this adoption with a cross-repo epic id (wt status --epic)")
	pos, _, err := parseInterspersed(fs, args)
	if err != nil {
		return 64
	}
	if len(pos) < 1 {
		ui.Err("usage: wt adopt <branch|pr#> [--epic <id>]")
		return 64
	}
	return withConfig(func(c *config.Config) int {
		peerHoldBanner(c)
		if err := claim.Adopt(c, pos[0], *epic); err != nil {
			ui.Err("%v", err)
			return 1
		}
		return 0
	})
}

func cmdRelease(args []string) int {
	if code, done := guardHelp(args, "usage: wt release <issue> [--clean]"); done {
		return code
	}
	fs := flag.NewFlagSet("release", flag.ContinueOnError)
	clean := fs.Bool("clean", false, "also remove the worktree if the branch is abandoned (clean, no live PR, WIP-only)")
	pos, _, err := parseInterspersed(fs, args)
	if err != nil {
		return 64
	}
	if len(pos) < 1 {
		ui.Err("usage: wt release <issue> [--clean]")
		return 64
	}
	return withConfig(func(c *config.Config) int {
		if err := claim.Release(c, pos[0], *clean); err != nil {
			ui.Err("%v", err)
			return 1
		}
		return 0
	})
}

func cmdMergePR(args []string) int {
	if code, done := guardHelp(args, "usage: wt merge-pr <pr> [--dry-run] [--bypass] [--merge-foreign] [--keep] [--confirm-deploy] [--admin] [--close-ok] [--checks-ok] [--no-close-check] [-- extra gh args]"); done {
		return code
	}
	fs := flag.NewFlagSet("merge-pr", flag.ContinueOnError)
	dryRun := fs.Bool("dry-run", false, "print the guard verdict without merging")
	bypass := fs.Bool("bypass", false, "merge despite a block verdict (rare)")
	mergeForeign := fs.Bool("merge-foreign", false, "merge a PR whose head branch has no wt worktree here (wt#15)")
	keep := fs.Bool("keep", false, "keep the worktree after merge (default: auto-remove it)")
	confirmDeploy := fs.Bool("confirm-deploy", false, "acknowledge merge auto-applies to prod (merge_is_deploy repos)")
	admin := fs.Bool("admin", false, "forward --admin to gh pr merge — maintainer bypass of a required-review branch (wt#20)")
	closeOK := fs.Bool("close-ok", false, "proceed even when the squash closes issues the PR's own closing references don't (#77)")
	checksOK := fs.Bool("checks-ok", false, "merge even when the PR's checks are pending, failed, missing or unreadable (#179); --bypass does not imply it")
	noCloseCheck := fs.Bool("no-close-check", false, "skip the close-keyword lint + post-merge issue-state verify (#77)")
	pos, ghArgs, err := parseInterspersed(fs, args)
	if err != nil {
		return 64
	}
	if len(pos) < 1 {
		ui.Err("usage: wt merge-pr <pr> [--dry-run] [--bypass] [--merge-foreign] [--keep] [--confirm-deploy] [--admin] [--close-ok] [--checks-ok] [--no-close-check] [-- extra gh args]")
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
	// #228: a forwarded -R/--repo merges THAT repository's PR, so every read
	// merge-pr makes about the PR must be of it, never of this clone's PR of
	// the same number. The PR is resolved there once; its URL is the selector
	// every gh read and the merge itself take (gh reads a URL as that PR
	// whatever -R or the cwd says). A PR given by its URL names its repository
	// the same way (#228 review: the guard's own hints print that form), so it
	// is resolved too, for the issue, squash-settings and auto-clean reads.
	// Unresolvable: refuse, rather than read another PR.
	repo := merge.ParseForwardedRepo(ghArgs)
	prRef := pr
	var at *ghx.PRAt
	if repo != "" || ghx.IsPRURL(pr) {
		a, err := resolvePRIn(pr, repo)
		if err != nil {
			if repo != "" {
				ui.Err("%v: refusing, rather than read this clone's PR %s in its place", err, merge.PRLabel(pr))
			} else {
				ui.Err("%v: refusing to merge a PR whose repository wt could not read", err)
			}
			return 1
		}
		at, prRef = &a, a.URL
	}
	// #39: PR-state precheck. An already-merged PR shouldn't raise a confusing gh
	// error AND orphan its worktree — skip the merge but still clean up. A
	// closed-not-merged PR has nothing to merge. Unknown state → fall through
	// (fail-open). Only on a real merge; dry-run still previews.
	if !*dryRun {
		switch merge.PreMergeVerdict(ghx.PRState(prRef)) {
		case merge.PreAlreadyMerged:
			ui.Info("PR %s is already merged: skipping the merge.", merge.PRLabel(prRef))
			if !*keep {
				autoCleanMergedWorktree(prRef, at)
			}
			return 0
		case merge.PreClosed:
			ui.Err("PR %s is closed (not merged): nothing to merge.", merge.PRLabel(prRef))
			return 1
		}
	}
	// #179: the checks gate. Before the deploy confirm, so a merge whose CI is
	// pending or red stops before anyone types "deploy". --admin bypasses
	// GitHub's required checks, so nothing after this would stop it; --checks-ok
	// is the only way past (not --bypass). Read on --dry-run too: a clean dry
	// run means the checks were read. The merge is pinned to the commit whose
	// checks it read (WithMatchHead, below). A forwarded -R/--repo is the PR's
	// repository, so the gate reads that PR; a passthrough that merges nothing
	// (--disable-auto, --help) skips the gate and the pin.
	// #223: the gates' settings come from this checkout AND the PR's base
	// branch, and the stricter applies. This checkout is usually the PR's own
	// worktree, so on its own the branch being merged decided its own gate: a
	// PR could turn merge_is_deploy off, and a branch cut before the base added
	// a deploy glob skipped the gate for a path that now deploys. A passthrough
	// that merges nothing reads neither, and no gate below needs them.
	nonMerging := merge.NonMerging(ghArgs)
	gates := []gateSource{{from: "this checkout", gate: c.Gate()}}
	if nonMerging == "" {
		gates = mergeGates(c, pr, repo, *dryRun)
	}
	pin, checksLabel, prHost := "", "skipped", ""
	if nonMerging != "" {
		fmt.Println(skippedChecksLine(pr, nonMerging))
	} else {
		var ok bool
		floor := strictestFloor(gates)
		pin, checksLabel, prHost, ok = checksGate(pr, checksOpts{dryRun: *dryRun, checksOK: *checksOK, admin: *admin, bypass: *bypass,
			auto: merge.ForwardsAuto(ghArgs), repo: repo,
			minChecks: floor.n, minChecksBad: floor.bad, badFrom: floor.badFrom, floorUnread: floor.unread}, os.Stdin, stdinIsTTY(), liveChecksReads)
		if !ok {
			return 1
		}
	}
	// ⚠ The deploy gate is EVALUATED on --dry-run too (#170), for the reason the close-set
	// preview below gives (#164): deployGateApplies and the draft read are
	// read-only, and a dry run that stayed silent here printed `verdict=ok` for a
	// PR the real merge then stopped at. A dry run never prompts.
	// #178: right before the confirm, GitHub's status page is read (github.com
	// only) and an Actions incident is warned about; a dry run prints the same
	// lines, and like the real gate reads nothing for a draft it would refuse.
	// A passthrough that merges nothing (--disable-auto, --help) deploys
	// nothing either, so it skips the deploy gate as it skips the checks gate.
	fires, unsure := false, ""
	if nonMerging == "" {
		fires, unsure = deployGateApplies(gates, pr, repo)
	}
	if fires {
		statusHost := deployStatusHost(prHost)
		if *dryRun {
			draft, derr := ghx.PRIsDraft(pr, repo)
			isDraft := draft && derr == nil
			ui.Warn("%s", deployDryRunNote(pr, isDraft, *confirmDeploy, unsure))
			if !isDraft {
				warnActionsStatus(statusHost)
			}
		} else if code := deployGate(pr, repo, *confirmDeploy, statusHost, unsure); code != 0 {
			return code
		}
	}
	// Cross-window coordination interlock (#13): refuse if another window holds
	// `merge-main` (a disruptive change in flight) and this window hasn't acked
	// it. --bypass overrides, matching the collision guard's escape hatch. #223:
	// hold_max_age and coord_issue are the stricter of the checkout's and the
	// base's, so a PR can't shorten a hold's life or drop the mirror.
	if !*dryRun && !*bypass {
		holdAge, issues := strictestHolds(gates)
		if code := mergeCoordGateWith(c, holdAge, issues); code != 0 {
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
	// maintainer bypass (wt#20). Added only here — AFTER the checks gate (#179),
	// the deploy gate and the coord + guard checks above — so it bypasses GitHub
	// branch protection, not wt's own safety checks (which is exactly the value
	// the raw `gh` fallback lost).
	// Close-keyword lint (#77): merge-pr is the only place that sees BOTH the PR
	// body and the squash commit it forwards — subject and body (#196) — the
	// texts that decide what auto-closes. Print the resolved close set; refuse
	// only when the squash closes something the PR's own closing references don't
	// (trap 2), unless --close-ok. Best-effort — a gh failure yields an empty plan
	// (never blocks).
	// ⚠ This runs on --dry-run TOO (#164). It used to be skipped there, which
	// made the one command you would reach for to preview a merge the one that
	// could not tell you what the merge closes — you had to perform the merge to
	// find out. The analysis is read-only, so previewing costs nothing, and a
	// dry-run reports what it WOULD refuse instead of refusing.
	// ⚠ A body forwarded after `--` IS the squash body, so the check reads it
	// instead of the commit bodies, and gh is handed the same bytes (#180) —
	// prepareMerge has the rules. wt's own gh flags (--admin here, the WIP
	// --subject in merge.Run) go IN FRONT of the passthrough, so a dangling
	// passthrough flag fails in gh instead of taking one of them as its value.
	reads := closeReadsAt(at)
	prep, ok := prepareMerge(prRef, ghArgs, closeOpts{dryRun: *dryRun, closeOK: *closeOK, skip: *noCloseCheck},
		os.Stdin, stdinIsTTY(), reads)
	if !ok {
		return 1
	}
	if err := merge.Run(prRef, *dryRun, *bypass, *mergeForeign, wtBranches, merge.WithMatchHead(pin, merge.WithAdmin(*admin, prep.args)), prep.stdin, checksLabel); err != nil {
		// ⚠ gh can fail AFTER merging (#196): `-- -d` merges, then cannot delete a
		// local branch that a wt worktree has checked out, and exits non-zero. So a
		// failed `gh pr merge` reads the PR state too, and only MERGED goes on to
		// the verify and the auto-clean (#187's shipped-tip guard included); a
		// guard's refusal, or any other state, exits 1 as before.
		if !errors.Is(err, merge.ErrMergeCommand) || !mergedDespiteFailure(prRef) {
			return 1
		}
	} else if !*dryRun && !mergeConfirmed(prRef) {
		// ⚠ gh exits 0 WITHOUT merging for --help, --auto, --disable-auto or a
		// merge queue, so only a PR that now reads MERGED is verified and
		// auto-cleaned (#185). Still exit 0: gh did what it was asked (printed help,
		// armed auto-merge, queued the PR), and keeping the worktree loses nothing.
		return 0
	}
	// Post-merge verification (#77): re-check the referenced issues + report any
	// that changed state — catches a silent close (trap 2) in the same command.
	if !*dryRun && !*noCloseCheck {
		verifyClosings(prep.plan, reads)
	}
	// Auto-clean: the PR just shipped, so its worktree is done. Only on a real
	// merge (not dry-run) and unless -keep. Best-effort — a cleanup miss must
	// not fail the merge that already succeeded, so we warn and return 0.
	if !*dryRun && !*keep {
		autoCleanMergedWorktree(prRef, at)
	}
	return 0
}

// resolvePRIn is ghx.ResolvePRIn; a var so a test can answer it.
var resolvePRIn = ghx.ResolvePRIn

// readyHint is the command that marks pr ready (prCommand). Pure.
func readyHint(pr, repo string) string { return prCommand("ready", pr, repo) }

// prCommand is the `gh pr <verb>` command line for pr that a hint prints: in
// repo when one was forwarded (#228 review: bare, `gh pr ready <n>` names this
// clone's PR of that number), and never with a "#" a shell would read as a
// comment (merge.PRArg). Pure.
func prCommand(verb, pr, repo string, extra ...string) string {
	parts := append([]string{"gh", "pr", verb, merge.PRArg(pr)}, extra...)
	if repo != "" {
		parts = append(parts, "-R", repo)
	}
	return strings.Join(parts, " ")
}

// laneMayBeHere reports whether this clone, whose origin is owner/name, can
// hold at's lane (#228 review): the PR is origin's, or its branch is (origin
// a fork, -R naming upstream). A PR whose repository and branch are both
// elsewhere has no worktree or claim here, and a branch of the same name here
// is another lane. Owner and name compare case-insensitively; the host is
// not compared, as an ssh alias names it differently. Unknown origin or head
// counts as elsewhere. Pure.
func laneMayBeHere(at ghx.PRAt, owner, name string) bool {
	same := func(o, n string) bool {
		return o != "" && n != "" && strings.EqualFold(o, owner) && strings.EqualFold(n, name)
	}
	return same(at.Owner, at.Name) || same(at.HeadOwner, at.HeadName)
}

// autoCleanMergedWorktree resolves the merged PR's head branch, finds the
// matching worktree under the configured root, and removes it (+ local branch).
// Every failure path is a soft warning: the merge is already done. at is the
// PR as a forwarded -R or its URL resolved it (#228), nil for a bare number:
// a PR whose repository and branch are both outside this clone's origin
// (laneMayBeHere) has no worktree here, and a branch of the same name here is
// another branch, so nothing is touched.
func autoCleanMergedWorktree(pr string, at *ghx.PRAt) {
	if at != nil {
		if _, owner, name := ghx.OriginRepo(); !laneMayBeHere(*at, owner, name) {
			head := at.HeadRepo()
			if head == "" {
				head = "unknown, a deleted fork?"
			}
			origin := owner + "/" + name
			if owner == "" {
				origin = "not a repository wt can read as owner/name"
			}
			ui.Info("merged %s: neither its repository (%s) nor its branch's (%s) is this clone's origin (%s), so no worktree or claim here is its; nothing to clean (if origin names that repository by an old name, after a rename or transfer, `wt clean` removes the lane once it ships)", at.URL, at.Repo(), head, origin)
			return
		}
	}
	c, err := config.Load()
	if err != nil {
		return
	}
	branch, err := ghx.PRHeadBranch(pr)
	if err != nil || branch == "" {
		ui.Info("merged; couldn't resolve PR head branch to auto-clean (use `wt clean`)")
		return
	}
	// #40: symmetric with claim.Release — the PR merged, so drop its active-work
	// claim section too (auto-clean previously removed the worktree but left the
	// section forever). Best-effort; fires regardless of whether a local worktree
	// exists (merge from the primary checkout still resolves + clears the claim).
	removeActiveWorkForBranch(c, branch)
	paths, err := gitx.WorktreePaths()
	if err != nil {
		return
	}
	for _, wt := range paths {
		br, _ := gitx.CurrentBranchIn(wt)
		if br != branch {
			continue
		}
		// #187: a squash merge leaves the branch unmerged in git's eyes, so
		// Remove deletes it with -D. Only when every local commit shipped in
		// the PR; otherwise keep the lane (and exit 0: the merge happened).
		msg, tip := unshippedLane(pr, branch)
		if msg != "" {
			ui.Warn("%s", msg)
			return
		}
		// #230: Remove reads where to go, and whether we are inside the
		// worktree, BEFORE it is removed, and prints the cd hint. c.Root is the
		// worktree itself when merge-pr runs in it (the usual case), so the old
		// hint named the directory just removed.
		if err := worktree.Remove(c, wt, branch, tip, false); err != nil {
			ui.Warn("worktree for %s not auto-removed: %v", branch, err)
			ui.Info("remove it manually once clean, or `wt clean -y`")
		}
		return
	}
	// No local worktree for the branch (e.g. merged from the primary checkout).
}

// removeActiveWorkForBranch drops the active-work claim section whose branch is
// `branch` (#40) — resolving the issue number from the active-work file, since
// autoClean has the branch, not the issue. Best-effort + soft: a miss must never
// fail the merge that already succeeded.
func removeActiveWorkForBranch(c *config.Config, branch string) {
	content := activework.Read(c.ActiveWork)
	if content == "" {
		return
	}
	issue := activework.IssueForBranch(activework.Parse(content), branch)
	if issue == "" {
		return
	}
	if newC, changed := activework.RemoveSection(content, issue); changed {
		if err := activework.Write(c.ActiveWork, newC); err == nil {
			ui.Info("removed #%s from active-work (claim resolved)", issue)
		}
	}
}

// anyDeployPath reports whether any changed file matches any deploy glob. Reuses
// collide.MatchDoubleStar so `**` spans path segments ("infrastructure/**" matches
// a deeply-nested file) while `*` stays within one segment — the same matcher the
// append-only globs use (#31), so deploy-path globs behave identically.
//
// A glob may be prefixed with `!` to EXCLUDE (#119). A file is a deploy path when
// it matches at least one positive glob and no exclusion. That covers the case a
// positive-only list cannot express: a subtree deploys, but one file class inside
// it demonstrably cannot. The concrete driver was a README living beside the
// modules it documents — the CI that applies that subtree already skips markdown,
// so the gate was firing on a change that could not deploy.
//
// ⚠ Exclusions are the safe direction to be wrong in, and the reason to prefer
// them over enumerating what DOES deploy. A file type nobody thought of still
// matches the broad positive glob and still gates; under positive enumeration it
// would silently stop gating, which is the failure that is never noticed.
//
// ⚠ Fails CLOSED when a list is exclusions-only. `len(globs) == 0` already means
// "whole repo is a deploy surface" to the caller, so a list that removes without
// ever adding must not quietly mean "nothing ever deploys" — that turns a typo
// into a disabled prod gate.
func anyDeployPath(files, globs []string) bool {
	var include, exclude []string
	for _, g := range globs {
		if g = strings.TrimSpace(g); g == "" {
			continue
		}
		if strings.HasPrefix(g, "!") {
			// A bare `!` excludes nothing, but it is still an exclusion: dropped,
			// a list of only that (`merge_is_deploy_paths = !`, a typo) was
			// neither include nor exclude and never gated (#223 review).
			exclude = append(exclude, strings.TrimSpace(g[1:]))
			continue
		}
		include = append(include, g)
	}
	if len(include) == 0 {
		return len(exclude) > 0 // exclusions-only → fail closed, gate everything
	}
	for _, f := range files {
		if f = strings.TrimSpace(f); f == "" {
			continue
		}
		if !matchesAny(include, f) || matchesAny(exclude, f) {
			continue
		}
		return true
	}
	return false
}

// matchesAny reports whether name matches at least one of the globs.
func matchesAny(globs []string, name string) bool {
	for _, g := range globs {
		if collide.MatchDoubleStar(g, name) {
			return true
		}
	}
	return false
}

// deployDryRunNote is what `merge-pr --dry-run` says about the merge==deploy gate
// once deployGateApplies has said it fires (#170). It follows deployGate's own
// order: a draft is refused before anything else, then --confirm-deploy decides
// whether the real merge would stop for the typed ack. Pure.
func deployDryRunNote(pr string, draft, confirmed bool, unsure string) string {
	applies := "AUTO-APPLIES to prod (merge_is_deploy)"
	if unsure != "" {
		applies = "MAY auto-apply to prod: " + unsure
	}
	switch {
	case draft && unsure != "":
		return "--dry-run: a real merge would REFUSE here: PR #" + pr + " is a DRAFT, and merging it MAY auto-apply to prod: " + unsure + ". Mark it ready first."
	case draft:
		return "--dry-run: a real merge would REFUSE here: PR #" + pr + " is a DRAFT in a merge==deploy repo. Mark it ready first."
	case confirmed:
		return "--dry-run: merging PR #" + pr + " " + applies + "; --confirm-deploy is set, so a real merge would proceed."
	default:
		return "--dry-run: a real merge stops at the deploy gate: merging PR #" + pr + " " + applies + ". Pass --confirm-deploy, or type deploy at a terminal."
	}
}

// deployGate enforces the merge==deploy prod safety check. Returns 0 to proceed,
// non-zero to abort. Refuses a draft PR outright, prints a prod banner, warns
// when GitHub's status page reports an Actions incident (statusHost is the PR's
// host; github.com only, #178), and requires either --confirm-deploy or a typed
// "deploy" at an interactive prompt. The warning sits right before the
// confirm, so whoever confirms has just read it; it never blocks.
func deployGate(pr, repo string, confirmed bool, statusHost, unsure string) int {
	name := merge.PRLabel(pr)
	if draft, err := ghx.PRIsDraft(pr, repo); err == nil && draft {
		if unsure != "" {
			ui.Err("PR %s is a DRAFT: refusing to merge, since merging it MAY auto-apply to prod (%s).", name, unsure)
		} else {
			ui.Err("PR %s is a DRAFT — refusing to merge in a merge==deploy repo (would auto-apply to prod).", name)
		}
		ui.Info("mark it ready first: %s", readyHint(pr, repo))
		return 1
	}
	if unsure != "" {
		// #223 review: nothing says this repo deploys, only that it can't be
		// ruled out; the banner must not state a deploy as fact.
		ui.Banner("⚠ merging PR " + name + " MAY auto-apply to prod: " + unsure)
	} else {
		ui.Banner("⚠ merge_is_deploy — merging PR " + name + " AUTO-APPLIES to prod")
	}
	warnActionsStatus(statusHost)
	if confirmed {
		if unsure != "" {
			ui.Info("--confirm-deploy set: merging, though it may auto-apply to prod.")
		} else {
			ui.Info("--confirm-deploy set — proceeding with the prod deploy.")
		}
		return 0
	}
	if !stdinIsTTY() {
		if unsure != "" {
			ui.Err("deploy not confirmed (merging PR %s may auto-apply to prod). Re-run with --confirm-deploy to merge it.", name)
		} else {
			ui.Err("prod deploy not confirmed. Re-run with --confirm-deploy to merge PR %s.", name)
		}
		return 1
	}
	target := "to prod"
	if unsure != "" {
		target = "(it may auto-apply to prod)"
	}
	fmt.Fprintf(os.Stderr, "%s Type %s to merge PR %s %s (anything else aborts): ",
		ui.Yellow("→"), ui.Bold("deploy"), name, target)
	line, _ := bufio.NewReader(os.Stdin).ReadString('\n')
	if strings.TrimSpace(line) == "deploy" {
		return 0
	}
	ui.Err("aborted — deploy not confirmed.")
	return 1
}

// stdinIsTTY reports whether stdin is an interactive terminal (so we only
// prompt a human; an agent/pipe must pass --confirm-deploy explicitly).
// ⚠ /dev/null is a character device too, and it is how an agent or CI runs
// with no stdin (Go also opens it in place of a closed fd 0), so it is not a
// terminal: read as one, a `-F -` with nothing piped in printed "end it with
// Ctrl-D" and skipped the empty-body warning (#180).
//
// A variable so a test can drive merge-pr's terminal prompts (#179).
var stdinIsTTY = func() bool {
	fi, err := os.Stdin.Stat()
	if err != nil || fi.Mode()&os.ModeCharDevice == 0 {
		return false
	}
	null, err := os.Stat(os.DevNull)
	return err != nil || !os.SameFile(fi, null)
}

func cmdStatus(args []string) int {
	fs := flag.NewFlagSet("status", flag.ContinueOnError)
	asJSON := fs.Bool("json", false, "structured JSON output")
	epic := fs.String("epic", "", "aggregate claims tagged with this epic id across sibling repos")
	blocking := fs.Bool("blocking", false, "print ONLY HIGH-risk collisions + exit non-zero if any (a gate, like check)")
	maxAge := fs.String("max-age", "", "override dormancy suppression for this run (e.g. 4d, 36h; 0/off = show all)")
	if err := fs.Parse(args); err != nil {
		return 64
	}
	if *epic != "" {
		return withConfig(func(c *config.Config) int { return cmdStatusEpic(c, *epic, *asJSON) })
	}
	return withConfig(func(c *config.Config) int {
		applyMaxAgeOverride(c, *maxAge)
		return statusReport(c, *asJSON, *blocking)
	})
}

// applyMaxAgeOverride lets a --max-age flag override the configured dormancy
// window for a single status/check run (#48). "0"/"off"/"never" disables
// suppression; empty leaves config untouched; unparseable is ignored.
func applyMaxAgeOverride(c *config.Config, v string) {
	v = strings.TrimSpace(v)
	if v == "" {
		return
	}
	switch strings.ToLower(v) {
	case "0", "off", "never":
		c.MaxAge = 0
		return
	}
	if d, err := config.ParseAge(v); err == nil {
		c.MaxAge = d
	}
}

// renderBlockingGate prints ONLY the HIGH-risk overlaps and returns exit 3 when
// any exist (0 otherwise) — `wt status --blocking` as a scriptable gate (#47),
// mirroring check's exit-3 contract. Reuses the normal HIGH render.
func renderBlockingGate(graded []StatusOverlap, ws []collide.Window, live map[string]collide.WindowLiveness) int {
	var high []StatusOverlap
	for _, o := range graded {
		if o.Category == CatBlocking {
			high = append(high, o)
		}
	}
	if len(high) == 0 {
		ui.OK("no HIGH-risk collisions across %d window(s)", len(ws))
		return 0
	}
	ui.Collision("%d file(s) with a HIGH-risk collision:", len(high))
	for _, o := range high {
		detail := ui.Yellow("indeterminate (no line-level diff, e.g. binary — can't prove disjoint)")
		if s := spansString(o.OverlapSpans); s != "" {
			detail = ui.Yellow("overlap " + s)
		} else if s := sectionsString(o.SharedSections); s != "" {
			detail = ui.Yellow(s)
		}
		fmt.Fprintf(os.Stderr, "   %s  %s %s  %s\n", ui.Bold(o.File), ui.Dim("←"),
			strings.Join(taggedWindows(o.Windows, live), ", "), detail)
	}
	return 3
}

func statusReport(c *config.Config, asJSON, blocking bool) int {
	if !asJSON {
		peerHoldBanner(c)
		blockReservationBanner(c)
		sharedCheckoutBanner(c) // #163/#197: another session active in THIS checkout
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

	if blocking {
		return renderBlockingGate(graded, ws, live)
	}
	if asJSON {
		return renderStatusJSON(c, ws, graded, len(benign))
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
			detail := ui.Yellow("indeterminate (no line-level diff, e.g. binary — can't prove disjoint)")
			if s := spansString(o.OverlapSpans); s != "" {
				detail = ui.Yellow("overlap " + s)
			} else if s := sectionsString(o.SharedSections); s != "" {
				detail = ui.Yellow(s)
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

// parseCheckArgs splits `check` args into flags + positional paths. A token that
// starts with '-' (and isn't a known flag, isn't "-", and is before a "--") is
// returned as unknownFlag so the caller REJECTS it instead of silently treating
// a typo as a path — a mistyped flag must never produce a false "clear" (#30).
// Everything after a "--" is a path (so a genuine '-'-prefixed filename works).
func parseCheckArgs(args []string) (paths []string, includeStale, showDiff, asJSON, blocking, allowMissing bool, maxAge, unknownFlag string) {
	afterDashes := false
	for i := 0; i < len(args); i++ {
		a := args[i]
		if afterDashes {
			paths = append(paths, a)
			continue
		}
		switch {
		case a == "--":
			afterDashes = true
		case a == "--include-stale" || a == "-include-stale":
			includeStale = true
		case a == "--show-diff" || a == "-show-diff":
			showDiff = true
		case a == "--json" || a == "-json":
			asJSON = true
		case a == "--blocking" || a == "-blocking":
			blocking = true
		case a == "--allow-missing" || a == "-allow-missing":
			allowMissing = true
		case a == "--max-age" || a == "-max-age": // value in the next token (#48)
			if i+1 < len(args) {
				maxAge = args[i+1]
				i++
			}
		case strings.HasPrefix(a, "--max-age="):
			maxAge = strings.TrimPrefix(a, "--max-age=")
		case strings.HasPrefix(a, "-") && a != "-":
			if unknownFlag == "" {
				unknownFlag = a
			}
		default:
			paths = append(paths, a)
		}
	}
	return
}

// checkArg is one `wt check` argument with the facts that decide both how it is
// matched (#181) and whether it is a typo (#93), gathered once so the two
// decisions read the same facts.
type checkArg struct {
	arg string // as typed, trimmed unless it names a real path as typed
	// queries is how it is matched against each window's touched files: one per
	// repo path it names (#201: a symlink and the file it reaches; or, where names
	// are case-insensitive, each spelling another window touches it by).
	queries []collide.Query
	exists  bool // present in the working tree
	tracked bool // known to git (a deleted-but-tracked path counts)
	// outside: it names no path in any worktree of this repository, so there is
	// nothing to check, and "clear" would be a claim about nothing (#201).
	outside  bool
	resolved string // where it resolves to (absolute), for the outside notice
}

// namesRealPath reports whether the argument names real paths only, each
// checked exactly (#181). Pure.
func (a checkArg) namesRealPath() bool {
	if a.outside || len(a.queries) == 0 {
		return false
	}
	for _, q := range a.queries {
		if q.Mode != collide.MatchExact {
			return false
		}
	}
	return true
}

// argBase is the place a check's arguments are read from: prefix is its path
// relative to the repo root (`git rev-parse --show-prefix`, "" = the root
// itself); exists answers for an argument as typed, tracked for a repo path in
// the worktree that holds it; fold says the worktree's filesystem folds case
// (foldsCase: core.ignorecase alone never folds a name that is on no disk
// here, #201 review); bare lists the bare entries of `git worktree list`,
// which hold no repo paths. Injected so the resolution is table-testable
// without a repo (resolveCheckArgs).
type argBase struct {
	prefix  string
	exists  func(arg string) bool       // present in the working tree
	tracked func(root, rel string) bool // known to git there (a deleted-but-tracked path counts)
	fold    bool
	bare    []string
}

// cwdArgBase is `wt check`'s: arguments are read relative to the current
// directory, the way the operator typed them (README.md typed in pkg/svc/ is
// pkg/svc/README.md). Asking git for the prefix avoids comparing os.Getwd, which
// can return the logical /var/… path on macOS, with git's physical root. Whether
// a path is tracked is asked of the worktree that holds it, about the repo path
// the argument resolves to, so the facts agree from a subdir (#92 review) and
// through a symlink (#201). I/O.
func cwdArgBase(root string) argBase {
	prefix, _ := gitx.ShowPrefix()
	return argBase{
		prefix:  prefix,
		exists:  func(a string) bool { _, err := os.Stat(a); return err == nil },
		tracked: gitx.IsTrackedIn,
		fold:    foldsCase(root),
		bare:    gitx.BareWorktreesIn(""),
	}
}

// rootArgBase is MCP wt_check's: arguments are read relative to the repo root,
// whatever directory the client started `wt mcp` in (#181 review). Its schema
// promises repo-relative paths, and a model has no way to know the server's cwd;
// read from a subdirectory, README.md named that subdirectory's README.md and
// missed a collision on the root one. I/O.
func rootArgBase(root string) argBase {
	at := func(a string) string {
		if filepath.IsAbs(a) {
			return a
		}
		return joinRaw(root, a) // uncleaned: the OS reads `link/..` from the link's target (#201)
	}
	return argBase{
		exists:  func(a string) bool { _, err := os.Stat(at(a)); return err == nil },
		tracked: gitx.IsTrackedIn,
		fold:    foldsCase(root),
		bare:    gitx.BareWorktreesIn(root),
	}
}

// resolveCheckArgs gathers each argument's facts from base and decides its
// queries (checkArgQueries): exact at each repo path it names when it names a
// real one, fuzzy as typed when it doesn't (#181). root is the repo's top
// level, which a relative argument is read from (below base.prefix). Shared by
// `wt check` (cwdArgBase) and MCP wt_check (rootArgBase), so the two differ only
// in where a relative path starts.
//
// Where an argument points is read the way the filesystem reads it, the same
// for a relative argument as for an absolute one (#201, placeCheckPath), and
// placed in whichever worktree of this repository holds it: this one, or
// another (ws, from `git worktree list`), whose paths are this repository's
// paths too. An argument no worktree holds is marked outside, for the caller to
// refuse.
//
// An argument is trimmed unless, as typed, it names a real path: a file name can
// begin or end with a space, and the pre-push and pre-commit checks, which read
// it from git verbatim (#200), ask about the name as it is.
func resolveCheckArgs(args []string, root string, base argBase, ws []collide.Window) []checkArg {
	roots := worktreeRoots(root, ws, base.bare)
	var out []checkArg
	for _, a := range args {
		t := strings.TrimSpace(a)
		if t == "" {
			continue
		}
		ca := resolveCheckArg(a, root, roots, base, ws)
		if !ca.namesRealPath() && t != a {
			ca = resolveCheckArg(t, root, roots, base, ws)
		}
		out = append(out, ca)
	}
	return out
}

// worktreeRoots is every worktree's top level, resolved as placePath compares
// them: this one (root) first, then each window's. A bare entry (a bare
// repository's own directory, `git clone --bare` with worktrees inside it) holds
// no repo paths and is left out (#201 review): placed there, `wt check ../HEAD`
// from a worktree inside it read as the repo path HEAD and clear, where a path
// outside every worktree is refused. I/O.
func worktreeRoots(root string, ws []collide.Window, bare []string) []string {
	var out []string
	seen := map[string]bool{}
	for _, b := range bare {
		if b != "" {
			seen[resolveExisting(b)] = true
		}
	}
	add := func(p string) {
		if p == "" {
			return
		}
		if r := resolveExisting(p); !seen[r] {
			seen[r] = true
			out = append(out, r)
		}
	}
	add(root)
	for _, w := range ws {
		add(w.Worktree)
	}
	return out
}

// resolveCheckArg is resolveCheckArgs for one argument, taken as given.
func resolveCheckArg(a, root string, roots []string, base argBase, ws []collide.Window) checkArg {
	ca := checkArg{arg: a, exists: base.exists(a)}
	abs := a
	if !filepath.IsAbs(a) {
		abs = joinRaw(root, filepath.FromSlash(base.prefix), a)
	}
	places := placeCheckPath(abs, roots)
	if len(places) == 0 {
		ca.outside, ca.resolved = true, resolveExisting(abs)
		return ca
	}
	var rels []string
	for _, p := range places {
		rels = append(rels, p.rel)
		if !ca.exists && !ca.tracked && p.rel != collide.RootPath {
			ca.tracked = base.tracked(p.root, p.rel)
		}
	}
	ca.queries = checkArgQueries(a, rels, ca.exists || ca.tracked, ws, base.fold)
	return ca
}

// placeCheckPath is where the absolute path abs points for `wt check` /
// wt_check (#201): the entry it names (a final symlink itself, which git tracks
// at that path and another window can retarget) and the file or directory it
// resolves to (whose content an edit through it changes), each placed in the
// worktree of this repository that holds it (placePath). Asking about both is
// what never reads "clear" for a symlink, whichever of the two the operator is
// about to change. Distinct, entry first; nil when no worktree holds either.
// I/O.
func placeCheckPath(abs string, roots []string) []placedPath {
	entry, target := resolvePath(abs, false), resolvePath(abs, true)
	resolved := []string{entry}
	if target != entry {
		resolved = append(resolved, target)
	}
	var out []placedPath
	for _, p := range resolved {
		if pp, ok := placePath(p, roots); ok && !slices.Contains(out, pp) {
			out = append(out, pp)
		}
	}
	return out
}

// checkArgQueries decides how one argument is matched, given the repo paths it
// names (rels: placeCheckPath's, RootPath for a worktree's top level) and
// whether it is on disk or tracked: collide.QueryFor for each, so a real path
// is exact and a name that is nothing here is a search. Where names are
// case-insensitive (fold), a path that would be a search because nothing here or
// in any window has it under its own spelling is first matched against the
// spellings windows touch (collide.TouchedSpellings): on such a filesystem
// another window's README.md IS readme.md (#201). Distinct, in order. Pure.
func checkArgQueries(arg string, rels []string, onDisk bool, ws []collide.Window, fold bool) []collide.Query {
	var qs []collide.Query
	add := func(q collide.Query) {
		if !slices.Contains(qs, q) {
			qs = append(qs, q)
		}
	}
	for _, rel := range rels {
		q := collide.QueryFor(arg, rel, onDisk, ws)
		if q.Mode == collide.MatchFuzzy && fold {
			if vs := collide.TouchedSpellings(rel, ws); len(vs) > 0 {
				for _, v := range vs {
					add(collide.Query{Path: v, Mode: collide.MatchExact})
				}
				continue
			}
		}
		add(q)
	}
	return qs
}

// checkQueries returns the resolved queries of every argument, in order. Pure.
func checkQueries(args []checkArg) []collide.Query {
	var qs []collide.Query
	for _, a := range args {
		qs = append(qs, a.queries...)
	}
	return qs
}

// unknownCheckPaths returns the requested paths that are almost certainly typos
// (#93): they look like a real path (contain '/' or whitespace) yet don't exist
// in the working tree, aren't tracked by git, and match no window's touched
// files under the mode they will actually be checked with (#181). A bare name
// (no '/' or whitespace) is never flagged: when it names no path here it is a
// legitimate fuzzy basename search. A worktree's top level (`./`, `$PWD`) is
// the whole repository, never a typo (#201); a path outside every worktree is
// outsideCheckPaths', not this. Pure.
func unknownCheckPaths(args []checkArg, ws []collide.Window) []string {
	var out []string
	for _, a := range args {
		if a.outside || a.arg == "" || !strings.ContainsAny(a.arg, "/ \t") {
			continue // bare single-token name → exact if it exists, else a fuzzy search; exempt
		}
		if a.exists || a.tracked {
			continue // exists in the working tree, or a deleted-but-tracked path (legit)
		}
		touched := false
		for _, q := range a.queries {
			if q == collide.RootQuery() || collide.PathTouchedByAny(q, ws) {
				touched = true // a window is genuinely touching it (collision / other-branch path)
				break
			}
		}
		if !touched {
			out = append(out, a.arg)
		}
	}
	return out
}

// outsideCheckPaths returns the requested paths that name no path in any
// worktree of this repository (#201), each with where it resolves to when that
// differs from the argument as typed: `/tmp/x`, `../elsewhere/x`, a path through
// a symlink that leads out of the repository, or into a worktree that is no
// longer one. They used to become fuzzy searches that matched nothing and read
// "clear". Pure.
func outsideCheckPaths(args []checkArg) []string {
	var out []string
	for _, a := range args {
		if !a.outside {
			continue
		}
		if a.resolved != "" && a.resolved != a.arg {
			out = append(out, fmt.Sprintf("%s (resolves to %s)", a.arg, a.resolved))
		} else {
			out = append(out, a.arg)
		}
	}
	return out
}

func cmdCheck(args []string) int {
	const checkUsage = "usage: wt check [--include-stale] [--show-diff] [--json] [--blocking] [--allow-missing] <path> [path...]"
	// route -h/--help to usage instead of parseCheckArgs' unknown-flag reject (#140)
	if code, done := guardHelp(args, checkUsage); done {
		return code
	}
	// paths are positional and flags may appear anywhere (a plain flag.Parse
	// would stop at the first positional), so we scan manually.
	paths, includeStale, showDiff, asJSON, blocking, allowMissing, maxAge, unknownFlag := parseCheckArgs(args)
	if unknownFlag != "" {
		ui.Err("wt check: unknown flag %q — a typo'd flag must not be checked as a path (that would falsely report 'clear'). Known: --include-stale --show-diff --json --blocking --allow-missing. Use `--` to check a path that starts with '-'.", unknownFlag)
		return 64
	}
	if len(paths) == 0 {
		ui.Err(checkUsage)
		return 64
	}
	return withConfig(func(c *config.Config) int {
		applyMaxAgeOverride(c, maxAge)
		if !asJSON && !blocking {
			peerHoldBanner(c)
		}
		ws, err := collide.Scan(c)
		if err != nil {
			ui.Err("scan failed: %v", err)
			return 1
		}
		root, _ := gitx.RepoRoot()
		// #181: a path that names a real location (here, tracked, or touched at
		// that exact path by a window) matches EXACTLY; only one that names
		// nothing in the repo is a fuzzy basename/suffix search.
		args := resolveCheckArgs(paths, root, cwdArgBase(root), ws)
		// #201: a path no worktree of this repository holds has nothing to check;
		// "clear" would be a claim about nothing. Not even --allow-missing makes it
		// checkable.
		if outside := outsideCheckPaths(args); len(outside) > 0 {
			ui.Err("wt check: not in any worktree of this repository: %s. Refusing to report 'clear' for a path wt cannot check; name it inside this worktree, or inside another worktree of this repository (see `git worktree list`).", strings.Join(outside, ", "))
			return 64
		}
		// #93: refuse a path that doesn't exist in the working tree, isn't tracked
		// by git, and isn't touched by any window — a typo (or a zsh non-word-split
		// single arg) that would otherwise falsely report '✓ clear'. Bare basenames
		// (no '/' or space) are fuzzy suffix queries and exempt; --allow-missing
		// opts into checking a genuinely-gone path.
		if !allowMissing {
			if unknown := unknownCheckPaths(args, ws); len(unknown) > 0 {
				ui.Err("wt check: no such path(s) — refusing to report 'clear' for path(s) that don't exist, aren't tracked, and no window is touching: %s. (typo, or zsh didn't word-split a $var? checking a path you're about to CREATE, or one that's deleted/on another branch? re-run with --allow-missing.)", strings.Join(unknown, ", "))
				return 64
			}
		}
		entries := buildCheckReport(c, ws, root, checkQueries(args))
		noBase := !gitx.BaseResolves(root, c.Base) // #247
		if asJSON {
			return renderCheckJSON(entries, includeStale, noBase)
		}
		if noBase {
			ui.Warn("%s", collide.NoBaseNote(c.Base))
		}
		if len(entries) > 0 {
			annotateLive(entries, worktreeSessionNotes(c)) // #250
		}
		if blocking {
			return renderCheckBlocking(entries, paths)
		}
		return renderCheckText(entries, paths, includeStale, showDiff)
	})
}

// cmdInit scaffolds a commented .wt.conf at the repo root, pre-filled with this
// repo's derived defaults (#44). Refuses to clobber an existing file without
// --force.
func cmdInit(args []string) int {
	fs := flag.NewFlagSet("init", flag.ContinueOnError)
	force := fs.Bool("force", false, "overwrite an existing .wt.conf")
	if err := fs.Parse(args); err != nil {
		return 64
	}
	return withConfig(func(c *config.Config) int {
		path := filepath.Join(c.Root, ".wt.conf")
		if _, err := os.Stat(path); err == nil && !*force {
			ui.Err(".wt.conf already exists at %s — use --force to overwrite", path)
			return 1
		}
		if err := os.WriteFile(path, []byte(config.ScaffoldConf(c)), 0o644); err != nil {
			ui.Err("write %s: %v", path, err)
			return 1
		}
		ui.OK("wrote %s — every key is commented; uncomment + edit what you need", path)
		return 0
	})
}

// cmdDoctor parses --json and runs the preflight checklist (#43).
func cmdDoctor(args []string) int {
	fs := flag.NewFlagSet("doctor", flag.ContinueOnError)
	asJSON := fs.Bool("json", false, "emit the report as JSON")
	if err := fs.Parse(args); err != nil {
		return 64
	}
	return doctor.Run(loadConfigOrNil(), *asJSON)
}

// cmdWhere resolves an issue number or branch to its worktree path and prints
// JUST the absolute path to stdout (so `cd $(wt where 42)` works); non-zero +
// stderr when not found (#46).
func cmdWhere(args []string) int {
	if code, done := guardPositionalArg(args, "usage: wt where <issue|branch>"); done {
		return code
	}
	if len(args) < 1 || strings.TrimSpace(args[0]) == "" {
		ui.Err("usage: wt where <issue|branch>")
		return 64
	}
	target := strings.TrimSpace(args[0])
	norm := strings.TrimPrefix(target, "#")
	return withConfig(func(c *config.Config) int {
		ws, err := collide.Scan(c)
		if err != nil {
			ui.Err("scan failed: %v", err)
			return 1
		}
		for _, w := range ws {
			if (w.Issue != "" && w.Issue == norm) || w.Branch == target || w.Branch == norm {
				fmt.Println(w.Worktree)
				return 0
			}
		}
		ui.Err("no worktree found for %q — see `wt status`", target)
		return 1
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
	// claude-edit is a Claude Code PreToolUse hook (#95): it derives the repo
	// from its stdin payload's cwd and always exits 0 (advisory), so it too runs
	// without a pre-loaded config.
	if args[0] == "claude-edit" {
		return hookClaudeEdit(os.Stdin)
	}
	// codex-context / claude-context are the per-turn UserPromptSubmit hooks
	// (both agents share the cwd-in / additionalContext-out shape): they derive the
	// repo from the stdin payload's cwd and always exit 0 (fail-open context), so
	// they run without a pre-loaded config.
	if args[0] == "codex-context" || args[0] == "claude-context" {
		return hookAgentContext(os.Stdin, args[0] == "codex-context")
	}
	// codex-edit is a Codex CLI PreToolUse hook on apply_patch (#117): it derives
	// the repo from its stdin payload's cwd and always exits 0 (fail-open), so it
	// too runs without a pre-loaded config.
	if args[0] == "codex-edit" {
		return hookCodexEdit(os.Stdin)
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

// todoWriteHookInstalled reports whether the Claude Code PostToolUse/TodoWrite
// hook that feeds `wt todos` is wired for the repo at root, across EVERY location
// Claude Code merges settings from: the user-level config dir ($CLAUDE_CONFIG_DIR
// or ~/.claude) AND the project's .claude — each as settings.json +
// settings.local.json. A machine-wide hook wired once at the user level (the
// natural place for a hook you want in every repo) therefore counts, instead of
// being misreported as absent and prompting a duplicate project-level install
// (#146). It lets cmdTodos distinguish an empty store's two causes — (a) the hook
// is not installed vs. (b) it IS installed but no window has recorded a todo yet —
// instead of asserting (a) as fact (#144). Fail-open to false: unreadable/absent/
// unparseable settings ⇒ treat as not-installed, so the message stays the
// actionable "wire the hook" one rather than a misleading "it's installed".
func todoWriteHookInstalled(root string) bool {
	dirs := []string{filepath.Join(root, ".claude")}
	if ucd := claudeUserConfigDir(); ucd != "" {
		dirs = append(dirs, ucd)
	}
	for _, dir := range dirs {
		for _, name := range []string{"settings.json", "settings.local.json"} {
			if settingsWiresTodoWrite(filepath.Join(dir, name)) {
				return true
			}
		}
	}
	return false
}

// claudeUserConfigDir returns Claude Code's user-level config directory —
// $CLAUDE_CONFIG_DIR when set (a relocated config dir), else ~/.claude. This is
// where a hook you want in every repo on the machine is wired. "" when neither is
// resolvable (home dir unknown), which the caller treats as "no user-level file".
func claudeUserConfigDir() string {
	if d := strings.TrimSpace(os.Getenv("CLAUDE_CONFIG_DIR")); d != "" {
		return d
	}
	home, err := os.UserHomeDir()
	if err != nil {
		return ""
	}
	return filepath.Join(home, ".claude")
}

// settingsWiresTodoWrite reports whether a single Claude Code settings file wires
// the todo-write hook. Fail-open to false on any read/parse error.
func settingsWiresTodoWrite(path string) bool {
	b, err := os.ReadFile(path)
	if err != nil {
		return false
	}
	var doc struct {
		Hooks map[string][]struct {
			Hooks []struct {
				Command string `json:"command"`
			} `json:"hooks"`
		} `json:"hooks"`
	}
	if err := json.Unmarshal(b, &doc); err != nil {
		return false
	}
	// The `_hook todo-write` subcommand is unique to this hook, so a command
	// carrying it under any event is unambiguous evidence it's wired — matched as
	// a substring so a path-qualified `/usr/local/bin/wt _hook todo-write` counts.
	for _, groups := range doc.Hooks {
		for _, g := range groups {
			for _, h := range g.Hooks {
				if strings.Contains(h.Command, "_hook todo-write") {
					return true
				}
			}
		}
	}
	return false
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

		hookInstalled := todoWriteHookInstalled(c.Root)

		ui.Banner("wt todos — " + filepath.Base(c.Root) + " (" + fmt.Sprintf("%d window(s)", len(ws)) + ")")
		recorded, idle := 0, 0
		for _, w := range ws {
			rec, _ := todos.ForWorktree(w.Worktree)
			if rec == nil || len(rec.Todos) == 0 {
				idle++
				if *all {
					fmt.Println(ui.Bold(w.Label()) + "  " + ui.Cyan(w.Branch))
					// Probe THIS window's own worktree: settings.json may be
					// gitignored and present in one checkout but not another.
					if todoWriteHookInstalled(w.Worktree) {
						fmt.Println("   " + ui.Dim("(no TODO list recorded yet — the agent may not have used the TodoWrite tool)"))
					} else {
						fmt.Println("   " + ui.Dim("(no todos recorded — TodoWrite hook not installed here; run `wt install-claude-hook --write`)"))
					}
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
			if hookInstalled {
				ui.Info("no todos recorded yet — the TodoWrite hook (PostToolUse/TodoWrite) is installed but no window has recorded a todo; the agent session may not have the TodoWrite tool enabled.")
			} else {
				ui.Info("no todos recorded yet — wire the Claude Code TodoWrite hook with `wt install-claude-hook --write` so each window mirrors its TODO list.")
			}
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
