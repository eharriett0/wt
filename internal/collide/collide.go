// Package collide is the core multi-window collision intelligence: with 3-4
// windows working on different things, it answers "is anyone else touching the
// files I'm touching?" — not just "is this issue claimed?".
//
// It works at the FILE level, derived from each worktree's git state, so it
// catches collisions even when windows are on different issues/branches AND
// even when a window never claimed a GitHub issue at all.
package collide

import (
	"fmt"
	"path/filepath"
	"runtime"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/eharriett0/wt/internal/activework"
	"github.com/eharriett0/wt/internal/config"
	"github.com/eharriett0/wt/internal/ghx"
	"github.com/eharriett0/wt/internal/gitx"
	"github.com/eharriett0/wt/internal/ui"
)

// Window is one worktree's live state.
type Window struct {
	Worktree string   // absolute worktree path
	Branch   string   // current branch (or "HEAD" if detached)
	Issue    string   // claimed issue number, or "" if unclaimed
	Title    string   // claimed issue title, or ""
	Touched  []string // files changed (uncommitted ∪ committed-on-branch vs base)
}

// Label is a short human identifier for a window.
func (w Window) Label() string {
	if w.Issue != "" {
		return "#" + w.Issue
	}
	if w.Branch != "" && w.Branch != "HEAD" {
		return w.Branch
	}
	return filepath.Base(w.Worktree)
}

// Scan enumerates every worktree of the repo, its branch, claim info (matched
// from the active-work file by worktree path then branch), and touched files.
func Scan(c *config.Config) ([]Window, error) {
	paths, err := gitx.WorktreePaths()
	if err != nil {
		return nil, err
	}
	claims := activework.Parse(activework.Read(c.ActiveWork))
	byWorktree := map[string]activework.Entry{}
	byBranch := map[string]activework.Entry{}
	for _, e := range claims {
		if e.Worktree != "" {
			byWorktree[e.Worktree] = e
		}
		if e.Branch != "" {
			byBranch[e.Branch] = e
		}
	}

	// Enumerate worktrees CONCURRENTLY (#22). Each window needs 3 git
	// subprocess calls (CurrentBranchIn + status + diff inside TouchedFiles);
	// run sequentially at ~30 windows this blew past 2 minutes, and `wt check`
	// inherited the cost since it also calls Scan. The reads are independent per
	// worktree (separate working trees, read-only — no git-lock contention), so
	// a bounded worker pool cuts wall time to roughly the slowest single
	// worktree. Indexed writes preserve WorktreePaths order (stable output);
	// per-worktree git errors stay swallowed, exactly as before.
	ws := make([]Window, len(paths))
	sem := make(chan struct{}, scanWorkers())
	var wg sync.WaitGroup
	for i, p := range paths {
		wg.Add(1)
		sem <- struct{}{}
		go func(i int, p string) {
			defer wg.Done()
			defer func() { <-sem }()
			br, _ := gitx.CurrentBranchIn(p)
			w := Window{Worktree: p, Branch: br, Touched: gitx.TouchedFiles(p, c.Base)}
			if e, ok := byWorktree[p]; ok {
				w.Issue, w.Title = e.Issue, e.Title
			} else if e, ok := byBranch[br]; ok {
				w.Issue, w.Title = e.Issue, e.Title
			}
			ws[i] = w
		}(i, p)
	}
	wg.Wait()
	return ws, nil
}

// scanWorkers bounds the concurrent per-worktree git reads in Scan: enough to
// hide latency at ~30 windows without spawning dozens of git processes at once.
func scanWorkers() int {
	n := runtime.NumCPU() * 2
	if n < 4 {
		n = 4
	}
	if n > 16 {
		n = 16
	}
	return n
}

// Overlap is a file touched by more than one window.
type Overlap struct {
	File    string
	Windows []string // window labels touching the file
}

