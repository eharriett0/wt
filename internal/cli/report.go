package cli

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"sort"
	"strings"
	"sync"

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
	Severity       string           `json:"severity"`                  // HIGH | low
	OtherRanges    []gitx.LineRange `json:"other_ranges,omitempty"`    // the other window's edits, base frame; Gap = an insertion (#199)
	OverlapSpans   []gitx.LineRange `json:"overlap_spans,omitempty"`   // where the two windows' edits are one conflict region (collide.ConflictSpans)
	SharedSections []string         `json:"shared_sections,omitempty"` // #22: same section(s) both windows edit → HIGH
	AlreadyMerged  bool             `json:"already_merged,omitempty"`  // #109: other window's blob == origin/base — stale index, not a live collision
	Untracked      bool             `json:"untracked,omitempty"`       // #113: other window's claim is an untracked file — no committed content to collide with
	Subsumed       bool             `json:"subsumed,omitempty"`        // #122: the other window's change to this file is already on base (landed elsewhere) — not contested

	// otherWorktree is the worktree this entry was graded against (#193), for
	// the pre-edit hooks' #122 re-check: Window is a label, and a label can name
	// two windows. Not in the JSON.
	otherWorktree string
}

// subsumedByBase reports whether the other window's change to path is already
// present on base (a clean 3-way merge into base is a no-op), so the file is not
// contested — a branch whose work landed via a DIFFERENT branch's squash (#122).
// Fail-safe: undeterminable → false (keeps the collision).
func subsumedByBase(otherWorktree, base, path string) bool {
	if otherWorktree == "" {
		return false
	}
	s, known := gitx.FileChangeSubsumed(otherWorktree, base, path)
	return known && s
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

// gradeFacts are the per-(worktree, path) git observations the `wt check` grade
// reads. gradeEntry asks for them lazily, in decision order, so an early verdict
// (stale, already merged, shared doc, append-only) never pays for a diff, and the
// subsumption merge (the costliest) runs only for a would-be HIGH. Injected so
// the decision is table-testable without a repo; production is gitGradeFacts.
type gradeFacts interface {
	// AlreadyMerged: the window's content for path is byte-identical to the
	// upstream base (#109). false when it can't tell (keep the collision).
	AlreadyMerged(worktree, path string) bool
	// Untracked: the window's only claim on path is an untracked file (#113).
	Untracked(worktree, path string) bool
	// Ranges: the window's changed line ranges for path, in the BASE frame
	// (#29/#108), so two windows' ranges are comparable.
	Ranges(worktree, path string) []gitx.LineRange
	// Subsumed: the window's change to path is already on base (#122).
	Subsumed(worktree, path string) bool
	// SharedSections: the structured-doc section grade across worktrees (#22),
	// single-sourced in collide.SharedSectionsAcross (#98).
	SharedSections(worktrees []string, path, delimiter string) (shared []string, graded bool)
}

// factKey memoizes one (worktree, path) observation.
type factKey struct{ worktree, path string }

// gitGradeFacts is the production gradeFacts: the gitx shell-outs, memoized per
// (worktree, path). One report grades the same window against several others
// (the banner and `wt status` grade every pair on a file), and without the memo
// each pair would re-run the same diffs. The memo is a set of plain maps, so an
// instance must never be shared between goroutines: the concurrent overlap
// grading takes a fresh one per overlap from gitFactsFor
// (TestGitFactsFor_FreshInstancePerCall).
type gitGradeFacts struct {
	base      string
	src       factSource
	merged    map[factKey]bool
	untracked map[factKey]bool
	subsumed  map[factKey]bool
	ranges    map[factKey][]gitx.LineRange
	rangesNew map[factKey][]gitx.LineRange
}

// factSource is the uncached lookups behind gitGradeFacts: the gitx shell-outs
// in production (gitFactSource), a fake in the memo's own test.
type factSource struct {
	alreadyMerged func(worktree, base, path string) bool
	untracked     func(worktree, path string) bool
	subsumed      func(worktree, base, path string) bool
	ranges        func(worktree, base, path string) []gitx.LineRange // base frame (#29/#108)
	rangesNew     func(worktree, base, path string) []gitx.LineRange // new frame (#123)
}

var gitFactSource = factSource{
	alreadyMerged: alreadyMerged,
	untracked:     untrackedInOther,
	subsumed:      subsumedByBase,
	ranges:        gitx.ChangedRanges,
	rangesNew:     gitx.ChangedRangesNew,
}

func newGitGradeFacts(base string) *gitGradeFacts {
	return newMemoFacts(base, gitFactSource)
}

func newMemoFacts(base string, src factSource) *gitGradeFacts {
	return &gitGradeFacts{
		base:      base,
		src:       src,
		merged:    map[factKey]bool{},
		untracked: map[factKey]bool{},
		subsumed:  map[factKey]bool{},
		ranges:    map[factKey][]gitx.LineRange{},
		rangesNew: map[factKey][]gitx.LineRange{},
	}
}

func memoBool(m map[factKey]bool, k factKey, f func() bool) bool {
	if v, ok := m[k]; ok {
		return v
	}
	v := f()
	m[k] = v
	return v
}

func memoRanges(m map[factKey][]gitx.LineRange, k factKey, f func() []gitx.LineRange) []gitx.LineRange {
	if v, ok := m[k]; ok {
		return v
	}
	v := f()
	m[k] = v
	return v
}

func (g *gitGradeFacts) AlreadyMerged(wt, path string) bool {
	return memoBool(g.merged, factKey{wt, path}, func() bool { return g.src.alreadyMerged(wt, g.base, path) })
}

func (g *gitGradeFacts) Untracked(wt, path string) bool {
	return memoBool(g.untracked, factKey{wt, path}, func() bool { return g.src.untracked(wt, path) })
}

func (g *gitGradeFacts) Subsumed(wt, path string) bool {
	return memoBool(g.subsumed, factKey{wt, path}, func() bool { return g.src.subsumed(wt, g.base, path) })
}

func (g *gitGradeFacts) Ranges(wt, path string) []gitx.LineRange {
	return memoRanges(g.ranges, factKey{wt, path}, func() []gitx.LineRange { return g.src.ranges(wt, g.base, path) })
}

// SharedSections attributes each window's diff to its OWN current-content
// sections, so it reads NEW-frame ranges (#123), memoized like the rest.
func (g *gitGradeFacts) SharedSections(worktrees []string, path, delimiter string) ([]string, bool) {
	newFrame := func(wt, base, p string) []gitx.LineRange {
		return memoRanges(g.rangesNew, factKey{wt, p}, func() []gitx.LineRange { return g.src.rangesNew(wt, base, p) })
	}
	return collide.SharedSectionsAcross(g.base, worktrees, path, delimiter, newFrame)
}

// gradeEntry is THE `wt check` decision for one path × other window: how the
// file grades from currentWorktree's side against otherWt. buildCheckReport
// (`wt check`, the MCP wt_check and both edit hooks) and gradeOverlaps (`wt
// status`, MCP wt_status and the per-turn agent banner) both grade through it,
// so they cannot drift apart again (#182: the banner had its own grade and said
// HIGH where `wt check` said low). Pure given f.
//
// cf.Path is what the caller asked about (it may be a basename), so it is what
// the shared-doc / append-only globs match; cf.MatchedFile is the resolved repo-
// relative file every git lookup uses.
func gradeEntry(c *config.Config, f gradeFacts, currentWorktree, otherWt string, cf collide.Conflict, wl collide.WindowLiveness) CheckEntry {
	e := CheckEntry{Path: cf.Path, Window: cf.Window, Liveness: wl.Label(), otherWorktree: otherWt}

	// Use the resolved repo-relative file (cf.MatchedFile) for hunk / blob
	// lookup — cf.Path may be a fuzzy basename that git pathspec can't
	// resolve for a nested file.
	rangesPath := cf.MatchedFile
	if rangesPath == "" {
		rangesPath = cf.Path
	}

	switch {
	case wl.Level.IsSuppressed():
		e.Category, e.Severity = CatStale, "low"
	case f.AlreadyMerged(otherWt, rangesPath):
		// #109: a stale index/worktree holding ALREADY-MERGED content (blob
		// identical to origin/base) is not a live collision — nothing unmerged
		// to clash with. Surface it (still listed) as informational, not HIGH,
		// so it's distinguishable from a real one instead of blocking on nothing.
		e.Category, e.Severity, e.AlreadyMerged = CatFYI, "low", true
	case f.Untracked(otherWt, rangesPath):
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
			if shared, graded := f.SharedSections([]string{currentWorktree, otherWt}, rangesPath, delim); graded && len(shared) > 0 {
				e.Category, e.Severity = CatBlocking, "HIGH"
				e.SharedSections = shared
			}
		}
	default:
		appendOnly := collide.IsAppendOnly(cf.Path, c.AppendOnlyPaths)
		var cur, other []gitx.LineRange
		if !appendOnly {
			cur = f.Ranges(currentWorktree, rangesPath)
			if otherWt != "" {
				other = f.Ranges(otherWt, rangesPath)
			}
		}
		e.OtherRanges = other
		sev := collide.ConflictSeverity(cur, other, appendOnly)
		e.OverlapSpans = collide.ConflictSpans(cur, other)
		switch {
		case sev != collide.SevHigh:
			e.Category, e.Severity = CatFYI, "low"
		case f.Subsumed(otherWt, rangesPath):
			// #122: the phantom "overlap" is base's OWN change to this file,
			// mis-attributed to a branch whose change already landed elsewhere.
			e.Category, e.Severity, e.Subsumed = CatFYI, "low", true
		default:
			e.Category, e.Severity = CatBlocking, "HIGH"
		}
	}
	return e
}

