package cli

import (
	"encoding/json"
	"fmt"
	"os"
	"sort"
	"strings"

	"github.com/eharriett0/wt/internal/collide"
	"github.com/eharriett0/wt/internal/config"
	"github.com/eharriett0/wt/internal/gitx"
	"github.com/eharriett0/wt/internal/ui"
)

// Category is how a collision is treated in output + exit code.
type Category string

const (
	CatBlocking Category = "blocking" // live window + overlapping/indeterminate hunks → exit 3
	CatAdvisory Category = "advisory" // shared doc (CLAUDE.md/MEMORY.md) — never blocks
	CatFYI      Category = "fyi"      // append-only or provably-disjoint hunks — never blocks
	CatStale    Category = "stale"    // other window merged/dormant — hidden unless --include-stale
)

// windowByLabel indexes windows by their display label for worktree lookups.
func windowByLabel(ws []collide.Window) map[string]collide.Window {
	m := make(map[string]collide.Window, len(ws))
	for _, w := range ws {
		m[w.Label()] = w
	}
	return m
}

func spansString(spans []gitx.LineRange) string {
	if len(spans) == 0 {
		return ""
	}
	parts := make([]string, 0, len(spans))
	for _, s := range spans {
		if s.Start == s.End {
			parts = append(parts, fmt.Sprintf("L%d", s.Start))
		} else {
			parts = append(parts, fmt.Sprintf("L%d-%d", s.Start, s.End))
		}
	}
	return strings.Join(parts, ",")
}

// ---- check report -------------------------------------------------------

// CheckEntry is one requested-path × other-window collision, graded.
type CheckEntry struct {
	Path         string           `json:"path"`
	Window       string           `json:"window"`
	Liveness     string           `json:"liveness"`
	Category     Category         `json:"category"`
	Severity     string           `json:"severity"` // HIGH | low
	OtherRanges  []gitx.LineRange `json:"other_ranges,omitempty"`
	OverlapSpans []gitx.LineRange `json:"overlap_spans,omitempty"`
}

// buildCheckReport classifies + hunk-grades every conflict for the requested
// paths. currentWorktree is the window running `check` (its own edits, if any,
// drive overlap detection). includeStale keeps merged/dormant windows.
func buildCheckReport(c *config.Config, ws []collide.Window, currentWorktree string, paths []string, includeStale bool) []CheckEntry {
	conflicts := collide.CheckPaths(ws, currentWorktree, paths)
	live := collide.ClassifyWindows(ws, c.Base, collide.ConflictWindowSet(conflicts), c.MaxAge)
	byLabel := windowByLabel(ws)

	var out []CheckEntry
	for _, cf := range conflicts {
		wl := live[cf.Window]
		e := CheckEntry{Path: cf.Path, Window: cf.Window, Liveness: wl.Label()}

		switch {
		case wl.Level.IsSuppressed():
			e.Category, e.Severity = CatStale, "low"
		case collide.IsSharedDoc(cf.Path, c.SharedDocs):
			e.Category, e.Severity = CatAdvisory, "low"
		default:
			appendOnly := collide.IsAppendOnly(cf.Path, c.AppendOnlyPaths)
			// Use the resolved repo-relative file (cf.MatchedFile) for hunk
			// lookup — cf.Path may be a basename that git pathspec can't resolve
			// for a nested file.
			rangesPath := cf.MatchedFile
			if rangesPath == "" {
				rangesPath = cf.Path
			}
			var cur, other []gitx.LineRange
			if !appendOnly {
				cur = gitx.ChangedRanges(currentWorktree, c.Base, rangesPath)
				if w, ok := byLabel[cf.Window]; ok {
					other = gitx.ChangedRanges(w.Worktree, c.Base, rangesPath)
				}
			}
			e.OtherRanges = other
			sev := collide.ConflictSeverity(cur, other, appendOnly)
			e.OverlapSpans = collide.OverlappingSpans(cur, other)
			if sev == collide.SevHigh {
				e.Category, e.Severity = CatBlocking, "HIGH"
			} else {
				e.Category, e.Severity = CatFYI, "low"
			}
		}
		if e.Category == CatStale && !includeStale {
			// keep for JSON/stale-count, filtered at render time
		}
		out = append(out, e)
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i].Path != out[j].Path {
			return out[i].Path < out[j].Path
		}
		return out[i].Window < out[j].Window
	})
	return out
}

