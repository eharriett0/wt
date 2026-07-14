// Package config resolves per-repo settings — the portability core that
// parameterizes away awesome-o's hardcodings. Resolution order (later wins):
// derived defaults → repo-root .wt.conf → environment.
package config

import (
	"os"
	"path/filepath"
	"strings"

	"github.com/eharriett0/wt/internal/gitx"
)

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
}

// Load resolves config for the repo containing cwd.
func Load() (*Config, error) {
	root, err := gitx.RepoRoot()
	if err != nil {
		return nil, err
	}
	name := filepath.Base(root)
	parent := filepath.Dir(root)

	activeWork := filepath.Join(root, ".git", "wt-active-work.md")
	if common, err := gitx.CommonDir(); err == nil && common != "" {
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
	}

	// Repo-root .wt.conf overlay.
	if b, err := os.ReadFile(filepath.Join(root, ".wt.conf")); err == nil {
		ApplyConf(c, ParseConf(string(b)))
	}
	// Environment overlay (highest precedence).
	applyEnv(c)

	return c, nil
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
