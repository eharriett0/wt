package config

import (
	"reflect"
	"testing"
	"time"
)

func TestParseConf(t *testing.T) {
	body := `
# a comment
base = develop
worktree_root=/tmp/wts

prefix= wip-
link_files = .env, secrets.yaml ,  .npmrc
claim_open_pr = false
# trailing comment
malformed line without equals
empty_key_ignored
`
	m := ParseConf(body)
	want := map[string]string{
		"base":          "develop",
		"worktree_root": "/tmp/wts",
		"prefix":        "wip-",
		"link_files":    ".env, secrets.yaml ,  .npmrc",
		"claim_open_pr": "false",
	}
	if !reflect.DeepEqual(m, want) {
		t.Errorf("ParseConf =\n%#v\nwant\n%#v", m, want)
	}
}

func TestApplyConf(t *testing.T) {
	c := &Config{Base: "main", Prefix: "feat-", LinkFiles: []string{".env"}, ClaimOpenPR: true}
	ApplyConf(c, ParseConf("base=trunk\nprefix=wip-\nlink_files=.env,.npmrc\nclaim_open_pr=no"))

	if c.Base != "trunk" {
		t.Errorf("Base = %q, want trunk", c.Base)
	}
	if c.Prefix != "wip-" {
		t.Errorf("Prefix = %q, want wip-", c.Prefix)
	}
	if !reflect.DeepEqual(c.LinkFiles, []string{".env", ".npmrc"}) {
		t.Errorf("LinkFiles = %v", c.LinkFiles)
	}
	if c.ClaimOpenPR {
		t.Error("ClaimOpenPR should be false")
	}
}

func TestApplyConf_EmptyValuesIgnored(t *testing.T) {
	c := &Config{Base: "main", Prefix: "feat-"}
	ApplyConf(c, map[string]string{"base": "", "prefix": ""})
	if c.Base != "main" || c.Prefix != "feat-" {
		t.Errorf("empty conf values must not clobber defaults: base=%q prefix=%q", c.Base, c.Prefix)
	}
}

func TestWorktreeRootAnchor(t *testing.T) {
	cases := []struct {
		name       string
		root       string // --show-toplevel (cwd's worktree)
		common     string // --git-common-dir (absolute)
		wantName   string
		wantParent string
	}{
		{
			// From the MAIN checkout: root == main, common == main/.git.
			name:       "main worktree",
			root:       "/home/u/repos/transwarp",
			common:     "/home/u/repos/transwarp/.git",
			wantName:   "transwarp",
			wantParent: "/home/u/repos",
		},
		{
			// The #11 case: invoked from a LINKED worktree. root is the
			// worktree dir, but common still points at the main .git, so the
			// anchor must resolve to the MAIN root (not "<wt>-worktrees").
			name:       "linked worktree resolves to main",
			root:       "/home/u/repos/transwarp-worktrees/feat-x",
			common:     "/home/u/repos/transwarp/.git",
			wantName:   "transwarp",
			wantParent: "/home/u/repos",
		},
		{
			// No/failed common dir → fall back to root (pre-#11 behavior).
			name:       "empty common falls back to root",
			root:       "/home/u/repos/transwarp",
			common:     "",
			wantName:   "transwarp",
			wantParent: "/home/u/repos",
		},
		{
			// Bare repo / unusual layout: common doesn't end in .git → fall back.
			name:       "bare common falls back to root",
			root:       "/home/u/repos/transwarp",
			common:     "/home/u/repos/transwarp.git",
			wantName:   "transwarp",
			wantParent: "/home/u/repos",
		},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			gotName, gotParent := worktreeRootAnchor(c.root, c.common)
			if gotName != c.wantName || gotParent != c.wantParent {
				t.Errorf("worktreeRootAnchor(%q, %q) = (%q, %q), want (%q, %q)",
					c.root, c.common, gotName, gotParent, c.wantName, c.wantParent)
			}
		})
	}
}

