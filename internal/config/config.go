// Package config resolves per-repo settings — the portability core that
// parameterizes away awesome-o's hardcodings. Resolution order (later wins):
// derived defaults → repo-root .wt.conf → environment.
package config

import (
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/eharriett0/wt/internal/gitx"
)

// DefaultHoldMaxAge is how long a coordination --hold hard-blocks merge-pr
// before it's treated as stale (a crashed/forgotten window) and downgraded to a
// warning. A legitimate merge-main hold lasts minutes; a day-old hold is dead.
// Override with `hold_max_age` / WT_HOLD_MAX_AGE; set 0/off to never expire.
const DefaultHoldMaxAge = 24 * time.Hour

// Config holds resolved settings for the repo at Root.
type Config struct {
	Root         string   // repo top-level
	Base         string   // base branch (main/master/trunk...)
	WorktreeRoot string   // where new worktrees live
	ActiveWork   string   // active-work state file path
	Prefix       string   // claim branch prefix
	LinkFiles    []string // gitignored files symlinked into new worktrees
	ClaimOpenPR  bool     // open a draft PR on claim
	SharedDocs   []string // basenames of append-heavy shared docs (CLAUDE.md,
	// MEMORY.md…) where a cross-window touch is an ADVISORY, not a blocking
	// collision — every window legitimately edits these, so they'd otherwise
	// cry wolf on every check/commit. Matched by basename.
	AppendOnlyPaths    []string          // globs (filepath.Match) whose overlaps are downgraded to FYI regardless of hunk overlap (changelogs, inventory lists…)
	MaxAge             time.Duration     // suppress unmerged branches whose last commit is older than this (0 = off); dormancy suppression (#7)
	HoldMaxAge         time.Duration     // a coordination --hold older than this stops HARD-blocking merge-pr and downgrades to a loud warn (a crashed window can't wedge everyone forever); 0 = never expire. Default DefaultHoldMaxAge (#32)
	MergeIsDeploy      bool              // this repo auto-deploys on merge to base — merge-pr adds a prod-safety gate (refuse draft, banner, confirm)
	MergeIsDeployPaths []string          // globs (** via collide.MatchDoubleStar) scoping the deploy gate: when set, merge-pr fires the prod gate ONLY if the PR changes a matching file — docs/CI/scripts-only PRs skip it. Unset → whole repo is a deploy surface (legacy). Prefix a glob with `!` to EXCLUDE a class that cannot deploy but sits inside one that can, e.g. "infrastructure/**,modules/**,!modules/**/*.md" (#119). Exclusions-only fails CLOSED (gates everything), so a typo can't disable the gate.
	CoordIssue         int               // a pinned GitHub issue used as the cross-machine coordination mirror (#36): when set, announce/ack/all-clear auto-mirror to it, and inbox + the merge gate read it back so a hold on one machine blocks/warns on another. 0 = off.
	StructuredDocs     map[string]string // basename → section-delimiter regex (#22): docs that partition into sections/lanes, so a cross-window touch grades by SECTION (same section = HIGH) instead of the blanket shared-doc advisory. Per-doc because delimiters differ (CLAUDE.md by "## " headings, the resume memory by "**═══" lane bars). Config-file only.
}

// Load resolves config for the repo containing cwd.
func Load() (*Config, error) {
	root, err := gitx.RepoRoot()
	if err != nil {
		return nil, err
	}
	common, _ := gitx.CommonDir() // "" on error; absolute (…/<main>/.git) when present
	name, parent := worktreeRootAnchor(root, common)

	activeWork := filepath.Join(root, ".git", "wt-active-work.md")
	if common != "" {
		activeWork = filepath.Join(common, "wt-active-work.md")
	}

	c := &Config{
		Root:         root,
		Base:         gitx.DefaultBranch(),
		WorktreeRoot: filepath.Join(parent, name+"-worktrees"),
		ActiveWork:   activeWork,
		Prefix:       "feat-",
		LinkFiles:    []string{".env"},
		ClaimOpenPR:  true,
		SharedDocs:   []string{"CLAUDE.md", "MEMORY.md"},
		HoldMaxAge:   DefaultHoldMaxAge,
	}

	// Repo-root .wt.conf overlay.
	if b, err := os.ReadFile(filepath.Join(root, ".wt.conf")); err == nil {
		ApplyConf(c, ParseConf(string(b)))
	}
	// Environment overlay (highest precedence).
	applyEnv(c)

	return c, nil
}

