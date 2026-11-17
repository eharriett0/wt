// wt block-id (eharriett0/wt#23): atomically allocate + reserve the next
// append-log block id (NEWEST-N) for a shared doc via the coordination log, so
// multiple windows prepending to the same monotonic-block memory file never
// grab the same N. Companion to the announce/ack coordination in coord.go.
package cli

import (
	"bufio"
	"flag"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"time"

	"github.com/eharriett0/wt/internal/config"
	"github.com/eharriett0/wt/internal/coord"
	"github.com/eharriett0/wt/internal/ui"
)

// defaultBlockPattern matches the resume-memory convention (NEWEST-<n>). The
// {n} placeholder is the numeric id; anything else is a literal.
const defaultBlockPattern = "NEWEST-{n}"

// blockReservationMaxAge bounds which reservations wt status surfaces — a
// reservation older than this is assumed already written (or abandoned), so it
// no longer signals an imminent prepend. Single source: the coord TTL that also
// governs id-freeing in NextBlock (#35).
const blockReservationMaxAge = coord.DefaultBlockReserveTTL

// blockPatternRe compiles a pattern like "NEWEST-{n}" into a regexp capturing
// the numeric id. Everything outside {n} is matched literally.
func blockPatternRe(pattern string) (*regexp.Regexp, error) {
	i := strings.Index(pattern, "{n}")
	if i < 0 {
		return nil, fmt.Errorf("pattern %q must contain {n} (the id placeholder)", pattern)
	}
	expr := regexp.QuoteMeta(pattern[:i]) + `(\d+)` + regexp.QuoteMeta(pattern[i+len("{n}"):])
	return regexp.Compile(expr)
}

// scanFileMaxBlock returns the highest block id already written in file per the
// pattern, or 0 when the file is missing or has none. A real read error (not
// not-exist) propagates — silently resetting the counter to 0 would hand out an
// id that collides with content already on disk.
func scanFileMaxBlock(file string, re *regexp.Regexp) (int, error) {
	f, err := os.Open(file)
	if err != nil {
		if os.IsNotExist(err) {
			return 0, nil
		}
		return 0, err
	}
	defer f.Close()
	max := 0
	sc := bufio.NewScanner(f)
	sc.Buffer(make([]byte, 0, 64*1024), 1024*1024)
	for sc.Scan() {
		for _, m := range re.FindAllStringSubmatch(sc.Text(), -1) {
			if n, err := strconv.Atoi(m[1]); err == nil && n > max {
				max = n
			}
		}
	}
	return max, sc.Err()
}

// fileHasBlock reports whether file contains a block whose id == n per the
// pattern — the "is this block actually written" check `--written` needs so it
// can't clear a reservation for a block that isn't there (#152). Missing file →
// false; a real read error propagates.
func fileHasBlock(file string, re *regexp.Regexp, n int) (bool, error) {
	f, err := os.Open(file)
	if err != nil {
		if os.IsNotExist(err) {
			return false, nil
		}
		return false, err
	}
	defer f.Close()
	sc := bufio.NewScanner(f)
	sc.Buffer(make([]byte, 0, 64*1024), 1024*1024)
	for sc.Scan() {
		for _, m := range re.FindAllStringSubmatch(sc.Text(), -1) {
			if v, err := strconv.Atoi(m[1]); err == nil && v == n {
				return true, nil
			}
		}
	}
	return false, sc.Err()
}

// canonicalFilePath resolves file to a STABLE absolute identity for keying the
// per-file block ledger (#152 review): symlinks are resolved so a per-repo symlink
// into a shared doc — the canonical way to share ONE append-log across repos —
// maps to the SAME ledger as the doc's real path. Without this, two windows would
// key different ledgers off the different symlink paths and re-collide. The
// append-log may not exist yet on the first reserve, so EvalSymlinks the deepest
// EXISTING ancestor and re-join the not-yet-existing tail (the house style used by
// repoRelativePath / collide / worktree). Falls back to a lexical Abs on error.
// Caveat: on case-insensitive volumes, paths differing only in case still key
// different ledgers — reference the file by one consistent path across windows.
func canonicalFilePath(file string) string {
	abs, err := filepath.Abs(file)
	if err != nil || abs == "" {
		return file
	}
	unresolved := ""
	cur := abs
	for {
		if resolved, rerr := filepath.EvalSymlinks(cur); rerr == nil {
			if unresolved == "" {
				return resolved
			}
			return filepath.Join(resolved, unresolved)
		}
		parent := filepath.Dir(cur)
		if parent == cur {
			return abs // reached root with nothing resolvable (shouldn't happen)
		}
		unresolved = filepath.Join(filepath.Base(cur), unresolved)
		cur = parent
	}
}