// Overlaps returns files touched by ≥2 windows (pure; sorted by file). This is
// the headline collision signal across all windows.
func Overlaps(ws []Window) []Overlap {
	hits := map[string][]string{}
	for _, w := range ws {
		label := w.Label()
		seen := map[string]bool{}
		for _, f := range w.Touched {
			if seen[f] {
				continue
			}
			seen[f] = true
			hits[f] = append(hits[f], label)
		}
	}
	var out []Overlap
	for f, labels := range hits {
		if len(labels) >= 2 {
			sort.Strings(labels)
			out = append(out, Overlap{File: f, Windows: labels})
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i].File < out[j].File })
	return out
}

// Conflict is a requested path that another window is already touching.
type Conflict struct {
	Path        string
	Window      string // the other window's label
	MatchedFile string // the other window's actual repo-relative touched file that
	// matched Path (may differ from Path when Path was a basename/suffix) — used
	// for hunk-range lookup so basename checks on nested files still grade.
}

// CheckPaths reports, for each requested path, which OTHER windows (not
// currentWorktree) are already touching it. Pure. paths are matched against
// each window's touched set both exactly and by suffix, so callers can pass
// repo-relative paths or basenames.
func CheckPaths(ws []Window, currentWorktree string, paths []string) []Conflict {
	var out []Conflict
	for _, w := range ws {
		if sameWorktree(w.Worktree, currentWorktree) {
			continue
		}
		for _, p := range paths {
			p = strings.TrimSpace(p)
			if p == "" {
				continue
			}
			if matched, ok := matchTouched(p, w.Touched); ok {
				out = append(out, Conflict{Path: p, Window: w.Label(), MatchedFile: matched})
			}
		}
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i].Path != out[j].Path {
			return out[i].Path < out[j].Path
		}
		return out[i].Window < out[j].Window
	})
	return out
}

// matchTouched reports whether requested path p matches any touched file — by
// exact repo-relative path, path suffix, or basename — and returns the actual
// matched touched file (repo-relative), preferring an exact match.
func matchTouched(p string, touched []string) (string, bool) {
	for _, f := range touched {
		if f == p {
			return f, true
		}
	}
	for _, f := range touched {
		if strings.HasSuffix(f, "/"+p) || filepath.Base(f) == p {
			return f, true
		}
	}
	return "", false
}

// IsSharedDoc reports whether path is one of the configured append-heavy shared
// docs (matched by basename) — CLAUDE.md, MEMORY.md, etc. A cross-window touch
// on these is expected (every window edits them) and is surfaced as an advisory
// rather than a blocking collision. Empty shared → always false (soft-list off).
func IsSharedDoc(path string, shared []string) bool {
	base := filepath.Base(strings.TrimSpace(path))
	for _, d := range shared {
		if base == filepath.Base(strings.TrimSpace(d)) {
			return true
		}
	}
	return false
}

// IsAppendOnly reports whether path matches any of the configured append-only
// globs, against both the full repo-relative path and the basename. Overlaps on
// these are always downgraded to FYI regardless of hunk analysis — files where
// concurrent appends are expected safe (changelogs, inventory lists, kustomize
// resource lists).
//
// Matching is doublestar-aware (#31): plain filepath.Match treats "**" as a
// single-level "*", so a configured `docs/**/*.md` (or any nested target)
// silently NEVER matched — the downgrade quietly no-op'd. MatchDoubleStar lets
// "**" span any number of path segments; a `*` still stays within one segment.
func IsAppendOnly(path string, globs []string) bool {
	path = strings.TrimSpace(path)
	base := filepath.Base(path)
	for _, g := range globs {
		g = strings.TrimSpace(g)
		if g == "" {
			continue
		}
		if MatchDoubleStar(g, path) || MatchDoubleStar(g, base) {
			return true
		}
	}
	return false
}

