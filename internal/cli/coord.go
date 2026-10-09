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

// coordCtx resolves the coordination log path + this process's identity for the
// repo containing cwd. self.Window = the checkout (coord.WindowID: WT_WINDOW →
// the worktree toplevel PATH → branch; c.Root is stable across branch switches,
// #18). self.Session = the session within that checkout (#163, coord.SessionToken:
// WT_SESSION → CLAUDE_CODE_SESSION_ID, else coord.SessionNone), so two agent
// sessions started in ONE checkout are two parties, not one. repo = the MAIN
// worktree's dir name (stable across linked
// worktrees, the same anchor WorktreeRoot uses), so every window on the machine
// shares one log per repo.
func coordCtx(c *config.Config) (logPath string, self coord.Self) {
	home, _ := os.UserHomeDir()
	branch, _ := gitx.CurrentBranch()
	self = coord.CurrentSelf(os.Getenv, c.Root, branch)
	return coord.LogPath(home, mainRepoName(c)), self
}

func mainRepoName(c *config.Config) string {
	common, _ := gitx.CommonDir()
	return repoNameFrom(common, c.Root)
}

// repoNameFrom names the repo's shared coordination log: the MAIN worktree's dir
// when the common dir is "<main>/.git", else root's basename. Pure.
func repoNameFrom(common, root string) string {
	if common != "" && filepath.Base(common) == ".git" {
		return filepath.Base(filepath.Dir(common))
	}
	return filepath.Base(root)
}

// postedBy renders who posted r, for humans: the other window's id — or, when r
// comes from THIS checkout (so its window id is yours), whether it is another
// session's ("same checkout, another session", #163) or yours. Without that, the
// reader sees its own window id and reasonably concludes it is looking at its own
// announcement. Derived from Self.SharesCheckout/Owns, never a raw window
// compare, so the label always agrees with the ownership decision. Pure.
func postedBy(r coord.Record, self coord.Self) string {
	switch {
	case self.SharesCheckout(r):
		return "same checkout, another session (" + coord.ShortSession(r.Session) + ")"
	case self.Owns(r):
		return "this window (yours)"
	default:
		return r.Window
	}
}

