// Package coord is wt's cross-window coordination channel (eharriett0/wt#13).
//
// Concurrent operator windows on the same machine need to hand off disruptive
// changes safely: one window ANNOUNCEs (optionally declaring a HOLD on some
// operations), other windows ACK with their in-flight state, and an ALL-CLEAR
// releases the hold. Today that handshake is carried by a human copy-pasting
// between windows; this package makes it native.
//
// Transport is an append-only JSONL log at ~/.wt/coordination/<repo>.jsonl —
// every window on the machine appends to it and tails it. No server, no daemon.
// The logic here is pure (records in, verdicts out) so it is fully unit-tested;
// the CLI layer owns IO, window identity, and the optional GitHub mirror.
package coord

import (
	"bufio"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/eharriett0/wt/internal/lock"
)

// Kind enumerates the record types on the coordination log.
const (
	KindAnnounce = "announce"
	KindAck      = "ack"
	KindAllClear = "all-clear"
	// KindBlockReserve records a reserved append-log block id (#23): a window
	// atomically claims the next NEWEST-N slot on a shared append-log doc so two
	// windows never grab the same N.
	KindBlockReserve = "block-reserve"
	// KindBlockWritten is the terminal signal for a reservation (#35): the window
	// actually wrote (prepended) block N to the file. It clears the "prepend
	// imminent" banner + `wt holds` entry immediately, and marks the (file, N)
	// pair resolved so prune-coord can GC the reservation. A reservation that is
	// never written ages out (DefaultBlockReserveTTL) and frees its id instead of
	// permanently burning it. File/Block identify the pair; AckOf links back to
	// the reservation record's ID (best-effort provenance).
	KindBlockWritten = "block-written"
	// KindBlockAbandoned is the OTHER terminal signal for a reservation (#160,
	// follow-up to #35): the window reserved N but decided NOT to write it (e.g. a
	// bad scan handed out a stale id, so it wrote a different number instead).
	// Like block-written it clears the banner + `wt holds` entry and lets
	// prune-coord GC the pair — but UNLIKE block-written it FREES the id for reuse
	// (the id is not in the file), so NextBlock stops counting it immediately
	// rather than waiting out DefaultBlockReserveTTL.
	KindBlockAbandoned = "block-abandoned"
)

// DefaultBlockReserveTTL bounds how long an UN-written block reservation is
// honored: after this it's assumed written-or-abandoned, so it stops surfacing
// in the wt-status banner AND stops inflating the next allocated id (a crashed
// window that reserved but never prepended no longer burns that N). The
// reserve→write gap is seconds in practice, so 30m is comfortably safe.
const DefaultBlockReserveTTL = 30 * time.Minute

// Record is one line on the coordination log.
type Record struct {
	ID      string   `json:"id"`
	TS      string   `json:"ts"`     // RFC3339
	Window  string   `json:"window"` // announcing/acking window (WindowID: the checkout)
	Repo    string   `json:"repo"`   // repo slug
	Kind    string   `json:"kind"`   // announce | ack | all-clear | block-reserve
	Message string   `json:"message,omitempty"`
	Issue   int      `json:"issue,omitempty"`  // mirrored GitHub issue #, if any
	Hold    []string `json:"hold,omitempty"`   // ops other windows should avoid until all-clear
	AckOf   string   `json:"ack_of,omitempty"` // announce id this record acks / clears
	State   string   `json:"state,omitempty"`  // one-line in-flight state (on ack)
	File    string   `json:"file,omitempty"`   // block-reserve: the append-log doc
	Block   int      `json:"block,omitempty"`  // block-reserve: the reserved block id (N)
	// Session is the writer's per-session token (#163): SessionToken, or
	// SessionNone when its environment carried none. Empty ONLY on a record
	// written before sessions existed — the back-compat wildcard in Self.Owns.
	Session string `json:"session,omitempty"`
}

// SessionNone is the session of a writer/reader whose environment carries no
// session token (#163). It is deliberately NON-empty: "" is reserved for records
// that predate sessions and stays a wildcard (exactly the old behaviour), while a
// token-less session that knows about sessions is told apart from a tagged one —
// a terminal with no session id and a Claude Code session sharing one checkout
// are two parties, not one.
const SessionNone = "none"

