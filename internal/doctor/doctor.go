// Package doctor runs preflight checks for wt.
package doctor

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"time"

	"github.com/eharriett0/wt/internal/collide"
	"github.com/eharriett0/wt/internal/config"
	"github.com/eharriett0/wt/internal/coord"
	"github.com/eharriett0/wt/internal/ghx"
	"github.com/eharriett0/wt/internal/gitx"
	"github.com/eharriett0/wt/internal/hooks"
	"github.com/eharriett0/wt/internal/ui"
)

// pruneBlockMaxAge mirrors `wt prune-coord`'s default so the prunable-record
// preview in `wt doctor` matches what a prune would actually drop.
const pruneBlockMaxAge = 24 * time.Hour

// Report is the machine-readable form of a doctor run (`wt doctor --json`).
type Report struct {
	Healthy bool `json:"healthy"` // false only when a hard prerequisite is missing

	Git      bool   `json:"git"`
	Repo     string `json:"repo"` // "" when not in a git repository
	GH       bool   `json:"gh"`
	GHAuthed bool   `json:"gh_authed"`

	Config      map[string]string `json:"config,omitempty"`          // all resolved settings
	Structured  []DocCheck        `json:"structured_docs,omitempty"` // regex validation per structured_doc
	UnknownKeys []string          `json:"unknown_keys,omitempty"`    // unrecognized .wt.conf keys

	Coord     *CoordHealth `json:"coord,omitempty"`
	Preflight *Preflight   `json:"preflight,omitempty"`

	Upstream       []UpstreamCheck `json:"upstream_checks,omitempty"` // worktree branches tracking the base / no upstream (#76)
	StaleCheckouts []StaleCheckout `json:"stale_checkouts,omitempty"` // base-branch checkouts, dirty + far behind, poisoning collision checks (#87)
	HooksInstalled *bool           `json:"hooks_installed,omitempty"` // wt pre-push guard present in this repo (#76)
}

// StaleCheckout flags a worktree checked out ON the base branch that is dirty
// AND far behind origin/base — the "poisons every check" phantom (#87). Its
// uncommitted diff is roughly the inverse of everything that has since landed,
// so `wt check`/`status` report it as an active window overlapping nearly every
// file. Nothing else surfaces it, so it silently degrades collision detection
// for as long as it stays dirty.
type StaleCheckout struct {
	Path       string `json:"path"`
	Branch     string `json:"branch"`      // == base
	BehindBase int    `json:"behind_base"` // commits origin/base is ahead
	DirtyFiles int    `json:"dirty_files"` // uncommitted changes in the checkout
	Severity   string `json:"severity"`    // "warn"
}

// UpstreamCheck flags a worktree whose branch tracks the BASE ref — a bare
// `git push` there lands straight on the base (the deploy branch in a
// merge_is_deploy repo) with no PR/CI/review — or has no upstream at all (#76).
type UpstreamCheck struct {
	Path     string `json:"path"`
	Branch   string `json:"branch"`
	Upstream string `json:"upstream,omitempty"` // e.g. "origin/main"; "" when none
	Issue    string `json:"issue"`              // "tracks_base" | "no_upstream"
	Severity string `json:"severity"`           // "fail" | "warn" | "info"
}

// DocCheck is one structured_doc's compiled-regex status.
type DocCheck struct {
	Doc   string `json:"doc"`
	Regex string `json:"regex"`
	OK    bool   `json:"ok"`
	Err   string `json:"err,omitempty"`
}

// CoordHealth summarizes the shared coordination log's state.
type CoordHealth struct {
	Path             string `json:"path"`
	Exists           bool   `json:"exists"`
	Readable         bool   `json:"readable"`
	Records          int    `json:"records"`
	OwnOpen          int    `json:"own_open_announcements"`
	OwnBlockReserves int    `json:"own_block_reservations"`
	Prunable         int    `json:"prunable"`
	Err              string `json:"err,omitempty"`
}

// Preflight is the create-time viability of worktree_root + base branch.
type Preflight struct {
	BaseResolves     bool   `json:"base_resolves"`
	Base             string `json:"base"`
	WorktreeRootOK   bool   `json:"worktree_root_parent_exists"`
	WorktreeRoot     string `json:"worktree_root"`
	WorktreeRootNote string `json:"worktree_root_note,omitempty"`
}