// cmdBlockID implements `wt block-id <file> [--pattern P] [--format]`.
func cmdBlockID(args []string) int {
	if code, done := guardHelp(args, `usage: wt block-id <file> [--pattern "NEWEST-{n}"] [--format] [--written N | --abandon N [--not-mine]]`); done {
		return code
	}
	fs := flag.NewFlagSet("block-id", flag.ContinueOnError)
	pattern := fs.String("pattern", defaultBlockPattern,
		"append-log id pattern ({n} = numeric id); MATCH a decorated heading (e.g. \"## ⚠ {n}:\") or the default scan under-reports the max and reserves a stale id (#160)")
	format := fs.Bool("format", false,
		"print the full formatted token (e.g. NEWEST-56) instead of the bare number")
	written := fs.Int("written", -1,
		"mark block N as WRITTEN (prepended) — clears the reservation instead of allocating a new id")
	abandon := fs.Int("abandon", -1,
		"abandon block N — a number you reserved but decided NOT to write: clears it from `wt holds`/the banner and frees the id (#160)")
	notMine := fs.Bool("not-mine", false,
		"with --abandon: free a reservation this session can't prove it made (written with no session token, like this shell): another token-less shell or agent may be about to write it (#197)")
	pos, _, err := parseInterspersed(fs, args)
	if err != nil {
		return 64
	}
	file := strings.TrimSpace(strings.Join(pos, " "))
	if file == "" {
		ui.Err(`usage: wt block-id <file> [--pattern "NEWEST-{n}"] [--format] [--written N | --abandon N [--not-mine]]`)
		return 64
	}
	if *notMine && *abandon < 0 {
		ui.Err("--not-mine goes with --abandon N only (nothing else frees a reservation)")
		return 64
	}
	re, err := blockPatternRe(*pattern)
	if err != nil {
		ui.Err("%v", err)
		return 64
	}
	// #160 review: the two terminal signals are mutually exclusive — --written needs
	// the block IN the file, --abandon needs it ABSENT — so passing both is
	// contradictory. Reject it rather than silently running one.
	if *written >= 0 && *abandon >= 0 {
		ui.Err("--written and --abandon are mutually exclusive (a block is either written or abandoned, not both)")
		return 64
	}
	// Canonical key: symlink-resolved absolute path (#152). The shared append-log
	// lives at ONE real location; every window (any repo, any worktree, reached via
	// a symlink or not) must resolve it to the SAME key so they coordinate on one
	// per-file ledger. See canonicalFilePath.
	absFile := canonicalFilePath(file)
	return withConfig(func(c *config.Config) int {
		path, self := coordCtx(c)
		// Reservations are owned by session, as every coordination record is
		// (#197, coord.Self.ReservationIsMine): another session in this checkout
		// can neither mark yours written nor abandon it.
		// block-id coordinates on the shared FILE, not the repo: use a per-file
		// ledger so windows in ANY repo editing this append-log share one lock +
		// reservation namespace (#152). Fall back to the per-repo log only if HOME
		// is unresolvable (degrades to the old, per-repo-only behavior).
		ledger := path
		if home, herr := os.UserHomeDir(); herr == nil && home != "" {
			ledger = coord.BlockLedgerPath(home, absFile)
		}

		// --written N: terminal signal that reservation N was actually prepended
		// (#35). REFUSE unless (a) the file really contains a block N AND (b) an
		// open reservation for N is this session's (#197) — otherwise the
		// bookkeeping would record a completion that never happened, or that is
		// another session's or window's (#152).
		if *written >= 0 {
			inFile, ferr := fileHasBlock(absFile, re, *written)
			if ferr != nil {
				ui.Err("could not read %s: %v", filepath.Base(absFile), ferr)
				return 1
			}
			if !inFile {
				ui.Err("block %d is not in %s — write your `%s` block before marking it written (nothing recorded)",
					*written, filepath.Base(absFile), strings.Replace(*pattern, "{n}", strconv.Itoa(*written), 1))
				return 1
			}
			recs, _ := coord.Load(ledger)
			res, ok := coord.FindOwnReservation(recs, self, absFile, *written)
			if !ok {
				// #197 re-review: a retried --written found nothing open and was
				// told to reserve a new id, for a block already in the file.
				switch coord.OwnClosedReservation(recs, self, absFile, *written) {
				case coord.KindBlockWritten:
					ui.OK("block %d on %s is already marked written (nothing recorded)", *written, filepath.Base(absFile))
					return 0
				case coord.KindBlockAbandoned:
					ui.Err("you abandoned your reservation of block %d on %s, so it is not yours to mark written (nothing recorded)", *written, filepath.Base(absFile))
					return 1
				}
				ui.Err("%s (nothing recorded)", notYourReservation(recs, self, absFile, *written, time.Now()))
				return 1
			}
			m := newRecord(c, self, coord.KindBlockWritten)
			m.File, m.Block, m.AckOf = absFile, *written, res.ID
			if err := coord.Append(ledger, m); err != nil {
				ui.Err("could not record block-written: %v", err)
				return 1
			}
			ui.OK("marked block %d written on %s — reservation cleared", *written, filepath.Base(absFile))
			return 0
		}

		// --abandon N: terminal signal that reservation N will NOT be written (#160).
		// The symmetric exit to --written for a number you reserved but decided not to
		// use (e.g. a bad scan handed out a stale id). REFUSE unless an open
		// reservation for N is this session's (#197) — but do NOT require the block
		// to be in the file (the whole point is it isn't). Clears holds/banner and
		// frees the id. ⚠ Freeing is where a wrong owner hurts (the next reservation
		// hands the id out again), so the token-less directory fallback, which wt
		// counts as yours but can't prove, asks first, or needs --not-mine, as
		// `wt all-clear` does (#197 review).
		if *abandon >= 0 {
			recs, _ := coord.Load(ledger)
			res, ok := coord.FindOwnReservation(recs, self, absFile, *abandon)
			if !ok {
				ui.Err("%s; nothing to abandon (`wt holds` lists yours)", notYourReservation(recs, self, absFile, *abandon, time.Now()))
				return 1
			}
			if self.Attribute(res) == coord.AttrMineFallback && !*notMine {
				why := "was reserved with no session token, like this shell: wt counts it as yours but can't prove it, " +
					"since every token-less shell or agent in this checkout reads it the same way, and abandoning frees the id for the next reservation"
				if interactive := confirmInteractive(); !interactive || !confirmAbandon(*abandon, why, os.Stdin) {
					if interactive {
						ui.Err("not abandoned: block %d.", *abandon)
					} else {
						ui.Err("not abandoned: block %d %s.", *abandon, why)
					}
					ui.Info("abandon it anyway: wt block-id %s --abandon %d --not-mine   ·   set WT_SESSION to a value unique to each shell or agent, and wt can prove which reservations are yours", file, *abandon)
					return 1
				}
			}
			m := newRecord(c, self, coord.KindBlockAbandoned)
			m.File, m.Block, m.AckOf = absFile, *abandon, res.ID
			if err := coord.Append(ledger, m); err != nil {
				ui.Err("could not record block-abandoned: %v", err)
				return 1
			}
			ui.OK("abandoned block %d on %s — reservation cleared, id freed for reuse", *abandon, filepath.Base(absFile))
			return 0
		}

		r := newRecord(c, self, coord.KindBlockReserve)
		out, rerr := coord.ReserveBlock(ledger, r, absFile, func() (int, error) {
			return scanFileMaxBlock(absFile, re)
		})
		if rerr != nil {
			ui.Err("could not reserve block id: %v", rerr)
			return 1
		}
		// The id (bare number, or --format token) is the ONLY thing on stdout — the
		// whole point of block-id is `N=$(wt block-id file)`. The human hint goes to
		// STDERR so it never pollutes the captured value (#152). (ui.Info/ui.OK print
		// to stdout, so they'd corrupt the number — use a stderr write here.)
		if *format {
			fmt.Println(strings.Replace(*pattern, "{n}", strconv.Itoa(out.Block), 1))
		} else {
			fmt.Println(out.Block)
		}
		fmt.Fprintln(os.Stderr, ui.Dim(fmt.Sprintf(
			"reserved block %d for %s (%s) — write your %s block now, then `wt block-id %s --written %d`",
			out.Block, filepath.Base(absFile), selfLabel(self),
			strings.Replace(*pattern, "{n}", strconv.Itoa(out.Block), 1), file, out.Block)))
		return 0
	})
}

