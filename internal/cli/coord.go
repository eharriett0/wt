// Cross-window coordination commands (eharriett0/wt#13): announce / inbox /
// ack / all-clear over the shared ~/.wt/coordination/<repo>.jsonl log, plus the
// merge-pr hold interlock. See internal/coord for the transport + pure logic.
package cli

import (
	"encoding/json"
	"flag"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/eharriett0/wt/internal/config"
	"github.com/eharriett0/wt/internal/coord"
	"github.com/eharriett0/wt/internal/ghx"
	"github.com/eharriett0/wt/internal/gitx"
	"github.com/eharriett0/wt/internal/ui"
)

// coordCtx resolves the coordination log path + this window's identity for the
// repo containing cwd. window = the worktree's branch (each window is its own
// worktree/branch); repo = the MAIN worktree's dir name (stable across linked
// worktrees, the same anchor WorktreeRoot uses), so every window on the machine
// shares one log per repo.
func coordCtx(c *config.Config) (logPath, window string) {
	home, _ := os.UserHomeDir()
	branch, _ := gitx.CurrentBranch()
	// c.Root is this worktree's toplevel (git rev-parse --show-toplevel) — stable
	// across branch switches, unlike the branch itself (#18). WT_WINDOW overrides
	// for pinning identity across separate checkouts.
	window = coord.WindowID(os.Getenv("WT_WINDOW"), c.Root, branch)
	return coord.LogPath(home, mainRepoName(c)), window
}

func mainRepoName(c *config.Config) string {
	if common, err := gitx.CommonDir(); err == nil && filepath.Base(common) == ".git" {
		return filepath.Base(filepath.Dir(common))
	}
	return filepath.Base(c.Root)
}

func splitHold(s string) []string {
	var out []string
	for _, p := range strings.Split(s, ",") {
		if p = strings.TrimSpace(p); p != "" {
			out = append(out, p)
		}
	}
	return out
}

func findAnnounce(recs []coord.Record, id string) (coord.Record, bool) {
	for _, r := range recs {
		if r.Kind == coord.KindAnnounce && r.ID == id {
			return r, true
		}
	}
	return coord.Record{}, false
}

func newRecord(c *config.Config, window, kind string) coord.Record {
	now := time.Now()
	return coord.Record{
		ID:     coord.NewID(now),
		TS:     now.UTC().Format(time.RFC3339),
		Window: window,
		Repo:   mainRepoName(c),
		Kind:   kind,
	}
}

// mirror posts humanBody + a machine-readable record block to issue n
// (best-effort — a failed GitHub mirror must not fail the local coordination
// write that already succeeded). The embedded block (#36) is what another
// machine reads back via remoteRecords, so the mirror is bi-directional.
func mirror(issue int, r coord.Record, humanBody string) {
	if issue <= 0 {
		return
	}
	body := humanBody + "\n\n" + coord.MirrorJSONBlock(r)
	if err := ghx.IssueComment(fmt.Sprintf("%d", issue), body); err != nil {
		ui.Warn("wrote locally but GitHub mirror to #%d failed: %v", issue, err)
		return
	}
	ui.Info("mirrored to issue #%d", issue)
}

// effectiveIssue resolves which issue to mirror to / read back from: an explicit
// --issue wins, else the pinned coord_issue config (#36), else 0 (off).
func effectiveIssue(explicit int, c *config.Config) int {
	if explicit > 0 {
		return explicit
	}
	return c.CoordIssue
}

// remoteRecords pulls the coordination records another machine mirrored onto
// issue — the read-back path (#36). Best-effort: no issue / gh absent or
// unauthed / read error → nil, so cross-machine coordination degrades to
// local-only rather than breaking the command.
func remoteRecords(issue int) []coord.Record {
	if issue <= 0 || !ghx.Present() || !ghx.Authed() {
		return nil
	}
	bodies, err := ghx.IssueComments(fmt.Sprintf("%d", issue))
	if err != nil {
		return nil
	}
	return coord.ParseMirroredRecords(bodies)
}