// MatchDoubleStar reports whether name matches pattern, where a "**" path
// segment matches ANY number of segments (including zero). Every other segment
// is matched with filepath.Match (so *, ?, [set] work within one segment). Both
// are '/'-separated. A pattern with no "**" behaves exactly like the pre-#31
// segment-wise filepath.Match.
func MatchDoubleStar(pattern, name string) bool {
	return matchSegments(strings.Split(pattern, "/"), strings.Split(name, "/"))
}

func matchSegments(pat, name []string) bool {
	for len(pat) > 0 {
		if pat[0] == "**" {
			rest := pat[1:]
			if len(rest) == 0 {
				return true // trailing ** absorbs everything remaining
			}
			for i := 0; i <= len(name); i++ { // ** consumes 0..len(name) segments
				if matchSegments(rest, name[i:]) {
					return true
				}
			}
			return false
		}
		if len(name) == 0 {
			return false
		}
		if ok, _ := filepath.Match(pat[0], name[0]); !ok {
			return false
		}
		pat, name = pat[1:], name[1:]
	}
	return len(name) == 0
}

// Severity grades a file collision after hunk analysis.
type Severity int

const (
	SevFYI  Severity = iota // disjoint hunks / append-only / indeterminate-safe — advisory, non-blocking
	SevHigh                 // overlapping line ranges — real conflict risk, blocks
)

func (s Severity) String() string {
	if s == SevHigh {
		return "HIGH"
	}
	return "low"
}

// OverlappingSpans returns the intersections between two sets of line ranges
// (empty ⇒ fully disjoint). O(n·m); the n,m here are per-file hunk counts, tiny.
func OverlappingSpans(a, b []gitx.LineRange) []gitx.LineRange {
	var hits []gitx.LineRange
	for _, ra := range a {
		for _, rb := range b {
			if ra.Overlaps(rb) {
				lo, hi := ra.Start, ra.End
				if rb.Start > lo {
					lo = rb.Start
				}
				if rb.End < hi {
					hi = rb.End
				}
				hits = append(hits, gitx.LineRange{Start: lo, End: hi})
			}
		}
	}
	return hits
}

// ConflictSeverity grades a check/pre-commit collision on a file between the
// CURRENT worktree's changed ranges and one OTHER window's ranges (pure):
//   - append-only path              → SevFYI
//   - either side has no ranges yet → SevHigh (indeterminate — can't prove
//     disjoint, so don't silently clear; preserves the pre-edit "heads up")
//   - ranges provably disjoint      → SevFYI (the crying-wolf case #7 targets)
//   - ranges overlap                → SevHigh
func ConflictSeverity(current, other []gitx.LineRange, appendOnly bool) Severity {
	if appendOnly {
		return SevFYI
	}
	if len(current) == 0 || len(other) == 0 {
		return SevHigh
	}
	if len(OverlappingSpans(current, other)) > 0 {
		return SevHigh
	}
	return SevFYI
}

// OverlapSeverity grades a status overlap on one file touched by several
// windows, given each window's ranges (pure). append-only ⇒ FYI. Otherwise:
// if ANY participating window has no computable ranges (untracked/binary/diff
// error), the overlap is indeterminate → SevHigh (can't prove disjoint, so
// don't clear — mirrors ConflictSeverity's fail-safe). Else HIGH iff any pair
// of windows has overlapping ranges; all-disjoint ⇒ FYI.
func OverlapSeverity(rangesByWindow [][]gitx.LineRange, appendOnly bool) Severity {
	if appendOnly {
		return SevFYI
	}
	for _, r := range rangesByWindow {
		if len(r) == 0 {
			return SevHigh // indeterminate — a real add/add or binary collision can't be graded disjoint
		}
	}
	for i := 0; i < len(rangesByWindow); i++ {
		for j := i + 1; j < len(rangesByWindow); j++ {
			if len(OverlappingSpans(rangesByWindow[i], rangesByWindow[j])) > 0 {
				return SevHigh
			}
		}
	}
	return SevFYI
}