// notYourReservation says why block on file is not self's to mark (#197): no
// open reservation at all, or whose it is, with when it frees itself. Pure.
func notYourReservation(recs []coord.Record, self coord.Self, file string, block int, now time.Time) string {
	r, ok := coord.FindReservation(recs, file, block)
	if !ok {
		return fmt.Sprintf("no open reservation for block %d on %s is yours (%s)", block, filepath.Base(file), selfLabel(self))
	}
	frees := fmt.Sprintf("an unwritten reservation frees its id by itself %s after it was made", blockReservationMaxAge)
	age := humanAge(coord.Age(r, now))
	switch self.Attribute(r) {
	case coord.AttrAnotherSession:
		who := "another session in this window (" + coord.ShortSession(r.Session) + ")"
		if r.Session == coord.SessionNone {
			who = "a shell or agent with no session token in this window"
		}
		msg := fmt.Sprintf("block %d on %s was reserved %s by %s, not by you (%s): only its owner marks it, and %s",
			block, filepath.Base(file), age, who, selfLabel(self), frees)
		switch {
		case self.Session == coord.SessionNone:
			msg += " (this shell has no session token: set WT_SESSION, unique per shell or agent, to tell them apart)"
		case r.Session != coord.SessionNone:
			msg += " (if this session ran /clear since reserving it, the reservation was yours before it)"
		}
		return msg
	case coord.AttrUnattributable:
		return fmt.Sprintf("block %d on %s was reserved %s in this window by a wt without session support, so no session can mark it; %s",
			block, filepath.Base(file), age, frees)
	}
	return fmt.Sprintf("block %d on %s was reserved %s from window %s (%s), not by you (%s): only its owner marks it, and %s",
		block, filepath.Base(file), age, r.Window, coord.ShortSession(r.Session), selfLabel(self), frees)
}

