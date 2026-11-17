package ghx

import (
	"strconv"
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

// parseBaseConf tells a base without the file (a tip, no tree entry and a null
// object) from a read that failed (no tip), and takes only a whole text Blob in
// a regular-file entry as the file (#223). Shapes as measured against GitHub's
// GraphQL: a missing file is `"object":null` with no error (and no entry), a
// directory is `{"__typename":"Tree"}` (mode 16384), a missing branch nulls the
// ref, a regular file's mode is 33188, and a symlink is a Blob whose text is
// its target (#223 review), told apart only by its mode (0o120000).
func TestParseBaseConf(t *testing.T) {
	const tip = `"oid":"c51032e9ec3c475f282af0f4f577461f2d225cbf"`
	blob := func(text string) string {
		return `{"__typename":"Blob","text":` + strconv.Quote(text) + `,"isBinary":false,"isTruncated":false}`
	}
	read, err := parseBaseConf(`{` + tip + `,"mode":33188,"conf":` + blob("merge_is_deploy = true\n") + `}`)
	if err != nil || !read.Found || read.Text != "merge_is_deploy = true\n" || read.Commit != "c51032e9ec3c475f282af0f4f577461f2d225cbf" {
		t.Errorf("a file: %+v, %v", read, err)
	}
	if read, err := parseBaseConf(`{` + tip + `,"mode":33261,"conf":` + blob("x = 1") + `}`); err != nil || !read.Found {
		t.Errorf("an executable file is a file: %+v, %v", read, err)
	}
	read, err = parseBaseConf(`{` + tip + `,"mode":null,"conf":null}`)
	if err != nil || read.Found || read.Text != "" {
		t.Errorf("no file: %+v, %v; want not found, no error", read, err)
	}
	if read, err := parseBaseConf(`{` + tip + `,"conf":null}`); err != nil || read.Found {
		t.Errorf("no file, mode omitted: %+v, %v; want not found, no error", read, err)
	}
	if read, err := parseBaseConf(`{` + tip + `,"mode":33188,"eoid":"aaa","conf":{"__typename":"Blob","oid":"aaa","text":"y","isBinary":false,"isTruncated":false}}`); err != nil || read.Text != "y" {
		t.Errorf("matching entry and object ids: %+v, %v", read, err)
	}
	if read, err := parseBaseConf(`{` + tip + `,"mode":33188,"conf":` + blob("") + `}`); err != nil || !read.Found {
		t.Errorf("an empty file is a file: %+v, %v", read, err)
	}
	for name, out := range map[string]string{
		"no tip (branch or repository unread)": `{"oid":null,"mode":null,"conf":null}`,
		"empty tip":                            `{"oid":"","conf":null}`,
		"a symlink (its text is the target)":   `{` + tip + `,"mode":40960,"conf":` + blob("conf/wt.conf") + `}`,
		"a directory":                          `{` + tip + `,"mode":16384,"conf":{"__typename":"Tree"}}`,
		"a submodule":                          `{` + tip + `,"mode":57344,"conf":{"__typename":"Commit"}}`,
		"an entry but no object":               `{` + tip + `,"mode":33188,"conf":null}`,
		"an object but no entry":               `{` + tip + `,"mode":null,"conf":` + blob("x") + `}`,
		"an odd mode":                          `{` + tip + `,"mode":420,"conf":` + blob("x") + `}`,
		"the entry and the object are two blobs (#223 re-review: a push between)": `{` + tip + `,"mode":33188,"eoid":"aaa","conf":{"__typename":"Blob","oid":"bbb","text":"x","isBinary":false,"isTruncated":false}}`,
		"binary":              `{` + tip + `,"mode":33188,"conf":{"__typename":"Blob","text":null,"isBinary":true,"isTruncated":false}}`,
		"truncated":           `{` + tip + `,"mode":33188,"conf":{"__typename":"Blob","text":"merge_is","isBinary":false,"isTruncated":true}}`,
		"a Blob without text": `{` + tip + `,"mode":33188,"conf":{"__typename":"Blob","isBinary":false,"isTruncated":false}}`,
		"not JSON":            `7`,
		"nothing":             ``,
	} {
		if read, err := parseBaseConf(out); err == nil {
			t.Errorf("%s: parseBaseConf = %+v, want an error", name, read)
		} else if name == "a directory" && !strings.Contains(err.Error(), "is a tree, not a file") {
			t.Errorf("%s: error %q doesn't say what is there", name, err)
		} else if strings.HasPrefix(name, "a symlink") && !strings.Contains(err.Error(), "symlink") {
			t.Errorf("%s: error %q doesn't say it is a symlink", name, err)
		}
	}
}

// parseRemoteRepo reads origin's owner/name, so the fallback reads this clone
// only for the PR's own repository (#223 review).
func TestParseRemoteRepo(t *testing.T) {
	for u, want := range map[string][3]string{
		"git@github.com:acme/svc.git":           {"github.com", "acme", "svc"},
		"https://github.com/acme/svc":           {"github.com", "acme", "svc"},
		"https://github.com/acme/svc.git/":      {"github.com", "acme", "svc"},
		"ssh://git@ghe.example.com:22/acme/svc": {"ghe.example.com", "acme", "svc"},
		"https://user:tok@github.com/acme/svc":  {"github.com", "acme", "svc"},
		"/srv/repos/svc.git":                    {},
		"":                                      {},
		"https://github.com/acme":               {},
		"https://ghe.example.com/scm/acme/svc":  {}, // not exactly owner/name
		"git@github.com:acme/svc/extra.git":     {},
	} {
		h, o, n := parseRemoteRepo(u)
		if [3]string{h, o, n} != want {
			t.Errorf("parseRemoteRepo(%q) = %q %q %q, want %v", u, h, o, n, want)
		}
	}
	if (BaseConf{}).Repo() != "" || (BaseConf{Owner: "o", Name: "r"}).Repo() != "o/r" {
		t.Error("BaseConf.Repo")
	}
}