func sameWorktree(a, b string) bool {
	return a == b || realPath(a) == realPath(b)
}

func realPath(p string) string {
	if r, err := filepath.EvalSymlinks(p); err == nil {
		return r
	}
	if a, err := filepath.Abs(p); err == nil {
		return a
	}
	return p
}

// Liveness classifies how "alive" a colliding window's branch is. A file
// collision only matters if the other window can still change that file —
// a branch whose work is already merged (squash-safe) and whose worktree is
// clean cannot, so flagging it is noise. Only LiveStale is suppressed; every
// other level (including LiveUnknown) is surfaced, so we never hide a real
// collision when the signal is ambiguous.
type Liveness int

const (
	LiveUnknown  Liveness = iota // couldn't determine (git error) — treated as active
	LiveStale                    // clean worktree, no open PR, nothing unshipped vs base — noise
	LiveDormant                  // unmerged commits but no activity for > max-age — parked, suppressed like stale (#7)
	LiveUnmerged                 // commits not yet on base, no open PR — latent collision
	LiveDirty                    // uncommitted changes in the worktree — actively editing
	LiveOpenPR                   // an open PR exists for the branch — active contention
)

// IsStale reports whether this level is the definitively-merged/abandoned case.
func (l Liveness) IsStale() bool { return l == LiveStale }

// IsSuppressed reports whether a collision against a window at this level should
// be filtered out of the default output — merged-and-clean (stale) or parked
// past the dormancy threshold (dormant). Both mean the other window is unlikely
// to change the file underneath you right now.
func (l Liveness) IsSuppressed() bool { return l == LiveStale || l == LiveDormant }

// Tag is a short human label for the level.
func (l Liveness) Tag() string {
	switch l {
	case LiveOpenPR:
		return "open PR"
	case LiveDirty:
		return "uncommitted edits"
	case LiveUnmerged:
		return "commits, no PR"
	case LiveDormant:
		return "dormant"
	case LiveStale:
		return "stale: merged / no PR"
	default:
		return "unknown"
	}
}

// WindowLiveness is a window's resolved liveness plus the open-PR number when
// the level is LiveOpenPR (for display, e.g. "[open PR #747]").
type WindowLiveness struct {
	Level    Liveness
	PR       string        // open PR number when Level == LiveOpenPR, else ""
	MergedPR string        // merged PR number when the branch is shipped (Level LiveStale, #73)
	Age      time.Duration // time since the branch's last commit (0 if unknown)
}

// Label is the plain (uncolored) liveness word, e.g. "open PR #747" or
// "stale: merged / no PR", with a "· last commit Nd ago" suffix when known.
func (wl WindowLiveness) Label() string {
	base := wl.Level.Tag()
	if wl.Level == LiveOpenPR && wl.PR != "" {
		base = "open PR #" + wl.PR
	}
	// #73: say "merged #N", not "no PR", when the branch actually shipped via a
	// squash-merged (and usually deleted) PR.
	if wl.Level == LiveStale && wl.MergedPR != "" {
		base = "merged #" + wl.MergedPR
	}
	if wl.Age > 0 && (wl.Level == LiveUnmerged || wl.Level == LiveDormant) {
		base += ", last commit " + HumanAge(wl.Age) + " ago"
	}
	return base
}

// Badge is the colored, bracketed liveness tag for output lines: red for an
// open PR, yellow for in-progress (dirty / unmerged), dim for stale/dormant/unknown.
func (wl WindowLiveness) Badge() string {
	tag := "[" + wl.Label() + "]"
	switch wl.Level {
	case LiveOpenPR:
		return ui.Red(tag)
	case LiveDirty, LiveUnmerged:
		return ui.Yellow(tag)
	default: // LiveStale, LiveDormant, LiveUnknown
		return ui.Dim(tag)
	}
}