func cmdAnnounce(args []string) int {
	fs := flag.NewFlagSet("announce", flag.ContinueOnError)
	issue := fs.Int("issue", 0, "mirror this announcement as a comment on GitHub issue #N")
	hold := fs.String("hold", "", "comma-separated ops other windows should avoid until all-clear (e.g. \"merge-main,flux-reconcile\")")
	clear := fs.String("clear", "", "post an all-clear for announcement <id> instead of announcing")
	file := fs.String("file", "", "read the message from a file (or - for stdin) instead of the argument — opaque to the shell (#75)")
	pos, _, err := parseInterspersed(fs, args)
	if err != nil {
		return 64
	}
	return withConfig(func(c *config.Config) int {
		path, window := coordCtx(c)
		if *clear != "" {
			return allClear(c, path, window, *clear)
		}
		msg, ferr := readFreeform(*file, pos)
		if ferr != nil {
			ui.Err("could not read --file: %v", ferr)
			return 1
		}
		if msg == "" {
			ui.Err("usage: wt announce \"<message>\" [--file <path>] [--issue N] [--hold \"op,...\"]   (or --clear <id>)")
			return 64
		}
		warnSuspiciousFreeform(*file, msg)
		iss := effectiveIssue(*issue, c)
		r := newRecord(c, window, coord.KindAnnounce)
		r.Message, r.Issue, r.Hold = msg, iss, splitHold(*hold)
		if err := coord.Append(path, r); err != nil {
			ui.Err("could not write coordination log: %v", err)
			return 1
		}
		ui.OK("announced %s (window %s)", ui.Bold(r.ID), window)
		echoStored(msg)
		if len(r.Hold) > 0 {
			ui.Info("hold: %s — other windows are asked to avoid these until `wt all-clear %s`", strings.Join(r.Hold, ", "), r.ID)
		}
		mirror(iss, r, fmt.Sprintf("📣 **wt announce** — window `%s`, id `%s`\n\n%s%s", window, r.ID, msg, holdLine(r.Hold)))
		return 0
	})
}

func cmdInbox(args []string) int {
	fs := flag.NewFlagSet("inbox", flag.ContinueOnError)
	asJSON := fs.Bool("json", false, "emit raw JSON")
	issue := fs.Int("issue", 0, "also read back the coordination mirror on GitHub issue #N (cross-machine); default: coord_issue")
	if err := fs.Parse(args); err != nil {
		return 64
	}
	return withConfig(func(c *config.Config) int {
		path, window := coordCtx(c)
		recs, err := coord.Load(path)
		if err != nil {
			ui.Err("could not read coordination log: %v", err)
			return 1
		}
		// Fold in cross-machine records from the mirror issue (#36).
		if iss := effectiveIssue(*issue, c); iss > 0 {
			recs = coord.MergeByID(recs, remoteRecords(iss))
		}
		box := coord.Inbox(recs, window)
		if *asJSON {
			enc := json.NewEncoder(os.Stdout)
			enc.SetIndent("", "  ")
			_ = enc.Encode(box)
			return 0
		}
		if len(box) == 0 {
			ui.OK("inbox clear — no un-acked announcements from other windows")
			return 0
		}
		ui.Info("%d un-acked announcement(s) from other windows (you: %s):", len(box), ui.Bold(window))
		now := time.Now()
		for _, a := range box {
			var tags string
			if len(a.Hold) > 0 {
				tags += ui.Yellow("  [hold: " + strings.Join(a.Hold, ",") + "]")
			}
			if a.Issue > 0 {
				tags += ui.Dim(fmt.Sprintf("  #%d", a.Issue))
			}
			fmt.Printf("  %s  %s  %s%s\n    %s\n", ui.Bold(a.ID), ui.Cyan(a.Window), ui.Dim(humanAge(coord.Age(a, now))), tags, a.Message)
		}
		ui.Step("ack: wt ack <id> --state \"<what this window is touching>\"")
		return 0
	})
}

func cmdAck(args []string) int {
	fs := flag.NewFlagSet("ack", flag.ContinueOnError)
	state := fs.String("state", "", "one-line report of what THIS window is currently touching")
	file := fs.String("file", "", "read --state from a file (or - for stdin) instead of the flag — opaque to the shell (#75)")
	pos, _, err := parseInterspersed(fs, args)
	if err != nil {
		return 64
	}
	if len(pos) < 1 {
		ui.Err("usage: wt ack <id> [--state \"<current-state>\"] [--file <path>]")
		return 64
	}
	id := pos[0]
	// --file wins over --state; both are optional (a bare ack is fine).
	stateVal := strings.TrimSpace(*state)
	if *file != "" {
		s, ferr := readFreeform(*file, nil)
		if ferr != nil {
			ui.Err("could not read --file: %v", ferr)
			return 1
		}
		stateVal = s
	}
	warnSuspiciousFreeform(*file, stateVal)
	return withConfig(func(c *config.Config) int {
		path, window := coordCtx(c)
		// Fold in remote records so a cross-machine announce is ackable (#36).
		local, _ := coord.Load(path)
		recs := coord.MergeByID(local, remoteRecords(c.CoordIssue))
		ann, ok := findAnnounce(recs, id)
		if !ok {
			ui.Err("no announcement with id %s (see `wt inbox`)", id)
			return 1
		}
		r := newRecord(c, window, coord.KindAck)
		r.AckOf, r.State = id, stateVal
		if err := coord.Append(path, r); err != nil {
			ui.Err("could not write coordination log: %v", err)
			return 1
		}
		ui.OK("acked %s (from window %s)", id, ann.Window)
		echoStored(stateVal)
		iss := effectiveIssue(ann.Issue, c)
		mirror(iss, r, fmt.Sprintf("✅ **wt ack** of `%s` — window `%s`%s", id, window, stateLine(r.State)))
		return 0
	})
}