// SessionEnvVars is the session-token precedence (#163), first non-empty wins:
//
//  1. WT_SESSION — explicit override; set the same value in several shells to
//     make them ONE session, or a distinct one per agent to split them.
//  2. CLAUDE_CODE_SESSION_ID — Claude Code exports it into every Bash-tool shell
//     AND into its hook processes, set from the same session id the hook payload
//     carries, so the CLI and the per-turn hook agree. `--resume`/`--continue`
//     keep it, so a resumed agent still owns its holds. `--fork-session` AND
//     `/clear` mint a new one (verified in the 2.1.292 binary: the conversation
//     reset rewrites it), and so does starting a fresh session: that session has
//     no memory of the old one's holds, so they gate it like another session's —
//     the gate names /clear as the likely reason. Subagents share their parent's.
//  3. CODEX_SESSION_ID — Codex exports it into every shell command it runs
//     (openai/codex#37848, `inject_session_env`): the root thread's id, shared by
//     its subagent threads. Codex hook processes do NOT get it (a hook runs with
//     the Codex process's own environment), so the codex-context hook falls back
//     to its payload's session_id, which Codex fills from the same
//     Session::session_id() (cli.agentHookSession).
//
// Terminal ids (TERM_SESSION_ID from Terminal.app, ITERM_SESSION_ID from iTerm2)
// are deliberately NOT used. They are inherited by everything started in the tab
// — tmux/screen panes, an editor launched from it — so two agents there would
// share one "session" with false confidence; and they split one human's tabs
// into separate parties, so a hold announced in one tab would block its owner's
// merge in another and a WT_WINDOW pinned across terminals would stop exempting
// its own hold (#18). A shell with none of these variables is SessionNone.
var SessionEnvVars = []string{"WT_SESSION", "CLAUDE_CODE_SESSION_ID", "CODEX_SESSION_ID"}

// SessionToken resolves this process's session token from getenv per
// SessionEnvVars, returning it and the variable it came from. ("", "") when none
// is set (callers record that as SessionNone). Pure given getenv.
func SessionToken(getenv func(string) string) (token, source string) {
	for _, k := range SessionEnvVars {
		if v := strings.TrimSpace(getenv(k)); v != "" {
			return v, k
		}
	}
	return "", ""
}

// Self is a process's coordination identity: the checkout (WindowID — path
// based and deliberately unchanged, #18/#156) plus the session within it (#163).
// The zero Session matches every session (the pre-#163 behaviour); live callers
// build Self with CurrentSelf, which always sets one.
type Self struct {
	Window  string
	Session string
}

// CurrentSelf builds this process's identity: WindowID(WT_WINDOW, toplevel,
// branch) + SessionToken, with SessionNone when no token is set. Pure given getenv.
func CurrentSelf(getenv func(string) string, toplevel, branch string) Self {
	s, _ := SessionToken(getenv)
	if s == "" {
		s = SessionNone
	}
	return Self{Window: WindowID(getenv("WT_WINDOW"), toplevel, branch), Session: s}
}

// Owns reports whether r was written by self: same window AND (same session, or
// either side carries no session at all). It is the ONE ownership predicate —
// inbox, acks, `wt holds` and the merge-pr hold gate's own-hold exemption all go
// through it (#163). Two sessions sharing a checkout resolve to the same window,
// and before sessions each treated the other's announcements as its own: inbox
// said "clear", `wt holds` listed them as yours, and the gate exempted their
// holds. Both sides carrying a session (a token, or SessionNone) that differ is
// another party — never own. An empty Session only exists on pre-#163 records
// (and on a zero Self), so the wildcard keeps them exactly as they were.
//
// A record read back from the GitHub mirror carries MirrorSession(token), never
// the token, so self also owns a record whose session is the hash of its own
// token. Without that, #18 broke across checkouts that share only the mirror
// (two clones with different directory names keep separate local logs): a
// session pinned with WT_WINDOW announced a hold in one clone and was blocked by
// it in the other. Only a real token hashes; "" and SessionNone never do, so a
// token-less shell can't claim a tagged session's mirrored record. Pure.
func (s Self) Owns(r Record) bool {
	if r.Window != s.Window {
		return false
	}
	if r.Session == "" || s.Session == "" || r.Session == s.Session {
		return true
	}
	return s.Session != SessionNone && r.Session == MirrorSession(s.Session)
}

// SharesCheckout reports whether r was written from self's checkout by ANOTHER
// session — the party #163 was about, whose records carry self's own window id.
// Derived from Owns, so labels ("same checkout, another session") can never
// disagree with the ownership decision. Pure.
func (s Self) SharesCheckout(r Record) bool {
	return r.Window == s.Window && !s.Owns(r)
}

// ShortSession renders a session token for humans: SessionNone as "no session
// token", a short token (a WT_SESSION label) as is, a long one (a UUID) cut to
// its first 8 runes, and a mirrored hash (MirrorSession) as its first 8 hex
// digits marked "(mirrored)" — the "sha256:" prefix alone told every remote
// session apart from none of the others. Pure.
func ShortSession(s string) string {
	switch s {
	case "":
		return "session unknown"
	case SessionNone:
		return "no session token"
	}
	return "session " + ShortToken(s)
}

// ShortToken is ShortSession's token part alone, for a field already labelled
// "session" (wt doctor): a mirrored hash as its first 8 hex digits + "(mirrored)",
// a long token cut to 8 runes + "…", a short one as is. Pure.
func ShortToken(s string) string {
	if hexPart, ok := strings.CutPrefix(s, mirrorSessionPrefix); ok {
		if r := []rune(hexPart); len(r) > 8 {
			hexPart = string(r[:8])
		}
		return hexPart + " (mirrored)"
	}
	if r := []rune(s); len(r) > 12 {
		return string(r[:8]) + "…"
	}
	return s
}