// HumanAge renders a duration coarsely: "3d", "5h", "12m", or "just now".
func HumanAge(d time.Duration) string {
	switch {
	case d >= 24*time.Hour:
		return fmt.Sprintf("%dd", int(d.Hours()/24))
	case d >= time.Hour:
		return fmt.Sprintf("%dh", int(d.Hours()))
	case d >= time.Minute:
		return fmt.Sprintf("%dm", int(d.Minutes()))
	default:
		return "just now"
	}
}

// LiveFacts are the raw observations ClassifyFacts turns into a Liveness. Split
// out from the I/O so the decision boundary is pure and unit-testable.
type LiveFacts struct {
	HasOpenPR bool
	PRChecked bool // whether open-PR status was actually resolvable (gh present+authed)
	Dirty     bool
	Merged    bool          // a MERGED PR exists for the branch — squash-merged/shipped (#73)
	Unshipped int           // git cherry "+" count; <0 means it could not be computed
	Age       time.Duration // time since last commit (0 = unknown)
}

// ClassifyFacts maps observations to a Liveness (pure). Precedence is by
// descending certainty-of-activity: open PR > dirty worktree > unmerged commits
// > (fully merged ⇒ stale). An uncomputable unshipped count with no other
// signal is LiveUnknown (surfaced, not suppressed).
//
// maxAge (>0) enables dormancy: an unmerged, no-PR, clean branch whose last
// commit is older than maxAge is LiveDormant (suppressed) rather than
// LiveUnmerged. A dirty or open-PR branch is never dormant — it's active by
// definition. maxAge==0 disables dormancy entirely (backward-compatible).
//
// Dormancy also requires PRChecked: if open-PR status couldn't be resolved (gh
// offline/unauthed) we must NOT downgrade an idle unmerged branch to dormant,
// because it might have an open PR we couldn't see — suppressing it would hide
// a live collision. Ambiguous PR status ⇒ stay LiveUnmerged (active).
func ClassifyFacts(f LiveFacts, maxAge time.Duration) Liveness {
	switch {
	case f.HasOpenPR:
		return LiveOpenPR
	case f.Dirty:
		return LiveDirty
	case f.Merged:
		// A MERGED PR ⇒ shipped. Suppress even when Unshipped>0 — squash-merge
		// breaks patch-equivalence to base, so git cherry reads the branch as
		// unmerged, which used to surface a merged-and-deleted branch as a false
		// HIGH collision (#73). A dirty worktree still wins above (someone reopened
		// the branch to edit it), so we only reach here when it's clean.
		return LiveStale
	case f.Unshipped > 0:
		if maxAge > 0 && f.Age > maxAge && f.PRChecked {
			return LiveDormant
		}
		return LiveUnmerged
	case f.Unshipped == 0:
		return LiveStale
	default:
		return LiveUnknown
	}
}

// Classify resolves one window's liveness via I/O (gh + git). gh is consulted
// only when present+authed; otherwise classification degrades to git-only
// signals (a merged clean branch is still correctly LiveStale offline). maxAge
// enables dormancy suppression; now is injected for testability.
func Classify(w Window, base string, maxAge time.Duration, now time.Time) WindowLiveness {
	var f LiveFacts
	pr := ""
	if ghx.Present() && ghx.Authed() {
		f.PRChecked = true
		if n, ok := ghx.OpenPRForBranch(w.Branch); ok {
			f.HasOpenPR, pr = true, n
		}
	}
	f.Dirty = !gitx.IsClean(w.Worktree)
	if n, err := gitx.CountUnshipped(base, w.Branch); err == nil {
		f.Unshipped = n
	} else {
		f.Unshipped = -1
	}
	// #73: a squash-merged-then-deleted branch has Unshipped>0 (squash breaks
	// patch-equivalence to base) + no OPEN PR, so it read as LiveUnmerged — a
	// false HIGH collision. A MERGED PR means it's shipped ⇒ stale. Only checked
	// when it could change the verdict (gh available, not open-PR, not dirty, and
	// not already fully-merged by ancestry) to avoid an extra gh call per window.
	mergedPR := ""
	if f.PRChecked && !f.HasOpenPR && !f.Dirty && f.Unshipped != 0 {
		if n, ok := ghx.MergedPRForBranchNum(w.Branch); ok {
			f.Merged, mergedPR = true, n
		}
	}
	if age, err := gitx.LastCommitAge(w.Worktree, now); err == nil {
		f.Age = age
	}
	wl := WindowLiveness{Level: ClassifyFacts(f, maxAge), Age: f.Age}
	if wl.Level == LiveOpenPR {
		wl.PR = pr
	}
	if f.Merged {
		wl.MergedPR = mergedPR
	}
	return wl
}

