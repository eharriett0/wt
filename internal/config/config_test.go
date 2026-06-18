package config

import (
	"reflect"
	"testing"
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