// LogPath returns the coordination log path for repo under home's ~/.wt.
func LogPath(home, repo string) string {
	return filepath.Join(home, ".wt", "coordination", slug(repo)+".jsonl")
}

// slug makes a repo identifier filesystem-safe.
func slug(repo string) string {
	repo = strings.TrimSpace(repo)
	if repo == "" {
		return "repo"
	}
	var b strings.Builder
	for _, r := range repo {
		switch {
		case r >= 'a' && r <= 'z', r >= 'A' && r <= 'Z', r >= '0' && r <= '9', r == '-', r == '_', r == '.':
			b.WriteRune(r)
		default:
			b.WriteRune('-')
		}
	}
	return b.String()
}

// mirrorFence is the code-block language tag wrapping the machine-readable
// record inside a mirrored GitHub comment (#36) — human markdown above, exact
// record below, so the write-only mirror becomes read-back-able.
const mirrorFence = "wt-record"

var mirrorBlockRe = regexp.MustCompile("(?s)```" + mirrorFence + "\\s*\\n(.*?)\\n```")

// MirrorJSONBlock renders r as a fenced JSON block to append to a mirrored
// comment, so a machine reading the issue back can reconstruct the exact record.
func MirrorJSONBlock(r Record) string {
	r.Session = MirrorSession(r.Session)
	b, _ := json.Marshal(r)
	return "```" + mirrorFence + "\n" + string(b) + "\n```"
}

// mirrorSessionPrefix marks a session as MirrorSession's hash of a token.
const mirrorSessionPrefix = "sha256:"

// MirrorSession is the session a mirrored record carries (#163): a stable hash
// of the token, never the token itself. The raw value is a local agent session
// id (Claude Code names its transcripts after it) and a mirror issue may be
// public; a hash still lets another machine tell sessions apart. Only the
// session whose token hashes to it owns a mirrored record (Self.Owns), so a
// session still owns its own records when it reads them back from the mirror
// (#18), and no other session can. "" (pre-#163) and SessionNone pass through.
// Pure.
func MirrorSession(s string) string {
	if s == "" || s == SessionNone || strings.HasPrefix(s, mirrorSessionPrefix) {
		return s
	}
	sum := sha256.Sum256([]byte(s))
	return mirrorSessionPrefix + hex.EncodeToString(sum[:])[:16]
}

// ParseMirroredRecords extracts coord.Records from mirrored comment bodies — the
// read-back half of the --issue mirror (#36). A body with no wt-record block, or
// malformed JSON, is skipped. Pure.
func ParseMirroredRecords(bodies []string) []Record {
	var out []Record
	for _, body := range bodies {
		for _, m := range mirrorBlockRe.FindAllStringSubmatch(body, -1) {
			var r Record
			if json.Unmarshal([]byte(m[1]), &r) == nil && r.ID != "" {
				out = append(out, r)
			}
		}
	}
	return out
}

// MergeByID unions local and remote records, de-duped by ID (local wins). Order
// is all local, then remote records whose ID isn't already present — so a
// machine folds cross-machine mirror records into its own log view without
// double-counting its own echoed-back announces (#36). Pure.
func MergeByID(local, remote []Record) []Record {
	seen := make(map[string]bool, len(local))
	out := make([]Record, 0, len(local)+len(remote))
	for _, r := range local {
		seen[r.ID] = true
		out = append(out, r)
	}
	for _, r := range remote {
		if !seen[r.ID] {
			seen[r.ID] = true
			out = append(out, r)
		}
	}
	return out
}

// Append writes r as one JSON line to path, creating parent dirs as needed.
func Append(path string, r Record) error {
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return err
	}
	f, err := os.OpenFile(path, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o644)
	if err != nil {
		return err
	}
	defer f.Close()
	b, err := json.Marshal(r)
	if err != nil {
		return err
	}
	if _, err := f.Write(append(b, '\n')); err != nil {
		return err
	}
	return nil
}

// Load reads every record from path in order. A missing file is not an error
// (no coordination yet) — it returns an empty slice. Malformed lines are
// skipped rather than failing the read (a partial/corrupt line must not blind a
// window to every other announcement).
func Load(path string) ([]Record, error) {
	f, err := os.Open(path)
	if err != nil {
		if os.IsNotExist(err) {
			return nil, nil
		}
		return nil, err
	}
	defer f.Close()
	var recs []Record
	sc := bufio.NewScanner(f)
	sc.Buffer(make([]byte, 0, 64*1024), 1024*1024)
	for sc.Scan() {
		line := strings.TrimSpace(sc.Text())
		if line == "" {
			continue
		}
		var r Record
		if json.Unmarshal([]byte(line), &r) == nil && r.ID != "" {
			recs = append(recs, r)
		}
	}
	return recs, sc.Err()
}

// cleared reports the set of announce ids that have an all-clear record.
func cleared(recs []Record) map[string]bool {
	m := map[string]bool{}
	for _, r := range recs {
		if r.Kind == KindAllClear && r.AckOf != "" {
			m[r.AckOf] = true
		}
	}
	return m
}

