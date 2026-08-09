package cli

import (
	"flag"
	"io"
	"testing"
)

func eqStrs(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}

// mergeLikeFlags mirrors cmdMergePR: all-bool flags + a `--` passthrough.
func mergeLikeFlags() (*flag.FlagSet, *bool, *bool) {
	fs := flag.NewFlagSet("merge-pr", flag.ContinueOnError)
	fs.SetOutput(io.Discard)
	keep := fs.Bool("keep", false, "")
	confirm := fs.Bool("confirm-deploy", false, "")
	return fs, keep, confirm
}

func TestParseInterspersed_flagBeforePositional(t *testing.T) {
	fs, _, confirm := mergeLikeFlags()
	pos, pass, err := parseInterspersed(fs, []string{"--confirm-deploy", "968"})
	if err != nil {
		t.Fatalf("unexpected err: %v", err)
	}
	if !eqStrs(pos, []string{"968"}) || len(pass) != 0 || !*confirm {
		t.Fatalf("pos=%v pass=%v confirm=%v", pos, pass, *confirm)
	}
}

// The regression: a flag AFTER the positional must still be parsed (Go's flag
// package would otherwise stop at "968" and drop --confirm-deploy — the exact
// bug where the prod gate wasn't acknowledged and --confirm-deploy leaked to gh).
func TestParseInterspersed_flagAfterPositional(t *testing.T) {
	fs, _, confirm := mergeLikeFlags()
	pos, pass, err := parseInterspersed(fs, []string{"968", "--confirm-deploy"})
	if err != nil {
		t.Fatalf("unexpected err: %v", err)
	}
	if !eqStrs(pos, []string{"968"}) || len(pass) != 0 {
		t.Fatalf("pos=%v pass=%v", pos, pass)
	}
	if !*confirm {
		t.Fatal("--confirm-deploy after the positional was dropped (regression)")
	}
}

func TestParseInterspersed_passthroughAfterDashDash(t *testing.T) {
	fs, keep, _ := mergeLikeFlags()
	pos, pass, err := parseInterspersed(fs, []string{"968", "--keep", "--", "--admin", "-f"})
	if err != nil {
		t.Fatalf("unexpected err: %v", err)
	}
	if !eqStrs(pos, []string{"968"}) || !*keep {
		t.Fatalf("pos=%v keep=%v", pos, *keep)
	}
	if !eqStrs(pass, []string{"--admin", "-f"}) {
		t.Fatalf("passthrough=%v, want [--admin -f]", pass)
	}
}

// claim-shaped: a VALUE-taking flag (--epic <id>) after the positional must
// consume its value, not be treated as two positionals.
func TestParseInterspersed_valueFlagAfterPositional(t *testing.T) {
	fs := flag.NewFlagSet("claim", flag.ContinueOnError)
	fs.SetOutput(io.Discard)
	epic := fs.String("epic", "", "")
	pos, _, err := parseInterspersed(fs, []string{"913", "--epic", "43"})
	if err != nil {
		t.Fatalf("unexpected err: %v", err)
	}
	if !eqStrs(pos, []string{"913"}) {
		t.Fatalf("pos=%v, want [913]", pos)
	}
	if *epic != "43" {
		t.Fatalf("epic=%q, want 43 (value flag after positional was dropped)", *epic)
	}
}

func TestParseInterspersed_multiplePositionals(t *testing.T) {
	fs, _, _ := mergeLikeFlags()
	pos, _, err := parseInterspersed(fs, []string{"a", "--keep", "b"})
	if err != nil {
		t.Fatalf("unexpected err: %v", err)
	}
	if !eqStrs(pos, []string{"a", "b"}) {
		t.Fatalf("pos=%v, want [a b]", pos)
	}
}

func TestParseInterspersed_noPositional(t *testing.T) {
	fs, keep, _ := mergeLikeFlags()
	pos, _, err := parseInterspersed(fs, []string{"--keep"})
	if err != nil {
		t.Fatalf("unexpected err: %v", err)
	}
	if len(pos) != 0 || !*keep {
		t.Fatalf("pos=%v keep=%v", pos, *keep)
	}
}