// Run prints a checklist (or JSON) and returns a process exit code (0 healthy,
// 1 if a hard prerequisite is missing).
func Run(c *config.Config, asJSON bool) int {
	rep := build(c)
	if asJSON {
		b, _ := json.MarshalIndent(rep, "", "  ")
		fmt.Println(string(b))
	} else {
		render(rep)
	}
	if !rep.Healthy {
		return 1
	}
	return 0
}

// build assembles the Report — pure aside from the git/gh/fs probes it drives.
func build(c *config.Config) *Report {
	rep := &Report{Healthy: true}

	rep.Git = gitx.Present()
	if !rep.Git {
		rep.Healthy = false
	}
	if c != nil && c.Root != "" {
		rep.Repo = c.Root
	} else {
		rep.Healthy = false
	}
	rep.GH = ghx.Present()
	rep.GHAuthed = rep.GH && ghx.Authed()

	if c == nil || c.Root == "" {
		return rep
	}

	rep.Config = resolvedConfig(c)
	rep.Structured = validateStructured(c)
	rep.UnknownKeys = unknownKeys(c)
	rep.Coord = coordHealth(c)
	rep.Preflight = preflight(c)
	rep.Upstream = upstreamChecks(c)
	rep.StaleCheckouts = staleCheckouts(c)
	rep.HooksInstalled = hooksInstalled(c)
	// A worktree tracking the base branch in a merge_is_deploy repo is a hard
	// finding — a bare `git push` there ships to prod. That fails doctor.
	for _, u := range rep.Upstream {
		if u.Severity == "fail" {
			rep.Healthy = false
		}
	}
	return rep
}

// upstreamChecks flags every worktree whose branch tracks the base ref (a bare
// `git push` lands on the base — prod, in a merge_is_deploy repo) or has no
// upstream at all. The base branch itself legitimately tracks base, so it's
// skipped. `wt new` sets upstreams correctly; the broken ones are ad-hoc
// branches created outside wt (#76).
func upstreamChecks(c *config.Config) []UpstreamCheck {
	refs, err := gitx.WorktreeList()
	if err != nil {
		return nil
	}
	var out []UpstreamCheck
	for _, r := range refs {
		if r.Branch == "" || r.Branch == c.Base {
			continue // detached, or the base branch itself
		}
		merge, _ := gitx.RunDir(r.Path, "config", "--get", "branch."+r.Branch+".merge")
		issue, sev := classifyUpstream(merge, c.Base, c.MergeIsDeploy)
		if issue == "" {
			continue // tracks its own remote branch — fine
		}
		uc := UpstreamCheck{Path: r.Path, Branch: r.Branch, Issue: issue, Severity: sev}
		if issue == "tracks_base" {
			remote, _ := gitx.RunDir(r.Path, "config", "--get", "branch."+r.Branch+".remote")
			uc.Upstream = c.Base
			if remote != "" {
				uc.Upstream = remote + "/" + c.Base
			}
		}
		out = append(out, uc)
	}
	return out
}

// staleCheckouts finds every worktree checked out ON the base branch that is
// dirty AND at least collide.StaleBaseBehindThreshold commits behind origin/base
// (#87). That is precisely the checkout whose uncommitted edits poison `wt
// check`/`status` — its diff overlaps almost any file — and which nothing else
// names. A base checkout that is clean, or only slightly behind, is not flagged.
func staleCheckouts(c *config.Config) []StaleCheckout {
	refs, err := gitx.WorktreeList()
	if err != nil {
		return nil
	}
	baseRef := gitx.ResolveRemoteBase(c.Base)
	if baseRef == "" {
		return nil
	}
	var out []StaleCheckout
	for _, r := range refs {
		if r.Branch != c.Base {
			continue // only the base-branch checkout can be this phantom
		}
		if gitx.IsClean(r.Path) {
			continue // a clean checkout has no dirty diff to overlap anything
		}
		headSHA, err := gitx.RunDir(r.Path, "rev-parse", "HEAD")
		if err != nil {
			continue
		}
		behind := gitx.BehindCount(strings.TrimSpace(headSHA), baseRef)
		if _, sev := classifyStaleCheckout(behind); sev == "" {
			continue
		}
		out = append(out, StaleCheckout{
			Path:       r.Path,
			Branch:     r.Branch,
			BehindBase: behind,
			DirtyFiles: dirtyFileCount(r.Path),
			Severity:   "warn",
		})
	}
	return out
}