// renderCheckText prints the check report and returns the process exit code
// (3 iff any blocking entry). includeStale reveals suppressed windows;
// showDiff previews the other window's hunk ranges inline.
func renderCheckText(entries []CheckEntry, paths []string, includeStale, showDiff bool) int {
	var blocking, advisory, fyi, stale []CheckEntry
	for _, e := range entries {
		switch e.Category {
		case CatBlocking:
			blocking = append(blocking, e)
		case CatAdvisory:
			advisory = append(advisory, e)
		case CatFYI:
			fyi = append(fyi, e)
		case CatStale:
			stale = append(stale, e)
		}
	}
	if includeStale { // promote stale to visible (as low-risk) for transparency
		fyi = append(fyi, stale...)
		stale = nil
	}

	if len(blocking) == 0 {
		if len(advisory)+len(fyi) == 0 {
			ui.OK("clear — no other window is touching %s", strings.Join(paths, ", "))
		} else {
			ui.OK("clear of blocking collisions on %s", strings.Join(paths, ", "))
		}
		printCheckAdvisories(advisory, fyi, showDiff)
		if len(stale) > 0 {
			fmt.Fprintln(os.Stderr, ui.Dim(fmt.Sprintf("   +%d on stale/dormant branch(es) — ignored; --include-stale to show", len(stale))))
		}
		return 0
	}

	ui.Collision("%d path(s) with a HIGH-risk collision (overlapping edits by an active window):", len(blocking))
	for _, e := range blocking {
		line := fmt.Sprintf("   %s  %s %s [%s]", ui.Bold(e.Path), ui.Dim("←"), e.Window, e.Liveness)
		if s := spansString(e.OverlapSpans); s != "" {
			line += "  " + ui.Yellow("overlap "+s)
		}
		fmt.Fprintln(os.Stderr, line)
		if showDiff {
			printOtherHunks(e)
		}
	}
	printCheckAdvisories(advisory, fyi, showDiff)
	if len(stale) > 0 {
		fmt.Fprintln(os.Stderr, ui.Dim(fmt.Sprintf("   +%d on stale/dormant branch(es) — ignored; --include-stale to show", len(stale))))
	}
	return 3
}

func printCheckAdvisories(advisory, fyi []CheckEntry, showDiff bool) {
	for _, e := range advisory {
		fmt.Fprintln(os.Stderr, "   "+ui.Dim(fmt.Sprintf("%s ← %s [%s] · shared doc, advisory — coordinate sections", e.Path, e.Window, e.Liveness)))
		if showDiff {
			printOtherHunks(e)
		}
	}
	for _, e := range fyi {
		msg := "disjoint hunks"
		if len(e.OtherRanges) == 0 {
			msg = "append-only / low-risk"
		}
		fmt.Fprintln(os.Stderr, "   "+ui.Dim(fmt.Sprintf("%s ← %s [%s] · %s → low (FYI)", e.Path, e.Window, e.Liveness, msg)))
		if showDiff {
			printOtherHunks(e)
		}
	}
}

func printOtherHunks(e CheckEntry) {
	if len(e.OtherRanges) == 0 {
		return
	}
	fmt.Fprintln(os.Stderr, "        "+ui.Dim(e.Window+" edits: "+spansString(e.OtherRanges)))
}

