// Package collide is the core multi-window collision intelligence: with 3-4
// windows working on different things, it answers "is anyone else touching the
// files I'm touching?" — not just "is this issue claimed?".
//
// It works at the FILE level, derived from each worktree's git state, so it
// catches collisions even when windows are on different issues/branches AND
// even when a window never claimed a GitHub issue at all.
package collide

import (
	"errors"
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
	// Unread is why git could not read the window's uncommitted changes, ""
	// when it could (#253). Touched then holds what was read, so the window
	// is named, never shown as one with no changes.
	Unread string
	// UnreadSome: git read the window but said it could not read some paths
	// (#254 review). CommitsUnread: its committed changes were not read
	// either. See NotCompared.
	UnreadSome, CommitsUnread bool
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
	refs, err := gitx.WorktreeList()
	if err != nil {
		return nil, err
	}
	paths := make([]string, len(refs))
	for i, r := range refs {
		paths[i] = r.Path
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
	// worktree. Indexed writes preserve `git worktree list` order (stable
	// output). A window whose uncommitted changes can't be read is marked
	// Unread (#253), except a bare repository (no work tree) and one git lists
	// as prunable whose directory is gone (not locked, nothing to read); other
	// per-worktree git errors stay swallowed, as before.
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
			touched, terr := gitx.TouchedFiles(p, c.Base)
			w := Window{Worktree: p, Branch: br, Touched: touched}
			switch {
			case refs[i].Bare || refs[i].Prunable && gone(p):
				// no work tree to read
			case i > 0 && !gone(p) && gone(filepath.Join(p, ".git")):
				// #255: a linked worktree whose directory is there without its
				// .git file (deleted, or an unmounted volume's empty mount
				// point; locked or not, round 7): whatever git status read
				// there (the work tree around it, which may itself be partly
				// unread) or failed on is not this window's. Named by its own
				// branch, nothing of it read.
				w.Branch, w.Touched = refs[i].Branch, nil
				w.Unread, w.UnreadSome, w.CommitsUnread = ErrNoGitLink, false, true
			case terr != nil:
				if errors.Is(terr, gitx.ErrNoDir) { // git never ran there: br is not its branch
					w.Branch = refs[i].Branch
				}
				markUnread(&w, terr)
			}
			// Claims by the window's own branch: for a #255 entry, br is the
			// enclosing work tree's (#254 review, round 6).
			if e, ok := byWorktree[p]; ok {
				w.Issue, w.Title = e.Issue, e.Title
			} else if e, ok := byBranch[w.Branch]; ok {
				w.Issue, w.Title = e.Issue, e.Title
			}
			ws[i] = w
		}(i, p)
	}
	wg.Wait()
	readOwnEntry(c, ws, byWorktree)
	return ws, nil
}

// ErrNoGitLink is the unread reason of a linked worktree whose directory is
// there without its .git file (#255): git reads the work tree around it, or
// none. git lists it as prunable unless it is locked.
const ErrNoGitLink = "its .git link is missing, so git can't read it as its own work tree"

// markUnread records TouchedFiles' error on w (#253): the reason, and what
// of the window was not read (#254 review).
func markUnread(w *Window, err error) {
	w.Unread = err.Error()
	var ue *gitx.UnreadError
	if errors.As(err, &ue) {
		w.UnreadSome, w.CommitsUnread = ue.Partial, !ue.CommitsRead
	}
}

// gone reports whether path no longer exists (a symlink is followed). git
// lists a worktree as prunable when it can't reach the worktree's .git file,
// which a directory it may not enter causes too (measured, git 2.39): only
// one whose directory is gone has nothing to read (#254 review).
func gone(path string) bool {
	_, err := os.Stat(path)
	return errors.Is(err, os.ErrNotExist)
}

// readOwnEntry reads, by its work tree, the window Scan runs in when git
// lists it by its git directory instead (#254 review): the main work tree of
// a `--separate-git-dir` repository (an entry TouchedFiles can't read,
// gitx.ErrGitDirOnly) and the checkout of a submodule with linked worktrees
// (an entry git reads through core.worktree). Left as listed, the window is
// named unread to itself, or its own edits grade as another window's. Asked
// only when no window is listed at this work tree's root (c.Root, which
// config.Load read). From another window the entry stays as listed: git
// does not say where that work tree is.
func readOwnEntry(c *config.Config, ws []Window, byWorktree map[string]activework.Entry) {
	root := c.Root
	if root == "" {
		r, err := gitx.RepoRoot()
		if err != nil {
			return
		}
		root = r
	}
	for _, w := range ws {
		if sameWorktree(w.Worktree, root) {
			return
		}
	}
	gitDir, err := gitx.GitDir()
	if err != nil {
		return
	}
	for i := range ws {
		if !sameWorktree(ws[i].Worktree, gitDir) {
			continue
		}
		touched, terr := gitx.TouchedFiles(root, c.Base)
		ws[i].Worktree, ws[i].Touched = root, touched
		ws[i].Unread, ws[i].UnreadSome, ws[i].CommitsUnread = "", false, false
		if terr != nil {
			markUnread(&ws[i], terr)
		}
		if e, ok := byWorktree[root]; ok {
			ws[i].Issue, ws[i].Title = e.Issue, e.Title
		}
		return
	}
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
	// Worktrees are the same windows' worktree paths, index-aligned with Windows
	// (#182). A label is not an identity: two worktrees that claimed one issue
	// are both "#N", and detached worktrees are named by their directory. Anything
	// that must tell two windows apart, such as grading a pair, keys on these.
	// nil on an Overlap built by hand; readers then fall back to the label.
	Worktrees []string
}