func cmdAllClear(args []string) int {
	if len(args) < 1 {
		ui.Err("usage: wt all-clear <id>")
		return 64
	}
	return withConfig(func(c *config.Config) int {
		path, window := coordCtx(c)
		return allClear(c, path, window, args[0])
	})
}

func allClear(c *config.Config, path, window, id string) int {
	local, _ := coord.Load(path)
	recs := coord.MergeByID(local, remoteRecords(c.CoordIssue)) // allow clearing a remote hold (#36)
	ann, ok := findAnnounce(recs, id)
	if !ok {
		ui.Err("no announcement with id %s", id)
		return 1
	}
	r := newRecord(c, window, coord.KindAllClear)
	r.AckOf = id
	if err := coord.Append(path, r); err != nil {
		ui.Err("could not write coordination log: %v", err)
		return 1
	}
	ui.OK("all-clear posted for %s — hold released", id)
	iss := effectiveIssue(ann.Issue, c)
	mirror(iss, r, fmt.Sprintf("🟢 **wt all-clear** for `%s` — window `%s`, hold released.", id, window))
	return 0
}

// mergeCoordGate refuses a merge when another window holds `merge-main` and this
// window hasn't acked it — the coordination log acting as a real interlock, the
// same shape as the collision guard. Best-effort: a coord read error never
// blocks a merge (fail-open — coordination is advisory infrastructure, not a
// gate that can wedge the user's ship path if the log is unreadable).
func mergeCoordGate(c *config.Config) int {
	path, window := coordCtx(c)
	recs, err := coord.Load(path)
	if err != nil {
		return 0
	}
	// Fold in cross-machine holds from the pinned mirror issue so a hold on
	// another machine actually gates this merge (#36). Best-effort — a read
	// failure leaves the gate local-only (fail-open, as before).
	if c.CoordIssue > 0 {
		recs = coord.MergeByID(recs, remoteRecords(c.CoordIssue))
	}
	now := time.Now()
	fresh, stale := coord.ActiveHoldsAt(recs, window, "merge-main", now, c.HoldMaxAge)
	// Stale holds (aged out past hold_max_age — almost always a crashed/forgotten
	// window) WARN but never block, so a dead window can't wedge merge forever (#32).
	if len(stale) > 0 {
		ui.Warn("%d stale merge-main hold(s) past hold_max_age — NOT blocking (likely a crashed window); clear with wt all-clear:", len(stale))
		for _, h := range stale {
			fmt.Fprintf(os.Stderr, "    %s  %s  %s  (all-clear: wt all-clear %s)\n",
				ui.Bold(h.ID), ui.Cyan(h.Window), ui.Dim(humanAge(coord.Age(h, now))), h.ID)
		}
	}
	if len(fresh) == 0 {
		return 0
	}
	ui.Collision("merge blocked — another window holds `merge-main` (change in flight):")
	for _, h := range fresh {
		iss := ""
		if h.Issue > 0 {
			iss = fmt.Sprintf("  #%d", h.Issue)
		}
		fmt.Fprintf(os.Stderr, "    %s  %s  %s%s\n      %s\n", ui.Bold(h.ID), ui.Cyan(h.Window), ui.Dim(humanAge(coord.Age(h, now))), iss, h.Message)
	}
	ui.Info("ack it first: wt ack <id> --state \"merging PR ...\"   (then it won't block)")
	ui.Info("or override with --bypass if you've confirmed the merge is safe alongside it.")
	return 1
}