// classifyStaleCheckout decides whether a dirty base checkout is stale enough to
// report, from how far behind origin/base it is. Pure — the testable core.
// Returns ("","") below the threshold (or when behind couldn't be computed, so a
// git error never manufactures a warning).
func classifyStaleCheckout(behind int) (issue, severity string) {
	if behind >= collide.StaleBaseBehindThreshold {
		return "stale_base_checkout", "warn"
	}
	return "", ""
}

// dirtyFileCount counts uncommitted changes in the worktree at path (porcelain
// lines) for the report message; 0 on any error.
func dirtyFileCount(path string) int {
	out, err := gitx.RunDir(path, "status", "--porcelain")
	if err != nil {
		return 0
	}
	n := 0
	for _, ln := range strings.Split(out, "\n") {
		if strings.TrimSpace(ln) != "" {
			n++
		}
	}
	return n
}

// classifyUpstream decides a worktree branch's upstream finding from its git
// merge ref (branch.<b>.merge, e.g. "refs/heads/main" or "" for no upstream),
// the base branch, and whether merge_is_deploy is set. Returns ("","") when the
// branch is fine (tracks its own remote branch). Pure — the testable core of the
// #76 check.
func classifyUpstream(mergeRef, base string, mergeIsDeploy bool) (issue, severity string) {
	if strings.TrimSpace(mergeRef) == "" {
		return "no_upstream", "info"
	}
	if strings.TrimPrefix(mergeRef, "refs/heads/") != base {
		return "", "" // tracks its own branch — fine
	}
	if mergeIsDeploy {
		return "tracks_base", "fail" // a bare push here ships to prod
	}
	return "tracks_base", "warn"
}

// hooksInstalled reports whether the wt pre-push base-branch guard is present in
// this repo's shared hooks dir — nil when the git common dir can't be resolved.
func hooksInstalled(c *config.Config) *bool {
	common, err := gitx.CommonDir()
	if err != nil {
		return nil
	}
	v := hooks.PrePushInstalled(common)
	return &v
}

func resolvedConfig(c *config.Config) map[string]string {
	return map[string]string{
		"base":              c.Base,
		"worktree_root":     c.WorktreeRoot,
		"active_work":       c.ActiveWork,
		"prefix":            c.Prefix,
		"link_files":        strings.Join(c.LinkFiles, ","),
		"claim_open_pr":     boolStr(c.ClaimOpenPR),
		"shared_docs":       strings.Join(c.SharedDocs, ","),
		"append_only_paths": strings.Join(c.AppendOnlyPaths, ","),
		"max_age":           ageStr(c.MaxAge, "off"),
		"hold_max_age":      ageStr(c.HoldMaxAge, "never"),
		"merge_is_deploy":   boolStr(c.MergeIsDeploy),
		"coord_issue":       coordIssueStr(c.CoordIssue),
	}
}

func coordIssueStr(n int) string {
	if n <= 0 {
		return "off"
	}
	return fmt.Sprintf("#%d", n)
}

func validateStructured(c *config.Config) []DocCheck {
	if len(c.StructuredDocs) == 0 {
		return nil
	}
	names := make([]string, 0, len(c.StructuredDocs))
	for k := range c.StructuredDocs {
		names = append(names, k)
	}
	sort.Strings(names)
	out := make([]DocCheck, 0, len(names))
	for _, name := range names {
		re := c.StructuredDocs[name]
		dc := DocCheck{Doc: name, Regex: re, OK: true}
		if _, err := regexp.Compile(re); err != nil {
			dc.OK = false
			dc.Err = err.Error()
		}
		out = append(out, dc)
	}
	return out
}

func unknownKeys(c *config.Config) []string {
	b, err := os.ReadFile(filepath.Join(c.Root, ".wt.conf"))
	if err != nil {
		return nil // no config file → nothing to warn about
	}
	return config.UnknownKeys(config.ParseConf(string(b)))
}

