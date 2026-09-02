package cli

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
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

// sectionsString renders the shared section headings for a section-graded HIGH
// line (#22). The preamble ("") shows as "(preamble)".
func sectionsString(headings []string) string {
	if len(headings) == 0 {
		return ""
	}
	parts := make([]string, len(headings))
	for i, h := range headings {
		if h == "" {
			h = "(preamble)"
		}
		parts[i] = "\"" + h + "\""
	}
	return "same section: " + strings.Join(parts, ", ")
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
	Path           string           `json:"path"`
	Window         string           `json:"window"`
	Liveness       string           `json:"liveness"`
	Category       Category         `json:"category"`
	Severity       string           `json:"severity"` // HIGH | low
	OtherRanges    []gitx.LineRange `json:"other_ranges,omitempty"`
	OverlapSpans   []gitx.LineRange `json:"overlap_spans,omitempty"`
	SharedSections []string         `json:"shared_sections,omitempty"` // #22: same section(s) both windows edit → HIGH
	AlreadyMerged  bool             `json:"already_merged,omitempty"`  // #109: other window's blob == origin/base — stale index, not a live collision
	Untracked      bool             `json:"untracked,omitempty"`       // #113: other window's claim is an untracked file — no committed content to collide with
}

// alreadyMerged reports whether the other window's content for path is byte-
// identical to origin/base — already-merged work sitting in a stale index, not a
// live collision (#109). Fails safe: unknown (no upstream ref, unreadable blob)
// → false, so the collision keeps its normal grade.
func alreadyMerged(otherWorktree, base, path string) bool {
	if otherWorktree == "" {
		return false
	}
	merged, known := collide.PathMatchesUpstream(otherWorktree, base, path)
	return known && merged
}

