package ghx

import (
	"strings"
	"testing"
)

// parsePRRefBase reads `"<number> <url> <baseRefName>"`; anything short of a
// PR and a branch name is an error, never a placeholder (#168, #223).
func TestParsePRRefBase(t *testing.T) {
	ref, base, err := parsePRRefBase("7 https://github.com/o/r/pull/7 release/1.2\n")
	if err != nil || ref.number != 7 || ref.host != "github.com" || base != "release/1.2" {
		t.Errorf("parsePRRefBase = (%+v, %q, %v), want PR 7 on github.com, base release/1.2", ref, base, err)
	}
	for _, out := range []string{
		"",
		"7 https://github.com/o/r/pull/7",          // no base
		"7 https://github.com/o/r/pull/7 null",     // gh's placeholder for a missing field
		"7 https://github.com/o/r/pull/7 ",         // empty base
		"null https://github.com/o/r/pull/7 main",  // no number
		"7 https://github.com/o/r/pull/8 main",     // URL of another PR
		"7 https://github.com/o/r/pull/7 main:x",   // ':' would change the expression
		"7 https://github.com/o/r/pull/7 -main",    // not a branch name
		"7 https://github.com/o/r/pull/7 a..b",     // not a branch name
		"7 https://github.com/o/r/pull/7 main@{1}", // a reflog expression
	} {
		if _, _, err := parsePRRefBase(out); err == nil {
			t.Errorf("parsePRRefBase(%q) = no error, want one", out)
		}
	}
}

func TestPROwnerName(t *testing.T) {
	owner, name, err := prOwnerName(prRef{number: 7, url: "https://ghe.example.com/acme/svc/pull/7", host: "ghe.example.com"})
	if err != nil || owner != "acme" || name != "svc" {
		t.Errorf("prOwnerName = (%q, %q, %v), want acme svc", owner, name, err)
	}
	for _, u := range []string{"https://github.com/o/pull/7", "https://github.com/o/r/issues/7", "https://github.com//r/pull/7"} {
		if _, _, err := prOwnerName(prRef{number: 7, url: u}); err == nil {
			t.Errorf("prOwnerName(%q) = no error, want one", u)
		}
	}
}

// parseBaseConf tells a base without the file (a tip and a null object) from
// a read that failed (no tip), and takes only a whole text Blob as the file
// (#223). Shapes as measured against GitHub's GraphQL: a missing file is
// `"object":null` with no error, a directory is `{"__typename":"Tree"}`, a
// missing branch nulls the ref.
func TestParseBaseConf(t *testing.T) {
	const tip = `"oid":"c51032e9ec3c475f282af0f4f577461f2d225cbf"`
	read, err := parseBaseConf(`{` + tip + `,"conf":{"__typename":"Blob","text":"merge_is_deploy = true\n","isBinary":false,"isTruncated":false}}`)
	if err != nil || !read.Found || read.Text != "merge_is_deploy = true\n" || read.Commit != "c51032e9ec3c475f282af0f4f577461f2d225cbf" {
		t.Errorf("a file: %+v, %v", read, err)
	}
	read, err = parseBaseConf(`{` + tip + `,"conf":null}`)
	if err != nil || read.Found || read.Text != "" {
		t.Errorf("no file: %+v, %v; want not found, no error", read, err)
	}
	read, err = parseBaseConf(`{` + tip + `,"conf":{"__typename":"Blob","text":"","isBinary":false,"isTruncated":false}}`)
	if err != nil || !read.Found {
		t.Errorf("an empty file is a file: %+v, %v", read, err)
	}
	for name, out := range map[string]string{
		"no tip (branch or repository unread)": `{"oid":null,"conf":null}`,
		"empty tip":                            `{"oid":"","conf":null}`,
		"a directory":                          `{` + tip + `,"conf":{"__typename":"Tree"}}`,
		"a submodule":                          `{` + tip + `,"conf":{"__typename":"Commit"}}`,
		"binary":                               `{` + tip + `,"conf":{"__typename":"Blob","text":null,"isBinary":true,"isTruncated":false}}`,
		"truncated":                            `{` + tip + `,"conf":{"__typename":"Blob","text":"merge_is","isBinary":false,"isTruncated":true}}`,
		"a Blob without text":                  `{` + tip + `,"conf":{"__typename":"Blob","isBinary":false,"isTruncated":false}}`,
		"not JSON":                             `7`,
		"nothing":                              ``,
	} {
		if read, err := parseBaseConf(out); err == nil {
			t.Errorf("%s: parseBaseConf = %+v, want an error", name, read)
		} else if name == "a directory" && !strings.Contains(err.Error(), "is a tree, not a file") {
			t.Errorf("%s: error %q doesn't say what is there", name, err)
		}
	}
}
