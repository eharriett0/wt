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
// AND far behind origin/base (#87) — very likely rot. Its uncommitted edits
// surface as an active (HIGH) window in any `wt check`/`status` that touches one
// of them; `wt check` correctly still flags them (a dirty checkout can hold real
// work, so they are never hidden), but nothing else nudges you to clean the
// checkout up. This probe names it so the root cause gets fixed.
type StaleCheckout struct {
	Path       string `json:"path"`
	Branch     string `json:"branch"`      // == base
	BehindBase int    `json:"behind_base"` // commits origin/base is ahead
	DirtyFiles int    `json:"dirty_files"` // uncommitted changes in the checkout
	Severity   string `json:"severity"`    // "warn"
}

// UpstreamCheck flags a worktree whose branch tracks the BASE ref — under
// push.default=upstream/tracking a bare `git push` there aims at the base branch
// (though the wt pre-push guard still blocks it), while under simple it refuses
// on the name mismatch — or has no upstream at all (#76, #138).
type UpstreamCheck struct {
	Path        string `json:"path"`
	Branch      string `json:"branch"`
	Upstream    string `json:"upstream,omitempty"`     // e.g. "origin/main"; "" when none
	Issue       string `json:"issue"`                  // "tracks_base" | "no_upstream"
	Severity    string `json:"severity"`               // "warn" | "info"
	PushDefault string `json:"push_default,omitempty"` // effective push.default; "" = simple (#138)
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
	// A base-tracking worktree branch is NOT a hard failure (#138): under git's
	// default push.default=simple a bare push refuses on the name mismatch, and
	// the wt pre-push guard blocks a base push regardless — so it's a warn/info,
	// never a ✗ that fails doctor.
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
		pushDefault, _ := gitx.RunDir(r.Path, "config", "--get", "push.default")
		issue, sev := classifyUpstream(merge, c.Base, pushDefault)
		if issue == "" {
			continue // tracks its own remote branch — fine
		}
		uc := UpstreamCheck{Path: r.Path, Branch: r.Branch, Issue: issue, Severity: sev}
		if issue == "tracks_base" {
			uc.PushDefault = pushDefault
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
// the base branch, and the effective push.default. Returns ("","") when the
// branch is fine (tracks its own remote branch). Pure — the testable core of the
// #76 check.
//
// Severity for a base-tracking branch turns on push.default, NOT merge_is_deploy
// (#138): a bare `git push` only follows the UPSTREAM ref (→ the base branch)
// under push.default=upstream/tracking. Under simple — git's default since 2.0 —
// it targets the SAME-NAMED remote branch, so a base-tracking feature branch
// REFUSES on the name mismatch rather than silently shipping to the base. And
// the wt pre-push guard blocks a base push regardless. So the old hard ✗ ("a
// bare push lands DIRECTLY on the deploy branch") over-claimed on every repo
// running git's default config: warn only when push.default actually aims at the
// upstream, else report the real, smaller consequence (info).
func classifyUpstream(mergeRef, base, pushDefault string) (issue, severity string) {
	if strings.TrimSpace(mergeRef) == "" {
		return "no_upstream", "info"
	}
	if strings.TrimPrefix(mergeRef, "refs/heads/") != base {
		return "", "" // tracks its own branch — fine
	}
	if pushDefaultFollowsUpstream(pushDefault) {
		return "tracks_base", "warn" // a bare push aims at the base (guard still blocks it)
	}
	return "tracks_base", "info" // simple refuses on the name mismatch; real cost is pull/status
}

// pushDefaultFollowsUpstream reports whether push.default makes a bare `git push`
// follow the branch's UPSTREAM ref rather than its same-named remote branch. Only
// "upstream" and its deprecated alias "tracking" do; "simple" (unset default
// since git 2.0), "current", "matching", "nothing" all key off the branch name.
func pushDefaultFollowsUpstream(v string) bool {
	switch strings.TrimSpace(v) {
	case "upstream", "tracking":
		return true
	}
	return false
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

	// Worktree upstream sanity (#76, #138): a base-tracking branch only aims a
	// bare `git push` at the base under push.default=upstream/tracking — and the
	// wt pre-push guard blocks that regardless; under simple it refuses on the
	// name mismatch. State the consequence that's actually true for the config.
	if len(rep.Upstream) == 0 {
		ui.OK("worktree upstreams — no branch tracks the base branch")
	}
	for _, u := range rep.Upstream {
		fix := ui.Cyan(fmt.Sprintf("git -C %s branch --unset-upstream", u.Path))
		switch {
		case u.Issue == "no_upstream":
			ui.Info("worktree branch %q has no upstream — a `git push` there prompts rather than landing anywhere", u.Branch)
		case u.Severity == "warn": // tracks_base under push.default=upstream/tracking
			ui.Warn("worktree branch %q tracks the base (upstream %s) and push.default=%s, so a bare `git push` aims at the base branch — the wt pre-push guard blocks it, but set a same-named upstream: %s", u.Branch, u.Upstream, u.PushDefault, fix)
		default: // tracks_base under push.default=simple (or current/matching/nothing)
			pd := u.PushDefault
			if pd == "" {
				pd = "simple"
			}
			ui.Info("worktree branch %q tracks the base (upstream %s) — `git pull` here merges the base INTO your branch and `git status` reports ahead/behind against it; a bare `git push` does not reach the base under push.default=%s. Fix: %s", u.Branch, u.Upstream, pd, fix)
		}
	}

	// Stale base checkout (#87): a dirty base-branch checkout fallen far behind
	// origin/base surfaces as an active (HIGH) window in every `wt check`/`status`
	// that touches one of its dirty files, and is very likely rot. `wt check`
	// still flags it (a dirty checkout can hold real edits — never hidden), so
	// nothing else nudges you to clean it up; name it here.
	for _, s := range rep.StaleCheckouts {
		ui.Warn("base checkout %s is %d commit(s) behind origin/%s with %d dirty file(s) — likely stale, and its edits show up as HIGH collisions in `wt check`. Fix: commit/stash + `git -C %s pull --ff-only`, or move the edits to a `wt new` branch.",
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
