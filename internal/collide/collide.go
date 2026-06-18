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

	"github.com/eharriett0/wt/internal/activework"
	"github.com/eharriett0/wt/internal/config"
	"github.com/eharriett0/wt/internal/gitx"
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