// selfLabel names this process's identity for headers: the window id, plus the
// session within it (#163). Pure.
func selfLabel(self coord.Self) string {
	return self.Window + ", " + coord.ShortSession(self.Session)
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

// newRecord stamps a fresh record with this process's window AND session (#163)
// — every record names the session that wrote it, so readers can tell two
// sessions in one checkout apart.
func newRecord(c *config.Config, self coord.Self, kind string) coord.Record {
	return stampRecord(self, mainRepoName(c), kind, time.Now())
}

// stampRecord is every writer's record header, written by self at t: id,
// timestamp, window AND session (#163). A record without the session is a
// pre-#163 wildcard that every session in the checkout owns, so dropping it here
// silently re-merges sessions. Pure.
func stampRecord(self coord.Self, repo, kind string, t time.Time) coord.Record {
	return coord.Record{
		ID:      coord.NewID(t),
		TS:      t.UTC().Format(time.RFC3339),
		Window:  self.Window,
		Session: self.Session,
		Repo:    repo,
		Kind:    kind,
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
	if code, done := guardHelp(args, `usage: wt announce "<message>" [--file <path>] [--issue N] [--hold "op,..."]   (or --clear <id>)`); done {
		return code
	}
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
		path, self := coordCtx(c)
		if *clear != "" {
			return allClear(c, path, self, *clear)
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
		r := newRecord(c, self, coord.KindAnnounce)
		r.Message, r.Issue, r.Hold = msg, iss, splitHold(*hold)
		if err := coord.Append(path, r); err != nil {
			ui.Err("could not write coordination log: %v", err)
			return 1
		}
		ui.OK("announced %s (window %s)", ui.Bold(r.ID), selfLabel(self))
		echoStored(msg)
		if len(r.Hold) > 0 {
			ui.Info("hold: %s — other windows are asked to avoid these until `wt all-clear %s`", strings.Join(r.Hold, ", "), r.ID)
		}
		mirror(iss, r, fmt.Sprintf("📣 **wt announce** — window `%s`, id `%s`\n\n%s%s", self.Window, r.ID, msg, holdLine(r.Hold)))
		return 0
	})
}

// inboxEntry is one `wt inbox --json` element: the record plus whether it comes
// from another session in THIS checkout (#163) — its window equals yours, so a
// JSON consumer can't tell otherwise. Embedding keeps the array-of-records shape.
type inboxEntry struct {
	coord.Record
	SameCheckout bool `json:"same_checkout,omitempty"`
}

// inboxClearMessage is what `wt inbox` prints when nothing is pending. It is a
// bare "inbox clear" ONLY when that is provable: when this session has no
// session token while records under this checkout carry one (#163), other
// sessions demonstrably post from here, and a token-less one among them would
// read as this session — so it hedges instead of reporting a confident clear.
// Pure.
func inboxClearMessage(recs []coord.Record, self coord.Self, now time.Time) (msg string, hedged bool) {
	if !coord.InboxClearAmbiguous(recs, self, now) {
		return "inbox clear — no un-acked announcements from other windows", false
	}
	return "no un-acked announcements found — but NOT a confirmed clear: " + tokenlessCaveat(), true
}

// tokenlessCaveat explains the #163 hedge: why a session with no session token
// can't vouch that nothing else is pending in a checkout other sessions use.
func tokenlessCaveat() string {
	return "this shell has no session token (neither " + strings.Join(coord.SessionEnvVars, " nor ") + " is set), " +
		"and other sessions posted from this checkout in the last day, so another token-less session sharing it " +
		"would be indistinguishable from you. Set WT_SESSION to a value unique to this session, or move to your own " +
		"worktree (`wt new <branch>`)."
}

func cmdInbox(args []string) int {
	fs := flag.NewFlagSet("inbox", flag.ContinueOnError)
	asJSON := fs.Bool("json", false, "emit raw JSON")
	issue := fs.Int("issue", 0, "also read back the coordination mirror on GitHub issue #N (cross-machine); default: coord_issue")
	if err := fs.Parse(args); err != nil {
		return 64
	}
	return withConfig(func(c *config.Config) int {
		path, self := coordCtx(c)
		recs, err := coord.Load(path)
		if err != nil {
			ui.Err("could not read coordination log: %v", err)
			return 1
		}
		// Fold in cross-machine records from the mirror issue (#36).
		if iss := effectiveIssue(*issue, c); iss > 0 {
			recs = coord.MergeByID(recs, remoteRecords(iss))
		}
		box := coord.Inbox(recs, self)
		// newest-first: a fresh announcement is always at the top, never buried
		// under a deep backlog of old un-acked records (#147).
		for i, j := 0, len(box)-1; i < j; i, j = i+1, j-1 {
			box[i], box[j] = box[j], box[i]
		}
		clearMsg, hedged := inboxClearMessage(recs, self, time.Now())
		if *asJSON {
			out := make([]inboxEntry, 0, len(box))
			for _, a := range box {
				out = append(out, inboxEntry{Record: a, SameCheckout: self.SharesCheckout(a)})
			}
			enc := json.NewEncoder(os.Stdout)
			enc.SetIndent("", "  ")
			_ = enc.Encode(out)
			if len(box) == 0 && hedged {
				ui.Warn("%s", clearMsg) // stderr: the JSON on stdout stays parseable
			}
			return 0
		}
		if len(box) == 0 {
			if hedged {
				ui.Warn("%s", clearMsg)
			} else {
				ui.OK("%s", clearMsg)
			}
			return 0
		}
		ui.Info("%d un-acked announcement(s) from other windows/sessions (you: %s):", len(box), ui.Bold(selfLabel(self)))
		now := time.Now()
		for _, a := range box {
			var tags string
			if len(a.Hold) > 0 {
				tags += ui.Yellow("  [hold: " + strings.Join(a.Hold, ",") + "]")
			}
			if a.Issue > 0 {
				tags += ui.Dim(fmt.Sprintf("  #%d", a.Issue))
			}
			fmt.Printf("  %s  %s  %s%s\n    %s\n", ui.Bold(a.ID), ui.Cyan(postedBy(a, self)), ui.Dim(humanAge(coord.Age(a, now))), tags, a.Message)
		}
		if hedged {
			ui.Warn("note: %s", tokenlessCaveat())
		}
		ui.Step("ack: wt ack <id> --state \"<what this window is touching>\"")
		return 0
	})
}

func cmdAck(args []string) int {
	if code, done := guardHelp(args, `usage: wt ack <id> [--state "<current-state>"] [--file <path>]  |  wt ack --all`); done {
		return code
	}
	fs := flag.NewFlagSet("ack", flag.ContinueOnError)
	state := fs.String("state", "", "one-line report of what THIS window is currently touching")
	file := fs.String("file", "", "read --state from a file (or - for stdin) instead of the flag — opaque to the shell (#75)")
	all := fs.Bool("all", false, "ack EVERY un-acked announcement from other windows in one step — clears a saturated backlog (#147)")
	pos, _, err := parseInterspersed(fs, args)
	if err != nil {
		return 64
	}
	if *all {
		return withConfig(ackAll)
	}
	if len(pos) < 1 {
		ui.Err("usage: wt ack <id> [--state \"<current-state>\"] [--file <path>]  (or `wt ack --all` to clear the whole backlog)")
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
		path, self := coordCtx(c)
		// Fold in remote records so a cross-machine announce is ackable (#36).
		local, _ := coord.Load(path)
		recs := coord.MergeByID(local, remoteRecords(c.CoordIssue))
		ann, ok := findAnnounce(recs, id)
		if !ok {
			ui.Err("no announcement with id %s (see `wt inbox`)", id)
			return 1
		}
		r := newRecord(c, self, coord.KindAck)
		r.AckOf, r.State = id, stateVal
		if err := coord.Append(path, r); err != nil {
			ui.Err("could not write coordination log: %v", err)
			return 1
		}
		from := postedBy(ann, self) // #163: same checkout → another session's, or yours
		if from == ann.Window {
			from = "window " + from
		}
		ui.OK("acked %s (from %s)", id, from)
		echoStored(stateVal)
		iss := effectiveIssue(ann.Issue, c)
		mirror(iss, r, fmt.Sprintf("✅ **wt ack** of `%s` — window `%s`%s", id, self.Window, stateLine(r.State)))
		return 0
	})
}