func coordHealth(c *config.Config) *CoordHealth {
	home, _ := os.UserHomeDir()
	path := coord.LogPath(home, repoName(c))
	h := &CoordHealth{Path: path}
	if _, err := os.Stat(path); err != nil {
		return h // Exists=false — a fresh repo, healthy
	}
	h.Exists = true
	recs, err := coord.Load(path)
	if err != nil {
		h.Err = err.Error()
		return h
	}
	h.Readable = true
	h.Records = len(recs)
	branch, _ := gitx.CurrentBranch()
	self := coord.WindowID(os.Getenv("WT_WINDOW"), c.Root, branch)
	h.OwnOpen = len(coord.OwnOpenAnnouncements(recs, self))
	h.OwnBlockReserves = len(coord.OwnBlockReservations(recs, self))
	_, h.Prunable = coord.PruneRecords(recs, time.Now(), pruneBlockMaxAge)
	return h
}

func preflight(c *config.Config) *Preflight {
	p := &Preflight{Base: c.Base, WorktreeRoot: c.WorktreeRoot}

	// Base branch: resolves as a local ref OR a remote-tracking ref.
	p.BaseResolves = refExists(c.Root, c.Base) || refExists(c.Root, "origin/"+c.Base)

	// worktree_root: git worktree add creates the leaf, but its PARENT must
	// already exist. If the root itself exists it's fine; else the parent must.
	if info, err := os.Stat(c.WorktreeRoot); err == nil {
		p.WorktreeRootOK = info.IsDir()
		if !p.WorktreeRootOK {
			p.WorktreeRootNote = "exists but is not a directory"
		}
	} else if _, perr := os.Stat(filepath.Dir(c.WorktreeRoot)); perr == nil {
		p.WorktreeRootOK = true
		p.WorktreeRootNote = "will be created on first `wt new`/`wt claim`"
	} else {
		p.WorktreeRootNote = "parent dir " + filepath.Dir(c.WorktreeRoot) + " does not exist"
	}
	return p
}

