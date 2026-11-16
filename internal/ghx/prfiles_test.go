package ghx

import (
	"encoding/json"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
)

// unquoteGitPath undoes git's C-quoting of a path (#200): what `gh pr diff
// --name-only` prints for a name git had to quote in the diff header.
func TestUnquoteGitPath(t *testing.T) {
	for _, tc := range []struct{ in, want string }{
		{"plain.go", "plain.go"},
		{"a b.md", "a b.md"}, // a diff header doesn't quote a space
		{`"caf\303\251.md"`, "café.md"},
		{`"d\303\255r \303\251/na\303\257ve.md"`, "dír é/naïve.md"},
		{`"q\"uote.md"`, `q"uote.md`},
		{`"back\\slash.md"`, `back\slash.md`},
		{`"tab\tname\nline\r\a\b\f\v.md"`, "tab\tname\nline\r\a\b\f\v.md"},
		{`"\001\177"`, "\x01\x7f"},
		{`"\377"`, "\xff"}, // a byte, not a rune: names need not be UTF-8
		// not git's quoting: returned as is
		{`"`, `"`},
		{`""`, ""},
		{`"unterminated`, `"unterminated`},
		{`"bad\escape"`, `"bad\escape"`},
		{`"short\30"`, `"short\30"`},
		{`"big\400"`, `"big\400"`},
		{`"trailing\"`, `"trailing\"`},
		{`"inner"quote"`, `"inner"quote"`},
	} {
		if got := unquoteGitPath(tc.in); got != tc.want {
			t.Errorf("unquoteGitPath(%q) = %q, want %q", tc.in, got, tc.want)
		}
	}
}

// The deploy gate's file list (#221) comes from REST pulls/{n}/files, at the
// PR's own host and repository: both sides of a rename are listed (a file
// moved OUT of a deploy path is a deploy change), each once, and names come
// verbatim from JSON, never C-quoted.
func TestPRChangedFiles_ListsBothSidesOfARename(t *testing.T) {
	dir := fakeGh(t, `echo "$*" >> "$d/calls"
case "$*" in
"pr view 7 --json number,url"*) echo "7 https://ghe.example.com/o/r/pull/7" ;;
"api --hostname ghe.example.com repos/o/r/pulls/7/files --paginate"*)
	printf '%s\n' '{"f":"infra/plain.yaml","p":null}' '{"f":"docs/x.yaml","p":"infra/x.yaml"}' '{"f":"infra/café.yaml","p":null}' '{"f":"q\"uote.md","p":null}' '{"f":"infra/x.yaml","p":null}' ;;
*) echo "unexpected: $*" >&2; exit 2 ;;
esac
`)
	got, err := PRChangedFiles("7")
	if err != nil {
		t.Fatal(err)
	}
	if want := []string{"infra/plain.yaml", "docs/x.yaml", "infra/x.yaml", "infra/café.yaml", `q"uote.md`}; !reflect.DeepEqual(got, want) {
		t.Errorf("PRChangedFiles = %q, want %q", got, want)
	}
	if c := readFile(t, filepath.Join(dir, "calls")); strings.Contains(c, "pr diff") || strings.Contains(c, "graphql") {
		t.Errorf("gh calls = %q, want the REST file list only", c)
	}
}

func TestParsePRFileList(t *testing.T) {
	e := func(f string, p any) string {
		b, _ := json.Marshal(map[string]any{"f": f, "p": p})
		return string(b) + "\n"
	}
	for _, tc := range []struct {
		name, out string
		want      []string
		err       string
	}{
		{"plain files", e("a.md", nil) + e("pkg/b.go", nil), []string{"a.md", "pkg/b.go"}, ""},
		{"a rename lists both sides", e("docs/x.yaml", "infra/x.yaml"), []string{"docs/x.yaml", "infra/x.yaml"}, ""},
		{"each path once", e("b.md", "a.md") + e("a.md", nil), []string{"b.md", "a.md"}, ""},
		{"an empty previous name is no rename", e("a.md", ""), []string{"a.md"}, ""},
		{"names verbatim", e("dír é/naïve.md", nil), []string{"dír é/naïve.md"}, ""},
		{"no files", "", []string{}, ""},
		{"a file with no path", `{"f":null,"p":"x"}`, nil, "has no path"},
		{"a file with an empty path", `{"f":"","p":null}`, nil, "has no path"},
		{"not JSON", "gh: Not Found (HTTP 404)", nil, "unreadable file list"},
		{"at GitHub's cap", strings.Repeat(e("f", nil), 3000), nil, "lists at most 3000"},
	} {
		got, err := parsePRFileList(tc.out)
		if tc.err == "" {
			if err != nil || !reflect.DeepEqual(got, tc.want) {
				t.Errorf("%s: parsePRFileList = (%q, %v), want %q", tc.name, got, err, tc.want)
			}
			continue
		}
		if err == nil || got != nil || !strings.Contains(err.Error(), tc.err) {
			t.Errorf("%s: parsePRFileList = (%q, %v), want an error containing %q", tc.name, got, err, tc.err)
		}
	}
	if got, err := parsePRFileList(strings.Repeat(e("f", nil), 2999)); err != nil || len(got) != 1 {
		t.Errorf("just under the cap: (%d paths, %v), want the list", len(got), err)
	}
}

func TestPRRepoPath(t *testing.T) {
	for _, tc := range []struct {
		ref  prRef
		want string
	}{
		{prRef{number: 7, url: "https://github.com/o/r/pull/7", host: "github.com"}, "repos/o/r"},
		{prRef{number: 12, url: "https://ghe.example.com/my-org/my.repo/pull/12", host: "ghe.example.com"}, "repos/my-org/my.repo"},
		{prRef{number: 7, url: "https://github.com/o/r/pull/8"}, ""},
		{prRef{number: 7, url: "https://github.com/o/pull/7"}, ""},
		{prRef{number: 7, url: "https://github.com/o/r/issues/7"}, ""},
	} {
		got, err := prRepoPath(tc.ref)
		if got != tc.want || (err == nil) != (tc.want != "") {
			t.Errorf("prRepoPath(%+v) = (%q, %v), want %q", tc.ref, got, err, tc.want)
		}
	}
}
