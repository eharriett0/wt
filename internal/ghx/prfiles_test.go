package ghx

import (
	"reflect"
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

// The deploy gate's file list: gh prints a quoted name for a file whose name
// git quotes, and PRChangedFiles hands the caller the name itself.
func TestPRChangedFiles_UnquotesNames(t *testing.T) {
	fakeGh(t, `[ "$1 $2" = "pr diff" ] || exit 1
printf '%s\n' 'infra/plain.yaml' '"infra/caf\303\251.yaml"' 'a b.md' '"q\"uote.md"'
`)
	got, err := PRChangedFiles("7")
	if err != nil {
		t.Fatal(err)
	}
	if want := []string{"infra/plain.yaml", "infra/café.yaml", "a b.md", `q"uote.md`}; !reflect.DeepEqual(got, want) {
		t.Errorf("PRChangedFiles = %q, want %q", got, want)
	}
}