// Overlaps returns files touched by ≥2 windows (pure; sorted by file, then by
// label and worktree). This is the headline collision signal across all windows.
func Overlaps(ws []Window) []Overlap {
	type member struct{ label, worktree string }
	hits := map[string][]member{}
	for _, w := range ws {
		label := w.Label()
		seen := map[string]bool{}
		for _, f := range w.Touched {
			if seen[f] {
				continue
			}
			seen[f] = true
			hits[f] = append(hits[f], member{label, w.Worktree})
		}
	}
	var out []Overlap
	for f, ms := range hits {
		if len(ms) < 2 {
			continue
		}
		sort.Slice(ms, func(i, j int) bool {
			if ms[i].label != ms[j].label {
				return ms[i].label < ms[j].label
			}
			return ms[i].worktree < ms[j].worktree
		})
		o := Overlap{File: f}
		for _, m := range ms {
			o.Windows = append(o.Windows, m.label)
			o.Worktrees = append(o.Worktrees, m.worktree)
		}
		out = append(out, o)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].File < out[j].File })
	return out
}

// Conflict is a requested path that another window is already touching.
type Conflict struct {
	Path        string
	Window      string // the other window's label
	MatchedFile string // the other window's actual repo-relative touched file that
	// matched Path (may differ from Path when Path was a fuzzy basename/suffix
	// query) — used for hunk-range lookup so basename checks on nested files
	// still grade.
	// Worktree is the other window's worktree path, as Scan recorded it (#193).
	// A label is not an identity: two worktrees that claimed one issue are both
	// "#N", and two detached ones whose directories share a name are both that
	// name. Everything that reads the other window's files (ranges, blobs,
	// untracked, subsumed) reads THIS worktree; resolving the label instead picked
	// whichever namesake came last, and graded a real overlap against its
	// disjoint hunks. "" only on a Conflict built by hand (WorktreeOf).
	Worktree string
}

// WorktreeOf is the worktree conflict cf is with: the one CheckPaths recorded,
// or, for a Conflict built by hand without one, the worktree of the window its
// label names in ws (#193). Labels collide only between real windows, and a
// CheckPaths conflict always carries its worktree. Pure.
func WorktreeOf(cf Conflict, ws []Window) string {
	if cf.Worktree != "" {
		return cf.Worktree
	}
	for _, w := range ws {
		if w.Label() == cf.Window {
			return w.Worktree
		}
	}
	return ""
}

// MatchMode is how a requested path is compared with a window's touched files.
//
// ⚠ #181: every path used to be matched by suffix and basename too, so a file
// at the REPO ROOT collided with any same-named file in a subdirectory —
// "README.md" is a suffix of "pkg/svc/README.md". `wt check README.md` reported
// HIGH and the pre-push guard blocked the push for a file the other branch never
// touched, which teaches bypassing the one check that prevents duplicate PRs.
// The fuzzy tiers are a convenience for a name a human types without knowing
// where it lives. A path that names a real location is a question about THAT
// location, and is compared as one.
type MatchMode int

const (
	// MatchExact compares repo-relative paths: equality for a file, a
	// segment-anchored prefix for a directory. It is the zero value on purpose:
	// a query whose mode was never decided can't invent a collision. Every path
	// that comes from git or a hook payload is exact (pre-push outgoing files,
	// pre-commit staged files, Claude/Codex edit targets), and so is a `wt check`
	// argument that names a path in the repo (QueryFor).
	MatchExact MatchMode = iota
	// MatchFuzzy is for a `wt check` argument that names NO path in the repo: a
	// search term. It adds a suffix/basename tier (`foo.go` reaches
	// internal/foo.go) and a directory-suffix tier (`kiali` reaches
	// envs/.../configs/kiali/), both on a segment boundary.
	MatchFuzzy
)

// Query is one requested path and how to match it.
type Query struct {
	Path string
	Mode MatchMode
}

// cmpPath is q's path as it is compared with a window's touched files. A search
// term is trimmed, as typed input always was. An exact query names a real path
// (from git, a hook payload, or an argument naming one), and a real name may
// begin or end with a space: git reports it verbatim (#200), so trimmed, it
// matched no window. Pure.
func (q Query) cmpPath() string {
	if q.Mode == MatchFuzzy {
		return strings.TrimSpace(q.Path)
	}
	return q.Path
}

// RootPath is the Path of the query for the whole repository: `wt check .` at
// a worktree's top level (#201). git never names a file ".", so it matches no
// touched file by equality; as a directory (matchTouchedDir) it holds every
// touched file. Only an EXACT "." is the root: the zero Query has Path "", and
// still matches nothing.
const RootPath = "."

// RootQuery is the query for the whole repository (RootPath). Pure.
func RootQuery() Query { return Query{Path: RootPath, Mode: MatchExact} }