// ackedBy reports the set of announce ids that self has acked. Another session's
// ack in the same checkout is not self's (#163): it read the announcement, self
// did not.
func ackedBy(recs []Record, self Self) map[string]bool {
	m := map[string]bool{}
	for _, r := range recs {
		if r.Kind == KindAck && r.AckOf != "" && self.Owns(r) {
			m[r.AckOf] = true
		}
	}
	return m
}

// Announcements returns all announce records (in log order).
func announcements(recs []Record) []Record {
	var out []Record
	for _, r := range recs {
		if r.Kind == KindAnnounce {
			out = append(out, r)
		}
	}
	return out
}

// Inbox returns announcements from OTHER windows — or from another session in
// this same checkout (#163) — that self has not acked and that have not been
// all-cleared, i.e. the ones needing this session's attention. Self's own
// announcements are excluded (you don't ack yourself).
func Inbox(recs []Record, self Self) []Record {
	cl := cleared(recs)
	acked := ackedBy(recs, self)
	var out []Record
	for _, a := range announcements(recs) {
		if self.Owns(a) || cl[a.ID] || acked[a.ID] {
			continue
		}
		out = append(out, a)
	}
	return out
}

// PendingHolds returns the subset of this window's inbox (un-acked, un-cleared
// announcements from OTHER windows or sessions) that declare a hold — the ones wt
// should surface proactively before the window acts (the ambient-banner signal).
func PendingHolds(recs []Record, self Self) []Record {
	var out []Record
	for _, a := range Inbox(recs, self) {
		if len(a.Hold) > 0 {
			out = append(out, a)
		}
	}
	return out
}

// Acks returns the ack records for a given announce id, in order.
func Acks(recs []Record, announceID string) []Record {
	var out []Record
	for _, r := range recs {
		if r.Kind == KindAck && r.AckOf == announceID {
			out = append(out, r)
		}
	}
	return out
}

// HoldCovers reports whether a declared hold set covers op. An entry matches op
// exactly, matches the family before a ':' scope (entry "kubectl-mutate" covers
// "kubectl-mutate:harbor"), or is the catch-all "*".
func HoldCovers(hold []string, op string) bool {
	for _, h := range hold {
		h = strings.TrimSpace(h)
		if h == "*" || h == op {
			return true
		}
		if strings.HasPrefix(op, h+":") {
			return true
		}
	}
	return false
}

// WindowID picks a STABLE window identity for coordination, resilient to branch
// switches within a checkout (#18). The old identity was the current branch, so
// announcing a `--hold` from branch W then running `merge-pr` after the branch
// flipped (the shared-checkout contamination of #15) made a window self-block on
// its OWN hold: ActiveHolds exempts only holds self.Owns — same window AND same
// session (#163) — so the window half must be stable. WindowID is only that half:
// two sessions in one checkout get the SAME window id, and Self.Session (not this
// function) tells them apart. Precedence:
//
//  1. WT_WINDOW env — explicit, survives dir AND branch changes; set per terminal
//     to pin identity across checkouts (the fix for announcing in one dir and
//     merging from another — within one session; a different agent session is a
//     different party even under the same WT_WINDOW) and to get a short label.
//  2. worktree toplevel PATH (canonical, full) — stable across `git checkout`
//     within a dir (the branch flips, the dir doesn't), so announce + merge from
//     one checkout keep one identity even if the branch changed between them.
//     The FULL path (not its basename) is load-bearing: two distinct working
//     trees that share a dir leaf name — e.g. two `git clone`s both named "me" on
//     different branches, which share one coordination log — must NOT collapse to
//     one identity, or one would silently bypass the other's merge-main hold (the
//     adversarial-verify regression, 2026-07-23). The old branch identity was
//     collision-free only because git enforces one-worktree-per-branch; the path
//     is collision-free by construction.
//  3. current branch — last-resort fallback (the original behavior).
//
// Never returns "" — everything empty degrades to "detached".
func WindowID(env, toplevel, branch string) string {
	if w := strings.TrimSpace(env); w != "" {
		return w
	}
	if t := strings.TrimSpace(toplevel); t != "" {
		return filepath.Clean(t)
	}
	if b := strings.TrimSpace(branch); b != "" {
		return b
	}
	return "detached"
}

// ActiveHolds returns announcements from OTHER windows (or another session in
// this checkout, #163) whose hold covers op, that have not been all-cleared and
// that self has not acked. These are the holds that should block/warn an
// operation (e.g. merge-pr checking "merge-main"). Acking a hold clears it for
// self — you've acknowledged the coordination. Only self.Owns exempts a hold.
func ActiveHolds(recs []Record, self Self, op string) []Record {
	cl := cleared(recs)
	acked := ackedBy(recs, self)
	var out []Record
	for _, a := range announcements(recs) {
		if self.Owns(a) || cl[a.ID] || acked[a.ID] {
			continue
		}
		if HoldCovers(a.Hold, op) {
			out = append(out, a)
		}
	}
	return out
}

