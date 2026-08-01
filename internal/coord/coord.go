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
	"encoding/json"
	"os"
	"path/filepath"
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
	Window  string   `json:"window"` // announcing/acking window (worktree branch)
	Repo    string   `json:"repo"`   // repo slug
	Kind    string   `json:"kind"`   // announce | ack | all-clear | block-reserve
	Message string   `json:"message,omitempty"`
	Issue   int      `json:"issue,omitempty"`  // mirrored GitHub issue #, if any
	Hold    []string `json:"hold,omitempty"`   // ops other windows should avoid until all-clear
	AckOf   string   `json:"ack_of,omitempty"` // announce id this record acks / clears
	State   string   `json:"state,omitempty"`  // one-line in-flight state (on ack)
	File    string   `json:"file,omitempty"`   // block-reserve: the append-log doc
	Block   int      `json:"block,omitempty"`  // block-reserve: the reserved block id (N)
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

// ackedBy reports the set of announce ids that window `w` has acked.
func ackedBy(recs []Record, w string) map[string]bool {
	m := map[string]bool{}
	for _, r := range recs {
		if r.Kind == KindAck && r.Window == w && r.AckOf != "" {
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

// Inbox returns announcements from OTHER windows that self has not acked and
// that have not been all-cleared — i.e. the ones needing this window's
// attention. Self's own announcements are excluded (you don't ack yourself).
func Inbox(recs []Record, self string) []Record {
	cl := cleared(recs)
	acked := ackedBy(recs, self)
	var out []Record
	for _, a := range announcements(recs) {
		if a.Window == self || cl[a.ID] || acked[a.ID] {
			continue
		}
		out = append(out, a)
	}
	return out
}

// PendingHolds returns the subset of this window's inbox (un-acked, un-cleared
// announcements from OTHER windows) that declare a hold — the ones wt should
// surface proactively before the window acts (the ambient-banner signal).
func PendingHolds(recs []Record, self string) []Record {
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
// its OWN hold: ActiveHolds already exempts own-window holds (a.Window == self),
// but only if `self` is stable. Precedence:
//
//  1. WT_WINDOW env — explicit, survives dir AND branch changes; set per terminal
//     to pin identity across checkouts (the only fix for announcing in one dir and
//     merging from another) and to get a short readable label.
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

// ActiveHolds returns announcements from OTHER windows whose hold covers op,
// that have not been all-cleared and that self has not acked. These are the
// holds that should block/warn an operation (e.g. merge-pr checking "merge-main").
// Acking a hold clears it for self — you've acknowledged the coordination.
func ActiveHolds(recs []Record, self, op string) []Record {
	cl := cleared(recs)
	acked := ackedBy(recs, self)
	var out []Record
	for _, a := range announcements(recs) {
		if a.Window == self || cl[a.ID] || acked[a.ID] {
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
	cl := cleared(recs)              // announce ids that have an all-clear
	consumed := consumedBlocks(recs) // (file,block) pairs with a block-written marker (#35)
	for _, r := range recs {
		drop := false
		switch r.Kind {
		case KindAnnounce:
			drop = cl[r.ID]
		case KindAck, KindAllClear:
			drop = cl[r.AckOf]
		case KindBlockReserve:
			// A written reservation is a completed handshake — drop it (and its
			// marker below) regardless of age; else drop only when aged out.
			drop = consumed[r.File][r.Block] || (blockMaxAge > 0 && Age(r, now) > blockMaxAge)
		case KindBlockWritten:
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
func ActiveHoldsAt(recs []Record, self, op string, now time.Time, maxAge time.Duration) (fresh, stale []Record) {
	for _, a := range ActiveHolds(recs, self, op) {
		if maxAge > 0 && Age(a, now) > maxAge {
			stale = append(stale, a)
		} else {
			fresh = append(fresh, a)
		}
	}
	return fresh, stale
}

// OwnOpenAnnouncements returns THIS window's own announcements that have not been
// all-cleared — the holds/announcements you still own and can `wt all-clear`
// (#34). Includes both hold and plain announcements; excludes cleared ones.
func OwnOpenAnnouncements(recs []Record, self string) []Record {
	cl := cleared(recs)
	var out []Record
	for _, a := range announcements(recs) {
		if a.Window == self && !cl[a.ID] {
			out = append(out, a)
		}
	}
	return out
}

// OwnBlockReservations returns THIS window's block-id reservations, newest first
// — the ids you hold (and may not have written yet), for `wt holds` (#34).
func OwnBlockReservations(recs []Record, self string) []Record {
	consumed := consumedBlocks(recs)
	var out []Record
	for _, r := range recs {
		if r.Kind == KindBlockReserve && r.Window == self && !consumed[r.File][r.Block] {
			out = append(out, r) // #35: hide reservations you've already written
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
	max := fileMax
	for _, r := range recs {
		if r.Kind != KindBlockReserve || r.File != file || r.Block <= max {
			continue
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
	out := map[string]map[int]bool{}
	for _, r := range recs {
		if r.Kind != KindBlockWritten || r.File == "" {
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
	var out []Record
	for _, r := range recs {
		if r.Kind != KindBlockReserve || r.Window == self {
			continue
		}
		if consumed[r.File][r.Block] { // #35: written → prepend already happened
			continue
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