// worktreeRootAnchor returns the (name, parent) used to derive the default
// WorktreeRoot ("<name>-worktrees" under <parent>). It anchors to the MAIN
// worktree, not cwd's: when Load() runs from inside a LINKED worktree,
// RepoRoot() (--show-toplevel) returns that worktree's own dir, so anchoring on
// it would nest a bogus "<worktree>-worktrees" that no worktree is ever under —
// breaking Remove()'s under-root guard (merge auto-clean refused, eharriett0/wt#11).
// The shared common dir is "…/<main>/.git" from ANY worktree, so its parent is
// the stable main root. Falls back to `root` when common isn't a "…/.git"
// (bare repo / unusual layout) — the pre-#11 behavior.
func worktreeRootAnchor(root, common string) (name, parent string) {
	if common != "" && filepath.Base(common) == ".git" {
		mainRoot := filepath.Dir(common)
		return filepath.Base(mainRoot), filepath.Dir(mainRoot)
	}
	return filepath.Base(root), filepath.Dir(root)
}

// ScaffoldConf returns a commented .wt.conf template for c (#44). Every line is
// commented — uncommenting one overrides that default — and each key is preceded
// by a "# resolved:" line showing what wt DERIVED for this repo, so the operator
// can see (and confirm) the effective value without triggering behavior.
func ScaffoldConf(c *Config) string {
	boolStr := func(b bool) string {
		if b {
			return "true"
		}
		return "false"
	}
	ageStr := func(d time.Duration) string {
		if d == 0 {
			return "off"
		}
		return d.String()
	}
	var b strings.Builder
	b.WriteString("# wt config — repo-root .wt.conf (key=value).\n")
	b.WriteString("# Every line is commented: uncomment + edit only what you want to override.\n")
	b.WriteString("# '# resolved:' shows the value wt derived for THIS repo.\n\n")
	entry := func(key, resolved, example string) {
		if resolved != "" {
			b.WriteString("# resolved: " + resolved + "\n")
		}
		b.WriteString("# " + key + " = " + example + "\n\n")
	}
	entry("base", c.Base, c.Base)
	entry("worktree_root", c.WorktreeRoot, c.WorktreeRoot)
	entry("active_work", c.ActiveWork, c.ActiveWork)
	entry("prefix", c.Prefix, "feat-")
	entry("link_files", strings.Join(c.LinkFiles, ","), ".env,.envrc")
	entry("claim_open_pr", boolStr(c.ClaimOpenPR), "true")
	entry("shared_docs", strings.Join(c.SharedDocs, ","), "CLAUDE.md,MEMORY.md")
	b.WriteString("# structured_doc.<basename> = <section-delimiter regexp> — grade that shared\n")
	b.WriteString("# doc by SECTION (two windows editing the SAME section = HIGH):\n")
	b.WriteString("# structured_doc.CLAUDE.md = ^##\\s\n\n")
	entry("append_only_paths", strings.Join(c.AppendOnlyPaths, ","), "CHANGELOG.md,docs/**/*.md")
	entry("max_age", ageStr(c.MaxAge), "4d")
	entry("hold_max_age", ageStr(c.HoldMaxAge), "24h")
	entry("merge_is_deploy", boolStr(c.MergeIsDeploy), "false")
	entry("merge_is_deploy_paths", strings.Join(c.MergeIsDeployPaths, ","), "infrastructure/**,envs/**")
	issueStr := ""
	if c.CoordIssue > 0 {
		issueStr = strconv.Itoa(c.CoordIssue)
	}
	entry("coord_issue", issueStr, "0")
	return b.String()
}

// knownKeys is every recognized flat .wt.conf key. structured_doc.<name> is
// handled by prefix (unbounded suffix), so it's not listed here.
var knownKeys = map[string]bool{
	"base": true, "worktree_root": true, "active_work": true, "prefix": true,
	"link_files": true, "claim_open_pr": true, "shared_docs": true,
	"append_only_paths": true, "max_age": true, "hold_max_age": true,
	"merge_is_deploy": true, "merge_is_deploy_paths": true, "coord_issue": true,
}