// render prints the human checklist.
func render(rep *Report) {
	ui.Banner("wt doctor")

	if rep.Git {
		ui.OK("git — found")
	} else {
		ui.Err("git — NOT FOUND on PATH (required)")
	}
	if rep.Repo != "" {
		ui.OK("repo — %s", rep.Repo)
	} else {
		ui.Err("repo — not inside a git repository")
	}
	switch {
	case rep.GHAuthed:
		ui.OK("gh — authenticated")
	case rep.GH:
		ui.Warn("gh — found but NOT authenticated (claim/release/merge-pr need `gh auth login`)")
	default:
		ui.Warn("gh — not found (claim/release/merge-pr need it; new/clean/status/check/hooks don't)")
	}

	if rep.Repo == "" {
		if !rep.Healthy {
			ui.Err("unhealthy — fix the above")
		}
		return
	}

	// Resolved config (all keys, sorted).
	ui.Banner("resolved config")
	keys := make([]string, 0, len(rep.Config))
	for k := range rep.Config {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	for _, k := range keys {
		v := rep.Config[k]
		if v == "" {
			v = ui.Dim("(unset)")
		}
		ui.Info("%-18s %s", k, v)
	}
	br, _ := gitx.CurrentBranch()
	ui.Info("%-18s %s", "window id", coord.WindowID(os.Getenv("WT_WINDOW"), rep.Repo, br))

	// structured_doc regex validation.
	for _, dc := range rep.Structured {
		if dc.OK {
			ui.OK("structured_doc.%s — regex compiles (/%s/)", dc.Doc, dc.Regex)
		} else {
			ui.Err("structured_doc.%s — BAD regex /%s/: %s (this doc falls back to blanket shared-doc advisory)", dc.Doc, dc.Regex, dc.Err)
		}
	}

	// Unknown keys.
	if len(rep.UnknownKeys) > 0 {
		ui.Warn(".wt.conf — unrecognized key(s) (ignored — typo?): %s", strings.Join(rep.UnknownKeys, ", "))
	}

	// Coordination log health.
	if h := rep.Coord; h != nil {
		switch {
		case !h.Exists:
			ui.Info("%-18s %s %s", "coord log", h.Path, ui.Dim("(none yet)"))
		case h.Err != "":
			ui.Err("coord log — UNREADABLE %s: %s", h.Path, h.Err)
		default:
			extra := ""
			if h.OwnOpen > 0 || h.OwnBlockReserves > 0 {
				extra = fmt.Sprintf(" — YOU have %d open announcement(s), %d block reservation(s) (see `wt holds`)", h.OwnOpen, h.OwnBlockReserves)
			}
			ui.OK("coord log — %d record(s)%s", h.Records, extra)
			if h.Prunable > 0 {
				ui.Warn("coord log — %d resolved/expired record(s) prunable (run `wt prune-coord`)", h.Prunable)
			}
		}
	}

	// Preflight.
	if p := rep.Preflight; p != nil {
		if p.BaseResolves {
			ui.OK("base branch %q resolves", p.Base)
		} else {
			ui.Warn("base branch %q does not resolve (local or origin/) — new/claim will fail; set `base` in .wt.conf", p.Base)
		}
		if p.WorktreeRootOK {
			note := ""
			if p.WorktreeRootNote != "" {
				note = " " + ui.Dim("("+p.WorktreeRootNote+")")
			}
			ui.OK("worktree root %s ok%s", p.WorktreeRoot, note)
		} else {
			ui.Err("worktree root unusable — %s", p.WorktreeRootNote)
		}
	}

	// Worktree upstream sanity (#76): a branch tracking the base ref turns a
	// reflexive `git push` into a direct push to the base/deploy branch.
	if len(rep.Upstream) == 0 {
		ui.OK("worktree upstreams — no branch tracks the base branch")
	}
	for _, u := range rep.Upstream {
		fix := ui.Cyan(fmt.Sprintf("git -C %s branch --unset-upstream", u.Path))
		switch u.Severity {
		case "fail":
			ui.Err("worktree branch %q tracks the base (upstream %s) — a bare `git push` here lands DIRECTLY on the deploy branch (merge_is_deploy: no PR/CI/review). Fix: %s", u.Branch, u.Upstream, fix)
		case "warn":
			ui.Warn("worktree branch %q tracks the base (upstream %s) — a bare `git push` here goes to the base branch, not a PR. Fix: %s", u.Branch, u.Upstream, fix)
		case "info":
			ui.Info("worktree branch %q has no upstream — a `git push` there prompts rather than landing anywhere", u.Branch)
		}
	}

	// Stale base checkout poisoning collision checks (#87): a dirty base-branch
	// checkout that's fallen far behind origin/base overlaps nearly every file in
	// `wt check`/`status`. Nothing else names it, so surface it here by name.
	for _, s := range rep.StaleCheckouts {
		ui.Warn("base checkout %s is %d commit(s) behind origin/%s with %d dirty file(s) — it is poisoning collision checks (its diff overlaps almost every path). Fix: commit/stash + `git -C %s pull --ff-only`, or move the edits to a `wt new` branch.",
			s.Path, s.BehindBase, s.Branch, s.DirtyFiles, s.Path)
	}

	// Hooks installed? An unguarded repo is how the base-tracking worktrees above
	// go unnoticed in the first place (#76).
	if rep.HooksInstalled != nil && !*rep.HooksInstalled {
		ui.Warn("git hooks not installed in this repo — the pre-push base-branch guard is not active. Run `wt install-hooks`.")
	}

	if !rep.Healthy {
		ui.Err("unhealthy — fix the above")
	}
}

func refExists(dir, ref string) bool {
	_, err := gitx.RunDir(dir, "rev-parse", "--verify", "--quiet", ref+"^{commit}")
	return err == nil
}

// repoName is the MAIN worktree's dir name (stable across linked worktrees) —
// the same anchor coordCtx uses for the shared coordination log.
func repoName(c *config.Config) string {
	if common, err := gitx.CommonDir(); err == nil && filepath.Base(common) == ".git" {
		return filepath.Base(filepath.Dir(common))
	}
	return filepath.Base(c.Root)
}

func boolStr(b bool) string {
	if b {
		return "true"
	}
	return "false"
}

func ageStr(d time.Duration, zero string) string {
	if d == 0 {
		return zero
	}
	return d.String()
}