// ExactQueries wraps repo-relative paths that came from git or a hook payload,
// which name real locations and are never search terms (#181). Pure.
func ExactQueries(paths []string) []Query {
	qs := make([]Query, 0, len(paths))
	for _, p := range paths {
		qs = append(qs, Query{Path: p, Mode: MatchExact})
	}
	return qs
}

// QueryFor decides how a `wt check` argument is matched (#181). arg is the
// argument as typed. rel is the repo-relative path it names when read relative
// to the current directory, RootPath for the top level itself (the whole
// repository, #201), or "" when it names no location inside the repo. onDisk
// reports that it exists in this working tree or is tracked by git.
//
// An argument that names a real location is compared EXACTLY at its
// repo-relative path: present here, or touched at exactly that path by some
// window (a file or directory that exists only on another branch is still a
// real place). Anything else is a search term, matched fuzzily as typed, which
// keeps `wt check foo.go` reaching internal/foo.go.
//
// The root README.md exists, so `wt check README.md` at the root asks about
// THAT file; matching it fuzzily is what made it collide with
// pkg/svc/README.md. From pkg/svc/ the same argument names pkg/svc/README.md,
// and is no longer matched against the root README.md either: the mirror image
// of the same bug. Pure.
func QueryFor(arg, rel string, onDisk bool, ws []Window) Query {
	arg = strings.TrimSpace(arg)
	if rel == RootPath {
		return RootQuery() // the top level always exists: a question about every file
	}
	if rel != "" {
		exact := Query{Path: rel, Mode: MatchExact}
		if onDisk || PathTouchedByAny(exact, ws) {
			return exact
		}
	}
	return Query{Path: arg, Mode: MatchFuzzy}
}

// CheckPaths reports, for each query, which OTHER windows (not
// currentWorktree) are already touching it. Pure. A query matches a window's
// touched file by repo-relative equality, and — for a DIRECTORY — by expanding
// to every touched file beneath it (#154). A MatchFuzzy query also matches by
// suffix and basename (#181: only a query that names no real path is fuzzy).
//
// A directory expands to one Conflict PER FILE rather than one for the
// directory, so each file keeps its own hunk grading downstream. Those entries
// carry the matched file in Path as well, because a report that printed the
// requested directory three times would not tell you which files to look at.
// Entries are deduped by (window, file): passing both a directory and a file
// under it is a natural thing to do and must not double-report. Exact queries
// claim a file before fuzzy ones (exactFirst), so when both reach it the entry
// carries the path that names it, whatever order the arguments came in.
func CheckPaths(ws []Window, currentWorktree string, qs []Query) []Conflict {
	qs = exactFirst(qs)
	var out []Conflict
	for _, w := range ws {
		if sameWorktree(w.Worktree, currentWorktree) {
			continue
		}
		seen := map[string]struct{}{}
		for _, q := range qs {
			q.Path = q.cmpPath()
			if strings.TrimSpace(q.Path) == "" {
				continue
			}
			if matched, ok := matchTouched(q, w.Touched); ok {
				if _, dup := seen[matched]; !dup {
					seen[matched] = struct{}{}
					out = append(out, Conflict{Path: q.Path, Window: w.Label(), MatchedFile: matched, Worktree: w.Worktree})
				}
				continue
			}
			// #154: no file matched, so try the path as a directory. `wt check
			// <dir>` used to match NOTHING and print "clear — no other window is
			// touching <dir>", silently and regardless of commit state: a
			// positive claim about a whole subtree made after matching nothing
			// in it. A directory is the cheapest way to ask the question, so it
			// is what gets used when someone is being careful.
			for _, f := range matchTouchedDir(q, w.Touched) {
				if _, dup := seen[f]; dup {
					continue
				}
				seen[f] = struct{}{}
				out = append(out, Conflict{Path: f, Window: w.Label(), MatchedFile: f, Worktree: w.Worktree})
			}
		}
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i].Path != out[j].Path {
			return out[i].Path < out[j].Path
		}
		if out[i].Window != out[j].Window {
			return out[i].Window < out[j].Window
		}
		return out[i].Worktree < out[j].Worktree // namesakes (#193)
	})
	return out
}

// exactFirst returns qs with every MatchExact query ahead of every MatchFuzzy
// one, each group in its original order (#181 review). CheckPaths keeps the
// first query to reach a file, and its Path is what the entry reports and what
// the shared-doc / append-only globs match: `wt check NEW.md pkg/svc/NEW.md`
// used to report the fuzzy "NEW.md" for pkg/svc/NEW.md, and the reverse order
// the real path. Pure.
func exactFirst(qs []Query) []Query {
	out := make([]Query, 0, len(qs))
	for _, q := range qs {
		if q.Mode != MatchFuzzy {
			out = append(out, q)
		}
	}
	for _, q := range qs {
		if q.Mode == MatchFuzzy {
			out = append(out, q)
		}
	}
	return out
}

// PathTouchedByAny reports whether query q matches ANY window's touched set,
// under q's own mode — including the current window. Used by `wt check` to tell
// a real path (a live collision target, or a path that exists only on another
// window's branch) from a typo, so it can refuse to report 'clear' for a
// nonexistent path (#93) without false-refusing a path another window is
// genuinely editing; and by QueryFor, to recognize such a path as real.
func PathTouchedByAny(q Query, ws []Window) bool {
	q.Path = q.cmpPath()
	if strings.TrimSpace(q.Path) == "" {
		return false
	}
	for _, w := range ws {
		if _, ok := matchTouched(q, w.Touched); ok {
			return true
		}
		// #154: a DIRECTORY that exists only on another window's branch is a real
		// path, not a typo. Widening here can only remove a false "no such path",
		// which is the safe direction for this guard.
		if len(matchTouchedDir(q, w.Touched)) > 0 {
			return true
		}
	}
	return false
}