// bulkAckTargets splits an inbox into the plain announcements `wt ack --all`
// should clear and the count of HOLDs it must LEAVE STANDING. A hold is a
// merge-main interlock: acking it removes it from coord.Inbox (which excludes
// acked ids), so PendingHolds and the merge-pr gate stop seeing it. Silently
// waiving a fresh hold the user never saw — the exact case ack --all exists for,
// a hold buried past the 12-line cap — would defeat the very interlock wt exists
// to protect (#147 review). Holds are cleared only by a deliberate `wt ack <id>`
// or `wt all-clear <id>`. Pure.
func bulkAckTargets(box []coord.Record) (notes []coord.Record, holdsLeft int) {
	for _, a := range box {
		if len(a.Hold) > 0 {
			holdsLeft++
			continue
		}
		notes = append(notes, a)
	}
	return notes, holdsLeft
}

// ackAll acks every un-acked PLAIN announcement in this window's inbox in one
// step — the recovery path for a saturated coordination backlog (#147), where
// clearing by hand would mean one `wt ack <id>` per stale record. HOLDs are left
// standing (see bulkAckTargets). Local-only: bulk-clearing stale local noise must
// not spray N GitHub-mirror API calls (an all-clear on a specific hold is still
// the way to release it cross-machine).
func ackAll(c *config.Config) int {
	path, self := coordCtx(c)
	local, _ := coord.Load(path)
	recs := coord.MergeByID(local, remoteRecords(c.CoordIssue))
	notes, holdsLeft := bulkAckTargets(coord.Inbox(recs, self))

	heldNote := ""
	if holdsLeft > 0 {
		heldNote = fmt.Sprintf(" — %d hold(s) left standing (ack or all-clear each deliberately; `wt inbox`)", holdsLeft)
	}
	if len(notes) == 0 {
		if holdsLeft > 0 {
			ui.OK("no plain announcements to ack%s", heldNote)
		} else {
			ui.OK("inbox already clear — nothing to ack")
		}
		return 0
	}
	for i, r := range bulkAckRecords(notes, self, mainRepoName(c), time.Now()) {
		if err := coord.Append(path, r); err != nil {
			ui.Err("could not write coordination log after %d ack(s): %v", i, err)
			return 1
		}
	}
	ui.OK("acked %d announcement(s)%s", len(notes), heldNote)
	return 0
}

