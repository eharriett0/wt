package cli

import (
	"reflect"
	"slices"
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
		queries: []collide.Query{collide.QueryFor(arg, rel, exists || tracked, ws)},
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

// fakeBase is an argBase answering from fixed sets: exists from arguments as
// typed, tracked from repo paths.
func fakeBase(prefix string, exists, tracked []string) argBase {
	return argBase{
		prefix:  prefix,
		exists:  func(a string) bool { return slices.Contains(exists, a) },
		tracked: func(_, rel string) bool { return slices.Contains(tracked, rel) },
	}
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
			if len(got) != 1 || !reflect.DeepEqual(got[0].queries, []collide.Query{tc.want}) {
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

// #201 at the resolution layer: the top level is the whole repository from
// wherever it is named, and a path inside ANOTHER worktree of the repository is
// that worktree's repo path (they share the layout). The fixture paths exist
// nowhere, so this pins the placement, not the filesystem (the real-file cases
// are in check_paths_test.go).
func TestResolveCheckArgs_TopLevelAndOtherWorktrees(t *testing.T) {
	ws := []collide.Window{
		{Branch: "feat-b", Worktree: "/repo-b", Touched: []string{"pkg/svc/README.md", "internal/foo.go"}},
		{Branch: "nested", Worktree: "/repo/.worktrees/nested", Touched: []string{"x.go"}},
	}
	exact := func(p string) collide.Query { return collide.Query{Path: p, Mode: collide.MatchExact} }
	root := collide.RootQuery()
	cases := []struct {
		name string
		base argBase
		arg  string
		want []collide.Query
	}{
		{"`.` at the top", fakeBase("", nil, nil), ".", []collide.Query{root}},
		{"`./` at the top", fakeBase("", nil, nil), "./", []collide.Query{root}},
		{"$PWD at the top", fakeBase("", nil, nil), "/repo", []collide.Query{root}},
		{"`..` from a subdirectory", fakeBase("pkg/", nil, nil), "..", []collide.Query{root}},
		{"`.` in a subdirectory is that subdirectory (#181)", fakeBase("pkg/", nil, nil), ".", []collide.Query{exact("pkg")}},
		{"another worktree's top level", fakeBase("", nil, nil), "/repo-b", []collide.Query{root}},
		{"a file in another worktree, absolute", fakeBase("pkg/", nil, nil), "/repo-b/pkg/svc/README.md", []collide.Query{exact("pkg/svc/README.md")}},
		{"a file in another worktree, relative", fakeBase("pkg/", nil, nil), "../../repo-b/internal/foo.go", []collide.Query{exact("internal/foo.go")}},
		{"tracked in another worktree, touched by nobody", fakeBase("", nil, []string{"gone.go"}), "/repo-b/gone.go", []collide.Query{exact("gone.go")}},
		{"a worktree nested in this one: its own path, not ours", fakeBase("", nil, nil), ".worktrees/nested/x.go", []collide.Query{exact("x.go")}},
	}
	for _, tc := range cases {
		got := resolveCheckArgs([]string{tc.arg}, "/repo", tc.base, ws)
		if len(got) != 1 || got[0].outside || !reflect.DeepEqual(got[0].queries, tc.want) {
			t.Errorf("%s: resolveCheckArgs(%q, prefix %q) = %+v, want queries %+v", tc.name, tc.arg, tc.base.prefix, got, tc.want)
		}
	}
	// outside every worktree: refused by the caller, never a search that reads clear
	for _, tc := range []struct{ prefix, arg string }{
		{"", "/elsewhere/x.go"},
		{"", "../x.go"},
		{"pkg/", "../../.."},
		{"", "/repo-2/x.go"}, // a sibling sharing a name prefix is not inside /repo
		{"", "/"},
	} {
		got := resolveCheckArgs([]string{tc.arg}, "/repo", fakeBase(tc.prefix, []string{tc.arg}, nil), ws)
		if len(got) != 1 || !got[0].outside || got[0].queries != nil || got[0].namesRealPath() {
			t.Errorf("resolveCheckArgs(%q, prefix %q) = %+v, want outside, no queries", tc.arg, tc.prefix, got)
		}
	}
	// a bare repository's own directory holds no repo paths (#201 review)
	bws := []collide.Window{{Branch: "HEAD", Worktree: "/proj.git"}, {Branch: "feat", Worktree: "/proj.git/feat", Touched: []string{"x.md"}}}
	bb := fakeBase("", []string{"../HEAD"}, nil)
	bb.bare = []string{"/proj.git"}
	if got := resolveCheckArgs([]string{"../HEAD"}, "/proj.git/main", bb, bws); len(got) != 1 || !got[0].outside {
		t.Errorf("../HEAD beside a worktree inside a bare repo = %+v, want outside", got)
	}
	if got := resolveCheckArgs([]string{"../feat/x.md"}, "/proj.git/main", bb, bws); len(got) != 1 || !reflect.DeepEqual(got[0].queries, []collide.Query{exact("x.md")}) {
		t.Errorf("../feat/x.md = %+v, want exact x.md in the sibling worktree", got)
	}
}

// checkArgQueries decides an argument's queries from the repo paths it names
// (#201): every one exact when real; one search when it names nothing; the
// spellings windows touch it by only where names are case-insensitive.
func TestCheckArgQueries(t *testing.T) {
	ws := []collide.Window{{Worktree: "/repo-b", Touched: []string{"docs/NEW.md", "README.md"}}}
	exact := func(p string) collide.Query { return collide.Query{Path: p, Mode: collide.MatchExact} }
	fuzzy := func(p string) collide.Query { return collide.Query{Path: p, Mode: collide.MatchFuzzy} }
	cases := []struct {
		name   string
		arg    string
		rels   []string
		onDisk bool
		fold   bool
		want   []collide.Query
	}{
		{"a symlink and its target: both", "link.md", []string{"link.md", "README.md"}, true, false, []collide.Query{exact("link.md"), exact("README.md")}},
		{"one path", "README.md", []string{"README.md"}, true, false, []collide.Query{exact("README.md")}},
		{"the top level", ".", []string{collide.RootPath}, false, false, []collide.Query{collide.RootQuery()}},
		{"names nothing: one search, as typed", "x/new.md", []string{"x/new.md"}, false, false, []collide.Query{fuzzy("x/new.md")}},
		{"a dangling link that names nothing: one search", "dl", []string{"dl", "gone"}, false, false, []collide.Query{fuzzy("dl")}},
		{"case-insensitive: the spelling a window touches", "docs/new.md", []string{"docs/new.md"}, false, true, []collide.Query{exact("docs/NEW.md")}},
		{"case-sensitive: no folding, a search", "docs/new.md", []string{"docs/new.md"}, false, false, []collide.Query{fuzzy("docs/new.md")}},
		{"case-insensitive, nothing touched by any spelling: a search", "docs/zzz.md", []string{"docs/zzz.md"}, false, true, []collide.Query{fuzzy("docs/zzz.md")}},
		// a real path is a question about THAT path: folding never widens it
		{"case-insensitive, on disk: exact as placed", "readme.md", []string{"readme.md"}, true, true, []collide.Query{exact("readme.md")}},
		{"duplicates collapse", "a.md", []string{"a.md", "a.md"}, true, false, []collide.Query{exact("a.md")}},
	}
	for _, tc := range cases {
		if got := checkArgQueries(tc.arg, tc.rels, tc.onDisk, ws, tc.fold); !reflect.DeepEqual(got, tc.want) {
			t.Errorf("%s: checkArgQueries(%q, %q) = %+v, want %+v", tc.name, tc.arg, tc.rels, got, tc.want)
		}
	}
}

// #201: the top level is never a typo, and a path outside every worktree is
// refused by outsideCheckPaths (naming where it resolves), not by the typo
// guard.
func TestUnknownAndOutsideCheckPaths(t *testing.T) {
	ws := []collide.Window{{Worktree: "/repo-b"}}
	args := []checkArg{
		{arg: "./", queries: []collide.Query{collide.RootQuery()}},
		{arg: "/tmp/x.md", outside: true, resolved: "/private/tmp/x.md"},
		{arg: "/elsewhere/y.md", outside: true, resolved: "/elsewhere/y.md"},
		{arg: "docs/typo.md", queries: []collide.Query{{Path: "docs/typo.md", Mode: collide.MatchFuzzy}}},
	}
	if got := unknownCheckPaths(args, ws); !reflect.DeepEqual(got, []string{"docs/typo.md"}) {
		t.Errorf("unknownCheckPaths = %q, want only the typo", got)
	}
	want := []string{"/tmp/x.md (resolves to /private/tmp/x.md)", "/elsewhere/y.md"}
	if got := outsideCheckPaths(args); !reflect.DeepEqual(got, want) {
		t.Errorf("outsideCheckPaths = %q, want %q", got, want)
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
		if len(got) != 1 || !reflect.DeepEqual(got[0].queries, []collide.Query{tc.want}) || got[0].arg != tc.wantArg {
			t.Errorf("%s: resolveCheckArgs(%q) = %+v, want query %+v, arg %q", tc.name, tc.arg, got, tc.want, tc.wantArg)
		}
	}
}