// TouchedSpellings lists the spellings under which some window touches rel
// when names are compared without case (#201): a touched file equal to rel but
// for case, or the directory spelling of rel above a touched file. Sorted,
// distinct, rel itself left out. Only for a repository whose names ARE
// case-insensitive (the filesystem folds case, or core.ignorecase): there
// `readme.md` and another window's `README.md` are one file, and an argument
// that names nothing here under its own spelling would otherwise be a search
// that misses it. Never used to widen an exact match where case matters.
// Compared segment by segment, so `pkg` never reaches `pkg2/x`. Pure.
func TouchedSpellings(rel string, ws []Window) []string {
	rel = strings.Trim(rel, "/")
	if rel == "" || rel == RootPath {
		return nil
	}
	want := strings.Split(rel, "/")
	seen := map[string]bool{}
	var out []string
	for _, w := range ws {
		for _, f := range w.Touched {
			segs := strings.Split(f, "/")
			if len(segs) < len(want) {
				continue
			}
			match := true
			for i := range want {
				if !strings.EqualFold(segs[i], want[i]) {
					match = false
					break
				}
			}
			if !match {
				continue
			}
			if s := strings.Join(segs[:len(want)], "/"); s != rel && !seen[s] {
				seen[s] = true
				out = append(out, s)
			}
		}
	}
	sort.Strings(out)
	return out
}

// matchTouched reports whether query q matches any touched file — by exact
// repo-relative path, and for a MatchFuzzy query also by path suffix or
// basename — and returns the actual matched touched file (repo-relative),
// preferring an exact match.
//
// ⚠ The suffix/basename tier is fuzzy-only (#181). For a repo-relative path it
// is never a better answer than equality, only a wrong one: a root file has no
// "/" to anchor on, so "README.md" matched "pkg/svc/README.md".
func matchTouched(q Query, touched []string) (string, bool) {
	for _, f := range touched {
		if f == q.Path {
			return f, true
		}
	}
	if q.Mode != MatchFuzzy {
		return "", false
	}
	for _, f := range touched {
		if strings.HasSuffix(f, "/"+q.Path) || filepath.Base(f) == q.Path {
			return f, true
		}
	}
	return "", false
}

