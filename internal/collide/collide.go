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
	"os"
	"path/filepath"
	"runtime"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/eharriett0/wt/internal/activework"
	"github.com/eharriett0/wt/internal/config"
	"github.com/eharriett0/wt/internal/ghx"
	"github.com/eharriett0/wt/internal/gitx"
	"github.com/eharriett0/wt/internal/section"
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

// PathTouchedByAny reports whether requested path p matches ANY window's touched
// set (exact / suffix / basename) — including the current window. Used by
// `wt check` to tell a real path (a live collision target, or a path that exists
// only on another window's branch) from a typo, so it can refuse to report
// 'clear' for a nonexistent path (#93) without false-refusing a path another
// window is genuinely editing.
func PathTouchedByAny(p string, ws []Window) bool {
	p = strings.TrimSpace(p)
	if p == "" {
		return false
	}
	for _, w := range ws {
		if _, ok := matchTouched(p, w.Touched); ok {
			return true
		}
	}
	return false
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

// RangeFn resolves the line ranges a worktree changed in a path, relative to
// base. Injected so section/hunk grading is unit-testable without a live repo;
// production callers pass gitx.ChangedRanges.
type RangeFn func(worktree, base, path string) []gitx.LineRange

// SharedSectionsAcross returns the section headings edited by ≥2 of the given
// worktrees for the structured doc at repo-relative path rel (partitioned by the
// delimiter regexp). Empty → every window touches DISJOINT sections (safe,
// advisory); non-empty → a same-section collision (HIGH). graded=false when it
// can't section-grade at all (bad regexp, or the doc is unreadable/untracked in
// every worktree — e.g. an out-of-repo memory doc) so the caller falls back to
// the blanket shared-doc advisory.
//
// This is the #22 grade, and it lives here rather than in the check renderer so
// `wt check` AND both git hooks can single-source it (#98) — the hooks used to
// stop at the blanket shared-doc advisory, so two windows editing the SAME
// section of a structured doc blocked in `wt check` and sailed through the
// pre-push guard. Heading text is the stable cross-worktree identity: line
// numbers drift between branches, headings do not.
func SharedSectionsAcross(base string, worktrees []string, rel, delimiter string, ranges RangeFn) (shared []string, graded bool) {
	re, err := section.Compile(delimiter)
	if err != nil {
		return nil, false
	}
	count := map[string]int{}
	var order []string
	any := false
	for _, wt := range worktrees {
		content, rerr := os.ReadFile(filepath.Join(wt, rel))
		if rerr != nil {
			continue // this worktree lacks the doc; others may still grade
		}
		any = true
		heads := section.EditedHeadings(section.Parse(string(content), re), ranges(wt, base, rel))
		for _, h := range heads {
			if count[h] == 0 {
				order = append(order, h)
			}
			count[h]++
		}
	}
	if !any {
		return nil, false
	}
	for _, h := range order {
		if count[h] >= 2 {
			shared = append(shared, h)
		}
	}
	return shared, true
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

// mergedVerdict decides, from the three blob hashes, whether a window holds
// nothing UNMERGED for a path — i.e. its content is already merged and sitting in
// a stale index/worktree, not a real collision (#109). merged=true requires the
// working tree to equal upstream AND, if the path is staged, the index to equal
// it too. known=false when upstream or the worktree blob can't be read, so the
// caller fails safe (keep the collision HIGH — never clear on can't-tell). A
// differing worktree or index is real content → merged=false, known=true. Pure.
func mergedVerdict(up string, upOK bool, wt string, wtOK bool, staged string, stagedOK bool) (merged, known bool) {
	// The INDEX blob is required, not optional: a path that IS a live conflict
	// (in the other window's touched set) but has NO index entry (stagedOK=false)
	// is a STAGED DELETION — a divergent change from upstream, not already-merged.
	// So clear only when upstream, worktree, AND index all resolve and all equal
	// upstream; a missing/unreadable index → known=false → the collision stays HIGH.
	if !upOK || !wtOK || !stagedOK {
		return false, false
	}
	if wt != up || staged != up {
		return false, true
	}
	return true, true
}

// PathMatchesUpstream reports whether the file at path in worktree is byte-
// identical to origin/<base>:<path> (falling back to <base>:<path>) — i.e. the
// window's content for this path is already merged, not a live collision (#109).
// known=false when there's no upstream ref for the path or the blob can't be
// hashed; callers MUST fail safe (treat not-known as NOT merged, keep HIGH).
// Blob-hash only — no diff, no network.
func PathMatchesUpstream(worktree, base, path string) (merged, known bool) {
	up, upOK := gitx.RefBlob(worktree, "origin/"+base, path)
	if !upOK {
		up, upOK = gitx.RefBlob(worktree, base, path)
	}
	wt, wtOK := gitx.WorktreeBlob(worktree, path)
	staged, stagedOK := gitx.RefBlob(worktree, "", path)
	return mergedVerdict(up, upOK, wt, wtOK, staged, stagedOK)
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

// LabelForWorktree returns the label of the window whose worktree is worktree
// (symlink-normalized), or "" if none — used to exclude the CURRENT window from
// cross-window summaries (e.g. the Codex awareness hook, #codex).
func LabelForWorktree(ws []Window, worktree string) string {
	for _, w := range ws {
		if sameWorktree(w.Worktree, worktree) {
			return w.Label()
		}
	}
	return ""
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
	LiveClosedPR                 // the branch's PR was CLOSED unmerged — abandoned/superseded, branch kept on purpose (#79)
)

// StaleBaseBehindThreshold is how many commits behind origin/base a dirty
// base-branch checkout must be before wt flags it as likely-stale (#87). A
// shared `main` checkout drifted this far with dirty edits is very likely rot a
// reader wants to dismiss quickly + clean up. It is deliberately NOT suppressed:
// a dirty checkout can still hold real live edits, and hiding a dirty collision
// on the default `wt check` path would violate the core "never hide a real
// collision" invariant (the #87 adversarial review caught exactly this — the
// operator commits to base directly, and base churns past 20-behind within a day
// or two of genuine WIP). Instead the count enriches the LiveDirty label
// ("· N behind base — likely stale") so a reader dismisses it in one second, and
// the `wt doctor` probe (same threshold) names it so the root cause gets fixed.
// Well above "just haven't pulled today".
const StaleBaseBehindThreshold = 20

// IsStale reports whether this level is the definitively-merged/abandoned case.
func (l Liveness) IsStale() bool { return l == LiveStale }

// IsSuppressed reports whether a collision against a window at this level should
// be filtered out of the default output. Merged-and-clean (stale), parked past
// the dormancy threshold (dormant), or CLOSED-PR (#79 — the work already lost
// its adjudication) all mean the other window won't change the file underneath
// you right now. A DIRTY window is never suppressed — even a badly-drifted base
// checkout: its uncommitted edits could be real work, so it stays HIGH (just
// labelled "likely stale"). #87 surfaces the stale base checkout; it never hides
// it.
func (l Liveness) IsSuppressed() bool {
	return l == LiveStale || l == LiveDormant || l == LiveClosedPR
}

// Tag is a short human label for the level.
func (l Liveness) Tag() string {
	switch l {
	case LiveOpenPR:
		return "open PR"
	case LiveDirty:
		return "uncommitted edits"
	case LiveUnmerged:
		// #79 comment 3a: distinguish "never opened a PR" (possibly-live, HIGH)
		// from the merged/closed cases, which now read "merged #N" / "PR #N closed".
		return "commits, no PR opened"
	case LiveDormant:
		return "dormant"
	case LiveStale:
		return "stale: merged / no PR"
	case LiveClosedPR:
		return "PR closed"
	default:
		return "unknown"
	}
}

// WindowLiveness is a window's resolved liveness plus the PR number attached to
// the deciding level (open / merged / closed) for display, e.g. "[open PR #747]".
type WindowLiveness struct {
	Level      Liveness
	PR         string        // open PR number when Level == LiveOpenPR, else ""
	MergedPR   string        // merged PR number when the branch is shipped (Level LiveStale, #73)
	ClosedPR   string        // closed-unmerged PR number when Level == LiveClosedPR (#79)
	Dirty      bool          // the worktree also has uncommitted edits — noted on a suppressed level so a reopened-merged/closed branch is legible (#79 comment 2)
	BehindBase int           // commits behind origin/base; >0 only computed for a dirty base checkout, enriches the LiveDirty "likely stale" label (#87)
	Age        time.Duration // time since the branch's last commit (0 if unknown)
}

// Label is the plain (uncolored) liveness word, e.g. "open PR #747" or
// "merged #84", with a "· last commit Nd ago" suffix when known and a leftover-
// edits note when a shipped/closed branch still has a dirty index.
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
	// #79: a CLOSED-unmerged PR — name it so the row reads "PR #N closed", not
	// "in progress". The branch is kept on purpose; this isn't a judgement on it.
	if wl.Level == LiveClosedPR && wl.ClosedPR != "" {
		base = "PR #" + wl.ClosedPR + " closed"
	}
	if wl.Age > 0 && (wl.Level == LiveUnmerged || wl.Level == LiveDormant) {
		base += ", last commit " + HumanAge(wl.Age) + " ago"
	}
	// #79 comment 2: when a shipped/closed branch ALSO has a leftover dirty index
	// (the unresolvable false-HIGH case — `wt clean` won't remove the unshared
	// index, so `check` would flag it forever), say WHY it's still only an FYI.
	if wl.Dirty && (wl.Level == LiveStale || wl.Level == LiveClosedPR) {
		base += " · leftover uncommitted edits"
	}
	// #87: a dirty base checkout that's fallen behind origin/base stays HIGH (its
	// uncommitted edits could be live work — never hidden), but the drift is
	// noted so a reader can dismiss likely rot fast. Past the threshold, say so.
	if wl.Level == LiveDirty && wl.BehindBase > 0 {
		base += " · " + strconv.Itoa(wl.BehindBase) + " behind base"
		if wl.BehindBase >= StaleBaseBehindThreshold {
			base += " — likely stale"
		}
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
	PRChecked bool // whether PR status was actually resolvable (gh present+authed)
	Dirty     bool
	Merged    bool          // a MERGED PR exists for the branch — squash-merged/shipped (#73)
	ClosedPR  bool          // the branch's most-recent PR was CLOSED unmerged — abandoned/superseded (#79)
	Unshipped int           // git cherry "+" count; <0 means it could not be computed
	Age       time.Duration // time since last commit (0 = unknown)
}

// ClassifyFacts maps observations to a Liveness (pure). Precedence, by descending
// certainty that the OTHER window can still change a file underneath you:
//
//	open PR > merged PR > closed PR > dirty worktree > unmerged commits > merged-by-ancestry
//
// The load-bearing ordering change (#79 comment 2): a resolved PR state — merged
// (#73) OR closed-unmerged (#79) — now outranks a dirty worktree. A shipped or
// abandoned branch frequently carries a leftover staged index that `wt clean`
// refuses to delete (unshared), which previously read as LiveDirty ⇒ HIGH
// forever, an unresolvable false positive. PR state is the stronger signal:
// merged ⇒ the work already landed; closed ⇒ it already lost its adjudication.
// Neither can create NEW contention on base, so both suppress regardless of the
// index. Only when NO PR resolved does a dirty worktree mean live editing.
//
// maxAge (>0) enables dormancy on an unmerged, no-PR, clean branch idle past the
// threshold (LiveDormant, suppressed). Dormancy also requires PRChecked: if PR
// status couldn't be resolved (gh offline/unauthed) we must NOT downgrade an
// idle unmerged branch, because it might have an open PR we couldn't see.
func ClassifyFacts(f LiveFacts, maxAge time.Duration) Liveness {
	switch {
	case f.HasOpenPR:
		return LiveOpenPR
	case f.Merged:
		// MERGED ⇒ shipped. Suppress even with Unshipped>0 (squash-merge breaks
		// git-cherry patch-equivalence) and even with a leftover dirty index (#73,
		// #79 comment 2). The dirtiness is carried into the label, not the level.
		return LiveStale
	case f.ClosedPR:
		// CLOSED-unmerged ⇒ abandoned/superseded, branch kept on purpose. Same
		// suppression + same reasoning as merged; the row reads "PR #N closed" (#79).
		return LiveClosedPR
	case f.Dirty:
		// A dirty worktree is live editing — ALWAYS surfaced (HIGH), including a
		// far-behind base checkout (#87): its uncommitted edits could be real work,
		// so it is never hidden. The base-drift is carried into the label
		// ("· N behind base — likely stale"), and `wt doctor` names it — but it is
		// not suppressed. The #87 review proved suppressing it hides real WIP.
		return LiveDirty
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
	pr, mergedPR, closedPR := "", "", ""
	// One `gh pr list --head <branch> --state all` resolves OPEN / MERGED / CLOSED
	// in a single call (#79) — replacing the separate open- + merged-lookups (#73).
	// The most-recent PR's state decides: OPEN ⇒ contention, MERGED ⇒ shipped (#73),
	// CLOSED ⇒ abandoned/superseded (#79). Absent ⇒ fall through to git signals.
	if ghx.Present() && ghx.Authed() {
		f.PRChecked = true
		if n, state, ok := ghx.PRForBranch(w.Branch); ok {
			switch state {
			case "OPEN":
				f.HasOpenPR, pr = true, n
			case "MERGED":
				f.Merged, mergedPR = true, n
			case "CLOSED":
				f.ClosedPR, closedPR = true, n
			}
		}
	}
	f.Dirty = !gitx.IsClean(w.Worktree)
	if n, err := gitx.CountUnshipped(base, w.Branch); err == nil {
		f.Unshipped = n
	} else {
		f.Unshipped = -1
	}
	if age, err := gitx.LastCommitAge(w.Worktree, now); err == nil {
		f.Age = age
	}
	// #87: for the dirty base-branch checkout (only the primary can be on base —
	// git forbids the same branch in two worktrees), measure how far behind
	// origin/base it is. Computed ONLY for that one window (no extra git call in
	// the common case). It does NOT change the level (a dirty checkout stays
	// LiveDirty/HIGH — never hidden); it enriches the label so a reader can spot
	// likely rot, and `wt doctor` names it.
	behindBase := 0
	if w.Branch == base && f.Dirty {
		if ref := gitx.ResolveRemoteBase(base); ref != "" {
			if headSHA, err := gitx.RunDir(w.Worktree, "rev-parse", "HEAD"); err == nil {
				if n := gitx.BehindCount(strings.TrimSpace(headSHA), ref); n > 0 {
					behindBase = n
				}
			}
		}
	}
	wl := WindowLiveness{Level: ClassifyFacts(f, maxAge), Age: f.Age, Dirty: f.Dirty, BehindBase: behindBase}
	if wl.Level == LiveOpenPR {
		wl.PR = pr
	}
	if f.Merged {
		wl.MergedPR = mergedPR
	}
	if f.ClosedPR {
		wl.ClosedPR = closedPR
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