// PruneRecords returns the log with resolved + expired records removed (#33):
// (a) every announcement that has been all-cleared, together with its acks and
// the all-clear record itself (a completed handshake — no longer live); and (b)
// block-id reservations older than blockMaxAge (they're consumed within
// minutes). Every STILL-OPEN announcement (incl. un-cleared stale holds — the
// operator all-clears those, they're not silently GC'd), its acks, and any
// other record are kept. Pure. dropped = len(recs) - len(kept).
func PruneRecords(recs []Record, now time.Time, blockMaxAge time.Duration) (kept []Record, dropped int) {
	cl := cleared(recs)                        // announce ids that have an all-clear
	consumed := consumedBlocks(recs)           // (file,block) with a block-written marker (#35)
	abandoned := abandonedReservationIDs(recs) // reservation ids with a block-abandoned marker (#160)
	for _, r := range recs {
		drop := false
		switch r.Kind {
		case KindAnnounce:
			drop = cl[r.ID]
		case KindAck, KindAllClear:
			drop = cl[r.AckOf]
		case KindBlockReserve:
			// A written (#35) or abandoned (#160) reservation is a completed
			// handshake — drop it (and its marker below) regardless of age; else
			// drop only when aged out.
			drop = consumed[r.File][r.Block] || abandoned[r.ID] || (blockMaxAge > 0 && Age(r, now) > blockMaxAge)
		case KindBlockWritten, KindBlockAbandoned:
			drop = true // the marker is only needed while its reservation lives
		}
		if !drop {
			kept = append(kept, r)
		}
	}
	return kept, len(recs) - len(kept)
}

// PruneLog GCs the coordination log at path under an exclusive lock (#33): load
// -> PruneRecords -> rewrite the file with only the survivors. Returns how many
// records were dropped. A missing/empty log is a no-op.
func PruneLog(path string, now time.Time, blockMaxAge time.Duration) (dropped int, err error) {
	// MkdirAll the parent (like Append): O_CREATE makes the file but not the dir,
	// and a block-only repo may never have created ~/.wt/coordination/ (its block
	// records live in the per-file ledgers now, #152) — pruning must not error there.
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return 0, err
	}
	lf, err := os.OpenFile(path, os.O_CREATE|os.O_RDWR, 0o644)
	if err != nil {
		return 0, err
	}
	defer lf.Close()
	if err := lock.Exclusive(lf); err != nil {
		return 0, err
	}
	defer lock.Release(lf)

	recs, err := Load(path)
	if err != nil {
		return 0, err
	}
	kept, dropped := PruneRecords(recs, now, blockMaxAge)
	if dropped == 0 {
		return 0, nil
	}
	if _, err := lf.Seek(0, 0); err != nil {
		return 0, err
	}
	if err := lf.Truncate(0); err != nil {
		return 0, err
	}
	w := bufio.NewWriter(lf)
	for _, r := range kept {
		b, mErr := json.Marshal(r)
		if mErr != nil {
			continue
		}
		if _, err := w.Write(append(b, '\n')); err != nil {
			return 0, err
		}
	}
	if err := w.Flush(); err != nil {
		return 0, err
	}
	return dropped, nil
}

// ActiveHoldsAt splits ActiveHolds into fresh vs stale by age (#32). A hold
// older than maxAge (when maxAge > 0) is `stale` — aged out, almost always a
// crashed/forgotten window — and callers should WARN rather than hard-block on
// it, so a dead window's --hold can't wedge everyone's merge-pr forever. maxAge
// <= 0 disables expiry (everything fresh — the pre-#32 behavior).
func ActiveHoldsAt(recs []Record, self Self, op string, now time.Time, maxAge time.Duration) (fresh, stale []Record) {
	for _, a := range ActiveHolds(recs, self, op) {
		if maxAge > 0 && Age(a, now) > maxAge {
			stale = append(stale, a)
		} else {
			fresh = append(fresh, a)
		}
	}
	return fresh, stale
}

// OwnOpenAnnouncements returns THIS session's own announcements that have not
// been all-cleared — the holds/announcements you still own and can `wt
// all-clear` (#34). Includes both hold and plain announcements; excludes cleared
// ones, and another session's announcements in this same checkout (#163).
func OwnOpenAnnouncements(recs []Record, self Self) []Record {
	cl := cleared(recs)
	var out []Record
	for _, a := range announcements(recs) {
		if self.Owns(a) && !cl[a.ID] {
			out = append(out, a)
		}
	}
	return out
}

// SameCheckoutOpen returns the open (not all-cleared) announcements under self's
// window that self does NOT own — another session's, in this same checkout
// (#163). `wt holds` names them so reading your holds reveals a second party
// instead of silently omitting it. Pure.
func SameCheckoutOpen(recs []Record, self Self) []Record {
	cl := cleared(recs)
	var out []Record
	for _, a := range announcements(recs) {
		if self.SharesCheckout(a) && !cl[a.ID] {
			out = append(out, a)
		}
	}
	return out
}