// bulkAckRecords builds `wt ack --all`'s ack records, one per note, each stamped
// with self's window AND session (#163: these acks are this session's, not the
// checkout's) via stampRecord. IDs are distinct and increasing so MergeByID (in
// every reader's path) can never collapse two acks that target DIFFERENT
// announcements: NewID is UnixNano-base36, and a tight loop can outrun the clock,
// so record i is stamped at base+i ns. Pure.
func bulkAckRecords(notes []coord.Record, self coord.Self, repo string, base time.Time) []coord.Record {
	out := make([]coord.Record, 0, len(notes))
	for i, a := range notes {
		r := stampRecord(self, repo, coord.KindAck, base.Add(time.Duration(i)))
		r.AckOf = a.ID
		out = append(out, r)
	}
	return out
}

func cmdAllClear(args []string) int {
	if code, done := guardPositionalArg(args, "usage: wt all-clear <id>"); done {
		return code
	}
	if len(args) < 1 {
		ui.Err("usage: wt all-clear <id>")
		return 64
	}
	return withConfig(func(c *config.Config) int {
		path, self := coordCtx(c)
		return allClear(c, path, self, args[0])
	})
}

func allClear(c *config.Config, path string, self coord.Self, id string) int {
	local, _ := coord.Load(path)
	recs := coord.MergeByID(local, remoteRecords(c.CoordIssue)) // allow clearing a remote hold (#36)
	ann, ok := findAnnounce(recs, id)
	if !ok {
		ui.Err("no announcement with id %s", id)
		return 1
	}
	r := newRecord(c, self, coord.KindAllClear)
	r.AckOf = id
	if err := coord.Append(path, r); err != nil {
		ui.Err("could not write coordination log: %v", err)
		return 1
	}
	ui.OK("all-clear posted for %s — hold released", id)
	iss := effectiveIssue(ann.Issue, c)
	mirror(iss, r, fmt.Sprintf("🟢 **wt all-clear** for `%s` — window `%s`, hold released.", id, self.Window))
	return 0
}

