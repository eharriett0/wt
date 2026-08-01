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

	"github.com/eharriett0/wt/internal/config"
	"github.com/eharriett0/wt/internal/coord"
	"github.com/eharriett0/wt/internal/ghx"
	"github.com/eharriett0/wt/internal/gitx"
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
	return rep
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
	}
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