func renderCheckJSON(entries []CheckEntry, includeStale bool) int {
	type payload struct {
		Blocking bool         `json:"blocking"`
		Entries  []CheckEntry `json:"entries"`
	}
	blocking := false
	shown := make([]CheckEntry, 0, len(entries))
	for _, e := range entries {
		if e.Category == CatStale && !includeStale {
			continue
		}
		if e.Category == CatBlocking {
			blocking = true
		}
		shown = append(shown, e)
	}
	b, _ := json.MarshalIndent(payload{Blocking: blocking, Entries: shown}, "", "  ")
	fmt.Println(string(b))
	if blocking {
		return 3
	}
	return 0
}

// ---- status report ------------------------------------------------------

// StatusOverlap is one file touched by ≥2 windows, graded.
type StatusOverlap struct {
	File         string           `json:"file"`
	Windows      []string         `json:"windows"`
	Category     Category         `json:"category"`
	Severity     string           `json:"severity"`
	OverlapSpans []gitx.LineRange `json:"overlap_spans,omitempty"`
}

// gradeStatusOverlaps hunk-grades the ACTIVE overlaps (already partitioned).
func gradeStatusOverlaps(c *config.Config, ws []collide.Window, active []collide.Overlap) []StatusOverlap {
	byLabel := windowByLabel(ws)
	var out []StatusOverlap
	for _, o := range active {
		so := StatusOverlap{File: o.File, Windows: o.Windows}
		switch {
		case collide.IsSharedDoc(o.File, c.SharedDocs):
			so.Category, so.Severity = CatAdvisory, "low"
		default:
			appendOnly := collide.IsAppendOnly(o.File, c.AppendOnlyPaths)
			var rangesByWindow [][]gitx.LineRange
			if !appendOnly {
				for _, label := range o.Windows {
					if w, ok := byLabel[label]; ok {
						rangesByWindow = append(rangesByWindow, gitx.ChangedRanges(w.Worktree, c.Base, o.File))
					}
				}
			}
			sev := collide.OverlapSeverity(rangesByWindow, appendOnly)
			so.OverlapSpans = allPairSpans(rangesByWindow)
			if sev == collide.SevHigh {
				so.Category, so.Severity = CatBlocking, "HIGH"
			} else {
				so.Category, so.Severity = CatFYI, "low"
			}
		}
		out = append(out, so)
	}
	return out
}

// renderStatusJSON emits the window list + graded overlaps as JSON.
func renderStatusJSON(ws []collide.Window, graded []StatusOverlap, benignCount int) int {
	type win struct {
		Label    string   `json:"label"`
		Branch   string   `json:"branch"`
		Issue    string   `json:"issue,omitempty"`
		Title    string   `json:"title,omitempty"`
		Worktree string   `json:"worktree"`
		Touched  []string `json:"touched"`
	}
	type payload struct {
		Blocking    bool            `json:"blocking"`
		Windows     []win           `json:"windows"`
		Overlaps    []StatusOverlap `json:"overlaps"`
		BenignCount int             `json:"benign_count"`
	}
	p := payload{Overlaps: graded, BenignCount: benignCount}
	for _, w := range ws {
		p.Windows = append(p.Windows, win{w.Label(), w.Branch, w.Issue, w.Title, w.Worktree, w.Touched})
	}
	for _, o := range graded {
		if o.Category == CatBlocking {
			p.Blocking = true
		}
	}
	b, _ := json.MarshalIndent(p, "", "  ")
	fmt.Println(string(b))
	return 0
}

// allPairSpans collects the overlapping spans across every window pair (for the
// "overlap L88-95" display in status).
func allPairSpans(rangesByWindow [][]gitx.LineRange) []gitx.LineRange {
	var spans []gitx.LineRange
	for i := 0; i < len(rangesByWindow); i++ {
		for j := i + 1; j < len(rangesByWindow); j++ {
			spans = append(spans, collide.OverlappingSpans(rangesByWindow[i], rangesByWindow[j])...)
		}
	}
	return spans
}
