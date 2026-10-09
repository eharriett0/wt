package ghx

import (
	"strings"
	"testing"
)

func TestParseHeadOid(t *testing.T) {
	sha1 := "0123456789abcdef0123456789abcdef01234567"
	sha256 := strings.Repeat("ab", 32)
	cases := []struct {
		out, want string
		ok        bool
	}{
		{sha1, sha1, true},
		{sha1 + "\n", sha1, true},
		{strings.ToUpper(sha1), sha1, true},
		{sha256, sha256, true},
		{"", "", false},     // `// empty`: the PR has no head commit
		{"null", "", false}, // never a parsed placeholder (#168)
		{sha1[:39], "", false},
		{sha1 + "0", "", false},
		{"g123456789abcdef0123456789abcdef01234567", "", false},
		{sha1 + " " + sha1, "", false},
	}
	for _, tc := range cases {
		got, ok := parseHeadOid(tc.out)
		if got != tc.want || ok != tc.ok {
			t.Errorf("parseHeadOid(%q) = (%q, %v), want (%q, %v)", tc.out, got, ok, tc.want, tc.ok)
		}
	}
}