// mergeCoordGate refuses a merge when another window holds `merge-main` and this
// window hasn't acked it — the coordination log acting as a real interlock, the
// same shape as the collision guard. Best-effort: a coord read error never
// blocks a merge (fail-open — coordination is advisory infrastructure, not a
// gate that can wedge the user's ship path if the log is unreadable).
func mergeCoordGate(c *config.Config) int {
	path, self := coordCtx(c)
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
	// Only self.Owns exempts a hold: another session's hold in THIS checkout
	// gates the merge exactly like another window's (#163).
	fresh, stale := coord.ActiveHoldsAt(recs, self, "merge-main", now, c.HoldMaxAge)
	// Stale holds (aged out past hold_max_age — almost always a crashed/forgotten
	// window) WARN but never block, so a dead window can't wedge merge forever (#32).
	if len(stale) > 0 {
		ui.Warn("%d stale merge-main hold(s) past hold_max_age — NOT blocking (likely a crashed window); wt all-clear releases one for EVERY window:", len(stale))
		for _, h := range stale {
			fmt.Fprintf(os.Stderr, "    %s  %s  %s  (all-clear: wt all-clear %s)\n",
				ui.Bold(h.ID), ui.Cyan(postedBy(h, self)), ui.Dim(humanAge(coord.Age(h, now))), h.ID)
		}
	}
	if len(fresh) == 0 {
		return 0
	}
	ui.Collision("merge blocked — %s holds `merge-main` (change in flight):", holdersPhrase(fresh, self))
	for _, h := range fresh {
		iss := ""
		if h.Issue > 0 {
			iss = fmt.Sprintf("  #%d", h.Issue)
		}
		fmt.Fprintf(os.Stderr, "    %s  %s  %s%s\n      %s\n", ui.Bold(h.ID), ui.Cyan(postedBy(h, self)), ui.Dim(humanAge(coord.Age(h, now))), iss, h.Message)
	}
	for _, line := range holdAdvice(fresh, recs, self, now) {
		ui.Info("%s", line)
	}
	ui.Info("or override with --bypass if you've confirmed the merge is safe alongside it.")
	return 1
}

// holdAdvice is what wt tells a reader about holds that gate it (#163), for the
// merge gate and `wt holds`. It leads with `wt ack <id>`, which waives a hold for
// the reader ONLY and leaves it gating every other window. `wt all-clear <id>`
// releases a hold for EVERY window, so it is offered only for a hold that looks
// orphaned (coord.HoldLooksOrphaned), and always says so: the gate used to tell
// any same-checkout reader to all-clear, which handed a global release to a plain
// terminal next to a live session. A same-checkout holder also gets the likely
// reason it reads as someone else's: its session id changed (/clear,
// --fork-session or a fresh start each mint a new one). Pure.
func holdAdvice(holds, recs []coord.Record, self coord.Self, now time.Time) []string {
	lines := []string{"ack it first: wt ack <id> --state \"…\"   (waives the hold for YOU only; it keeps gating every other window)"}
	if sameCheckoutHolder(holds, self) {
		lines = append(lines, "a \"same checkout, another session\" hold may be your own from before a /clear, --fork-session or fresh start (each mints a new session id); if so, ack it")
	}
	for _, h := range holds {
		if orphaned, why := coord.HoldLooksOrphaned(recs, h, now); orphaned {
			lines = append(lines, fmt.Sprintf("%s looks orphaned (%s): if that session is gone, `wt all-clear %s` releases it for EVERY window", h.ID, why, h.ID))
		}
	}
	return lines
}

// sameCheckoutHolder reports whether any hold comes from another session in
// THIS checkout (#163). Pure.
func sameCheckoutHolder(holds []coord.Record, self coord.Self) bool {
	for _, h := range holds {
		if self.SharesCheckout(h) {
			return true
		}
	}
	return false
}

