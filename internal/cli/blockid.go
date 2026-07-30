// wt block-id (eharriett0/wt#23): atomically allocate + reserve the next
// append-log block id (NEWEST-N) for a shared doc via the coordination log, so
// multiple windows prepending to the same monotonic-block memory file never
// grab the same N. Companion to the announce/ack coordination in coord.go.
package cli

import (
	"bufio"
	"flag"
	"fmt"
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
// no longer signals an imminent prepend.
const blockReservationMaxAge = 30 * time.Minute

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

// cmdBlockID implements `wt block-id <file> [--pattern P] [--format]`.
func cmdBlockID(args []string) int {
	fs := flag.NewFlagSet("block-id", flag.ContinueOnError)
	pattern := fs.String("pattern", defaultBlockPattern,
		"append-log id pattern; {n} is the numeric placeholder")
	format := fs.Bool("format", false,
		"print the full formatted token (e.g. NEWEST-56) instead of the bare number")
	pos, _, err := parseInterspersed(fs, args)
	if err != nil {
		return 64
	}
	file := strings.TrimSpace(strings.Join(pos, " "))
	if file == "" {
		ui.Err(`usage: wt block-id <file> [--pattern "NEWEST-{n}"] [--format]`)
		return 64
	}
	re, err := blockPatternRe(*pattern)
	if err != nil {
		ui.Err("%v", err)
		return 64
	}
	// Canonical key: absolute path. The resume memory doc lives at ONE shared
	// location, so every window (in any worktree) abspaths to the same key and
	// coordinates on the same N-space via the shared per-repo log. Fall back to
	// the given path if Abs fails (never blocks the allocation).
	absFile := file
	if a, aerr := filepath.Abs(file); aerr == nil {
		absFile = a
	}
	return withConfig(func(c *config.Config) int {
		path, window := coordCtx(c)
		r := newRecord(c, window, coord.KindBlockReserve)
		out, rerr := coord.ReserveBlock(path, r, absFile, func() (int, error) {
			return scanFileMaxBlock(absFile, re)
		})
		if rerr != nil {
			ui.Err("could not reserve block id: %v", rerr)
			return 1
		}
		if *format {
			fmt.Println(strings.Replace(*pattern, "{n}", strconv.Itoa(out.Block), 1))
		} else {
			fmt.Println(out.Block)
		}
		ui.Info("reserved block %d for %s (window %s) — write your %s block now",
			out.Block, filepath.Base(absFile), window,
			strings.Replace(*pattern, "{n}", strconv.Itoa(out.Block), 1))
		return 0
	})
}

// blockReservationBanner surfaces recent block-id reservations from OTHER
// windows at the top of `wt status` — a hint that a prepend to a shared append
// log is imminent, so don't anchor your own prepend on the same header.
// Best-effort: no repo / unreadable log / none → prints nothing.
func blockReservationBanner(c *config.Config) {
	if c == nil {
		return
	}
	path, window := coordCtx(c)
	recs, err := coord.Load(path)
	if err != nil {
		return
	}
	now := time.Now()
	res := coord.RecentBlockReservations(recs, window, now, blockReservationMaxAge)
	if len(res) == 0 {
		return
	}
	ui.Banner(fmt.Sprintf("%d recent block-id reservation(s) from another window — a prepend may be imminent", len(res)))
	for _, r := range res {
		fmt.Fprintf(os.Stderr, "  %s reserved block %s on %s  %s\n",
			ui.Cyan(r.Window), ui.Bold(strconv.Itoa(r.Block)),
			ui.Dim(filepath.Base(r.File)), ui.Dim(humanAge(coord.Age(r, now))))
	}
}
