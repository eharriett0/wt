package cli

import (
	"reflect"
	"strings"
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
			"pkg/svc/README.md", "envs/landru/configs/kiali/netpol.yaml", "docs/NEW.md", "TOP.md",
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
		// typed in a subdirectory, the argument and the path it names differ: the
		// guard must ask about the path (#181 review), not the text typed
		{"../ to a path that exists only on another branch", argFor("../TOP.md", "TOP.md", false, false, ws), false},
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

// fakeBase is an argBase answering from fixed sets of arguments as typed.
func fakeBase(prefix string, exists, tracked []string) argBase {
	in := func(set []string) func(string) bool {
		return func(a string) bool {
			for _, s := range set {
				if s == a {
					return true
				}
			}
			return false
		}
	}
	return argBase{prefix: prefix, exists: in(exists), tracked: in(tracked)}
}

// TestResolveCheckArgs pins how an argument becomes a query, given where it is
// read from (#181): the cwd's prefix for `wt check`, the root for wt_check.
func TestResolveCheckArgs(t *testing.T) {
	ws := []collide.Window{
		{Branch: "feat-b", Worktree: "/repo-b", Touched: []string{
			"pkg/svc/README.md", "internal/foo.go", "TOP.md", "envs/landru/configs/kiali/netpol.yaml",
		}},
	}
	exact := func(p string) collide.Query { return collide.Query{Path: p, Mode: collide.MatchExact} }
	fuzzy := func(p string) collide.Query { return collide.Query{Path: p, Mode: collide.MatchFuzzy} }
	cases := []struct {
		name string
		base argBase
		arg  string
		want collide.Query
	}{
		{"root file at the root", fakeBase("", []string{"README.md"}, nil), "README.md", exact("README.md")},
		{"same name in pkg/svc/ is pkg/svc/README.md", fakeBase("pkg/svc/", []string{"README.md"}, nil), "README.md", exact("pkg/svc/README.md")},
		{"subdirectory-relative path", fakeBase("pkg/", []string{"svc/README.md"}, nil), "svc/README.md", exact("pkg/svc/README.md")},
		{"../ out of a subdirectory", fakeBase("pkg/svc/", []string{"../../README.md"}, nil), "../../README.md", exact("README.md")},
		{"./ prefix is cleaned", fakeBase("", []string{"./README.md"}, nil), "./README.md", exact("README.md")},
		{"deleted but tracked", fakeBase("", nil, []string{"gone.go"}), "gone.go", exact("gone.go")},
		{"exists only on another branch, exactly there", fakeBase("", nil, nil), "TOP.md", exact("TOP.md")},
		{"../ to a path that exists only on another branch", fakeBase("docs/", nil, nil), "../TOP.md", exact("TOP.md")},
		{"bare name that is nothing here: a search, as typed", fakeBase("", nil, nil), "foo.go", fuzzy("foo.go")},
		{"bare name in a subdirectory: a search, as typed", fakeBase("pkg/", nil, nil), "foo.go", fuzzy("foo.go")},
		{"partial directory that names nothing: a search (#154)", fakeBase("", nil, nil), "configs/kiali/", fuzzy("configs/kiali/")},
		{"absolute path inside the repo", fakeBase("pkg/", []string{"/repo/README.md"}, nil), "/repo/README.md", exact("README.md")},
		{"surrounding whitespace", fakeBase("", []string{"README.md"}, nil), "  README.md ", exact("README.md")},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := resolveCheckArgs([]string{tc.arg}, "/repo", tc.base, ws)
			if len(got) != 1 || got[0].query != tc.want {
				t.Fatalf("resolveCheckArgs(%q, prefix %q) = %+v, want query %+v", tc.arg, tc.base.prefix, got, tc.want)
			}
			if got[0].arg != strings.TrimSpace(tc.arg) {
				t.Errorf("arg = %q, want it as typed (trimmed)", got[0].arg)
			}
		})
	}
	if got := resolveCheckArgs([]string{"", "  "}, "/repo", fakeBase("", nil, nil), ws); len(got) != 0 {
		t.Errorf("blank arguments must be skipped, got %+v", got)
	}
}

// A file name can begin or end with a space, and git reports it verbatim
// (#200), so the pre-push and pre-commit checks ask about it as it is. An
// argument that names such a path as typed keeps its spaces; one that names
// nothing as typed is trimmed, as before.
func TestResolveCheckArgs_SpacesInARealName(t *testing.T) {
	ws := []collide.Window{{Branch: "feat-b", Worktree: "/repo-b", Touched: []string{" top.md"}}}
	exact := func(p string) collide.Query { return collide.Query{Path: p, Mode: collide.MatchExact} }
	cases := []struct {
		name, arg string
		base      argBase
		want      collide.Query
		wantArg   string
	}{
		{"on disk with a leading space", " lead.md", fakeBase("", []string{" lead.md"}, nil), exact(" lead.md"), " lead.md"},
		{"on disk with a trailing space, in a subdirectory", "trail.md ", fakeBase("pkg/", []string{"trail.md "}, nil), exact("pkg/trail.md "), "trail.md "},
		{"tracked, not on disk", " gone.md", fakeBase("", nil, []string{" gone.md"}), exact(" gone.md"), " gone.md"},
		{"only on another branch, exactly there", " top.md", fakeBase("", nil, nil), exact(" top.md"), " top.md"},
		{"names nothing as typed: trimmed", " README.md ", fakeBase("", []string{"README.md"}, nil), exact("README.md"), "README.md"},
	}
	for _, tc := range cases {
		got := resolveCheckArgs([]string{tc.arg}, "/repo", tc.base, ws)
		if len(got) != 1 || got[0].query != tc.want || got[0].arg != tc.wantArg {
			t.Errorf("%s: resolveCheckArgs(%q) = %+v, want query %+v, arg %q", tc.name, tc.arg, got, tc.want, tc.wantArg)
		}
	}
}