// SharedCheckoutWindow bounds how recent another session's activity under this
// checkout must be for wt doctor / wt status to warn that the checkout is shared
// (#163). Matches the default hold_max_age.
const SharedCheckoutWindow = 24 * time.Hour

// SessionActivity summarizes one other session's records under self's window.
type SessionActivity struct {
	Session string    `json:"session"`
	Records int       `json:"records"`
	Last    time.Time `json:"last"`
}

// OtherSessions reports the sessions OTHER than self's that wrote records under
// self's window within `within` of now (within <= 0 → any age) — evidence that
// another session shares this checkout (#163). A pre-#163 record carries no
// session and names no party, so it is skipped; so is everything when self has
// no session (a zero Self). One entry per session, most recent first. Pure.
func OtherSessions(recs []Record, self Self, now time.Time, within time.Duration) []SessionActivity {
	if self.Session == "" {
		return nil
	}
	idx := map[string]int{}
	var out []SessionActivity
	for _, r := range recs {
		if !self.SharesCheckout(r) {
			continue // another window, self's own session, or a pre-#163 record
		}
		t, err := time.Parse(time.RFC3339, r.TS)
		if err != nil {
			continue // undatable — can't call it recent
		}
		if within > 0 && now.Sub(t) > within {
			continue
		}
		i, ok := idx[r.Session]
		if !ok {
			idx[r.Session] = len(out)
			out = append(out, SessionActivity{Session: r.Session, Last: t})
			i = len(out) - 1
		}
		out[i].Records++
		if t.After(out[i].Last) {
			out[i].Last = t
		}
	}
	sort.SliceStable(out, func(i, j int) bool { return out[i].Last.After(out[j].Last) })
	return out
}

// SharedCheckoutWarning renders the wt doctor / wt status warning for other
// sessions using self's checkout (#163): two lines, who and the remedy. "" when
// there are none. others is OtherSessions' output (most recent first). The only
// remedy is a separate worktree: a distinct WT_WINDOW splits the coordination
// identity but not the tree, and it would also hide the other session from the
// single-worktree per-turn hook. Pure.
func SharedCheckoutWarning(others []SessionActivity, self Self, now time.Time) string {
	if len(others) == 0 {
		return ""
	}
	who := "another session"
	if len(others) > 1 {
		who = fmt.Sprintf("%d other sessions", len(others))
	}
	latest := others[0]
	you := ShortSession(self.Session)
	if self.Session == SessionNone {
		you += " (set WT_SESSION)"
	}
	return fmt.Sprintf("checkout shared — %s posted from THIS checkout in the last %dh (latest: %s, %s); you: %s.\n"+
		"  One working tree, no file-level warning between you: give one session its own worktree (`wt new <branch>`); its holds: `wt inbox`.",
		who, int(SharedCheckoutWindow.Hours()), ShortSession(latest.Session), ago(now.Sub(latest.Last)), you)
}

// Thresholds for HoldLooksOrphaned (#163). A hold this old, or whose session has
// written nothing for this long, has probably outlived the session that placed
// it. Both sit well inside the default hold_max_age (24h), past which a hold
// stops blocking at all.
const (
	HoldOrphanAge    = 12 * time.Hour
	HoldOrphanSilent = 4 * time.Hour
)

// HoldLooksOrphaned reports whether hold h looks abandoned by the session that
// placed it, and why: it is HoldOrphanAge old, or that session (h's window AND
// session, the hold itself included) has written nothing for HoldOrphanSilent.
// It is the ONLY condition under which wt suggests `wt all-clear` for a hold that
// still gates: all-clear releases the hold for EVERY window, while `wt ack`
// waives it only for the reader, so a live session's hold is acked, never
// cleared. An undatable hold is not called orphaned. Pure.
func HoldLooksOrphaned(recs []Record, h Record, now time.Time) (orphaned bool, why string) {
	placed, err := time.Parse(time.RFC3339, h.TS)
	if err != nil {
		return false, ""
	}
	if age := now.Sub(placed); age >= HoldOrphanAge {
		return true, "placed " + ago(age)
	}
	last := placed
	for _, r := range recs {
		if r.Window != h.Window || r.Session != h.Session {
			continue
		}
		if t, err := time.Parse(time.RFC3339, r.TS); err == nil && t.After(last) {
			last = t
		}
	}
	if silent := now.Sub(last); silent >= HoldOrphanSilent {
		return true, "its session last wrote " + ago(silent)
	}
	return false, ""
}