// cmdPruneCoord GCs the coordination log — drops completed (all-cleared)
// announce+ack+all-clear handshakes and aged-out block reservations, keeping
// every still-open record (#33). The log is append-only and re-parsed on every
// command, so this bounds a file that otherwise grows forever.
func cmdPruneCoord(args []string) int {
	fs := flag.NewFlagSet("prune-coord", flag.ContinueOnError)
	blockAge := fs.String("block-max-age", "24h", "drop block-id reservations older than this")
	if err := fs.Parse(args); err != nil {
		return 64
	}
	dur, derr := config.ParseAge(*blockAge)
	if derr != nil {
		ui.Err("bad --block-max-age: %v", derr)
		return 64
	}
	return withConfig(func(c *config.Config) int {
		path, _ := coordCtx(c)
		dropped, err := coord.PruneLog(path, time.Now(), dur)
		if err != nil {
			ui.Err("prune failed: %v", err)
			return 1
		}
		if dropped == 0 {
			ui.OK("coordination log already tidy — nothing to prune")
		} else {
			ui.OK("pruned %d resolved/expired record(s) from the coordination log", dropped)
		}
		return 0
	})
}

// cmdHolds lists THIS window's own outstanding announcements/holds (each with a
// copy-pasteable all-clear line) + its block-id reservations, so the
// announce->hold->all-clear lifecycle is self-service instead of grepping the
// jsonl for an id (#34).
func cmdHolds(args []string) int {
	fs := flag.NewFlagSet("holds", flag.ContinueOnError)
	if err := fs.Parse(args); err != nil {
		return 64
	}
	return withConfig(func(c *config.Config) int {
		path, window := coordCtx(c)
		recs, err := coord.Load(path)
		if err != nil {
			ui.Err("could not read coordination log: %v", err)
			return 1
		}
		own := coord.OwnOpenAnnouncements(recs, window)
		reserves := coord.OwnBlockReservations(recs, window)
		if len(own) == 0 && len(reserves) == 0 {
			ui.OK("no outstanding holds/announcements or block reservations for this window (%s)", window)
			return 0
		}
		now := time.Now()
		if len(own) > 0 {
			ui.Banner(fmt.Sprintf("your open announcements — window %s", window))
			for _, a := range own {
				tag := ""
				if len(a.Hold) > 0 {
					tag = " " + ui.Yellow("[hold: "+strings.Join(a.Hold, ",")+"]")
				}
				fmt.Printf("  %s  %s%s\n    %s\n    all-clear: %s\n",
					ui.Bold(a.ID), ui.Dim(humanAge(coord.Age(a, now))), tag, a.Message,
					ui.Cyan("wt all-clear "+a.ID))
			}
		}
		if len(reserves) > 0 {
			ui.Banner("your block-id reservations")
			for _, r := range reserves {
				fmt.Printf("  block %s on %s  %s\n",
					ui.Bold(fmt.Sprintf("%d", r.Block)), filepath.Base(r.File),
					ui.Dim(humanAge(coord.Age(r, now))))
			}
		}
		return 0
	})
}

// peerHoldBanner surfaces active coordination holds from OTHER windows before a
// command that's about to touch shared state (status / new / claim / check).
// This is wt's ambient "another window is mid-change" signal — you find out the
// next time you touch wt, without having to run `wt inbox`. Best-effort and
// never fatal: no repo, unreadable log, or no holds → it simply prints nothing.
func peerHoldBanner(c *config.Config) {
	if c == nil {
		return
	}
	path, window := coordCtx(c)
	recs, err := coord.Load(path)
	if err != nil {
		return
	}
	holds := coord.PendingHolds(recs, window)
	if len(holds) == 0 {
		return
	}
	ui.Banner(fmt.Sprintf("⚠ %d active coordination hold(s) from another window — you: %s", len(holds), window))
	now := time.Now()
	for _, h := range holds {
		iss := ""
		if h.Issue > 0 {
			iss = fmt.Sprintf("  #%d", h.Issue)
		}
		fmt.Fprintf(os.Stderr, "  %s  %s  %s  %s%s\n    %s\n",
			ui.Bold(h.ID), ui.Cyan(h.Window),
			ui.Yellow("[hold: "+strings.Join(h.Hold, ",")+"]"),
			ui.Dim(humanAge(coord.Age(h, now))), iss, h.Message)
	}
	ui.Info("ack: wt ack <id> --state \"…\"   ·   detail: wt inbox")
}

func holdLine(hold []string) string {
	if len(hold) == 0 {
		return ""
	}
	return "\n\n**Hold:** `" + strings.Join(hold, "`, `") + "` — until all-clear."
}

func stateLine(s string) string {
	if s == "" {
		return ""
	}
	return "\n\n> " + s
}

func humanAge(d time.Duration) string {
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