// confirmAbandon asks, on stderr, whether to abandon a reservation this
// session can't prove it made (#197 review); true only on y/yes. Enter, EOF
// and anything else is no.
func confirmAbandon(block int, why string, in io.Reader) bool {
	ui.Warn("block %d %s.", block, why)
	fmt.Fprintf(os.Stderr, "%s abandon block %d anyway? [y/N] ", ui.Yellow("→"), block)
	line, _ := bufio.NewReader(in).ReadString('\n')
	switch strings.ToLower(strings.TrimSpace(line)) {
	case "y", "yes":
		return true
	}
	return false
}

// blockReservationBanner surfaces recent block-id reservations that are not
// yours at the top of `wt status` (other windows', and another session's in
// this checkout, #197): a hint that a prepend to a shared append log is
// imminent, so don't anchor your own prepend on the same header.
// Best-effort: no repo / unreadable log / none → prints nothing.
func blockReservationBanner(c *config.Config) {
	if c == nil {
		return
	}
	_, self := coordCtx(c)
	home, herr := os.UserHomeDir()
	if herr != nil || home == "" {
		return
	}
	// #152: block reservations live in per-file ledgers now, so aggregate across
	// them — this also surfaces a reservation made from ANOTHER repo on a shared file.
	recs := coord.LoadBlockLedgers(home)
	now := time.Now()
	res := coord.RecentBlockReservations(recs, self, now, blockReservationMaxAge)
	if len(res) == 0 {
		return
	}
	ui.Banner(fmt.Sprintf("%d recent block-id reservation(s) not yours (any repo — reservations are per shared file) — a prepend may be imminent", len(res)))
	for _, r := range res {
		who := r.Window
		switch {
		case self.ProvablyWrote(r):
			who += " (your session, another checkout: a subagent's?)"
		case r.Window == self.Window:
			who += " (" + coord.ShortSession(r.Session) + ", this window)"
		}
		fmt.Fprintf(os.Stderr, "  %s reserved block %s on %s  %s\n",
			ui.Cyan(who), ui.Bold(strconv.Itoa(r.Block)),
			ui.Dim(filepath.Base(r.File)), ui.Dim(humanAge(coord.Age(r, now))))
	}
}