// ago is a compact age for messages built in this package.
func ago(d time.Duration) string {
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

// InboxClearAmbiguous reports whether an empty inbox must NOT be presented as a
// bare "inbox clear" (#163): this session has no session token (SessionNone),
// yet a session-tagged record was written under its window within
// SharedCheckoutWindow — so other sessions are actively posting from this
// checkout, and any token-less one doing the same is indistinguishable from this
// one. A hedge beats a false negative; bounding it by recency keeps an old,
// long-finished session from hedging every token-less inbox forever (a permanent
// hedge teaches people to ignore it). Pure.
func InboxClearAmbiguous(recs []Record, self Self, now time.Time) bool {
	if self.Session != SessionNone {
		return false
	}
	for _, r := range recs {
		if !self.SharesCheckout(r) {
			continue // another window, a token-less writer (indistinguishable), or pre-#163
		}
		if t, err := time.Parse(time.RFC3339, r.TS); err == nil && now.Sub(t) <= SharedCheckoutWindow {
			return true
		}
	}
	return false
}

// OwnBlockReservations returns THIS window's block-id reservations, newest first
// — the ids you hold (and may not have written yet), for `wt holds` (#34).
func OwnBlockReservations(recs []Record, self string) []Record {
	consumed := consumedBlocks(recs)
	abandoned := abandonedReservationIDs(recs)
	var out []Record
	for _, r := range recs {
		if r.Kind == KindBlockReserve && r.Window == self &&
			!consumed[r.File][r.Block] && !abandoned[r.ID] {
			out = append(out, r) // hide reservations you've written (#35) or abandoned (#160)
		}
	}
	for i, j := 0, len(out)-1; i < j; i, j = i+1, j-1 {
		out[i], out[j] = out[j], out[i]
	}
	return out
}

// NewID derives a short, log-sortable id from a timestamp (nanos, base36).
func NewID(t time.Time) string {
	return strconv.FormatInt(t.UnixNano(), 36)
}

// Age returns how old a record is relative to now (0 if TS is unparseable).
func Age(r Record, now time.Time) time.Duration {
	t, err := time.Parse(time.RFC3339, r.TS)
	if err != nil {
		return 0
	}
	return now.Sub(t)
}

// NextBlock returns the next append-log block id for file: one past the max of
// (a) every block-reserve record for file already on the coordination log and
// (b) fileMax — the highest block id already written in the target file itself.
// Seeding from fileMax is what lets wt take over allocation for a file that
// predates it (existing NEWEST-55 in the doc → next is 56, not 1). Pure.
func NextBlock(recs []Record, file string, fileMax int, now time.Time, ttl time.Duration) int {
	consumed := consumedBlocks(recs)
	abandoned := abandonedReservationIDs(recs)
	max := fileMax
	for _, r := range recs {
		if r.Kind != KindBlockReserve || r.File != file || r.Block <= max {
			continue
		}
		if abandoned[r.ID] {
			continue // #160: THIS reservation was abandoned → free its id NOW (per-record)
		}
		// Count a reservation only if it's still live (younger than ttl) or has
		// been written (a written id is also ≤ fileMax, so this is belt-and-braces).
		// A stale, never-written reservation is skipped — its id is freed for reuse
		// instead of permanently burned (#35). ttl<=0 disables aging (count all).
		fresh := ttl <= 0 || Age(r, now) <= ttl
		if fresh || consumed[r.File][r.Block] {
			max = r.Block
		}
	}
	return max + 1
}

// consumedBlocks indexes (file → block → true) for every reservation that has a
// block-written terminal record (#35). A written pair is resolved: it no longer
// signals an imminent prepend and can be pruned. Pure.
func consumedBlocks(recs []Record) map[string]map[int]bool {
	return blockMarkers(recs, KindBlockWritten)
}

// abandonedReservationIDs is the set of RESERVATION record IDs that have a
// block-abandoned marker (#160). Keyed by the reservation's record ID (the abandon
// record's AckOf) — NOT (file, block) like consumed — because --abandon FREES the
// id for reuse: a LATER reservation of the same number is a DIFFERENT record and
// must stay live, not be silently masked by the old abandonment (which would
// re-introduce the exact same-id collision block-id exists to prevent). Pure.
func abandonedReservationIDs(recs []Record) map[string]bool {
	out := map[string]bool{}
	for _, r := range recs {
		if r.Kind == KindBlockAbandoned && r.AckOf != "" {
			out[r.AckOf] = true
		}
	}
	return out
}

// blockMarkers indexes (file → block → true) for every record of the given
// terminal kind. Pure.
func blockMarkers(recs []Record, kind string) map[string]map[int]bool {
	out := map[string]map[int]bool{}
	for _, r := range recs {
		if r.Kind != kind || r.File == "" {
			continue
		}
		if out[r.File] == nil {
			out[r.File] = map[int]bool{}
		}
		out[r.File][r.Block] = true
	}
	return out
}

// FindOwnReservation returns this window's newest block-reserve for (file,
// block), for linking a block-written record back to it. Pure.
func FindOwnReservation(recs []Record, self, file string, block int) (Record, bool) {
	var found Record
	ok := false
	for _, r := range recs {
		if r.Kind == KindBlockReserve && r.Window == self && r.File == file && r.Block == block {
			found = r // log is append-order; last match is newest
			ok = true
		}
	}
	return found, ok
}

// RecentBlockReservations returns block-reserve records from OTHER windows that
// are younger than maxAge — the "a prepend is imminent, don't anchor on the
// same header" signal for wt status. Self's own reservations are excluded (you
// know your own). Newest first. Pure.
func RecentBlockReservations(recs []Record, self string, now time.Time, maxAge time.Duration) []Record {
	consumed := consumedBlocks(recs)
	abandoned := abandonedReservationIDs(recs)
	var out []Record
	for _, r := range recs {
		if r.Kind != KindBlockReserve || r.Window == self {
			continue
		}
		if consumed[r.File][r.Block] || abandoned[r.ID] {
			continue // written (#35) or abandoned (#160) → no longer imminent
		}
		if Age(r, now) <= maxAge {
			out = append(out, r)
		}
	}
	// Newest first: the log is append-order (oldest first), so reverse.
	for i, j := 0, len(out)-1; i < j; i, j = i+1, j-1 {
		out[i], out[j] = out[j], out[i]
	}
	return out
}

// ReserveBlock atomically allocates and records the next block id for file.
//
// It holds an EXCLUSIVE advisory lock (flock) on the coordination log across
// the whole read-modify-write — (load reservations → evaluate fileMax → append)
// — so two windows calling it concurrently can never allocate the same id. The
// bare Append used by announce/ack is O_APPEND (torn-write-safe) but has no such
// serialization; block ids need it because they're a read-then-write allocation.
//
// fileMax is a closure so the doc scan runs UNDER the lock (it sees the latest
// on-disk content, e.g. a block another window just wrote). Nil fileMax => 0.
// r should be a fresh record (ID/TS/Window/Repo set); Kind/File/Block are set
// here. Returns the completed record whose .Block is the reserved id.
func ReserveBlock(path string, r Record, file string, fileMax func() (int, error)) (Record, error) {
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return Record{}, err
	}
	lf, err := os.OpenFile(path, os.O_CREATE|os.O_RDWR, 0o644)
	if err != nil {
		return Record{}, err
	}
	defer lf.Close()
	if err := lock.Exclusive(lf); err != nil {
		return Record{}, err
	}
	defer lock.Release(lf)

	recs, err := Load(path)
	if err != nil {
		return Record{}, err
	}
	fm := 0
	if fileMax != nil {
		if fm, err = fileMax(); err != nil {
			return Record{}, err
		}
	}
	r.Kind = KindBlockReserve
	r.File = file
	r.Block = NextBlock(recs, file, fm, time.Now(), DefaultBlockReserveTTL)
	if err := Append(path, r); err != nil {
		return Record{}, err
	}
	return r, nil
}