// matchTouchedDir treats q's path as a DIRECTORY and returns every touched file
// beneath it, sorted (#154). It mirrors matchTouched's two tiers one level up:
// the path as a repo-relative directory prefix, and — for a MatchFuzzy query
// only (#181) — as a directory SUFFIX, so `wt check kiali` reaches
// `envs/.../configs/kiali/x.yaml` the same way `wt check foo.go` reaches
// `internal/foo.go`. An exact `docs` is the root docs/ directory, and must not
// reach `site/docs/x.md`.
//
// ⚠ Both tiers match on a SEGMENT BOUNDARY — the "/" in p+"/" is load-bearing.
// A bare prefix test would make `envs/app` match `envs/application/x.yaml`, i.e.
// invent a collision in a sibling directory, and a false HIGH on a safety tool
// is how people learn to pass --bypass.
//
// A trailing slash is accepted and stripped: it is how anyone types a directory,
// and `wt check envs/app/` reporting differently from `wt check envs/app` would
// be its own small trap.
//
// An exact RootPath is the whole repository (#201): every touched file is
// beneath it. `wt check .` at the top level matched nothing and said "clear"
// while another window had live HIGHs, though `.` in a subdirectory (that
// subdirectory, #181) and any other directory (#154) already expanded. A blank
// path still matches nothing: the zero Query must not be the whole repo.
func matchTouchedDir(q Query, touched []string) []string {
	p := strings.Trim(q.cmpPath(), "/")
	if q.Mode == MatchExact && p == RootPath {
		out := append([]string(nil), touched...)
		sort.Strings(out)
		return out
	}
	if strings.TrimSpace(p) == "" {
		return nil // a blank path names nothing (the whole repo is RootPath)
	}
	fuzzy := q.Mode == MatchFuzzy
	var out []string
	for _, f := range touched {
		if strings.HasPrefix(f, p+"/") || (fuzzy && strings.Contains(f, "/"+p+"/")) {
			out = append(out, f)
		}
	}
	sort.Strings(out)
	return out
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

// NoBaseNote is what `wt check` and pre-push say when no base branch
// resolves (#247): committed changes have no frame to compare in, and
// TouchedFiles doesn't list them, so a file another window only committed
// reads clear.
func NoBaseNote(base string) string {
	return fmt.Sprintf("no base branch resolves here (neither origin/%s nor %s): committed changes are not compared, so pushed commits, and a file another window only committed, read clear (`wt check` still compares uncommitted edits). Set `base` in .wt.conf to the branch windows start from.", base, base)
}

// UnreadWindows returns the windows of ws git could not read (Unread, #253),
// in ws's order, leaving out the one whose worktree is self ("" leaves out
// none): a window's own check says nothing about itself.
func UnreadWindows(ws []Window, self string) []Window {
	var out []Window
	for _, w := range ws {
		if w.Unread != "" && (self == "" || !sameWorktree(w.Worktree, self)) {
			out = append(out, w)
		}
	}
	return out
}

// UnreadNote is what every collision report says about windows git could not
// read (#253): who, git's reason, and that their uncommitted edits are not
// compared, so a clear is only about the windows that were read. "" for none.
// It never blocks: an unread window can't be tied to any path.
func UnreadNote(unread []Window) string {
	switch len(unread) {
	case 0:
		return ""
	case 1:
		w := unread[0]
		note := fmt.Sprintf("window %s could not be read (%s): %s.", w.Label(), w.Unread, UnreadScope(w))
		if strings.HasPrefix(w.Unread, "git status: ") { // git's own failure, not gitx.ErrGitDirOnly
			note += fmt.Sprintf(" Run `git status` in %s.", w.Worktree)
		}
		return note
	}
	parts := make([]string, len(unread))
	for i, w := range unread {
		parts[i] = fmt.Sprintf("%s at %s (%s)", w.Label(), w.Worktree, w.Unread)
	}
	return fmt.Sprintf("%d windows could not be read: %s. Edits wt could not read there are not compared.", len(unread), strings.Join(parts, "; "))
}

// NotCompared is what wt did not compare of an unread window (#253), as the
// JSON says it: "some_uncommitted" (git read it but named paths it could
// not), "all" (its commits were not read either), else "uncommitted"; "" for
// a window that was read (#254 review).
func NotCompared(w Window) string {
	switch {
	case w.Unread == "":
		return ""
	case w.UnreadSome:
		return "some_uncommitted"
	case w.CommitsUnread:
		return "all"
	}
	return "uncommitted"
}

// UnreadScope is NotCompared in words, for the note and wt status.
func UnreadScope(w Window) string {
	switch NotCompared(w) {
	case "some_uncommitted":
		return "some of its uncommitted edits are not compared"
	case "all":
		return "none of its edits are compared, committed or not"
	}
	return "its uncommitted edits are not compared"
}

// RangeFn resolves the line ranges a worktree changed in a path, relative to
// base. Injected so section/hunk grading is unit-testable without a live repo;
// production callers pass gitx.ChangedRanges.
type RangeFn func(worktree, base, path string) []gitx.LineRange

// DocEditsFn resolves what a worktree changed in a structured doc, both sides
// of its diff (#237); production callers pass gitx.ChangedDocEdits.
type DocEditsFn func(worktree, base, path string) gitx.DocEdits

// NewOnly is a DocEditsFn that knows only a RangeFn's new-frame ranges (no
// deletions): for tests that grade sections with stub ranges.
func NewOnly(ranges RangeFn) DocEditsFn {
	return func(wt, base, path string) gitx.DocEdits { return gitx.DocEdits{New: ranges(wt, base, path)} }
}

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
//
// ⚠ A window's sections are the ones its edits touch in its OWN copy (new
// frame, #123) AND the ones holding the lines it DELETED, read in the copy
// it diffed from (#237: gitx.DocEdits). A deleted line has no line in the
// window's copy, so deleting another window's section, or emptying or
// deleting the file, read disjoint while git conflicts. A window whose doc
// is gone counts its deletions too. A window whose measurement failed
// (DocEdits.All) edits every section, never none. A window BEHIND base also
// counts the sections of base's copy its edits land in (#247: DocEdits.Base),
// since its own copies carry the headings it forked with.
func SharedSectionsAcross(base string, worktrees []string, rel, delimiter string, edits DocEditsFn) (shared []string, graded bool) {
	return SharedSectionsAcrossWith(base, worktrees, rel, delimiter, edits, nil)
}

// PendingDoc is one worktree's copy of a structured doc as a pending edit will
// leave it, with what that worktree changed: Edits.New in the copy's own
// (NEW-frame) line numbers (#226), Edits.Deleted in the copy it is measured
// from (#237).
type PendingDoc struct {
	Content string
	Edits   gitx.DocEdits
}

// oldClaims are the lines of the old copy (secs, parsed from e.Old) whose
// sections a window's edit claims there (#237 and its review):
//   - every line it deleted or replaced;
//   - for a deleted or replaced HEADING line, the line above it too: git
//     conflicts the heading's change with an edit of the line before it,
//     the previous section's last line (a rename of "## Beta" against an
//     edit of the last Alpha line);
//   - for an insertion, the line it lands after (the first line, inserted
//     at the top): git conflicts it with a change of that line or another
//     insertion in that gap, and a new section's own lines have no section
//     in the old copy, so deleting the section it was added to, or emptying
//     the doc, read "disjoint". Not the line after the gap, on purpose:
//     appending a section at the end of your lane would claim the next lane
//     (an edit of the next heading line itself is caught from its side, by
//     the rule above).
//
// Pure.
func oldClaims(secs []section.Section, e gitx.DocEdits) []gitx.LineRange {
	heading := map[int]bool{}
	for _, s := range secs {
		if s.Heading != "" {
			heading[s.Start] = true
		}
	}
	out := append([]gitx.LineRange(nil), e.Deleted...)
	for _, d := range e.Deleted {
		if heading[d.Start] && d.Start > 1 {
			out = append(out, gitx.LineRange{Start: d.Start - 1, End: d.Start - 1})
		}
	}
	for _, g := range e.Gaps {
		k := max(g.Start, 1)
		out = append(out, gitx.LineRange{Start: k, End: k})
	}
	return out
}

// baseClaims are the lines of base's copy (secs, parsed from e.BaseCopy)
// whose sections a behind window's edits claim there (#247), by oldClaims'
// rules: a change claims its lines, and the line above when its first line
// is a heading; an insertion (a Gap) the line it lands after (the first
// line, at the top). Pure.
func baseClaims(secs []section.Section, e gitx.DocEdits) []gitx.LineRange {
	heading := map[int]bool{}
	for _, s := range secs {
		if s.Heading != "" {
			heading[s.Start] = true
		}
	}
	var out []gitx.LineRange
	for _, r := range e.Base {
		if r.Gap {
			k := max(r.Start, 1)
			out = append(out, gitx.LineRange{Start: k, End: k})
			continue
		}
		out = append(out, gitx.LineRange{Start: r.Start, End: r.End})
		if heading[r.Start] && r.Start > 1 {
			out = append(out, gitx.LineRange{Start: r.Start - 1, End: r.Start - 1})
		}
	}
	return out
}

// SharedSectionsAcrossWith is SharedSectionsAcross with some worktrees' copies
// given instead of read from disk (pending, keyed by worktree): the edit hooks
// grade a structured doc's sections as they will read once the edit is made,
// through the very computation `wt check` runs then (#226). Pure given ranges.
func SharedSectionsAcrossWith(base string, worktrees []string, rel, delimiter string, edits DocEditsFn, pending map[string]PendingDoc) (shared []string, graded bool) {
	re, err := section.Compile(delimiter)
	if err != nil {
		return nil, false
	}
	count := map[string]int{}
	var order []string
	seen := map[string]bool{}
	note := func(h string) {
		if !seen[h] {
			seen[h] = true
			order = append(order, h)
		}
	}
	all := 0 // windows that edit every section (a failed measurement)
	any := false
	for _, wt := range worktrees {
		var content string
		var e gitx.DocEdits
		if doc, ok := pending[wt]; ok {
			content, e = doc.Content, doc.Edits
		} else {
			b, rerr := os.ReadFile(filepath.Join(wt, rel))
			if rerr != nil && !os.IsNotExist(rerr) {
				continue // unreadable here; others may still grade
			}
			e = edits(wt, base, rel)
			if rerr != nil && len(e.Deleted) == 0 && !e.All {
				continue // this worktree lacks the doc and deleted nothing of it
			}
			content = string(b)
		}
		any = true
		heads := section.EditedHeadings(section.Parse(content, re), e.New)
		if len(e.Deleted) > 0 || len(e.Gaps) > 0 {
			old := section.Parse(e.Old, re)
			heads = append(heads, section.EditedHeadings(old, oldClaims(old, e))...)
		}
		if len(e.Base) > 0 {
			bs := section.Parse(e.BaseCopy, re)
			heads = append(heads, section.EditedHeadings(bs, baseClaims(bs, e))...)
		}
		if e.All {
			all++
			for _, s := range append(section.Parse(content, re), section.Parse(e.Old, re)...) {
				note(s.Heading)
			}
			continue // counted for every heading by all, never twice
		}
		mine := map[string]bool{}
		for _, h := range heads {
			note(h)
			if !mine[h] {
				mine[h] = true
				count[h]++
			}
		}
	}
	if !any {
		return nil, false
	}
	for _, h := range order {
		if count[h]+all >= 2 {
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

// ConflictSpans returns where two windows' edits would be ONE conflict region in
// a git 3-way merge (gitx.LineRange.Conflicts: they overlap, or touch with no
// unchanged line between, an insertion meeting only a change of a line next to
// its gap, #199), one span per conflicting pair: the lines both cover, or for two
// edits that only touch, the line on each side of the seam (A edits L10, B edits
// L11 → L10-11). Empty ⇒ git merges the two cleanly. O(n·m); the n,m here are
// per-file hunk counts, tiny.
func ConflictSpans(a, b []gitx.LineRange) []gitx.LineRange {
	var hits []gitx.LineRange
	for _, ra := range a {
		for _, rb := range b {
			if !ra.Conflicts(rb) {
				continue
			}
			lo, hi := max(ra.Start, rb.Start), min(ra.End, rb.End)
			if lo > hi { // they only touch: the seam's two lines
				lo, hi = hi, lo
			}
			hits = append(hits, gitx.LineRange{Start: lo, End: hi})
		}
	}
	return hits
}

// ConflictSeverity grades a check/pre-commit collision on a file between the
// CURRENT worktree's changed ranges and one OTHER window's ranges (pure):
//   - append-only path              → SevFYI
//   - either side has no ranges yet → SevHigh (indeterminate — can't prove
//     disjoint, so don't silently clear; preserves the pre-edit "heads up")
//   - git would merge every pair cleanly (an unchanged line between each)
//     → SevFYI (the crying-wolf case #7 targets)
//   - some pair is one conflict region (ConflictSpans: overlapping, or touching
//     with no unchanged line between, #199) → SevHigh
//
// The one grade `wt check`, the git hooks and the pre-edit hooks all apply
// (#92/#108), so changing it changes every one of them.
func ConflictSeverity(current, other []gitx.LineRange, appendOnly bool) Severity {
	if appendOnly {
		return SevFYI
	}
	if len(current) == 0 || len(other) == 0 {
		return SevHigh
	}
	if len(ConflictSpans(current, other)) > 0 {
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

// Self is the window a per-window view is taken from (the per-turn banner,
// #182): its label, which is what the reader calls it, and its worktree, which
// is what it is. Two windows can share a label, so whenever an Overlap carries
// worktrees, membership is decided by the worktree. The zero Self is "no side":
// the window-neutral view.
type Self struct{ Label, Worktree string }

// SelfFor identifies the window whose worktree is worktree (symlink-normalized),
// with the worktree path as Scan recorded it, so it compares equal to
// Overlap.Worktrees. Zero Self when no window matches.
func SelfFor(ws []Window, worktree string) Self {
	for _, w := range ws {
		if sameWorktree(w.Worktree, worktree) {
			return Self{Label: w.Label(), Worktree: w.Worktree}
		}
	}
	return Self{}
}

// IsZero reports whether s is "no side" (the window-neutral view).
func (s Self) IsZero() bool { return s == Self{} }

// Is reports whether o's i-th window is s: by worktree when o carries
// worktrees, else by label. Pure.
func (s Self) Is(o Overlap, i int) bool {
	if s.IsZero() {
		return false
	}
	if len(o.Worktrees) == len(o.Windows) && s.Worktree != "" {
		return o.Worktrees[i] == s.Worktree
	}
	return o.Windows[i] == s.Label
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
	// Unread: git could not read the window's working tree (#253). IsClean
	// failing counts it Dirty, so it is never hidden; the label says what is
	// actually known (#254 review).
	Unread bool
}

// Label is the plain (uncolored) liveness word, e.g. "open PR #747" or
// "merged #84", with a "· last commit Nd ago" suffix when known and a leftover-
// edits note when a shipped/closed branch still has a dirty index.
func (wl WindowLiveness) Label() string {
	base := wl.Level.Tag()
	if wl.Unread && wl.Level == LiveDirty { // #253: dirty only because it can't be read
		base = "working tree unread"
	}
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
		base += ", last commit " + agoPhrase(wl.Age)
	}
	// #79 comment 2: when a shipped/closed branch ALSO has a leftover dirty index
	// (the unresolvable false-HIGH case — `wt clean` won't remove the unshared
	// index, so `check` would flag it forever), say WHY it's still only an FYI.
	if wl.Dirty && !wl.Unread && (wl.Level == LiveStale || wl.Level == LiveClosedPR) {
		base += " · leftover uncommitted edits"
	}
	if wl.Unread && wl.Level != LiveDirty {
		base += " · working tree unread"
	}
	// #168: a dirty worktree whose commits merged under another branch name stays
	// HIGH (ClassifyFacts' tip exception). Name the PR so the reader can see the
	// edits are the only live part, and commit or discard them to clear it.
	if wl.Level == LiveDirty && wl.MergedPR != "" {
		base += " · its commits merged in #" + wl.MergedPR
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

// agoPhrase is HumanAge as a time in the past: "3d ago", and "just now"
// alone, never "just now ago" (#202). Pure.
func agoPhrase(d time.Duration) string {
	if a := HumanAge(d); a != "just now" {
		return a + " ago"
	}
	return "just now"
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
	ViaTip    bool          // that MERGED PR was found by the branch's tip commit, not its name (#168)
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
// #168 exception: a MERGED PR found only by the branch's TIP commit (ViaTip) does
// NOT outrank a dirty worktree. Found by name, the branch IS the merged PR's head,
// so a dirty index is leftover cruft. Found by tip, the branch only points into a
// merged PR, which is equally true of a follow-up branch started at that PR's
// head, whose uncommitted edits are new work. wt cannot tell the two apart, so the
// edits stay visible (HIGH, as before #168); a clean tree still suppresses.
//
// maxAge (>0) enables dormancy on an unmerged, no-PR, clean branch idle past the
// threshold (LiveDormant, suppressed). Dormancy also requires PRChecked: if PR
// status couldn't be resolved (gh offline/unauthed) we must NOT downgrade an
// idle unmerged branch, because it might have an open PR we couldn't see.
func ClassifyFacts(f LiveFacts, maxAge time.Duration) Liveness {
	switch {
	case f.HasOpenPR:
		return LiveOpenPR
	case f.Merged && !(f.ViaTip && f.Dirty):
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
	// #168: with no PR under the branch's own name, the branch's tip commit is
	// looked up, so work pushed under another name reads as its real PR (OPEN
	// stays contention; MERGED only when the tip is in that PR's final commits).
	if ghx.Present() && ghx.Authed() {
		f.PRChecked = true
		if n, state, viaTip, ok := ghx.PRForBranchOrTip(w.Branch, gitx.ResolveRemoteBase(base)); ok {
			switch state {
			case "OPEN":
				f.HasOpenPR, pr = true, n
			case "MERGED":
				f.Merged, f.ViaTip, mergedPR = true, viaTip, n
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
	wl := WindowLiveness{Level: ClassifyFacts(f, maxAge), Age: f.Age, Dirty: f.Dirty, BehindBase: behindBase, Unread: w.Unread != ""}
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
//
// A label is not unique (#182): two worktrees that claimed one issue are both
// "#N". Every window carrying a requested label is classified, and the label
// keeps the answer that suppresses LEAST (keepLeastSuppressed). The map can't
// say which of them a collision is with, so suppressing on the strength of the
// other one would hide a live window; this used to keep whichever came first.
func ClassifyWindows(ws []Window, base string, labels map[string]bool, maxAge time.Duration) map[string]WindowLiveness {
	type job struct {
		label string
		w     Window
	}
	var jobs []job
	for _, w := range ws {
		if l := w.Label(); labels[l] {
			jobs = append(jobs, job{l, w})
		}
	}

	res := make([]WindowLiveness, len(jobs))
	now := time.Now()
	var wg sync.WaitGroup
	sem := make(chan struct{}, 8) // bound concurrent gh/git shell-outs
	for i, j := range jobs {
		wg.Add(1)
		go func(i int, j job) {
			defer wg.Done()
			sem <- struct{}{}
			defer func() { <-sem }()
			res[i] = Classify(j.w, base, maxAge, now)
		}(i, j)
	}
	wg.Wait()
	out := make(map[string]WindowLiveness, len(jobs))
	for i, j := range jobs { // worktree order, so the answer doesn't depend on timing
		held, ok := out[j.label]
		out[j.label] = keepLeastSuppressed(held, ok, res[i])
	}
	return out
}

// keepLeastSuppressed folds one more classification of a label into the one held
// so far (ok=false: none yet). The held answer stands unless it is suppressed and
// the new one is not: never suppress on ambiguity (#182). Pure.
func keepLeastSuppressed(held WindowLiveness, ok bool, next WindowLiveness) WindowLiveness {
	if !ok || (held.Level.IsSuppressed() && !next.Level.IsSuppressed()) {
		return next
	}
	return held
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

// isLiveWindow reports whether a window's collisions should be surfaced: its
// level is not suppressed (merged / dormant / closed PR). Missing liveness counts
// as live (never suppress on ambiguity), and a dirty window is never suppressed.
func isLiveWindow(live map[string]WindowLiveness, label string) bool {
	wl, ok := live[label]
	return !ok || !wl.Level.IsSuppressed()
}

// PartitionOverlaps splits `status` overlaps into active vs benign (pure). An
// overlap is a real cross-window collision only when ≥2 of its windows are
// non-stale — one live editor plus N merged branches cannot conflict. Missing/
// unknown liveness counts as live (conservative).
//
// An ACTIVE overlap keeps only its live windows (#182). The suppressed ones used
// to ride along into the grade and the listing whenever two others were live, so
// a merged branch whose file now equals base (no ranges → "indeterminate") or a
// dormant branch's old hunk graded the file HIGH, in `wt status` and in the
// per-turn agent banner, while `wt check` hid both. A benign overlap is returned
// as-is (it is only counted).
func PartitionOverlaps(ov []Overlap, live map[string]WindowLiveness) (active, benign []Overlap) {
	for _, o := range ov {
		var idx []int
		for i, w := range o.Windows {
			if isLiveWindow(live, w) {
				idx = append(idx, i)
			}
		}
		if len(idx) >= 2 {
			active = append(active, o.keep(idx))
		} else {
			benign = append(benign, o)
		}
	}
	return active, benign
}

// keep returns o restricted to the windows at idx, with Worktrees kept aligned
// (and nil when o carries none). o itself is not modified.
func (o Overlap) keep(idx []int) Overlap {
	k := Overlap{File: o.File}
	aligned := len(o.Worktrees) == len(o.Windows)
	for _, i := range idx {
		k.Windows = append(k.Windows, o.Windows[i])
		if aligned {
			k.Worktrees = append(k.Worktrees, o.Worktrees[i])
		}
	}
	return k
}

// PartitionOverlapsFor is PartitionOverlaps from ONE window's side: the per-turn
// agent banner (#182), which answers the question `wt check` answers from that
// window. Pure.
//
// For a file self is editing, the overlap is active when at least ONE other
// window is live, and it lists self plus those live windows. Self's own liveness
// is not consulted, because `wt check` never consults it: a window still editing
// after its PR merged must still be told who it collides with. A file self is
// NOT editing is a heads-up about other windows and partitions exactly as
// PartitionOverlaps does. A zero self (the current window couldn't be
// identified) is PartitionOverlaps.
//
// Self is matched by worktree (Self.Is), so another window that merely shares
// self's label is an OTHER window, never folded into self (#182).
func PartitionOverlapsFor(ov []Overlap, live map[string]WindowLiveness, self Self) (active, benign []Overlap) {
	if self.IsZero() {
		return PartitionOverlaps(ov, live)
	}
	for _, o := range ov {
		participant, others := false, 0
		var idx []int
		for i, w := range o.Windows {
			switch {
			case self.Is(o, i):
				participant = true
				idx = append(idx, i)
			case isLiveWindow(live, w):
				others++
				idx = append(idx, i)
			}
		}
		need := 2
		if participant {
			need = 1
		}
		if others >= need {
			active = append(active, o.keep(idx))
		} else {
			benign = append(benign, o)
		}
	}
	return active, benign
}