// untrackedInOther reports whether the other window's claim on path is an
// untracked file (never added/staged/committed there) — nothing that can be
// pushed, so it can't collide until committed (#113).
func untrackedInOther(otherWorktree, path string) bool {
	return otherWorktree != "" && gitx.IsUntracked(otherWorktree, path)
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

		// Use the resolved repo-relative file (cf.MatchedFile) for hunk / blob
		// lookup — cf.Path may be a basename that git pathspec can't resolve for a
		// nested file.
		rangesPath := cf.MatchedFile
		if rangesPath == "" {
			rangesPath = cf.Path
		}
		otherWt := byLabel[cf.Window].Worktree

		switch {
		case wl.Level.IsSuppressed():
			e.Category, e.Severity = CatStale, "low"
		case alreadyMerged(otherWt, c.Base, rangesPath):
			// #109: a stale index/worktree holding ALREADY-MERGED content (blob
			// identical to origin/base) is not a live collision — nothing unmerged
			// to clash with. Surface it (still listed) as informational, not HIGH,
			// so it's distinguishable from a real one instead of blocking on nothing.
			e.Category, e.Severity, e.AlreadyMerged = CatFYI, "low", true
		case untrackedInOther(otherWt, rangesPath):
			// #113: the other window's only claim on this path is an UNTRACKED file
			// — never added/staged/committed there, so it has no diff and can't be
			// pushed. It cannot collide until it's committed (at which point it
			// grades normally). Advisory, still listed, labelled untracked — not a
			// permanent HIGH on a phantom "uncommitted edits" line range.
			e.Category, e.Severity, e.Untracked = CatFYI, "low", true
		case collide.IsSharedDoc(cf.Path, c.SharedDocs):
			e.Category, e.Severity = CatAdvisory, "low"
			// #22: a STRUCTURED shared doc (configured section delimiter) grades
			// by SECTION — both windows editing the SAME section is HIGH; disjoint
			// sections stay advisory. Falls back to the blanket advisory when it
			// can't section-grade (not structured / bad regexp / doc untracked).
			if delim, isStructured := c.StructuredDocs[filepath.Base(cf.Path)]; isStructured {
				if shared, graded := sharedSectionsAcross(c, []string{currentWorktree, otherWt}, rangesPath, delim); graded && len(shared) > 0 {
					e.Category, e.Severity = CatBlocking, "HIGH"
					e.SharedSections = shared
				}
			}
		default:
			appendOnly := collide.IsAppendOnly(cf.Path, c.AppendOnlyPaths)
			var cur, other []gitx.LineRange
			if !appendOnly {
				cur = gitx.ChangedRanges(currentWorktree, c.Base, rangesPath)
				if otherWt != "" {
					other = gitx.ChangedRanges(otherWt, c.Base, rangesPath)
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
		} else if s := sectionsString(e.SharedSections); s != "" {
			line += "  " + ui.Yellow(s)
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

// renderCheckBlocking is the scriptable-gate variant of `wt check` (#73/#74):
// print ONLY the HIGH-risk (blocking) collisions and exit 3 iff any exist, 0
// otherwise. Advisory / FYI / stale noise is suppressed so a pre-push hook (or
// any CI gate) can key purely on the exit code without parsing prose. Mirrors
// `wt status --blocking` (renderBlockingGate) but scoped to requested paths.
func renderCheckBlocking(entries []CheckEntry, paths []string) int {
	var blocking []CheckEntry
	for _, e := range entries {
		if e.Category == CatBlocking {
			blocking = append(blocking, e)
		}
	}
	if len(blocking) == 0 {
		ui.OK("clear of blocking collisions on %s", strings.Join(paths, ", "))
		return 0
	}
	ui.Collision("%d path(s) with a HIGH-risk collision (overlapping edits by an active window):", len(blocking))
	for _, e := range blocking {
		line := fmt.Sprintf("   %s  %s %s [%s]", ui.Bold(e.Path), ui.Dim("←"), e.Window, e.Liveness)
		if s := spansString(e.OverlapSpans); s != "" {
			line += "  " + ui.Yellow("overlap "+s)
		} else if s := sectionsString(e.SharedSections); s != "" {
			line += "  " + ui.Yellow(s)
		}
		fmt.Fprintln(os.Stderr, line)
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
		if e.AlreadyMerged {
			// #109: not a real collision — the other window's content for this path
			// is byte-identical to the upstream base (already-merged work in a stale
			// index). Still listed so it stays visible, not silently dropped.
			fmt.Fprintln(os.Stderr, "   "+ui.Dim(fmt.Sprintf("%s ← %s [%s] · content identical to upstream base — already merged", e.Path, e.Window, e.Liveness)))
			continue
		}
		if e.Untracked {
			// #113: the other window's claim is an untracked file — no committed
			// content to collide with (it can't be pushed until it's committed).
			fmt.Fprintln(os.Stderr, "   "+ui.Dim(fmt.Sprintf("%s ← %s [%s] · untracked there — nothing committed to collide with", e.Path, e.Window, e.Liveness)))
			continue
		}
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

// CheckPayload is the JSON shape of `wt check --json` — single-sourced so the
// MCP server (#115) returns byte-identical data.
type CheckPayload struct {
	Blocking bool         `json:"blocking"`
	Entries  []CheckEntry `json:"entries"`
}

// buildCheckPayload filters stale entries (unless includeStale) and computes the
// blocking flag. Pure.
func buildCheckPayload(entries []CheckEntry, includeStale bool) CheckPayload {
	p := CheckPayload{Entries: make([]CheckEntry, 0, len(entries))}
	for _, e := range entries {
		if e.Category == CatStale && !includeStale {
			continue
		}
		if e.Category == CatBlocking {
			p.Blocking = true
		}
		p.Entries = append(p.Entries, e)
	}
	return p
}

func renderCheckJSON(entries []CheckEntry, includeStale bool) int {
	p := buildCheckPayload(entries, includeStale)
	b, _ := json.MarshalIndent(p, "", "  ")
	fmt.Println(string(b))
	if p.Blocking {
		return 3
	}
	return 0
}

// ---- status report ------------------------------------------------------

// StatusOverlap is one file touched by ≥2 windows, graded.
type StatusOverlap struct {
	File           string           `json:"file"`
	Windows        []string         `json:"windows"`
	Category       Category         `json:"category"`
	Severity       string           `json:"severity"`
	OverlapSpans   []gitx.LineRange `json:"overlap_spans,omitempty"`
	SharedSections []string         `json:"shared_sections,omitempty"` // #22: same section(s) ≥2 windows edit → HIGH
}

// sharedSectionsAcross is a thin adapter over collide.SharedSectionsAcross,
// which is where the #22 section grade now lives so `wt check` and both git
// hooks single-source it (#98) — the hooks previously stopped at the blanket
// shared-doc advisory, so a same-section collision blocked here and sailed
// through the pre-push guard.
func sharedSectionsAcross(c *config.Config, worktrees []string, rel, delimiter string) (shared []string, graded bool) {
	return collide.SharedSectionsAcross(c.Base, worktrees, rel, delimiter, gitx.ChangedRangesNew)
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
			// #22: structured shared doc → grade by section across every window
			// touching it. Any section edited by ≥2 windows is a HIGH collision.
			if delim, isStructured := c.StructuredDocs[filepath.Base(o.File)]; isStructured {
				var wts []string
				for _, label := range o.Windows {
					if w, ok := byLabel[label]; ok {
						wts = append(wts, w.Worktree)
					}
				}
				if shared, graded := sharedSectionsAcross(c, wts, o.File, delim); graded && len(shared) > 0 {
					so.Category, so.Severity = CatBlocking, "HIGH"
					so.SharedSections = shared
				}
			}
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

// StatusWindow is one window in the `wt status --json` payload.
type StatusWindow struct {
	Label    string   `json:"label"`
	Branch   string   `json:"branch"`
	Issue    string   `json:"issue,omitempty"`
	Title    string   `json:"title,omitempty"`
	Worktree string   `json:"worktree"`
	Touched  []string `json:"touched"`
}

// StatusPayload is the JSON shape of `wt status --json` — single-sourced so the
// MCP server (#115) returns byte-identical data.
type StatusPayload struct {
	Blocking    bool            `json:"blocking"`
	Windows     []StatusWindow  `json:"windows"`
	Overlaps    []StatusOverlap `json:"overlaps"`
	BenignCount int             `json:"benign_count"`
}

// buildStatusPayload assembles the window list + graded overlaps + blocking
// flag. Pure.
func buildStatusPayload(ws []collide.Window, graded []StatusOverlap, benignCount int) StatusPayload {
	p := StatusPayload{Overlaps: graded, BenignCount: benignCount}
	for _, w := range ws {
		p.Windows = append(p.Windows, StatusWindow{w.Label(), w.Branch, w.Issue, w.Title, w.Worktree, w.Touched})
	}
	for _, o := range graded {
		if o.Category == CatBlocking {
			p.Blocking = true
		}
	}
	return p
}

// renderStatusJSON emits the window list + graded overlaps as JSON.
func renderStatusJSON(ws []collide.Window, graded []StatusOverlap, benignCount int) int {
	b, _ := json.MarshalIndent(buildStatusPayload(ws, graded, benignCount), "", "  ")
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