// BlocksDir is where per-file block-id ledgers live under ~/.wt.
func BlocksDir(home string) string { return filepath.Join(home, ".wt", "blocks") }

// BlockLedgerPath is the per-FILE block-reservation ledger for absFile (#152).
// block-id coordinates on the shared append-log, NOT the repo: two windows in
// DIFFERENT repos editing the same file must share ONE reservation namespace and
// ONE lock, or they hand out the same id (the per-repo coord log can't — each repo
// has its own, so cross-repo reservations are invisible to each other). Keyed by a
// hash of the absolute path so any window, in any repo, resolves the same ledger.
// The readable basename prefix is for humans debugging ~/.wt/blocks.
func BlockLedgerPath(home, absFile string) string {
	sum := sha256.Sum256([]byte(absFile))
	name := slug(filepath.Base(absFile)) + "-" + hex.EncodeToString(sum[:])[:12] + ".jsonl"
	return filepath.Join(BlocksDir(home), name)
}

// PruneBlockLedgers GCs every per-file block ledger under home (#152 review): the
// same age-based drop PruneLog applies to the per-repo log, applied to each ledger,
// so relocating block records to ledgers didn't strand them beyond the #33/#35 GC
// (each written or aged-out reservation is dropped). Returns total records dropped.
// Missing dir → (0, nil); a per-ledger error stops and returns what was dropped.
func PruneBlockLedgers(home string, now time.Time, blockMaxAge time.Duration) (dropped int, err error) {
	entries, rerr := os.ReadDir(BlocksDir(home))
	if rerr != nil {
		if os.IsNotExist(rerr) {
			return 0, nil
		}
		return 0, rerr
	}
	for _, e := range entries {
		if e.IsDir() || !strings.HasSuffix(e.Name(), ".jsonl") {
			continue
		}
		d, perr := PruneLog(filepath.Join(BlocksDir(home), e.Name()), now, blockMaxAge)
		if perr != nil {
			return dropped, perr
		}
		dropped += d
	}
	return dropped, nil
}

// LoadBlockLedgers reads every per-file block ledger this machine has coordinated
// on, so the wt-status banner + `wt holds` surface block reservations regardless of
// which repo made them. Missing dir / unreadable ledger → skipped (best-effort).
func LoadBlockLedgers(home string) []Record {
	entries, err := os.ReadDir(BlocksDir(home))
	if err != nil {
		return nil
	}
	var recs []Record
	for _, e := range entries {
		if e.IsDir() || !strings.HasSuffix(e.Name(), ".jsonl") {
			continue
		}
		if rs, err := Load(filepath.Join(BlocksDir(home), e.Name())); err == nil {
			recs = append(recs, rs...)
		}
	}
	return recs
}