func TestParseInterspersed_unknownFlagErrors(t *testing.T) {
	fs, _, _ := mergeLikeFlags()
	if _, _, err := parseInterspersed(fs, []string{"968", "--nope"}); err == nil {
		t.Fatal("expected error on undefined flag, got nil")
	}
}

func TestParseInterspersed_empty(t *testing.T) {
	fs, _, _ := mergeLikeFlags()
	pos, pass, err := parseInterspersed(fs, nil)
	if err != nil || len(pos) != 0 || len(pass) != 0 {
		t.Fatalf("pos=%v pass=%v err=%v", pos, pass, err)
	}
}

func TestParseCheckArgs(t *testing.T) {
	// known flags interspersed with paths
	paths, stale, diff, js, ma, unk := parseCheckArgs([]string{"--include-stale", "a.go", "--json", "b.go"})
	if unk != "" || !stale || !js || diff || ma != "" {
		t.Fatalf("flags: unk=%q stale=%v diff=%v json=%v maxAge=%q", unk, stale, diff, js, ma)
	}
	if len(paths) != 2 || paths[0] != "a.go" || paths[1] != "b.go" {
		t.Fatalf("paths = %v, want [a.go b.go]", paths)
	}
	// #30: a typo'd flag is CAPTURED as unknown, never silently treated as a path
	if _, _, _, _, _, u := parseCheckArgs([]string{"--includ-stale", "a.go"}); u != "--includ-stale" {
		t.Errorf("unknown flag not captured: %q", u)
	}
	// "--" escapes a dash-prefixed path
	if p, _, _, _, _, u := parseCheckArgs([]string{"--", "-weird.go"}); u != "" || len(p) != 1 || p[0] != "-weird.go" {
		t.Errorf("-- escape: paths=%v unk=%q", p, u)
	}
	// bare "-" is a path, not an unknown flag
	if p, _, _, _, _, u := parseCheckArgs([]string{"-"}); u != "" || len(p) != 1 || p[0] != "-" {
		t.Errorf("bare dash: paths=%v unk=%q", p, u)
	}
	// #48: --max-age captured in both value forms; still yields the path
	if p, _, _, _, m, u := parseCheckArgs([]string{"--max-age", "4d", "x.go"}); u != "" || m != "4d" || len(p) != 1 || p[0] != "x.go" {
		t.Errorf("--max-age D: maxAge=%q paths=%v unk=%q", m, p, u)
	}
	if _, _, _, _, m, _ := parseCheckArgs([]string{"--max-age=36h", "x.go"}); m != "36h" {
		t.Errorf("--max-age=D: maxAge=%q, want 36h", m)
	}
}

func TestAnyDeployPath(t *testing.T) {
	deploy := []string{"infrastructure/**", "envs/**"}
	cases := []struct {
		name  string
		files []string
		globs []string
		want  bool
	}{
		{"docs+scripts only → not a deploy", []string{"scripts/x.sh", "doc/y.md"}, deploy, false},
		{"one infra file → deploy", []string{"doc/plan.md", "infrastructure/controllers/istio-cni/helmrelease.yaml"}, deploy, true},
		{"one envs file → deploy", []string{"envs/landru/tofu/aws/vars.tfvars"}, deploy, true},
		{"root readme only → not a deploy", []string{"README.md"}, deploy, false},
		{"no files → no match", nil, deploy, false},
		{"* stays within a segment", []string{"doc/README.md"}, []string{"*.md"}, false},
		{"whitespace trimmed both sides", []string{"  infrastructure/x.yaml  "}, []string{"  infrastructure/**  "}, true},
		{"blank entries ignored", []string{"", "  ", "doc/z.md"}, deploy, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := anyDeployPath(tc.files, tc.globs); got != tc.want {
				t.Errorf("anyDeployPath(%v, %v) = %v, want %v", tc.files, tc.globs, got, tc.want)
			}
		})
	}
}