func TestParseBool(t *testing.T) {
	for _, v := range []string{"1", "true", "TRUE", "yes", "on"} {
		if !parseBool(v, false) {
			t.Errorf("parseBool(%q) should be true", v)
		}
	}
	for _, v := range []string{"0", "false", "no", "off"} {
		if parseBool(v, true) {
			t.Errorf("parseBool(%q) should be false", v)
		}
	}
	if !parseBool("garbage", true) {
		t.Error("parseBool falls back to default on unknown")
	}
}

func TestApplyConf_SharedDocs(t *testing.T) {
	// Override the default list.
	c := &Config{SharedDocs: []string{"CLAUDE.md", "MEMORY.md"}}
	ApplyConf(c, ParseConf("shared_docs=CLAUDE.md, NOTES.md"))
	if !reflect.DeepEqual(c.SharedDocs, []string{"CLAUDE.md", "NOTES.md"}) {
		t.Errorf("SharedDocs = %v, want [CLAUDE.md NOTES.md]", c.SharedDocs)
	}

	// Explicit empty value disables the soft-list (nil), unlike other keys
	// where empty is ignored — this is intentional so it can be turned off.
	c2 := &Config{SharedDocs: []string{"CLAUDE.md"}}
	ApplyConf(c2, ParseConf("shared_docs="))
	if len(c2.SharedDocs) != 0 {
		t.Errorf("shared_docs= should disable soft-list, got %v", c2.SharedDocs)
	}
}

func TestParseAge(t *testing.T) {
	day := 24 * time.Hour
	cases := []struct {
		in   string
		want time.Duration
		ok   bool
	}{
		{"4d", 4 * day, true},
		{"2w", 14 * day, true},
		{"36h", 36 * time.Hour, true},
		{"90m", 90 * time.Minute, true},
		{"7", 7 * day, true}, // bare int → days
		{"", 0, false},
		{"garbage", 0, false},
	}
	for _, c := range cases {
		got, err := ParseAge(c.in)
		if c.ok && (err != nil || got != c.want) {
			t.Errorf("ParseAge(%q) = %v, %v; want %v", c.in, got, err, c.want)
		}
		if !c.ok && err == nil {
			t.Errorf("ParseAge(%q) should error", c.in)
		}
	}
}

func TestApplyConf_Issue7Settings(t *testing.T) {
	c := &Config{}
	ApplyConf(c, ParseConf("append_only_paths=*.log,CHANGELOG.md\nmax_age=4d\nmerge_is_deploy=true"))
	if !reflect.DeepEqual(c.AppendOnlyPaths, []string{"*.log", "CHANGELOG.md"}) {
		t.Errorf("AppendOnlyPaths = %v", c.AppendOnlyPaths)
	}
	if c.MaxAge != 4*24*time.Hour {
		t.Errorf("MaxAge = %v, want 4d", c.MaxAge)
	}
	if !c.MergeIsDeploy {
		t.Error("MergeIsDeploy should be true")
	}
}

func TestApplyConf_HoldMaxAge(t *testing.T) {
	c := &Config{HoldMaxAge: DefaultHoldMaxAge}
	ApplyConf(c, map[string]string{"hold_max_age": "6h"})
	if c.HoldMaxAge != 6*time.Hour {
		t.Errorf("hold_max_age=6h → %v, want 6h", c.HoldMaxAge)
	}
	ApplyConf(c, map[string]string{"hold_max_age": "off"})
	if c.HoldMaxAge != 0 {
		t.Errorf("hold_max_age=off → %v, want 0 (never expire)", c.HoldMaxAge)
	}
	// "0" also means never-expire (distinct from max_age, where 0 is rejected)
	c.HoldMaxAge = time.Hour
	ApplyConf(c, map[string]string{"hold_max_age": "0"})
	if c.HoldMaxAge != 0 {
		t.Errorf("hold_max_age=0 → %v, want 0", c.HoldMaxAge)
	}
}
