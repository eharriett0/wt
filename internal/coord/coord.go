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
	// windows never grab the same N. Reservation-only — nothing marks it
	// "written"; recency is the "prepend imminent" signal.
	KindBlockReserve = "block-reserve"
)

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
func NextBlock(recs []Record, file string, fileMax int) int {
	max := fileMax
	for _, r := range recs {
		if r.Kind == KindBlockReserve && r.File == file && r.Block > max {
			max = r.Block
		}
	}
	return max + 1
}

// RecentBlockReservations returns block-reserve records from OTHER windows that
// are younger than maxAge — the "a prepend is imminent, don't anchor on the
// same header" signal for wt status. Self's own reservations are excluded (you
// know your own). Newest first. Pure.
func RecentBlockReservations(recs []Record, self string, now time.Time, maxAge time.Duration) []Record {
	var out []Record
	for _, r := range recs {
		if r.Kind != KindBlockReserve || r.Window == self {
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
	r.Block = NextBlock(recs, file, fm)
	if err := Append(path, r); err != nil {
		return Record{}, err
	}
	return r, nil
}
