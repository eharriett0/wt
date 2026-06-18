package gitx

import "testing"

func TestDefaultBranchFromRef(t *testing.T) {
	cases := map[string]string{
		"origin/main":          "main",
		"origin/master":        "master",
		"origin/trunk":         "trunk",
		"origin/feature/x":     "feature/x",
		"main":                 "main",
		"":                     "",
		"  origin/develop  \n": "develop",
	}
	for in, want := range cases {
		if got := DefaultBranchFromRef(in); got != want {
			t.Errorf("DefaultBranchFromRef(%q) = %q, want %q", in, got, want)
		}
	}
}
