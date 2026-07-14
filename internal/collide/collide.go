// Package collide is the core multi-window collision intelligence: with 3-4
// windows working on different things, it answers "is anyone else touching the
// files I'm touching?" — not just "is this issue claimed?".
//
// It works at the FILE level, derived from each worktree's git state, so it
// catches collisions even when windows are on different issues/branches AND
// even when a window never claimed a GitHub issue at all.
package collide

import (
	"path/filepath"
	"sort"
	"strings"
	"sync"

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

	var ws []Window
	for _, p := range paths {
		br, _ := gitx.CurrentBranchIn(p)
		w := Window{Worktree: p, Branch: br, Touched: gitx.TouchedFiles(p, c.Base)}
		if e, ok := byWorktree[p]; ok {
			w.Issue, w.Title = e.Issue, e.Title
		} else if e, ok := byBranch[br]; ok {
			w.Issue, w.Title = e.Issue, e.Title
		}
		ws = append(ws, w)
	}
	return ws, nil
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
	Path   string
	Window string // the other window's label
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
		touched := map[string]bool{}
		for _, f := range w.Touched {
			touched[f] = true
		}
		for _, p := range paths {
			p = strings.TrimSpace(p)
			if p == "" {
				continue
			}
			if matchesTouched(p, w.Touched, touched) {
				out = append(out, Conflict{Path: p, Window: w.Label()})
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

func matchesTouched(p string, touched []string, set map[string]bool) bool {
	if set[p] {
		return true
	}
	// suffix match so a basename or partial path still flags a real overlap.
	for _, f := range touched {
		if f == p || strings.HasSuffix(f, "/"+p) || filepath.Base(f) == p {
			return true
		}
	}
	return false
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
	LiveUnmerged                 // commits not yet on base, no open PR — latent collision
	LiveDirty                    // uncommitted changes in the worktree — actively editing
	LiveOpenPR                   // an open PR exists for the branch — active contention
)

// IsStale reports whether this level is the definitively-merged/abandoned case
// that should be suppressed from collision output.
func (l Liveness) IsStale() bool { return l == LiveStale }

// Tag is a short human label for the level.
func (l Liveness) Tag() string {
	switch l {
	case LiveOpenPR:
		return "open PR"
	case LiveDirty:
		return "uncommitted edits"
	case LiveUnmerged:
		return "commits, no PR"
	case LiveStale:
		return "stale: merged / no PR"
	default:
		return "unknown"
	}
}

// WindowLiveness is a window's resolved liveness plus the open-PR number when
// the level is LiveOpenPR (for display, e.g. "[open PR #747]").
type WindowLiveness struct {
	Level Liveness
	PR    string // open PR number when Level == LiveOpenPR, else ""
}

// Label is the plain (uncolored) liveness word, e.g. "open PR #747" or
// "stale: merged / no PR".
func (wl WindowLiveness) Label() string {
	if wl.Level == LiveOpenPR && wl.PR != "" {
		return "open PR #" + wl.PR
	}
	return wl.Level.Tag()
}

// Badge is the colored, bracketed liveness tag for output lines: red for an
// open PR, yellow for in-progress (dirty / unmerged), dim for stale/unknown.
func (wl WindowLiveness) Badge() string {
	tag := "[" + wl.Label() + "]"
	switch wl.Level {
	case LiveOpenPR:
		return ui.Red(tag)
	case LiveDirty, LiveUnmerged:
		return ui.Yellow(tag)
	default: // LiveStale, LiveUnknown
		return ui.Dim(tag)
	}
}

// LiveFacts are the raw observations ClassifyFacts turns into a Liveness. Split
// out from the I/O so the decision boundary is pure and unit-testable.
type LiveFacts struct {
	HasOpenPR bool
	Dirty     bool
	Unshipped int // git cherry "+" count; <0 means it could not be computed
}

// ClassifyFacts maps observations to a Liveness (pure). Precedence is by
// descending certainty-of-activity: open PR > dirty worktree > unmerged commits
// > (fully merged ⇒ stale). An uncomputable unshipped count with no other
// signal is LiveUnknown (surfaced, not suppressed).
func ClassifyFacts(f LiveFacts) Liveness {
	switch {
	case f.HasOpenPR:
		return LiveOpenPR
	case f.Dirty:
		return LiveDirty
	case f.Unshipped > 0:
		return LiveUnmerged
	case f.Unshipped == 0:
		return LiveStale
	default:
		return LiveUnknown
	}
}

// Classify resolves one window's liveness via I/O (gh + git). gh is consulted
// only when present+authed; otherwise classification degrades to git-only
// signals (a merged clean branch is still correctly LiveStale offline).
func Classify(w Window, base string) WindowLiveness {
	var f LiveFacts
	pr := ""
	if ghx.Present() && ghx.Authed() {
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
	wl := WindowLiveness{Level: ClassifyFacts(f)}
	if wl.Level == LiveOpenPR {
		wl.PR = pr
	}
	return wl
}

// ClassifyWindows resolves liveness, concurrently, for the windows whose label
// is in `labels` (the small set actually involved in a collision — NOT all
// windows, which would be one gh call each). Keyed by window label.
func ClassifyWindows(ws []Window, base string, labels map[string]bool) map[string]WindowLiveness {
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
	var mu sync.Mutex
	var wg sync.WaitGroup
	sem := make(chan struct{}, 8) // bound concurrent gh/git shell-outs
	for _, j := range jobs {
		wg.Add(1)
		go func(j job) {
			defer wg.Done()
			sem <- struct{}{}
			defer func() { <-sem }()
			wl := Classify(j.w, base)
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
		if wl, ok := live[c.Window]; ok && wl.Level.IsStale() {
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
			if wl, ok := live[w]; !ok || !wl.Level.IsStale() {
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
