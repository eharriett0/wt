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

// #167: `wt adopt <pr#>` compares a local branch to the PR's headRefOid, so the
// head read must refuse anything that is not a branch plus a full object id.
// "null null" is the shape a pre-#168 style `.headRefName` query would print for
// a missing field; it must never parse as a PR head.
func TestParsePRHead(t *testing.T) {
	sha1 := "9f8e7d6c5b4a39281706f5e4d3c2b1a098765432"
	sha256 := "9f8e7d6c5b4a39281706f5e4d3c2b1a0987654329f8e7d6c5b4a39281706f5e4"
	cases := []struct {
		out, branch, oid string
		ok               bool
	}{
		{"bot/image " + sha1, "bot/image", sha1, true},
		{"bot/image " + sha1 + "\n", "bot/image", sha1, true},
		{"feat-42-x " + strings.ToUpper(sha1), "feat-42-x", sha1, true}, // normalised to lower case
		{"bot/image " + sha256, "bot/image", sha256, true},              // SHA-256 repo
		{"", "", "", false},                             // the `// empty` output when a field is missing
		{"null null", "", "", false},                    // the placeholder shape: never a PR head
		{"bot/image null", "", "", false},               // branch without a commit
		{"bot/image", "", "", false},                    // one field
		{"bot/image 9f8e7d6c", "", "", false},           // abbreviated id: not comparable to a local tip
		{"bot/image " + sha1[:39] + "g", "", "", false}, // not hex
		{"a b " + sha1, "", "", false},                  // three fields
	}
	for _, c := range cases {
		b, o, ok := parsePRHead(c.out)
		if b != c.branch || o != c.oid || ok != c.ok {
			t.Errorf("parsePRHead(%q) = (%q, %q, %v), want (%q, %q, %v)", c.out, b, o, ok, c.branch, c.oid, c.ok)
		}
	}
}