// UnknownKeys returns the parsed .wt.conf keys that wt does not recognize,
// sorted — a typo'd key (e.g. `worktree-root` instead of `worktree_root`) is
// silently ignored by ApplyConf, so `wt doctor` surfaces it here. A
// `structured_doc.<name>` key is always recognized (prefix match). Pure (#43).
func UnknownKeys(m map[string]string) []string {
	var out []string
	for k := range m {
		if knownKeys[k] {
			continue
		}
		if name, ok := strings.CutPrefix(k, "structured_doc."); ok && name != "" {
			continue
		}
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}

// ParseConf parses a key=value config body. Lines beginning with # and blank
// lines are ignored; the first '=' splits key/value; both sides are trimmed.
func ParseConf(body string) map[string]string {
	m := map[string]string{}
	for _, ln := range strings.Split(body, "\n") {
		ln = strings.TrimSpace(ln)
		if ln == "" || strings.HasPrefix(ln, "#") {
			continue
		}
		i := strings.Index(ln, "=")
		if i < 0 {
			continue
		}
		key := strings.TrimSpace(ln[:i])
		val := strings.TrimSpace(ln[i+1:])
		if key != "" {
			m[key] = val
		}
	}
	return m
}

// ApplyConf overlays parsed key=value pairs onto c.
func ApplyConf(c *Config, m map[string]string) {
	if v, ok := m["base"]; ok && v != "" {
		c.Base = v
	}
	if v, ok := m["worktree_root"]; ok && v != "" {
		c.WorktreeRoot = expandHome(v)
	}
	if v, ok := m["active_work"]; ok && v != "" {
		c.ActiveWork = expandHome(v)
	}
	if v, ok := m["prefix"]; ok && v != "" {
		c.Prefix = v
	}
	if v, ok := m["link_files"]; ok {
		c.LinkFiles = splitCSV(v)
	}
	if v, ok := m["claim_open_pr"]; ok {
		c.ClaimOpenPR = parseBool(v, c.ClaimOpenPR)
	}
	if v, ok := m["shared_docs"]; ok {
		c.SharedDocs = splitCSV(v) // empty value → nil → soft-list disabled
	}
	if v, ok := m["append_only_paths"]; ok {
		c.AppendOnlyPaths = splitCSV(v)
	}
	if v, ok := m["max_age"]; ok {
		if d, err := ParseAge(v); err == nil {
			c.MaxAge = d // empty or unparseable → left at current (0 = off)
		}
	}
	if v, ok := m["merge_is_deploy"]; ok {
		c.MergeIsDeploy = parseBool(v, c.MergeIsDeploy)
	}
	if v, ok := m["merge_is_deploy_paths"]; ok {
		c.MergeIsDeployPaths = splitCSV(v)
	}
	if v, ok := m["hold_max_age"]; ok {
		c.HoldMaxAge = parseHoldMaxAge(v, c.HoldMaxAge)
	}
	if v, ok := m["coord_issue"]; ok {
		if n, err := strconv.Atoi(strings.TrimSpace(v)); err == nil && n > 0 {
			c.CoordIssue = n
		}
	}
	// structured_doc.<basename> = <section-delimiter regex> (#22). Prefixed keys
	// because the flat key=value config has no nesting, and each doc needs its
	// own delimiter. A bad regex is tolerated (that doc just falls back to the
	// blanket shared-doc advisory) — compilation happens at grade time, not here,
	// so config parsing never fails on a typo.
	for k, v := range m {
		if name, ok := strings.CutPrefix(k, "structured_doc."); ok && name != "" && v != "" {
			if c.StructuredDocs == nil {
				c.StructuredDocs = map[string]string{}
			}
			c.StructuredDocs[name] = v
		}
	}
}

func applyEnv(c *Config) {
	if v := os.Getenv("WT_BASE"); v != "" {
		c.Base = v
	}
	if v := os.Getenv("WT_WORKTREE_ROOT"); v != "" {
		c.WorktreeRoot = expandHome(v)
	}
	if v := os.Getenv("WT_ACTIVE_WORK"); v != "" {
		c.ActiveWork = expandHome(v)
	}
	if v := os.Getenv("WT_PREFIX"); v != "" {
		c.Prefix = v
	}
	if v := os.Getenv("WT_LINK_FILES"); v != "" {
		c.LinkFiles = splitCSV(v)
	}
	if v := os.Getenv("WT_CLAIM_OPEN_PR"); v != "" {
		c.ClaimOpenPR = parseBool(v, c.ClaimOpenPR)
	}
	// LookupEnv (not Getenv != "") so an explicit empty WT_SHARED_DOCS disables
	// the soft-list entirely, rather than being ignored.
	if v, ok := os.LookupEnv("WT_SHARED_DOCS"); ok {
		c.SharedDocs = splitCSV(v)
	}
	if v, ok := os.LookupEnv("WT_APPEND_ONLY_PATHS"); ok {
		c.AppendOnlyPaths = splitCSV(v)
	}
	if v := os.Getenv("WT_MAX_AGE"); v != "" {
		if d, err := ParseAge(v); err == nil {
			c.MaxAge = d
		}
	}
	if v := os.Getenv("WT_MERGE_IS_DEPLOY"); v != "" {
		c.MergeIsDeploy = parseBool(v, c.MergeIsDeploy)
	}
	if v, ok := os.LookupEnv("WT_MERGE_IS_DEPLOY_PATHS"); ok {
		c.MergeIsDeployPaths = splitCSV(v)
	}
	if v, ok := os.LookupEnv("WT_HOLD_MAX_AGE"); ok {
		c.HoldMaxAge = parseHoldMaxAge(v, c.HoldMaxAge)
	}
	if v := os.Getenv("WT_COORD_ISSUE"); v != "" {
		if n, err := strconv.Atoi(strings.TrimSpace(v)); err == nil && n > 0 {
			c.CoordIssue = n
		}
	}
}

// parseHoldMaxAge resolves a hold_max_age value: "0"/"off"/"never" → 0 (expiry
// disabled), a ParseAge duration otherwise; anything unparseable leaves the
// current value. Unlike max_age, 0 is a MEANINGFUL setting here (never expire),
// so it can't reuse ParseAge (which rejects non-positive).
func parseHoldMaxAge(v string, cur time.Duration) time.Duration {
	switch strings.TrimSpace(strings.ToLower(v)) {
	case "0", "off", "never":
		return 0
	}
	if d, err := ParseAge(v); err == nil {
		return d
	}
	return cur
}

// ParseAge parses a human duration for dormancy: Go's time.ParseDuration units
// (s/m/h…) plus "d" (days) and "w" (weeks), e.g. "4d", "36h", "2w". A bare
// number is treated as days. Empty, malformed, or non-positive → error (a
// negative/zero threshold would silently disable dormancy, so it's rejected
// loudly rather than mistaken for "off").
func ParseAge(s string) (time.Duration, error) {
	s = strings.TrimSpace(s)
	if s == "" {
		return 0, strconv.ErrSyntax
	}
	d, err := parseAgeRaw(s)
	if err != nil {
		return 0, err
	}
	if d <= 0 {
		return 0, fmt.Errorf("max_age must be positive, got %q", s)
	}
	return d, nil
}

func parseAgeRaw(s string) (time.Duration, error) {
	// Plain integer → days.
	if n, err := strconv.Atoi(s); err == nil {
		return time.Duration(n) * 24 * time.Hour, nil
	}
	// <number><d|w> — scale to hours and let ParseDuration do the rest.
	if unit := s[len(s)-1]; unit == 'd' || unit == 'w' {
		n, err := strconv.ParseFloat(s[:len(s)-1], 64)
		if err != nil {
			return 0, err
		}
		mult := 24.0 // days
		if unit == 'w' {
			mult = 24 * 7
		}
		return time.Duration(n * mult * float64(time.Hour)), nil
	}
	return time.ParseDuration(s)
}

func splitCSV(v string) []string {
	var out []string
	for _, p := range strings.Split(v, ",") {
		if p = strings.TrimSpace(p); p != "" {
			out = append(out, p)
		}
	}
	return out
}

func parseBool(v string, def bool) bool {
	switch strings.ToLower(strings.TrimSpace(v)) {
	case "1", "true", "yes", "on":
		return true
	case "0", "false", "no", "off":
		return false
	}
	return def
}

func expandHome(p string) string {
	if p == "~" || strings.HasPrefix(p, "~/") {
		if home, err := os.UserHomeDir(); err == nil {
			return filepath.Join(home, strings.TrimPrefix(p, "~"))
		}
	}
	return p
}