// ClassifyWindows resolves liveness, concurrently, for the windows whose label
// is in `labels` (the small set actually involved in a collision — NOT all
// windows, which would be one gh call each). Keyed by window label. maxAge
// enables dormancy suppression (0 = off).
func ClassifyWindows(ws []Window, base string, labels map[string]bool, maxAge time.Duration) map[string]WindowLiveness {
	type job struct {
		label string
		w     Window
	}
	var jobs []job
	seen := map[string]bool{}
	for _, w := range ws {
		l := w.Label()
		if labels[l] && !seen[l] {
			seen[l] = true
			jobs = append(jobs, job{l, w})
		}
	}

	out := make(map[string]WindowLiveness, len(jobs))
	now := time.Now()
	var mu sync.Mutex
	var wg sync.WaitGroup
	sem := make(chan struct{}, 8) // bound concurrent gh/git shell-outs
	for _, j := range jobs {
		wg.Add(1)
		go func(j job) {
			defer wg.Done()
			sem <- struct{}{}
			defer func() { <-sem }()
			wl := Classify(j.w, base, maxAge, now)
			mu.Lock()
			out[j.label] = wl
			mu.Unlock()
		}(j)
	}
	wg.Wait()
	return out
}

// ConflictWindowSet is the set of OTHER-window labels appearing in conflicts —
// the small set worth classifying for liveness (not all windows).
func ConflictWindowSet(cs []Conflict) map[string]bool {
	set := map[string]bool{}
	for _, c := range cs {
		set[c.Window] = true
	}
	return set
}

// OverlapWindowSet is the set of window labels appearing across all overlaps.
func OverlapWindowSet(ov []Overlap) map[string]bool {
	set := map[string]bool{}
	for _, o := range ov {
		for _, w := range o.Windows {
			set[w] = true
		}
	}
	return set
}

// PartitionConflicts splits `check` conflicts into active vs stale by the
// OTHER window's liveness (pure). A conflict against a stale branch is benign:
// that branch can no longer change the file. Missing/unknown liveness is
// treated as active (never suppress on ambiguity).
func PartitionConflicts(cs []Conflict, live map[string]WindowLiveness) (active, stale []Conflict) {
	for _, c := range cs {
		if wl, ok := live[c.Window]; ok && wl.Level.IsSuppressed() {
			stale = append(stale, c)
		} else {
			active = append(active, c)
		}
	}
	return active, stale
}

// PartitionOverlaps splits `status` overlaps into active vs benign (pure). An
// overlap is a real cross-window collision only when ≥2 of its windows are
// non-stale — one live editor plus N merged branches cannot conflict. Missing/
// unknown liveness counts as live (conservative).
func PartitionOverlaps(ov []Overlap, live map[string]WindowLiveness) (active, benign []Overlap) {
	for _, o := range ov {
		liveCount := 0
		for _, w := range o.Windows {
			if wl, ok := live[w]; !ok || !wl.Level.IsSuppressed() {
				liveCount++
			}
		}
		if liveCount >= 2 {
			active = append(active, o)
		} else {
			benign = append(benign, o)
		}
	}
	return active, benign
}