// buildCheckReport classifies + hunk-grades every conflict for the requested
// queries. currentWorktree is the window running `check` (its own edits, if any,
// drive overlap detection). Stale (merged/dormant/closed) entries are always
// returned, as CatStale, for the JSON + the stale count; the renderers drop them
// unless includeStale. Each query carries its match mode (#181): a hook's or an
// agent's real path is exact; only a `wt check` argument that names no path in
// the repo is fuzzy.
func buildCheckReport(c *config.Config, ws []collide.Window, currentWorktree string, qs []collide.Query, includeStale bool) []CheckEntry {
	conflicts := collide.CheckPaths(ws, currentWorktree, qs)
	live := collide.ClassifyWindows(ws, c.Base, collide.ConflictWindowSet(conflicts), c.MaxAge)
	return checkEntries(c, newGitGradeFacts(c.Base), ws, currentWorktree, conflicts, live)
}

// checkEntries is buildCheckReport's decision: every conflict graded by
// gradeEntry, sorted by path then window. Pure given f, which is what lets a test
// hold the per-turn banner against `wt check` itself (#182).
//
// Each conflict is graded against ITS window's worktree (collide.WorktreeOf,
// #193), never the label's: two windows can share a label, and the label lookup
// graded a real overlap against the namesake's disjoint hunks. Liveness stays
// per label, which ClassifyWindows resolves to the least-suppressed namesake
// (#182), so a shared label can only surface more, never hide.
func checkEntries(c *config.Config, f gradeFacts, ws []collide.Window, currentWorktree string, conflicts []collide.Conflict, live map[string]collide.WindowLiveness) []CheckEntry {
	var out []CheckEntry
	for _, cf := range conflicts {
		out = append(out, gradeEntry(c, f, currentWorktree, collide.WorktreeOf(cf, ws), cf, live[cf.Window]))
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i].Path != out[j].Path {
			return out[i].Path < out[j].Path
		}
		if out[i].Window != out[j].Window {
			return out[i].Window < out[j].Window
		}
		return out[i].otherWorktree < out[j].otherWorktree
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
		if e.Subsumed {
			// #122: the other window's change to this file is already on base (its
			// work landed via a different branch's squash) — the "overlap" was
			// base's own change mis-attributed to it. Not contested.
			fmt.Fprintln(os.Stderr, "   "+ui.Dim(fmt.Sprintf("%s ← %s [%s] · change already on base (landed elsewhere) — not contested", e.Path, e.Window, e.Liveness)))
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
	Subsumed       bool             `json:"subsumed,omitempty"`        // #122: <2 windows genuinely contest it (others' change already on base)
}

// gradeStatusOverlaps grades the ACTIVE overlaps (already partitioned, so each
// lists only live windows) for `wt status` and the MCP wt_status: the
// window-neutral view, through the same per-entry grade as `wt check` (#182).
func gradeStatusOverlaps(c *config.Config, ws []collide.Window, active []collide.Overlap) []StatusOverlap {
	return gradeOverlaps(c, gitFactsFor(c), ws, active, nil, collide.Self{})
}

// gitFactsFor returns the production facts factory: a FRESH memoized instance on
// every call (one call per overlap graded). gitGradeFacts' memo is unsynchronized
// maps, so returning one shared instance would race the concurrent grading.
func gitFactsFor(c *config.Config) func() gradeFacts {
	return func() gradeFacts { return newGitGradeFacts(c.Base) }
}

// gradeWorkers bounds the overlaps graded at once. Each grade is a handful of
// git shell-outs (#109 blob reads, the #113 status, two diffs), and the per-turn
// banner pays for every overlap in the repo on every prompt.
const gradeWorkers = 8

// gradeOverlaps grades partitioned overlaps with gradeEntry, the one decision
// `wt check` makes (#182). The banner, `wt status` and wt_status used to run a
// separate all-windows grade that `wt check` never ran: a merged branch whose
// file equals base (no ranges, "indeterminate") or two OTHER windows overlapping
// each other made a file HIGH that `wt check` grades low. Pure given newFacts.
//
// self is the window whose side to take: the per-turn agent banner passes the
// current window, `wt status` passes the zero Self (no side).
//
//   - A file self is editing lists the windows `wt check <file>` lists there,
//     and is HIGH iff the check grade from self blocks on at least one of them
//     AND self's own claim on the file contests something. HIGH ⇒ `wt check`
//     blocks, but NOT the converse, on purpose: when self's copy is already on
//     base (#109) or its change already landed (#122), `wt check` still blocks,
//     because self's empty or phantom ranges can't be proven disjoint (its
//     pre-edit heads-up), yet self holds nothing that can collide. A window whose
//     PR merged with its session still open would otherwise be told HIGH on every
//     turn about every file a live window edits, the noise #182 removed. Do not
//     tighten this back to "iff check blocks". An UNTRACKED self (#113) stays
//     HIGH: it is about to commit an add/add, and check from its side blocks.
//   - Any other file (and every file, for the zero Self) is HIGH iff some PAIR of
//     its windows collides: `wt check` would block in BOTH windows of the pair.
//     Both, because a window whose claim on the file is already merged (#109),
//     untracked (#113) or already on base (#122) contests nothing; `wt check`
//     run from inside it still says HIGH, but only as the pre-edit heads-up its
//     own empty ranges produce, which is not a collision between the two. This
//     keeps the #122 behaviour `wt status` already had and extends it to #109 and
//     #113: two windows creating the same new file read advisory here until one
//     of them commits it, while `wt check` from the untracked side still blocks.
//
// Windows are told apart by worktree, not label (#182): two worktrees that
// claimed one issue are both "#N", and grading by label compared one of them
// with itself and dropped the pair.
//
// live supplies the liveness label for the other window's entries; nil is fine
// for an already-partitioned overlap (every window in it is live).
//
// Each overlap is ONE file, and every fact is keyed by (worktree, that file), so
// no fact is shared across overlaps: each overlap grades with its own facts from
// newFacts, concurrently (bounded by gradeWorkers), and the output keeps the
// input order.
func gradeOverlaps(c *config.Config, newFacts func() gradeFacts, ws []collide.Window, active []collide.Overlap, live map[string]collide.WindowLiveness, self collide.Self) []StatusOverlap {
	byLabel := windowByLabel(ws)
	out := make([]StatusOverlap, len(active))
	sem := make(chan struct{}, gradeWorkers)
	var wg sync.WaitGroup
	for i, o := range active {
		wg.Add(1)
		go func(i int, o collide.Overlap) {
			defer wg.Done()
			sem <- struct{}{}
			defer func() { <-sem }()
			out[i] = gradeOverlap(c, newFacts(), byLabel, o, live, self)
		}(i, o)
	}
	wg.Wait()
	return out
}

// overlapMember is one window of an overlap: its label (liveness, display) and
// its worktree (identity, every git lookup).
type overlapMember struct{ label, worktree string }

// overlapMembers lists o's windows by worktree. An Overlap built by hand carries
// labels only; those resolve through byLabel, one member per label. Pure.
func overlapMembers(o collide.Overlap, byLabel map[string]collide.Window) []overlapMember {
	if len(o.Worktrees) == len(o.Windows) {
		ms := make([]overlapMember, len(o.Windows))
		for i := range o.Windows {
			ms[i] = overlapMember{o.Windows[i], o.Worktrees[i]}
		}
		return ms
	}
	var ms []overlapMember
	for _, l := range windowsExcluding(o.Windows, "") {
		ms = append(ms, overlapMember{l, byLabel[l].Worktree})
	}
	return ms
}

// isSelfMember reports whether m is self: by worktree, the identity, falling back
// to the label only for a Self that carries none. Pure.
func isSelfMember(self collide.Self, m overlapMember) bool {
	if self.Worktree != "" {
		return m.worktree == self.Worktree
	}
	return self.Label != "" && m.label == self.Label
}

// gradeOverlap grades ONE overlap, per the rules on gradeOverlaps. Pure given f.
func gradeOverlap(c *config.Config, f gradeFacts, byLabel map[string]collide.Window, o collide.Overlap, live map[string]collide.WindowLiveness, self collide.Self) StatusOverlap {
	ms := overlapMembers(o, byLabel)
	grade := func(from, to overlapMember) CheckEntry {
		cf := collide.Conflict{Path: o.File, Window: to.label, MatchedFile: o.File}
		return gradeEntry(c, f, from.worktree, to.worktree, cf, live[to.label])
	}
	var blocking, rest []CheckEntry
	if si := slices.IndexFunc(ms, func(m overlapMember) bool { return isSelfMember(self, m) }); si >= 0 {
		me := ms[si]
		// Asked only once an entry would block (the #122 merge is the costliest
		// fact); memoized by f either way.
		contestsNothing := func() bool {
			return f.AlreadyMerged(me.worktree, o.File) || f.Subsumed(me.worktree, o.File)
		}
		for j, m := range ms {
			if j == si {
				continue
			}
			e := grade(me, m)
			if e.Category == CatBlocking && contestsNothing() {
				e.Category, e.Severity = CatFYI, "low" // HIGH ⇒ check blocks, not ⟺ (see gradeOverlaps)
			}
			if e.Category == CatBlocking {
				blocking = append(blocking, e)
			} else {
				rest = append(rest, e)
			}
		}
		return summarizeOverlap(c, o, blocking, rest)
	}
	for i := 0; i < len(ms); i++ {
		for j := i + 1; j < len(ms); j++ {
			a := grade(ms[i], ms[j])
			if a.Category != CatBlocking {
				rest = append(rest, a)
				continue
			}
			if b := grade(ms[j], ms[i]); b.Category != CatBlocking {
				rest = append(rest, b)
				continue
			}
			blocking = append(blocking, a)
		}
	}
	return summarizeOverlap(c, o, blocking, rest)
}

// summarizeOverlap folds one overlap's graded entries into its status line: HIGH
// when any entry (or pair) blocks, carrying the overlapping spans / shared
// sections that made it so; otherwise a shared-doc advisory or an FYI, with
// Subsumed set when a would-be overlap was base's own change (#122). Pure.
func summarizeOverlap(c *config.Config, o collide.Overlap, blocking, rest []CheckEntry) StatusOverlap {
	so := StatusOverlap{File: o.File, Windows: o.Windows}
	if len(blocking) > 0 {
		so.Category, so.Severity = CatBlocking, "HIGH"
		for _, e := range blocking {
			so.OverlapSpans = appendNewSpans(so.OverlapSpans, e.OverlapSpans)
			for _, s := range e.SharedSections {
				if !slices.Contains(so.SharedSections, s) {
					so.SharedSections = append(so.SharedSections, s)
				}
			}
		}
		return so
	}
	so.Category, so.Severity = CatFYI, "low"
	if collide.IsSharedDoc(o.File, c.SharedDocs) {
		so.Category = CatAdvisory
	}
	for _, e := range rest {
		so.OverlapSpans = appendNewSpans(so.OverlapSpans, e.OverlapSpans)
		so.Subsumed = so.Subsumed || e.Subsumed
	}
	return so
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

// appendNewSpans appends the spans not already present (for the "overlap
// L88-95" display in status): several pairs on one file often intersect on the
// same lines, and the old all-pairs list printed "L11-12,L11-12". Pure.
func appendNewSpans(dst, spans []gitx.LineRange) []gitx.LineRange {
	for _, s := range spans {
		if !slices.Contains(dst, s) {
			dst = append(dst, s)
		}
	}
	return dst
}
