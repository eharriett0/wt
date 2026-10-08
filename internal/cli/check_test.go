package cli

import (
	"reflect"
	"testing"

	"github.com/eharriett0/wt/internal/collide"
)

// argFor builds the checkArg resolveCheckArgs would, from facts instead of a
// live repo: rel is the argument read relative to the cwd ("" = it names no
// location in the repo).
func argFor(arg, rel string, exists, tracked bool, ws []collide.Window) checkArg {
	return checkArg{
		arg:     arg,
		query:   collide.QueryFor(arg, rel, exists || tracked, ws),
		exists:  exists,
		tracked: tracked,
	}
}

func TestUnknownCheckPaths(t *testing.T) {
	ws := []collide.Window{
		{Branch: "feat-b", Worktree: "/w/b", Touched: []string{
			"pkg/svc/README.md", "envs/landru/configs/kiali/netpol.yaml", "docs/NEW.md",
		}},
	}
	cases := []struct {
		name    string
		a       checkArg
		unknown bool
	}{
		{"exists here", argFor("pkg/svc/README.md", "pkg/svc/README.md", true, false, ws), false},
		{"deleted here but tracked", argFor("old/gone.go", "old/gone.go", false, true, ws), false},
		{"exists only on another branch, at that exact path", argFor("docs/NEW.md", "docs/NEW.md", false, false, ws), false},
		{"names nothing but suffix-matches a touched dir (#154 fuzzy)", argFor("configs/kiali", "configs/kiali", false, false, ws), false},
		{"bare name is a fuzzy query, never flagged", argFor("nothing.go", "nothing.go", false, false, ws), false},
		{"a typo with a directory component is refused", argFor("pkg/svc/READNE.md", "pkg/svc/READNE.md", false, false, ws), true},
		{"a zsh non-word-split pair is refused", argFor("a.go b.go", "a.go b.go", false, false, ws), true},
	}
	for _, tc := range cases {
		got := unknownCheckPaths([]checkArg{tc.a}, ws)
		if (len(got) == 1) != tc.unknown {
			t.Errorf("%s: unknownCheckPaths(%q) = %v, want unknown=%v", tc.name, tc.a.arg, got, tc.unknown)
		}
	}
}

func TestCheckArgs_RootFileIsExact(t *testing.T) {
	// #181 at the `wt check` layer: the root README.md exists, so the argument
	// becomes an EXACT query and no longer reaches another window's
	// pkg/svc/README.md; the bare name of a file that is nowhere here keeps the
	// fuzzy convenience.
	ws := []collide.Window{
		{Branch: "feat-a", Worktree: "/w/a", Touched: []string{"README.md"}},
		{Branch: "feat-b", Worktree: "/w/b", Touched: []string{"pkg/svc/README.md", "internal/foo.go"}},
	}
	args := []checkArg{
		argFor("README.md", "README.md", true, false, ws),
		argFor("foo.go", "foo.go", false, false, ws),
	}
	want := []collide.Query{
		{Path: "README.md", Mode: collide.MatchExact},
		{Path: "foo.go", Mode: collide.MatchFuzzy},
	}
	qs := checkQueries(args)
	if !reflect.DeepEqual(qs, want) {
		t.Fatalf("checkQueries = %+v, want %+v", qs, want)
	}
	var got []string
	for _, cf := range collide.CheckPaths(ws, "/w/a", qs) {
		got = append(got, cf.Path+"→"+cf.MatchedFile)
	}
	if w := []string{"foo.go→internal/foo.go"}; !reflect.DeepEqual(got, w) {
		t.Errorf("conflicts = %v, want %v (README.md must not reach pkg/svc/README.md)", got, w)
	}
}

func TestRepoRelativePatch(t *testing.T) {
	// #181: apply_patch paths are relative to Codex's cwd. Matching is exact, so
	// a session started in pkg/ that patches svc/README.md must be checked (and
	// have its pending hunks read) as pkg/svc/README.md.
	files := []codexPatchFile{
		{path: "svc/README.md"},
		{path: "old.go", newPath: "../moved/new.go"},
		{path: "../../outside.go"},
		{path: "/repo/abs/x.go"},
	}
	got := repoRelativePatch(files, "/repo", "pkg/")
	want := []string{"pkg/svc/README.md", "pkg/old.go", "moved/new.go", "", "abs/x.go"}
	var have []string
	for _, f := range got {
		have = append(have, f.path)
		if f.newPath != "" {
			have = append(have, f.newPath)
		}
	}
	if !reflect.DeepEqual(have, want) {
		t.Errorf("repoRelativePatch paths = %q, want %q", have, want)
	}
	if p := patchPaths(got); !reflect.DeepEqual(p, []string{"pkg/svc/README.md", "pkg/old.go", "moved/new.go", "abs/x.go"}) {
		t.Errorf("patchPaths must drop the outside path: %q", p)
	}
	if files[0].path != "svc/README.md" {
		t.Error("repoRelativePatch must not mutate its input")
	}
}
