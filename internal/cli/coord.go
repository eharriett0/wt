// Cross-window coordination commands (eharriett0/wt#13): announce / inbox /
// ack / all-clear over the shared ~/.wt/coordination/<repo>.jsonl log, plus the
// merge-pr hold interlock. See internal/coord for the transport + pure logic.
package cli

import (
	"bufio"
	"encoding/json"
	"flag"
	"fmt"
	"io"
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
// comes from THIS checkout (so its window id is yours), whether it is yours,
// another session's ("same checkout, another session", #163), or one wt can't
// attribute (#211); and a record carrying this session's token from another
// checkout reads as yours, placed from there. Without that, the reader sees its
// own window id and reasonably concludes it is looking at its own announcement.
// Derived from coord.View (Attribute, ProvablyWrote, PreSession), never a raw
// window compare, so the label always agrees with the ownership decision. Pure.
func postedBy(r coord.Record, v coord.View) string {
	switch v.Attribute(r) {
	case coord.AttrAnotherSession:
		return "same checkout, another session (" + coord.ShortSession(r.Session) + ")"
	case coord.AttrMine:
		return "this window (yours)"
	case coord.AttrMineFallback:
		return "this window (yours by the directory fallback: no session token, so wt can't prove it)"
	case coord.AttrUnattributable:
		return "same checkout, can't tell whose (" + unattributedWhy(r, v) + ")"
	default:
		if v.ProvablyWrote(r) {
			return "yours (this session's token), placed from checkout " + r.Window
		}
		return r.Window
	}
}

// unattributedWhy says why wt can't attribute r, a record with no session
// (#211): pre-upgrade history, or a wt without session support that is still
// writing here after a session-aware one started (View.PreSession). Pure.
func unattributedWhy(r coord.Record, v coord.View) string {
	if v.PreSession(r) {
		return "written before wt recorded sessions"
	}
	return "written by a wt without session support"
}

// whoseRecord names who a record belongs to, for the all-clear guard (#211).
// Pure.
func whoseRecord(r coord.Record, v coord.View) string {
	switch v.Attribute(r) {
	case coord.AttrMine:
		return "yours"
	case coord.AttrMineFallback:
		return "yours only by the directory fallback (no session token, like this shell)"
	case coord.AttrAnotherSession:
		return "another session's, in this checkout (" + coord.ShortSession(r.Session) + ")"
	case coord.AttrUnattributable:
		return "unattributable (" + unattributedWhy(r, v) + ")"
	default:
		if v.ProvablyWrote(r) {
			return "yours, placed from checkout " + r.Window
		}
		return "another window's (" + r.Window + ")"
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
// timestamp, window AND session (#163). A record without the session is
// unattributable (#211): its own writer could no longer release it without
// --not-mine, its own hold would gate it, and after the log first carried a
// session it would read as a still-running older wt's. Pure.
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
	if code, done := guardHelp(args, `usage: wt announce "<message>" [--file <path>] [--issue N] [--hold "op,..."]   (or --clear <id> [--not-mine])`); done {
		return code
	}
	fs := flag.NewFlagSet("announce", flag.ContinueOnError)
	issue := fs.Int("issue", 0, "mirror this announcement as a comment on GitHub issue #N")
	hold := fs.String("hold", "", "comma-separated ops other windows should avoid until all-clear (e.g. \"merge-main,flux-reconcile\")")
	clear := fs.String("clear", "", "post an all-clear for announcement <id> instead of announcing")
	notMine := fs.Bool("not-mine", false, "with --clear: release a hold this session can't prove it placed — it is released for EVERY window (#211)")
	file := fs.String("file", "", "read the message from a file (or - for stdin) instead of the argument — opaque to the shell (#75)")
	pos, _, err := parseInterspersed(fs, args)
	if err != nil {
		return 64
	}
	return withConfig(func(c *config.Config) int {
		path, self := coordCtx(c)
		if *clear != "" {
			return allClear(c, path, self, *clear, *notMine)
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
// from THIS checkout (#163) — its window equals yours, so a JSON consumer can't
// tell otherwise — and, then, whose it is: "another_session", or
// "unattributable" when wt can't tell (#211). Embedding keeps the
// array-of-records shape.
type inboxEntry struct {
	coord.Record
	SameCheckout bool   `json:"same_checkout,omitempty"`
	Attribution  string `json:"attribution,omitempty"`
}

// attributionTag is the JSON form of a same-checkout record's attribution
// (#163/#211): "another_session", "unattributable", or "" (another window, or
// yours). Pure.
func attributionTag(r coord.Record, self coord.Self) string {
	switch self.Attribute(r) {
	case coord.AttrAnotherSession:
		return "another_session"
	case coord.AttrUnattributable:
		return "unattributable"
	}
	return ""
}

// backlogLine tells `wt inbox` about the pre-upgrade announcements it no longer
// shows because they are past coord.UnattributedHorizon (#211), so they don't
// vanish silently, and where they are listed: `wt holds` reads the same records
// (the mirror too, #211 review), so it names the --issue the inbox was given
// when that is not the configured coord_issue. "" when there are none. Pure.
func backlogLine(notes, holds, issue, coordIssue int) string {
	if notes+holds == 0 {
		return ""
	}
	cmd := "wt holds"
	if issue > 0 && issue != coordIssue {
		cmd = fmt.Sprintf("wt holds --issue %d", issue)
	}
	return fmt.Sprintf("+ %d older announcement(s) in this checkout (%d with a hold) that wt can't attribute to any session — written before wt recorded sessions and past %dh, so not shown; `%s` lists them (`wt prune-coord --legacy` clears the ones nothing needs)",
		notes+holds, holds, int(coord.UnattributedHorizon.Hours()), cmd)
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
		iss := effectiveIssue(*issue, c)
		if iss > 0 {
			recs = coord.MergeByID(recs, remoteRecords(iss))
		}
		now := time.Now()
		v := coord.ViewOf(recs, self)
		box := coord.Inbox(recs, self, now)
		// newest-first: a fresh announcement is always at the top, never buried
		// under a deep backlog of old un-acked records (#147).
		for i, j := 0, len(box)-1; i < j; i, j = i+1, j-1 {
			box[i], box[j] = box[j], box[i]
		}
		clearMsg, hedged := inboxClearMessage(recs, self, now)
		bn, bh := coord.UnattributedBacklog(recs, self, now)
		backlog := backlogLine(bn, bh, iss, c.CoordIssue)
		if *asJSON {
			out := make([]inboxEntry, 0, len(box))
			for _, a := range box {
				tag := attributionTag(a, self)
				out = append(out, inboxEntry{Record: a, SameCheckout: tag != "", Attribution: tag})
			}
			enc := json.NewEncoder(os.Stdout)
			enc.SetIndent("", "  ")
			_ = enc.Encode(out)
			if len(box) == 0 && hedged {
				ui.Warn("%s", clearMsg) // stderr: the JSON on stdout stays parseable
			}
			if backlog != "" {
				ui.Warn("%s", backlog) // stderr, as above
			}
			return 0
		}
		if len(box) == 0 {
			if hedged {
				ui.Warn("%s", clearMsg)
			} else {
				ui.OK("%s", clearMsg)
			}
			if backlog != "" {
				ui.Info("%s", backlog)
			}
			return 0
		}
		ui.Info("%d un-acked announcement(s) from other windows/sessions (you: %s):", len(box), ui.Bold(selfLabel(self)))
		for _, a := range box {
			var tags string
			if len(a.Hold) > 0 {
				tags += ui.Yellow("  [hold: " + strings.Join(a.Hold, ",") + "]")
			}
			if a.Issue > 0 {
				tags += ui.Dim(fmt.Sprintf("  #%d", a.Issue))
			}
			fmt.Printf("  %s  %s  %s%s\n    %s\n", ui.Bold(a.ID), ui.Cyan(postedBy(a, v)), ui.Dim(recordAge(a, now)), tags, a.Message)
		}
		if hedged {
			ui.Warn("note: %s", tokenlessCaveat())
		}
		if backlog != "" {
			ui.Info("%s", backlog)
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
		from := postedBy(ann, coord.ViewOf(recs, self)) // #163: same checkout → another session's, or yours
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
	notes, holdsLeft := bulkAckTargets(coord.Inbox(recs, self, time.Now()))

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

const allClearUsage = "usage: wt all-clear <id> [--not-mine]"

func cmdAllClear(args []string) int {
	if code, done := guardHelp(args, allClearUsage); done {
		return code
	}
	fs := flag.NewFlagSet("all-clear", flag.ContinueOnError)
	notMine := fs.Bool("not-mine", false, "release a hold this session can't prove it placed (another session's, another window's, or unattributable) — it is released for EVERY window (#211)")
	pos, _, err := parseInterspersed(fs, args)
	if err != nil {
		return 64
	}
	if len(pos) != 1 {
		ui.Err("%s", allClearUsage)
		return 64
	}
	return withConfig(func(c *config.Config) int {
		path, self := coordCtx(c)
		return allClear(c, path, self, pos[0], *notMine)
	})
}

// clearVerdict is what `wt all-clear <id>` may do with an announcement (#211).
type clearVerdict int

const (
	clearPost   clearVerdict = iota // yours, or --not-mine: post the all-clear
	clearAsk                        // not yours, a person at a terminal: ask first
	clearRefuse                     // not yours and nobody to ask: refuse, naming --not-mine
)

// allClearVerdict decides whether self may release ann (#211). An all-clear
// releases a hold for EVERY window, so only a record carrying this session's
// own token (or its mirror hash) is released without a word, from any checkout
// (coord.Self.ProvablyWrote). Anything else — another session's, another
// window's, one wt can't attribute, and the token-less directory fallback, which
// wt counts as yours but can't prove — is asked about on a terminal and refused
// otherwise until --not-mine says it is deliberate: #211's operator was told a
// month of other sessions' announcements were theirs, and one all-clear would
// have released another live session's hold silently. Pure.
func allClearVerdict(self coord.Self, ann coord.Record, notMine, interactive bool) clearVerdict {
	switch {
	case self.ProvablyWrote(ann) || notMine:
		return clearPost
	case interactive:
		return clearAsk
	default:
		return clearRefuse
	}
}

// confirmInteractive reports whether a person can answer a confirmation prompt:
// stdin is a terminal (not /dev/null) and stderr, where the prompt goes, is one
// too. A variable so tests can drive the prompt.
var confirmInteractive = func() bool {
	if !stdinIsTTY() {
		return false
	}
	fi, err := os.Stderr.Stat()
	return err == nil && fi.Mode()&os.ModeCharDevice != 0
}

// confirmAllClear asks, on stderr, whether to release a hold this session can't
// prove it placed (#211); true only on y/yes read from in. Enter, EOF and
// anything else is no: the default [N] is the safe answer.
func confirmAllClear(id, why string, in io.Reader) bool {
	ui.Warn("%s %s — all-clear releases it for EVERY window.", id, why)
	fmt.Fprintf(os.Stderr, "%s release %s anyway? [y/N] ", ui.Yellow("→"), id)
	line, _ := bufio.NewReader(in).ReadString('\n')
	switch strings.ToLower(strings.TrimSpace(line)) {
	case "y", "yes":
		return true
	}
	return false
}

// clearRefusal is why `wt all-clear` won't release ann without --not-mine (#211):
// what it is, and, for the token-less directory fallback, why wt can't prove a
// record it otherwise counts as yours and what makes it provable. Pure.
func clearRefusal(ann coord.Record, v coord.View) (why string, fix string) {
	if v.Attribute(ann) == coord.AttrMineFallback {
		return "was written with no session token, like this shell: wt counts it as yours (the directory fallback) but can't prove it, since every token-less shell or agent in this checkout reads it the same way",
			"set WT_SESSION to a value unique to each shell or agent, and wt can prove which records are yours"
	}
	return "is " + whoseRecord(ann, v) + ", not provably this session's", ""
}

func allClear(c *config.Config, path string, self coord.Self, id string, notMine bool) int {
	local, _ := coord.Load(path)
	recs := coord.MergeByID(local, remoteRecords(c.CoordIssue)) // allow clearing a remote hold (#36)
	ann, ok := findAnnounce(recs, id)
	if !ok {
		ui.Err("no announcement with id %s", id)
		return 1
	}
	v := coord.ViewOf(recs, self)
	why, fix := clearRefusal(ann, v)
	switch allClearVerdict(self, ann, notMine, confirmInteractive()) {
	case clearRefuse:
		ui.Err("refusing to all-clear %s: it %s, and all-clear releases it for EVERY window.", id, why)
		ui.Info("to waive it for yourself only: wt ack %s   ·   to release it for everyone anyway: wt all-clear %s --not-mine", id, id)
		if fix != "" {
			ui.Info("%s", fix)
		}
		return 1
	case clearAsk:
		if !confirmAllClear(id, why, os.Stdin) {
			ui.Err("not released — %s still holds for every window (`wt ack %s` waives it for you only).", id, id)
			return 1
		}
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
	v := coord.ViewOf(recs, self)
	// Only self.Owns exempts a hold: another session's hold in THIS checkout
	// gates the merge exactly like another window's (#163), and so does one wt
	// can't attribute to any session (#211).
	fresh, stale := coord.ActiveHoldsAt(recs, self, "merge-main", now, c.HoldMaxAge)
	// Stale holds (aged past their limit — hold_max_age, or 24h for pre-upgrade
	// records wt can't attribute) WARN but never block, so a dead window can't
	// wedge merge forever (#32).
	if len(stale) > 0 {
		warnStaleHolds(stale, v, now, c.HoldMaxAge)
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
		fmt.Fprintf(os.Stderr, "    %s  %s  %s%s\n      %s\n", ui.Bold(h.ID), ui.Cyan(postedBy(h, v)), ui.Dim(recordAge(h, now)), iss, h.Message)
	}
	for _, line := range holdAdvice(fresh, recs, v, now) {
		ui.Info("%s", line)
	}
	ui.Info("or override with --bypass if you've confirmed the merge is safe alongside it.")
	return 1
}

// warnStaleHolds is the merge gate's warning for holds past their limit (#32),
// grouped by the limit that actually expired them (#211 review): hold_max_age,
// or the 24h cap on pre-upgrade records wt can't attribute, which applies even
// when hold_max_age is longer or off. It names the limit and guesses at nothing
// (a stale hold's owner may still be working). Newest staleHoldsShown of each
// group are listed.
func warnStaleHolds(stale []coord.Record, v coord.View, now time.Time, maxAge time.Duration) {
	var aged, capped []coord.Record
	for _, h := range stale {
		if _, c := v.HoldLimit(h, maxAge); c {
			capped = append(capped, h)
		} else {
			aged = append(aged, h)
		}
	}
	list := func(hs []coord.Record) {
		shown, more := newestHolds(hs, staleHoldsShown)
		for _, h := range shown {
			fmt.Fprintf(os.Stderr, "    %s  %s  %s  (all-clear: wt all-clear %s)\n",
				ui.Bold(h.ID), ui.Cyan(postedBy(h, v)), ui.Dim(recordAge(h, now)), h.ID)
		}
		if more > 0 {
			fmt.Fprintf(os.Stderr, "    %s\n", ui.Dim(fmt.Sprintf("…and %d older (wt holds lists this checkout's)", more)))
		}
	}
	if len(aged) > 0 {
		ui.Warn("%d merge-main hold(s) older than hold_max_age (%s) — NOT blocking; wt can't tell whether their owners are still at work:", len(aged), fmtLimit(maxAge))
		list(aged)
	}
	if len(capped) > 0 {
		ui.Warn("%d merge-main hold(s) written before wt recorded sessions, older than %dh — NOT blocking: wt can't tell whose they are, so they stop gating at %dh (hold_max_age: %s):",
			len(capped), int(coord.UnattributedHorizon.Hours()), int(coord.UnattributedHorizon.Hours()), fmtLimit(maxAge))
		list(capped)
	}
	ui.Info("`wt all-clear <id>` releases one for EVERY window (it asks first; without a terminal it needs --not-mine). `wt prune-coord --legacy` lists the pre-upgrade ones nothing needs any more.")
}

// fmtLimit renders a hold limit for humans: "off" for 0 (never expires), whole
// days from two up as "7d", whole hours as "24h", whole minutes as "90m",
// anything else as Go prints it. Pure.
func fmtLimit(d time.Duration) string {
	switch {
	case d <= 0:
		return "off"
	case d >= 48*time.Hour && d%(24*time.Hour) == 0:
		return fmt.Sprintf("%dd", int(d/(24*time.Hour)))
	case d%time.Hour == 0:
		return fmt.Sprintf("%dh", int(d/time.Hour))
	case d%time.Minute == 0:
		return fmt.Sprintf("%dm", int(d/time.Minute))
	}
	return d.String()
}

// holdAdvice is what wt tells a reader about holds that gate it (#163), for the
// merge gate and `wt holds`. It leads with `wt ack <id>`, which waives a hold for
// the reader ONLY and leaves it gating every other window. `wt all-clear <id>`
// releases a hold for EVERY window, so it is offered only for a hold that looks
// orphaned (coord.HoldLooksOrphaned), and always says so: the gate used to tell
// any same-checkout reader to all-clear, which handed a global release to a plain
// terminal next to a live session. A same-checkout holder also gets the likely
// reason it reads as someone else's: its session id changed (/clear,
// --fork-session or a fresh start each mint a new one). A hold carrying this
// session's own token from another checkout is named as such, and one whose age
// can't be measured says why it keeps gating. Pure.
func holdAdvice(holds, recs []coord.Record, v coord.View, now time.Time) []string {
	lines := []string{"ack it first: wt ack <id> --state \"…\"   (waives the hold for YOU only; it keeps gating every other window)"}
	if sameCheckoutHolder(holds, v.Self) {
		lines = append(lines, "a \"same checkout, another session\" hold may be your own from before a /clear, --fork-session or fresh start (each mints a new session id); if so, ack it")
	}
	if unattributedHolder(holds, v.Self) {
		lines = append(lines, "a \"can't tell whose\" hold carries no session (written by a wt without session support); it may be yours: if so, ack it")
	}
	for _, h := range holds {
		if v.Attribute(h) == coord.AttrOtherWindow && v.ProvablyWrote(h) {
			lines = append(lines, fmt.Sprintf("%s carries this session's own token, placed from checkout %s (you before a cd, or a subagent of this session): `wt all-clear %s` releases it for EVERY window; ack it instead if it still guards work there (WT_WINDOW keeps one identity across checkouts)", h.ID, h.Window, h.ID))
			continue
		}
		if note := undatedNote(h, now); note != "" {
			lines = append(lines, fmt.Sprintf("%s %s", h.ID, note))
			continue
		}
		if orphaned, why := coord.HoldLooksOrphaned(recs, h, now); orphaned {
			lines = append(lines, fmt.Sprintf("%s looks orphaned (%s): if that session is gone, `wt all-clear %s` releases it for EVERY window (it asks first)", h.ID, why, h.ID))
		}
	}
	return lines
}

// recordAge renders r's age for humans (#211 review): an unparseable timestamp
// reads "undated" and one more than a minute ahead of now "dated <d> ahead",
// never "just now" (Age reads both as 0 or less, which humanAge rounded to
// "just now"). Pure.
func recordAge(r coord.Record, now time.Time) string {
	t, err := time.Parse(time.RFC3339, r.TS)
	switch {
	case err != nil:
		return "undated"
	case t.Sub(now) > time.Minute:
		return "dated " + strings.TrimSuffix(humanAge(t.Sub(now)), " ago") + " ahead"
	}
	return humanAge(now.Sub(t))
}

// undatedNote explains why a hold whose age wt can't measure keeps gating (#211
// review): blocking is the safe direction, and an all-clear is the way out. ""
// for a hold dated sensibly. Pure.
func undatedNote(h coord.Record, now time.Time) string {
	switch age := recordAge(h, now); {
	case age == "undated":
		return "has an undatable timestamp: wt can't tell its age, so it never ages out (blocking is the safe direction); if nothing needs it, `wt all-clear " + h.ID + "` releases it for EVERY window"
	case strings.HasPrefix(age, "dated "):
		return "is " + age + " of this clock (a skewed clock?): its age can't be measured yet, so it keeps gating until it ages past its limit (the safe direction)"
	}
	return ""
}

// staleHoldsShown caps the gate's stale-hold list (#211): an upgraded checkout
// can carry dozens of never-cleared pre-#163 holds, now that they no longer read
// as the reader's own.
const staleHoldsShown = 5

// newestHolds returns the newest n of recs (log order is oldest first) and how many
// it left out. Pure.
func newestHolds(recs []coord.Record, n int) (shown []coord.Record, more int) {
	if len(recs) <= n {
		return recs, 0
	}
	return recs[len(recs)-n:], len(recs) - n
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

// unattributedHolder reports whether any hold is one wt can't attribute to a
// session in THIS checkout (#211). Pure.
func unattributedHolder(holds []coord.Record, self coord.Self) bool {
	for _, h := range holds {
		if self.Unattributed(h) {
			return true
		}
	}
	return false
}

// holdersPhrase names who holds a gating hold: "another window", "another
// session in this checkout" (#163), "an unattributable writer in this checkout"
// (#211), joined when there are several kinds — a same-checkout holder shares
// your window id and would otherwise read as you. Pure.
func holdersPhrase(holds []coord.Record, self coord.Self) string {
	var other, same, unattr bool
	for _, h := range holds {
		switch self.Attribute(h) {
		case coord.AttrAnotherSession:
			same = true
		case coord.AttrUnattributable:
			unattr = true
		default:
			other = true
		}
	}
	var kinds []string
	if other {
		kinds = append(kinds, "another window")
	}
	if same {
		kinds = append(kinds, "another session in this checkout")
	}
	if unattr {
		kinds = append(kinds, "an unattributable writer in this checkout")
	}
	if len(kinds) == 0 {
		return "another window"
	}
	return strings.Join(kinds, " AND ")
}

// cmdPruneCoord GCs the coordination log — drops completed (all-cleared)
// announce+ack+all-clear handshakes and aged-out block reservations, keeping
// every still-open record (#33). The log is append-only and re-parsed on every
// command, so this bounds a file that otherwise grows forever.
//
// `--legacy` is the one-time cleanup of the pre-upgrade backlog (#211 review): it
// LISTS the open announcements written before wt recorded sessions that gate or
// show nothing for any reader any more (coord.LegacyPrunable) and drops them,
// with their acks, only with -y. A dry run by default because it deletes open
// records nobody released: the list is what the operator reviews.
func cmdPruneCoord(args []string) int {
	fs := flag.NewFlagSet("prune-coord", flag.ContinueOnError)
	blockAge := fs.String("block-max-age", "24h", "drop block-id reservations older than this")
	legacy := fs.Bool("legacy", false, "list the announcements written before wt recorded sessions that nothing needs any more (#211); add -y to drop them")
	apply := fs.Bool("y", false, "with --legacy: drop what it lists (default: list only)")
	fs.BoolVar(apply, "yes", false, "alias for -y")
	if err := fs.Parse(args); err != nil {
		return 64
	}
	if fs.NArg() > 0 {
		ui.Err("usage: wt prune-coord [--block-max-age D] | wt prune-coord --legacy [-y]")
		return 64
	}
	if *apply && !*legacy {
		ui.Err("-y only applies to --legacy (a plain prune-coord drops only resolved records, and needs no confirmation)")
		return 64
	}
	if *legacy {
		return withConfig(func(c *config.Config) int { return pruneLegacy(c, *apply) })
	}
	dur, derr := config.ParseAge(*blockAge)
	if derr != nil {
		ui.Err("bad --block-max-age: %v", derr)
		return 64
	}
	return withConfig(func(c *config.Config) int {
		path, _ := coordCtx(c)
		now := time.Now()
		dropped, err := coord.PruneLog(path, now, dur)
		if err != nil {
			ui.Err("prune failed: %v", err)
			return 1
		}
		// #152: block reservations live in per-file ledgers now — GC those too, or
		// they'd grow append-only forever (the per-repo PruneLog never sees them).
		if home, herr := os.UserHomeDir(); herr == nil && home != "" {
			if bd, berr := coord.PruneBlockLedgers(home, now, dur); berr == nil {
				dropped += bd
			}
		}
		if dropped == 0 {
			ui.OK("coordination log already tidy — nothing to prune")
		} else {
			ui.OK("pruned %d resolved/expired record(s) from the coordination log + block ledgers", dropped)
		}
		if recs, lerr := coord.Load(path); lerr == nil {
			if n := len(coord.LegacyPrunable(recs, now, c.HoldMaxAge)); n > 0 {
				ui.Info("%d open announcement(s) written before wt recorded sessions gate or show nothing for anyone any more — `wt prune-coord --legacy` lists them (it drops them only with -y)", n)
			}
		}
		return 0
	})
}

// pruneLegacy is `wt prune-coord --legacy [-y]` (#211 review): list, and with
// apply drop, the open announcements written before wt recorded sessions that
// nothing needs any more (coord.LegacyPrunable). It says what it keeps and why,
// so a hold left in place is never a surprise.
func pruneLegacy(c *config.Config, apply bool) int {
	path, _ := coordCtx(c)
	now := time.Now()
	recs, err := coord.Load(path)
	if err != nil {
		ui.Err("could not read coordination log: %v", err)
		return 1
	}
	cands, dropped, err := coord.PruneLegacy(path, now, c.HoldMaxAge, apply)
	if err != nil {
		ui.Err("prune failed: %v", err)
		return 1
	}
	kept := len(coord.NoSessionOpen(recs)) - len(cands)
	keptNote := ""
	if kept > 0 {
		keptNote = fmt.Sprintf(" %d other open announcement(s) with no session stay: a hold that still gates another window (hold_max_age %s), one younger than %dh, one written by a wt without session support after sessions began here, or one whose age can't be measured.",
			kept, fmtLimit(c.HoldMaxAge), int(coord.UnattributedHorizon.Hours()))
	}
	if len(cands) == 0 {
		ui.OK("no announcement written before wt recorded sessions is past every reader's limit — nothing to drop.%s", keptNote)
		return 0
	}
	if apply {
		ui.OK("dropped %d record(s): %d announcement(s) written before wt recorded sessions that gated or showed nothing for anyone, and their acks.%s", dropped, len(cands), keptNote)
		return 0
	}
	ui.Info("%d open announcement(s) written before wt recorded sessions gate or show nothing for any reader any more (dry run — nothing dropped):", len(cands))
	for _, a := range cands {
		tag := ""
		if len(a.Hold) > 0 {
			tag = "  " + ui.Yellow("[hold: "+strings.Join(a.Hold, ",")+"]")
		}
		fmt.Printf("  %s  %s  %s%s  %s\n", ui.Bold(a.ID), a.Window, ui.Dim(recordAge(a, now)), tag, oneLine(a.Message, 72))
	}
	if keptNote != "" {
		ui.Info("%s", strings.TrimSpace(keptNote))
	}
	ui.Step("drop them, and their acks: wt prune-coord --legacy -y")
	return 0
}

// oneLine is s on one line, cut to n runes with "…". Pure.
func oneLine(s string, n int) string {
	s = strings.Join(strings.Fields(s), " ")
	if r := []rune(s); len(r) > n {
		return string(r[:n]) + "…"
	}
	return s
}

// cmdHolds lists THIS session's own outstanding announcements/holds (each with a
// copy-pasteable all-clear line) + its block-id reservations, so the
// announce->hold->all-clear lifecycle is self-service instead of grepping the
// jsonl for an id (#34). Only records wt counts as this session's are "yours"
// (#211): the token-less directory fallback is listed with a caveat (wt can't
// prove it), ones wt can't attribute get their own section with no all-clear
// lines, and another session's are counted, never listed as yours. Like `wt
// inbox`, it reads the coordination mirror (--issue, default coord_issue), so the
// records inbox counts are the ones it lists.
func cmdHolds(args []string) int {
	fs := flag.NewFlagSet("holds", flag.ContinueOnError)
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
		if iss := effectiveIssue(*issue, c); iss > 0 {
			recs = coord.MergeByID(recs, remoteRecords(iss))
		}
		now := time.Now()
		v := coord.ViewOf(recs, self)
		own := coord.OwnOpenAnnouncements(recs, self)
		// #163: another session's open announcements in THIS checkout are not
		// yours — but say they exist, so reading your holds reveals a second party.
		others := coord.SameCheckoutOpen(recs, self)
		defer sameCheckoutHoldsNote(others, recs, v, now)
		// #211: ones wt can't attribute to any session are not yours either.
		defer unattributedHoldsSection(coord.UnattributedOpen(recs, self), coord.GatingHolds(recs, self, now, c.HoldMaxAge), v, now, c.HoldMaxAge)
		// #152: block reservations live in per-file ledgers (cross-repo shared), not
		// the per-repo announce/ack log. They stay keyed by window only — splitting
		// them by session is out of scope for #163/#211.
		var reserves []coord.Record
		if home, herr := os.UserHomeDir(); herr == nil && home != "" {
			reserves = coord.OwnBlockReservations(coord.LoadBlockLedgers(home), self.Window)
		}
		if len(own) == 0 && len(reserves) == 0 {
			ui.OK("no outstanding holds/announcements of yours, and no block reservations for this window (%s)", selfLabel(self))
			return 0
		}
		if len(own) > 0 {
			ui.Banner(fmt.Sprintf("your open announcements — window %s", selfLabel(self)))
			fallback := 0
			for _, a := range own {
				tag := ""
				if len(a.Hold) > 0 {
					tag = " " + ui.Yellow("[hold: "+strings.Join(a.Hold, ",")+"]")
				}
				if v.Attribute(a) == coord.AttrMineFallback {
					fallback++
					tag += " " + ui.Dim("(no session token: see below)")
				}
				fmt.Printf("  %s  %s%s\n    %s\n    all-clear: %s\n",
					ui.Bold(a.ID), ui.Dim(recordAge(a, now)), tag, a.Message,
					ui.Cyan("wt all-clear "+a.ID))
			}
			if fallback > 0 {
				ui.Warn("%s", fallbackCaveat(fallback))
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

// fallbackCaveat is `wt holds`' note on the directory fallback (#211 review): n
// of "your" records carry no session token, like the shell reading them, so wt
// counts them as yours without proof, and all-clear asks before releasing them.
// Pure.
func fallbackCaveat(n int) string {
	return fmt.Sprintf("%d of these carry no session token, like this shell: wt counts them as yours (the directory fallback) but can't prove it — every token-less shell or agent in this checkout reads them as its own. `wt all-clear` asks before releasing one (without a terminal it needs --not-mine). Set WT_SESSION to a value unique to each shell or agent and wt can prove which are yours.", n)
}

// unattributedShown caps the holds `wt holds` lists in its can't-tell-whose
// section that no longer gate anything (#211); the rest are counted. A hold that
// still gates is always listed.
const unattributedShown = 10

// unattributedView picks what `wt holds` lists of the open announcements in this
// checkout that wt can't attribute to any session (#211): EVERY hold that still
// gates (gating: coord.GatingHolds — never folded behind a count, #211 review),
// the newest unattributedShown holds that no longer gate, and the plain ones
// that are still a live signal (younger than coord.UnattributedHorizon, written
// by a still-running older wt, or undatable). Older ones are only counted:
// plain ones are informational and past any live signal, and a month of them is
// what buried #211's operator's real holds under "your open announcements".
// Newest first. Pure.
func unattributedView(open []coord.Record, gating map[string]bool, v coord.View, now time.Time) (show []coord.Record, olderHolds, olderNotes, live int) {
	held := 0
	for i := len(open) - 1; i >= 0; i-- {
		a := open[i]
		switch {
		case len(a.Hold) > 0 && gating[a.ID]:
			live++
			show = append(show, a)
		case len(a.Hold) > 0 && held < unattributedShown:
			held++
			show = append(show, a)
		case len(a.Hold) > 0:
			olderHolds++
		case !v.PreSession(a) || coord.Age(a, now) <= coord.UnattributedHorizon:
			show = append(show, a)
		default:
			olderNotes++
		}
	}
	return show, olderHolds, olderNotes, live
}

// holdStatus tags an unattributable hold in `wt holds` (#211 review): whether it
// still gates this reader (and merge-pr, when it holds merge-main), and if not,
// why — past its limit (named), or acked by this session. Pure.
func holdStatus(a coord.Record, gating map[string]bool, v coord.View, now time.Time, maxAge time.Duration) string {
	if gating[a.ID] {
		if coord.HoldCovers(a.Hold, "merge-main") {
			return "still blocks merge-pr"
		}
		return "still live"
	}
	if limit, capped := v.HoldLimit(a, maxAge); limit > 0 && coord.Age(a, now) > limit {
		if capped {
			return fmt.Sprintf("stale: past %dh, the limit for a hold wt can't attribute; no longer gates", int(coord.UnattributedHorizon.Hours()))
		}
		return "stale: past hold_max_age (" + fmtLimit(maxAge) + "); no longer gates"
	}
	return "acked by you; no longer gates you"
}

// unattributedHoldsSection prints `wt holds`' section for open announcements in
// this checkout that wt can't attribute to any session (#211), WITHOUT all-clear
// lines: they are not provably yours, and an all-clear releases a hold for every
// window. Nothing when there are none.
func unattributedHoldsSection(open []coord.Record, gating map[string]bool, v coord.View, now time.Time, maxAge time.Duration) {
	if len(open) == 0 {
		return
	}
	show, olderHolds, olderNotes, live := unattributedView(open, gating, v, now)
	ui.Banner(fmt.Sprintf("can't tell whose — %d open in this checkout, no session wt can match (NOT listed as yours)", len(open)))
	for _, a := range show {
		tag := ""
		if len(a.Hold) > 0 {
			tag = " " + ui.Yellow("[hold: "+strings.Join(a.Hold, ",")+"]") + " " + ui.Dim("("+holdStatus(a, gating, v, now, maxAge)+")")
		}
		why := ""
		if !v.PreSession(a) {
			why = "\n    " + ui.Dim("("+unattributedWhy(a, v)+")")
		}
		fmt.Printf("  %s  %s%s\n    %s%s\n", ui.Bold(a.ID), ui.Dim(recordAge(a, now)), tag, a.Message, why)
	}
	if olderHolds+olderNotes > 0 {
		fmt.Printf("  %s\n", ui.Dim(fmt.Sprintf("+ %d older hold(s) that no longer gate anything and %d plain announcement(s) older than %dh, not listed — %d hold(s) here still gate, all listed above",
			olderHolds, olderNotes, int(coord.UnattributedHorizon.Hours()), live)))
	}
	ui.Info("wt can't tell who wrote these (no session recorded). `wt ack <id>` waives a hold for you only; `wt all-clear <id>` asks first, since it releases it for EVERY window; `wt prune-coord --legacy` lists the pre-upgrade ones nothing needs any more.")
}

// sameCheckoutHoldsNote tells `wt holds` that ANOTHER session in this checkout
// has open announcements (#163) — they are not yours, and the issue was
// precisely that `wt holds` listed them as yours, hiding the second party. Its
// holds get the same advice as the merge gate (holdAdvice): ack first; all-clear
// only for one that looks orphaned, and only with "for EVERY window".
func sameCheckoutHoldsNote(others, recs []coord.Record, v coord.View, now time.Time) {
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
		for _, line := range holdAdvice(held, recs, v, now) {
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
	now := time.Now()
	holds := coord.PendingHolds(recs, self, now)
	if len(holds) == 0 {
		return
	}
	v := coord.ViewOf(recs, self)
	ui.Banner(fmt.Sprintf("⚠ %d active coordination hold(s) from %s — you: %s", len(holds), holdersPhrase(holds, self), selfLabel(self)))
	for _, h := range holds {
		iss := ""
		if h.Issue > 0 {
			iss = fmt.Sprintf("  #%d", h.Issue)
		}
		fmt.Fprintf(os.Stderr, "  %s  %s  %s  %s%s\n    %s\n",
			ui.Bold(h.ID), ui.Cyan(postedBy(h, v)),
			ui.Yellow("[hold: "+strings.Join(h.Hold, ",")+"]"),
			ui.Dim(recordAge(h, now)), iss, h.Message)
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