// holdersPhrase names who holds a gating hold: "another window", or — when any
// holder is a different session in THIS checkout (#163) — says so, since that
// holder shares your window id and would otherwise read as you. Pure.
func holdersPhrase(holds []coord.Record, self coord.Self) string {
	same, other := 0, 0
	for _, h := range holds {
		if self.SharesCheckout(h) {
			same++
		} else {
			other++
		}
	}
	switch {
	case same > 0 && other > 0:
		return "another window AND another session in this checkout"
	case same > 0:
		return "another session in this checkout"
	default:
		return "another window"
	}
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
		// #152: block reservations live in per-file ledgers now — GC those too, or
		// they'd grow append-only forever (the per-repo PruneLog never sees them).
		if home, herr := os.UserHomeDir(); herr == nil && home != "" {
			if bd, berr := coord.PruneBlockLedgers(home, time.Now(), dur); berr == nil {
				dropped += bd
			}
		}
		if dropped == 0 {
			ui.OK("coordination log already tidy — nothing to prune")
		} else {
			ui.OK("pruned %d resolved/expired record(s) from the coordination log + block ledgers", dropped)
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
		path, self := coordCtx(c)
		recs, err := coord.Load(path)
		if err != nil {
			ui.Err("could not read coordination log: %v", err)
			return 1
		}
		own := coord.OwnOpenAnnouncements(recs, self)
		// #163: another session's open announcements in THIS checkout are not
		// yours — but say they exist, so reading your holds reveals a second party.
		others := coord.SameCheckoutOpen(recs, self)
		defer sameCheckoutHoldsNote(others, recs, self, time.Now())
		// #152: block reservations live in per-file ledgers (cross-repo shared), not
		// the per-repo announce/ack log. They stay keyed by window only — splitting
		// them by session is out of scope for #163.
		var reserves []coord.Record
		if home, herr := os.UserHomeDir(); herr == nil && home != "" {
			reserves = coord.OwnBlockReservations(coord.LoadBlockLedgers(home), self.Window)
		}
		if len(own) == 0 && len(reserves) == 0 {
			ui.OK("no outstanding holds/announcements or block reservations for this window (%s)", selfLabel(self))
			return 0
		}
		now := time.Now()
		if len(own) > 0 {
			ui.Banner(fmt.Sprintf("your open announcements — window %s", selfLabel(self)))
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

// sameCheckoutHoldsNote tells `wt holds` that ANOTHER session in this checkout
// has open announcements (#163) — they are not yours, and the issue was
// precisely that `wt holds` listed them as yours, hiding the second party. Its
// holds get the same advice as the merge gate (holdAdvice): ack first; all-clear
// only for one that looks orphaned, and only with "for EVERY window".
func sameCheckoutHoldsNote(others, recs []coord.Record, self coord.Self, now time.Time) {
	if len(others) == 0 {
		return
	}
	var held []coord.Record
	for _, a := range others {
		if len(a.Hold) > 0 {
			held = append(held, a)
		}
	}
	ui.Warn("%d open announcement(s) (%d with a hold) in THIS checkout are another session's, so they are not listed as yours — `wt inbox` shows them. If it is still running you share one working tree (separate with `wt new <branch>`).", len(others), len(held))
	if len(held) > 0 {
		for _, line := range holdAdvice(held, recs, self, now) {
			ui.Info("%s", line)
		}
	}
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
	path, self := coordCtx(c)
	recs, err := coord.Load(path)
	if err != nil {
		return
	}
	holds := coord.PendingHolds(recs, self)
	if len(holds) == 0 {
		return
	}
	ui.Banner(fmt.Sprintf("⚠ %d active coordination hold(s) from %s — you: %s", len(holds), holdersPhrase(holds, self), selfLabel(self)))
	now := time.Now()
	for _, h := range holds {
		iss := ""
		if h.Issue > 0 {
			iss = fmt.Sprintf("  #%d", h.Issue)
		}
		fmt.Fprintf(os.Stderr, "  %s  %s  %s  %s%s\n    %s\n",
			ui.Bold(h.ID), ui.Cyan(postedBy(h, self)),
			ui.Yellow("[hold: "+strings.Join(h.Hold, ",")+"]"),
			ui.Dim(humanAge(coord.Age(h, now))), iss, h.Message)
	}
	ui.Info("ack: wt ack <id> --state \"…\"   ·   detail: wt inbox")
}

// sharedCheckoutBanner warns, at the top of `wt status`, that another session has
// recently posted coordination records under THIS checkout's window id (#163).
// Two sessions in one checkout share one working tree, and the collision engine
// is per-worktree, so nothing else can tell them apart. Best-effort: no repo /
// unreadable log / no other session → prints nothing.
func sharedCheckoutBanner(c *config.Config) {
	if c == nil {
		return
	}
	path, self := coordCtx(c)
	recs, err := coord.Load(path)
	if err != nil {
		return
	}
	now := time.Now()
	if msg := coord.SharedCheckoutWarning(coord.OtherSessions(recs, self, now, coord.SharedCheckoutWindow), self, now); msg != "" {
		ui.Warn("%s", msg)
	}
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
