package cli

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"unicode"
)

func TestParseClaudeEdit(t *testing.T) {
	edit := `{"cwd":"/repo","tool_name":"Edit","tool_input":{"file_path":"/repo/a.go","old_string":"x","new_string":"y"}}`
	if cwd, f, ok := parseClaudeEdit([]byte(edit)); !ok || cwd != "/repo" || f != "/repo/a.go" {
		t.Errorf("Edit: (%q,%q,%v)", cwd, f, ok)
	}
	write := `{"cwd":"/repo","tool_name":"Write","tool_input":{"file_path":"/repo/b.go","content":"..."}}`
	if _, f, ok := parseClaudeEdit([]byte(write)); !ok || f != "/repo/b.go" {
		t.Errorf("Write: (%q,%v)", f, ok)
	}
	multi := `{"tool_name":"MultiEdit","tool_input":{"file_path":"c.go","edits":[]}}`
	if _, f, ok := parseClaudeEdit([]byte(multi)); !ok || f != "c.go" {
		t.Errorf("MultiEdit: (%q,%v)", f, ok)
	}
	// a real name can end with a space: the path is used as sent (#200)
	trail := `{"tool_name":"Edit","tool_input":{"file_path":"/repo/trail.md "}}`
	if _, f, ok := parseClaudeEdit([]byte(trail)); !ok || f != "/repo/trail.md " {
		t.Errorf("trailing space: (%q,%v), want the path untrimmed", f, ok)
	}
	// non-editing tools + empty file_path + garbage → not relevant
	for _, s := range []string{
		`{"tool_name":"Bash","tool_input":{"command":"ls"}}`,
		`{"tool_name":"Read","tool_input":{"file_path":"/x"}}`,
		`{"tool_name":"Edit","tool_input":{"file_path":"  "}}`,
		`not json`,
	} {
		if _, _, ok := parseClaudeEdit([]byte(s)); ok {
			t.Errorf("should be irrelevant: %s", s)
		}
	}
}

// graded wraps entries as regradePending's output, all confirmed or all not.
func graded(confirmed bool, es ...CheckEntry) []pendingGrade {
	var out []pendingGrade
	for _, e := range es {
		out = append(out, pendingGrade{entry: e, confirmed: confirmed})
	}
	return out
}

func TestClaudeDecision(t *testing.T) {
	// no HIGH → nothing emitted
	if out, has := claudeDecision("a.go", nil, false); has || out != "" {
		t.Errorf("empty: %q,%v", out, has)
	}
	entries := []CheckEntry{
		{Path: "a.go", Window: "#42", Liveness: "uncommitted edits"},
		{Path: "a.go", Window: "#42", Liveness: "uncommitted edits"}, // dup window
		{Path: "a.go", Window: "feat/x", Liveness: "open PR #7"},
	}
	high := graded(true, entries...)
	// advisory (confirmed overlap) → additionalContext, no permissionDecision
	out, has := claudeDecision("a.go", high, false)
	if !has {
		t.Fatal("advisory: expected output")
	}
	var adv struct {
		HSO struct {
			AdditionalContext  string `json:"additionalContext"`
			PermissionDecision string `json:"permissionDecision"`
			HookEventName      string `json:"hookEventName"`
		} `json:"hookSpecificOutput"`
	}
	if err := json.Unmarshal([]byte(out), &adv); err != nil {
		t.Fatalf("advisory JSON: %v", err)
	}
	if adv.HSO.PermissionDecision != "" {
		t.Error("advisory must NOT deny")
	}
	if adv.HSO.HookEventName != "PreToolUse" {
		t.Errorf("hookEventName = %q", adv.HSO.HookEventName)
	}
	if !strings.Contains(adv.HSO.AdditionalContext, "#42") || !strings.Contains(adv.HSO.AdditionalContext, "feat/x") {
		t.Errorf("context missing windows: %q", adv.HSO.AdditionalContext)
	}
	if strings.Count(adv.HSO.AdditionalContext, "#42") != 1 {
		t.Error("window #42 should be deduped")
	}
	// confirmed overlap wording asserts an overlap; file-level must NOT (#108)
	if !strings.Contains(adv.HSO.AdditionalContext, "OVERLAPS") {
		t.Errorf("confirmed-overlap message should say it overlaps: %q", adv.HSO.AdditionalContext)
	}
	// block → permissionDecision deny
	out, _ = claudeDecision("a.go", high, true)
	var blk struct {
		HSO struct {
			PermissionDecision       string `json:"permissionDecision"`
			PermissionDecisionReason string `json:"permissionDecisionReason"`
		} `json:"hookSpecificOutput"`
	}
	if err := json.Unmarshal([]byte(out), &blk); err != nil {
		t.Fatalf("block JSON: %v", err)
	}
	if blk.HSO.PermissionDecision != "deny" || blk.HSO.PermissionDecisionReason == "" {
		t.Errorf("block: %+v", blk.HSO)
	}

	// #108: file-level wording must NOT claim hunk overlap (would contradict wt check)
	fl, _ := claudeDecision("a.go", graded(false, entries...), false)
	var flOut struct {
		HSO struct {
			AdditionalContext string `json:"additionalContext"`
		} `json:"hookSpecificOutput"`
	}
	if err := json.Unmarshal([]byte(fl), &flOut); err != nil {
		t.Fatalf("file-level JSON: %v", err)
	}
	if strings.Contains(flOut.HSO.AdditionalContext, "OVERLAPS") || strings.Contains(flOut.HSO.AdditionalContext, "overlapping hunks") {
		t.Errorf("file-level message must not claim overlap: %q", flOut.HSO.AdditionalContext)
	}
	if !strings.Contains(flOut.HSO.AdditionalContext, "file-level") {
		t.Errorf("file-level message should say file-level: %q", flOut.HSO.AdditionalContext)
	}
}

// #201 review: claudeDecision names every window once by identity (worktree,
// else label: namesakes are two windows, #193), each with its own grade when
// the grades differ; the deny predicate is unchanged (any kept entry denies
// under block).
func TestClaudeDecision_NamesEveryWindowWithItsOwnGrade(t *testing.T) {
	g := func(window, wt string, confirmed bool) pendingGrade {
		return pendingGrade{entry: CheckEntry{Path: "a.go", Window: window, Liveness: "live", otherWorktree: wt}, confirmed: confirmed}
	}
	cases := []struct {
		name    string
		in      []pendingGrade
		want    []string // substrings of the message, in order
		notWant []string
	}{
		{"namesakes are two windows", []pendingGrade{g("#5", "/p1/x", true), g("#5", "/p2/x", true)},
			[]string{"OVERLAPS a region that #5 [live], #5 [live] is"}, nil},
		{"one worktree twice collapses", []pendingGrade{g("#5", "/p1/x", true), g("#5", "/p1/x", true)},
			[]string{"that #5 [live] is"}, []string{"#5 [live], #5"}},
		{"no worktree: the label identifies", []pendingGrade{g("w", "", false), g("w", "", false)},
			[]string{"edited by w [live] — file-level"}, []string{"w [live], w"}},
		{"mixed: each window its own tag, no OVERLAPS claim", []pendingGrade{g("b", "/w/b", true), g("c", "/w/c", false)},
			[]string{"edited by b [live] (overlapping hunks), c [live] (hunk overlap not computed)"}, []string{"OVERLAPS", "file-level"}},
		{"mixed namesakes keep both tags", []pendingGrade{g("#5", "/p1/x", false), g("#5", "/p2/x", true)},
			[]string{"#5 [live] (hunk overlap not computed), #5 [live] (overlapping hunks)"}, nil},
		{"all file-level keeps its wording", []pendingGrade{g("b", "/w/b", false), g("c", "/w/c", false)},
			[]string{"edited by b [live], c [live] — file-level heads-up"}, []string{"OVERLAPS"}},
	}
	for _, tc := range cases {
		for _, block := range []bool{false, true} {
			out, has := claudeDecision("a.go", tc.in, block)
			if !has {
				t.Fatalf("%s: no output", tc.name)
			}
			ctx, dec, reason := decodeDecision(t, out)
			msg := ctx
			if block {
				msg = reason
				if dec != "deny" || ctx != "" {
					t.Errorf("%s: block must deny (any kept entry, as before): decision=%q ctx=%q", tc.name, dec, ctx)
				}
			} else if dec != "" {
				t.Errorf("%s: advisory must not deny, got %q", tc.name, dec)
			}
			for _, w := range tc.want {
				if !strings.Contains(msg, w) {
					t.Errorf("%s (block=%v): message lacks %q:\n%s", tc.name, block, w, msg)
				}
			}
			for _, w := range tc.notWant {
				if strings.Contains(msg, w) {
					t.Errorf("%s (block=%v): message has %q:\n%s", tc.name, block, w, msg)
				}
			}
		}
	}
}

// #199 review: the pre-edit hook grades the file the edit produces, so it must
// produce what Claude Code does: a Write's content; an Edit's replacement, which
// the tool refuses for an old_string that is absent or (without replace_all) not
// unique; MultiEdit's edits in turn; an empty new_string taking the deleted
// text's newline with it; a CRLF file edited as LF and written back as CRLF
// (both measured against Claude Code's own Edit and Write).
func TestClaudePendingContent(t *testing.T) {
	payload := func(tool string, input map[string]any) []byte {
		b, _ := json.Marshal(map[string]any{"tool_name": tool, "tool_input": input})
		return b
	}
	edit := func(old, new string, all bool) []byte {
		return payload("Edit", map[string]any{"file_path": "f", "old_string": old, "new_string": new, "replace_all": all})
	}
	multi := func(edits ...map[string]any) []byte {
		return payload("MultiEdit", map[string]any{"file_path": "f", "edits": edits})
	}
	e := func(old, new string) map[string]any { return map[string]any{"old_string": old, "new_string": new} }
	const file = "a\nb\nc\nb\n"
	cases := []struct {
		name    string
		raw     []byte
		content string
		exists  bool
		want    string
		ok      bool
	}{
		{"edit", edit("c\n", "C\n", false), file, true, "a\nb\nC\nb\n", true},
		{"old_string not unique: the tool refuses", edit("b", "B", false), file, true, "", false},
		{"replace_all", edit("b", "B", true), file, true, "a\nB\nc\nB\n", true},
		{"old_string absent", edit("zz", "Z", false), file, true, "", false},
		{"old_string empty", edit("", "Z", false), file, true, "", false},
		{"no change", edit("c", "c", false), file, true, "", false},
		{"deleting a line's text takes its newline", edit("c", "", false), file, true, "a\nb\nb\n", true},
		{"deleting the whole line, newline included", edit("c\n", "", false), file, true, "a\nb\nb\n", true},
		{"deleting text mid-line keeps the newline", edit("x", "", false), "axb\n", true, "ab\n", true},
		{"deleting a last line with no newline", edit("\nz", "", false), "a\nz", true, "a", true},
		{"replace_all deletes only the ones a newline follows with it", edit("b", "", true), "b\nb", true, "b", true},
		{"multiedit, in turn", multi(e("a\n", "A\n"), e("A\nb", "AB")), file, true, "AB\nc\nb\n", true},
		{"multiedit, a refused edit refuses all", multi(e("a\n", "A\n"), e("zz", "Z")), file, true, "", false},
		{"crlf: matched as LF, written as CRLF", edit("b\nc", "B\nC", false), "a\r\nb\r\nc\r\n", true, "a\r\nB\r\nC\r\n", true},
		{"crlf: an edit's own CRLFs read as LF", edit("b\r\nc\r\n", "B\r\n", false), "a\r\nb\r\nc\r\n", true, "a\r\nB\r\n", true},
		{"a CR in an edit of an LF file: not predicted", edit("b\r\n", "B\n", false), "a\nb\nc\n", true, "", false},
		{"mixed line endings: not predicted", edit("c", "C", false), "a\r\nb\nc\n", true, "", false},
		{"a lone CR: not predicted", edit("c", "C", false), "a\rb\nc\n", true, "", false},
		{"no file to edit", edit("c", "C", false), "", false, "", false},
		{"write: its content", payload("Write", map[string]any{"file_path": "f", "content": "new\n"}), file, true, "new\n", true},
		{"write onto a CRLF file: as given", payload("Write", map[string]any{"file_path": "f", "content": "a\nB\n"}), "a\r\nb\r\n", true, "a\nB\n", true},
		{"write creating a file", payload("Write", map[string]any{"file_path": "f", "content": "n\n"}), "", false, "n\n", true},
		{"another tool", payload("NotebookEdit", map[string]any{"file_path": "f"}), file, true, "", false},
		{"garbage", []byte("{"), file, true, "", false},
	}
	for _, c := range cases {
		got, ok := claudePendingContent(c.raw, c.content, c.exists)
		if ok != c.ok || got != c.want {
			t.Errorf("%s: claudePendingContent = %q, %v; want %q, %v", c.name, got, ok, c.want, c.ok)
		}
	}
}

func TestRepoRelativePath(t *testing.T) {
	cases := []struct {
		name, prefix, file, want string
	}{
		{"relative at the root stays as-is (cleaned)", "", "internal/./a.go", "internal/a.go"},
		// #181: matching is exact now, so a relative path must be read from the
		// cwd. Read from the root instead, README.md typed in pkg/svc/ would be
		// checked as the root README.md.
		{"relative in a subdirectory joins the cwd's prefix", "pkg/svc/", "README.md", "pkg/svc/README.md"},
		{"subdirectory-relative path with a directory component", "pkg/", "svc/README.md", "pkg/svc/README.md"},
		{"../ out of a subdirectory reaches the root file", "pkg/", "../README.md", "README.md"},
		{"a trailing slash (a directory) is cleaned", "", "envs/app/", "envs/app"},
		{"relative path leaving the repo → none", "", "../x.go", ""},
		{"the repo root itself → none", "pkg/", "..", ""},
		{"absolute inside the repo", "", "/repo/internal/a.go", "internal/a.go"},
		{"absolute inside the repo ignores the prefix", "pkg/", "/repo/README.md", "README.md"},
		{"absolute outside the repo → none", "", "/elsewhere/x.go", ""},
		{"absolute sibling with a shared name prefix → none", "", "/repo-2/x.go", ""},
	}
	for _, tc := range cases {
		if got := repoRelativePath("/repo", tc.prefix, tc.file); got != tc.want {
			t.Errorf("%s: repoRelativePath(%q, %q) = %q, want %q", tc.name, tc.prefix, tc.file, got, tc.want)
		}
	}
}

func TestRepoRelativePath_SymlinkedPathThatDoesNotExistYet(t *testing.T) {
	// git reports the PHYSICAL root (macOS /private/var/…), while an agent may
	// name a file through a symlinked parent (/var/…). EvalSymlinks fails for a
	// file that doesn't exist yet (a Write creating it, a path that exists only
	// on another branch), which left it on the logical side of the comparison,
	// outside the repo, and unchecked. Build the symlink explicitly so this runs
	// the same on Linux.
	base := t.TempDir()
	realDir := filepath.Join(base, "real-repo")
	if err := os.MkdirAll(filepath.Join(realDir, "pkg"), 0o755); err != nil {
		t.Fatal(err)
	}
	link := filepath.Join(base, "link-repo")
	if err := os.Symlink(realDir, link); err != nil {
		t.Skipf("symlinks unavailable: %v", err)
	}
	root, err := filepath.EvalSymlinks(realDir)
	if err != nil {
		t.Fatal(err)
	}
	for file, want := range map[string]string{
		filepath.Join(link, "pkg", "new.go"):        "pkg/new.go",     // parent exists, file doesn't
		filepath.Join(link, "fresh", "dir", "x.go"): "fresh/dir/x.go", // several missing levels
		filepath.Join(link, "pkg"):                  "pkg",            // exists
		filepath.Join(base, "elsewhere", "x.go"):    "",               // outside, missing
	} {
		if got := repoRelativePath(root, "", file); got != want {
			t.Errorf("repoRelativePath(%q) = %q, want %q", file, got, want)
		}
	}
}

// #201: a path is read the way the OS opens it. Built with real symlinks: a
// directory link, a file link, a link out of the tree, a dangling one, a loop,
// and absolute and relative targets.
func TestResolvePath_ReadsAPathAsTheOSDoes(t *testing.T) {
	base, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	repo := filepath.Join(base, "repo")
	out := filepath.Join(base, "outside")
	for _, d := range []string{filepath.Join(repo, "pkg", "svc"), out} {
		if err := os.MkdirAll(d, 0o755); err != nil {
			t.Fatal(err)
		}
	}
	for _, f := range []string{filepath.Join(repo, "README.md"), filepath.Join(repo, "pkg", "svc", "README.md"), filepath.Join(out, "x.md")} {
		if err := os.WriteFile(f, []byte("x\n"), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	links := map[string]string{
		"svclink": "pkg/svc", "link.md": "README.md", "ext": out, "abslink": filepath.Join(repo, "pkg"),
		"dangling": "pkg/nothere.md", "loop1": "loop2", "loop2": "loop1", "chain": "svclink",
	}
	for name, target := range links {
		if err := os.Symlink(target, filepath.Join(repo, name)); err != nil {
			t.Skipf("symlinks unavailable: %v", err)
		}
	}
	at := func(rel string) string { return filepath.Join(repo, filepath.FromSlash(rel)) }
	cases := []struct {
		name, p               string
		wantTarget, wantEntry string
	}{
		{"a plain file", joinRaw(repo, "README.md"), at("README.md"), at("README.md")},
		{"through a directory link", joinRaw(repo, "svclink/README.md"), at("pkg/svc/README.md"), at("pkg/svc/README.md")},
		{"a directory link itself: entry is the link", joinRaw(repo, "svclink"), at("pkg/svc"), at("svclink")},
		{"a trailing slash resolves it, as the OS does", joinRaw(repo, "svclink/"), at("pkg/svc"), at("pkg/svc")},
		{"a file link: entry is the link, target its file", joinRaw(repo, "link.md"), at("README.md"), at("link.md")},
		{"a chain of links", joinRaw(repo, "chain/README.md"), at("pkg/svc/README.md"), at("pkg/svc/README.md")},
		{"an absolute target", joinRaw(repo, "abslink/svc"), at("pkg/svc"), at("pkg/svc")},
		{".. after a link steps up from its target", joinRaw(repo, "svclink/../README.md"), at("pkg/README.md"), at("pkg/README.md")},
		{"a missing tail is kept", joinRaw(repo, "svclink/new/x.md"), at("pkg/svc/new/x.md"), at("pkg/svc/new/x.md")},
		{"a link out of the tree", joinRaw(repo, "ext/x.md"), filepath.Join(out, "x.md"), filepath.Join(out, "x.md")},
		{"a dangling link: where it points", joinRaw(repo, "dangling"), at("pkg/nothere.md"), at("dangling")},
		{"a relative path is only cleaned", "a/./b/../c", "a/c", "a/c"},
	}
	for _, tc := range cases {
		if got := resolvePath(tc.p, true); got != tc.wantTarget {
			t.Errorf("%s: resolvePath(%q, follow) = %q, want %q", tc.name, tc.p, got, tc.wantTarget)
		}
		if got := resolvePath(tc.p, false); got != tc.wantEntry {
			t.Errorf("%s: resolvePath(%q, entry) = %q, want %q", tc.name, tc.p, got, tc.wantEntry)
		}
	}
	// a loop ends (at one of its links, with the rest kept), inside the tree
	for _, follow := range []bool{true, false} {
		got := resolvePath(joinRaw(repo, "loop1/x"), follow)
		if got != at("loop1/x") && got != at("loop2/x") {
			t.Errorf("resolvePath(loop1/x, %v) = %q, want it to stop at a link of the loop", follow, got)
		}
	}
	// #201 as reported: a relative path through a symlinked directory names
	// what the absolute one does, from the root and from a subdirectory
	for _, tc := range []struct{ prefix, file string }{
		{"", "svclink/README.md"},
		{"", filepath.Join(repo, "svclink", "README.md")},
		{"pkg/", "../svclink/README.md"},
		{"", "chain/README.md"},
	} {
		if got := repoRelativePath(repo, tc.prefix, tc.file); got != "pkg/svc/README.md" {
			t.Errorf("repoRelativePath(prefix %q, %q) = %q, want pkg/svc/README.md", tc.prefix, tc.file, got)
		}
	}
	if got := repoRelativePath(repo, "", "ext/x.md"); got != "" {
		t.Errorf("a link out of the repo must leave it: repoRelativePath = %q", got)
	}
}

// joinRaw must not clean: a ".." after a symlink is resolvePath's to read.
func TestJoinRaw(t *testing.T) {
	for _, tc := range []struct {
		dir  string
		el   []string
		want string
	}{
		{"/r", []string{"pkg/", "a/../b"}, "/r/pkg/a/../b"},
		{"/r", []string{"", "x"}, "/r/x"},
		{"/", []string{"x"}, "/x"},
		{"/r/", []string{"./x"}, "/r/./x"},
	} {
		if got := joinRaw(tc.dir, tc.el...); got != tc.want {
			t.Errorf("joinRaw(%q, %q) = %q, want %q", tc.dir, tc.el, got, tc.want)
		}
	}
}

// locateIn places a resolved path in the worktree that holds it (#201): the
// deepest root containing it as spelled, else one equal to it but for case when
// the filesystem confirms they are one directory.
func TestLocateIn(t *testing.T) {
	roots := []string{"/w/repo", "/w/repo-b", "/w/repo/.worktrees/nested"}
	never := func(a, b string) bool { return false }
	always := func(a, b string) bool { return true }
	cases := []struct {
		name, p           string
		same              func(a, b string) bool
		wantRoot, wantRel string
		ok                bool
	}{
		{"this worktree", "/w/repo/pkg/a.go", never, "/w/repo", "pkg/a.go", true},
		{"its top level", "/w/repo", never, "/w/repo", ".", true},
		{"another worktree", "/w/repo-b/pkg/a.go", never, "/w/repo-b", "pkg/a.go", true},
		{"the deepest root wins", "/w/repo/.worktrees/nested/x.go", never, "/w/repo/.worktrees/nested", "x.go", true},
		{"a sibling with a shared name prefix is not inside", "/w/repo-c/x.go", never, "", "", false},
		{"outside", "/elsewhere/x.go", always, "", "", false},
		{"wrong case, confirmed one directory", "/W/Repo/pkg/a.go", always, "/w/repo", "pkg/a.go", true},
		{"wrong case, the deepest confirmed root", "/w/REPO/.worktrees/Nested", always, "/w/repo/.worktrees/nested", ".", true},
		{"wrong case, not one directory (a case-sensitive filesystem)", "/W/Repo/pkg/a.go", never, "", "", false},
		{"as spelled beats a case-folded match", "/w/repo-b/x", always, "/w/repo-b", "x", true},
		// #201 review: the deepest holder wins however it matched, so the main
		// checkout can't claim a nested worktree's file named under another case
		{"a deeper case-folded root beats a shallower as-spelled one", "/w/repo/.worktrees/NESTED/x.go", always, "/w/repo/.worktrees/nested", "x.go", true},
		{"unless it isn't one directory", "/w/repo/.worktrees/NESTED/x.go", never, "/w/repo", ".worktrees/NESTED/x.go", true},
	}
	for _, tc := range cases {
		root, rel, ok := locateIn(tc.p, roots, tc.same)
		if root != tc.wantRoot || rel != tc.wantRel || ok != tc.ok {
			t.Errorf("%s: locateIn(%q) = %q, %q, %v; want %q, %q, %v", tc.name, tc.p, root, rel, ok, tc.wantRoot, tc.wantRel, tc.ok)
		}
	}
	// a tie at one depth: as spelled wins, in either order
	for _, rs := range [][]string{{"/w/a/B", "/w/a/b"}, {"/w/a/b", "/w/a/B"}} {
		if root, _, _ := locateIn("/w/a/b/x", rs, always); root != "/w/a/b" {
			t.Errorf("locateIn(/w/a/b/x, %q) = %q, want the as-spelled /w/a/b", rs, root)
		}
	}
}

// pickListed and indexSpelling are the two spellings placePath reads where
// names are case-insensitive (#201).
func TestPickListedAndIndexSpelling(t *testing.T) {
	for _, tc := range []struct {
		name   string
		listed []string
		want   string
	}{
		{"readme.md", []string{"README.md", "go.mod"}, "README.md"},
		{"README.md", []string{"README.md", "readme.md"}, "README.md"}, // listed as is: kept
		{"Readme.md", []string{"README.md", "readme.md"}, "Readme.md"}, // two fold to it: can't tell
		{"x.md", []string{"README.md"}, "x.md"},
		{"x.md", nil, "x.md"},
	} {
		if got := pickListed(tc.name, tc.listed); got != tc.want {
			t.Errorf("pickListed(%q, %q) = %q, want %q", tc.name, tc.listed, got, tc.want)
		}
	}
	for _, tc := range []struct {
		rel    string
		listed []string
		want   string
	}{
		{"readme.md", []string{"README.md"}, "README.md"},                     // a case-only rename on disk: the index's
		{"README.md", []string{"README.md"}, "README.md"},                     // as tracked
		{"pkg/SVC", []string{"pkg/svc/README.md", "pkg/svc/a.go"}, "pkg/svc"}, // a directory
		{"pkg/svc", []string{"pkg/svc/README.md"}, "pkg/svc"},
		{"readme.md", []string{"README.md", "Readme.md"}, "readme.md"}, // case twins: as given
		{"readme.md", []string{"README.md", "readme.md"}, "readme.md"}, // one of them is it
		{"new.md", nil, "new.md"},                                      // untracked
		{"pkg", []string{"pkg2/x.go"}, "pkg"},                          // a segment, never a name prefix
	} {
		if got := indexSpelling(tc.rel, tc.listed); got != tc.want {
			t.Errorf("indexSpelling(%q, %q) = %q, want %q", tc.rel, tc.listed, got, tc.want)
		}
	}
}

// foldsCase agrees with the filesystem it probes: measured here by creating a
// file and opening it under the other case.
//
// #201 review: it probes the worktree's OWN `.git`, never an ancestor (an
// ancestor can sit on another filesystem: a case-sensitive volume mounted at
// /2024/1 under a folding parent read "folds"). A directory with no `.git` is
// not a worktree top and reads case-sensitive whatever its parents do, so a
// probe that walked up to a folding ancestor fails here.
func TestFoldsCase_MatchesTheFilesystem(t *testing.T) {
	dir, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	wt := filepath.Join(dir, "2024", "1") // no letter anywhere in its own names
	if err := os.MkdirAll(filepath.Join(wt, ".git"), 0o755); err != nil {
		t.Fatal(err)
	}
	a, _ := os.Stat(filepath.Join(wt, ".git"))
	b, statErr := os.Stat(filepath.Join(wt, ".GIT"))
	want := statErr == nil && os.SameFile(a, b)
	if got := foldsCase(wt); got != want {
		t.Errorf("foldsCase(%q) = %v, but the filesystem opens .GIT as .git: %v", wt, got, want)
	}
	if foldsCase(dir) {
		t.Errorf("foldsCase(%q) = true for a directory with no .git: it must probe the worktree's own entry", dir)
	}
	dot, swapped := caseProbe("/mnt/2024/1")
	if dot != "/mnt/2024/1/.git" || swapped != "/mnt/2024/1/.GIT" {
		t.Errorf("caseProbe = %q, %q: both must be inside the worktree", dot, swapped)
	}
}

// acceptFold: a folded spelling stands for the typed one only when the
// filesystem says they are one file, or neither exists (#201 review).
func TestAcceptFold(t *testing.T) {
	for _, tc := range []struct {
		name                            string
		typedExists, foldedExists, same bool
		want                            bool
	}{
		{"one file under two spellings (a folding filesystem)", true, true, true, true},
		{"two existing files (a case-sensitive volume)", true, true, false, false},
		{"a new readme.md beside a tracked README.md (the add/add case)", false, true, false, false},
		{"the typed one exists, the folded one doesn't", true, false, false, false},
		{"neither exists: git's index reading decides", false, false, false, true},
	} {
		if got := acceptFold(tc.typedExists, tc.foldedExists, tc.same); got != tc.want {
			t.Errorf("%s: acceptFold = %v, want %v", tc.name, got, tc.want)
		}
	}
}

// swapCase swaps the case of every letter in s.
func swapCase(s string) string {
	return strings.Map(func(r rune) rune {
		if unicode.IsUpper(r) {
			return unicode.ToLower(r)
		}
		if unicode.IsLower(r) {
			return unicode.ToUpper(r)
		}
		return r
	}, s)
}

// #201 item 4: where names are case-insensitive, a path is checked under the
// name git tracks. core.ignorecase is set by hand so this runs on a
// case-sensitive filesystem too, where only the index can supply the spelling;
// on one that folds case, the on-disk spelling is read as well.
func TestPlacePath_SpellsAsGitTracks(t *testing.T) {
	hermeticGitT(t)
	tmp, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	root := filepath.Join(tmp, "repo") // a name with letters, to reach it under another case
	gitT(t, tmp, "init", "-q", "-b", "main", "repo")
	gitT(t, root, "config", "user.email", "t@t.test")
	gitT(t, root, "config", "user.name", "t")
	gitT(t, root, "config", "core.ignorecase", "true")
	writeNested(t, root, "pkg/svc/README.md", "x\n")
	writeNested(t, root, "pkg/svc/GONE.md", "x\n")
	gitT(t, root, "add", "-A")
	gitT(t, root, "commit", "-qm", "base")
	writeNested(t, root, "docs/new.md", "untracked\n")
	if err := os.Remove(filepath.Join(root, "pkg", "svc", "GONE.md")); err != nil {
		t.Fatal(err)
	}
	// Where the filesystem folds case, a variant spelling IS the tracked file. On a
	// case-sensitive one (core.ignorecase hand-set here), a variant that doesn't
	// exist beside a tracked file that does is ANOTHER file (#201 review: folding it
	// hid an add/add conflict), so it keeps the given spelling.
	folds := foldsCase(root)
	pick := func(folded, sensitive string) string {
		if folds {
			return folded
		}
		return sensitive
	}
	for in, want := range map[string]string{
		"pkg/svc/readme.md": pick("pkg/svc/README.md", "pkg/svc/readme.md"),
		"PKG/Svc/README.md": pick("pkg/svc/README.md", "PKG/Svc/README.md"),
		"pkg/SVC":           pick("pkg/svc", "pkg/SVC"),
		"pkg/svc/README.md": "pkg/svc/README.md",
		"pkg/svc/other.md":  "pkg/svc/other.md", // nothing to spell it by
		"pkg/svc/gone.md":   "pkg/svc/GONE.md",  // neither on disk: the index's reading decides
	} {
		if got := repoRelativePath(root, "", in); got != want {
			t.Errorf("repoRelativePath(%q) = %q, want %q (folds case: %v)", in, got, want, folds)
		}
	}
	// a case-only rename on disk (mv, not git mv): git still names the file by its
	// index entry (status and diff report README.md), so where the filesystem
	// folds case that is the path asked about, whichever spelling is typed
	svc := filepath.Join(root, "pkg", "svc")
	if err := os.Rename(filepath.Join(svc, "README.md"), filepath.Join(svc, "tmp-name")); err != nil {
		t.Fatal(err)
	}
	if err := os.Rename(filepath.Join(svc, "tmp-name"), filepath.Join(svc, "readme.md")); err != nil {
		t.Fatal(err)
	}
	for in, want := range map[string]string{
		"pkg/svc/readme.md": pick("pkg/svc/README.md", "pkg/svc/readme.md"),
		"pkg/svc/README.md": "pkg/svc/README.md",
	} {
		if got := repoRelativePath(root, "", in); got != want {
			t.Errorf("after a case-only mv on disk: repoRelativePath(%q) = %q, want %q (folds case: %v)", in, got, want, folds)
		}
	}
	if folds { // the untracked file is spelled as listed only where the filesystem folds case
		if got := repoRelativePath(root, "", "DOCS/NEW.md"); got != "docs/new.md" {
			t.Errorf("repoRelativePath(DOCS/NEW.md) = %q, want docs/new.md (as listed)", got)
		}
		// the worktree itself reached under another case (a cwd typed as ~/Repo)
		wrong := filepath.Join(filepath.Dir(root), swapCase(filepath.Base(root)), "docs", "new.md")
		if got := repoRelativePath(root, "", wrong); got != "docs/new.md" {
			t.Errorf("repoRelativePath(%q) = %q, want docs/new.md: the same directory under another case", wrong, got)
		}
		// a nested worktree named under another case is still the deepest holder
		nested := filepath.Join(root, ".wt", "Feat")
		if err := os.MkdirAll(nested, 0o755); err != nil {
			t.Fatal(err)
		}
		p := resolveExisting(filepath.Join(root, ".wt", "feat", "pkg", "a.go"))
		if pp, ok := placePath(p, []string{root, nested}); !ok || pp.root != nested || pp.rel != "pkg/a.go" {
			t.Errorf("placePath(%q) = %+v, %v; want the nested %q, pkg/a.go", p, pp, ok, nested)
		}
	}
	// a case-sensitive repository keeps the spelling it was given
	cs, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	gitT(t, cs, "init", "-q", "-b", "main")
	gitT(t, cs, "config", "core.ignorecase", "false")
	writeNested(t, cs, "README.md", "x\n")
	gitT(t, cs, "add", "-A")
	if !foldsCase(cs) {
		if got := repoRelativePath(cs, "", "readme.md"); got != "readme.md" {
			t.Errorf("case-sensitive: repoRelativePath(readme.md) = %q, want it as given", got)
		}
	}
}

func TestMergeClaudeHook_FreshAndIdempotent(t *testing.T) {
	dir := t.TempDir()
	path := dir + "/settings.json"
	// fresh file → adds the entry
	out, changed, err := mergeClaudeHook(path)
	if err != nil || !changed {
		t.Fatalf("fresh: changed=%v err=%v", changed, err)
	}
	s := string(out)
	for _, want := range []string{
		claudeHookCommand, "Edit|Write|MultiEdit",
		claudeContextCommand, "UserPromptSubmit",
		todoWriteCommand, "PostToolUse", "TodoWrite", // #144: the todos mirror
	} {
		if !strings.Contains(s, want) {
			t.Errorf("fresh output missing %q:\n%s", want, s)
		}
	}
	// write it, then a second merge is a no-op (idempotent)
	if err := os.WriteFile(path, out, 0o644); err != nil {
		t.Fatal(err)
	}
	if _, changed, err := mergeClaudeHook(path); err != nil || changed {
		t.Errorf("idempotent: changed=%v err=%v", changed, err)
	}
}

func TestMergeClaudeHook_PreservesExisting(t *testing.T) {
	dir := t.TempDir()
	path := dir + "/settings.json"
	existing := `{"hooks":{"PreToolUse":[{"matcher":"Bash","hooks":[{"type":"command","command":"echo hi"}]}]},"model":"opus"}`
	if err := os.WriteFile(path, []byte(existing), 0o644); err != nil {
		t.Fatal(err)
	}
	out, changed, err := mergeClaudeHook(path)
	if err != nil || !changed {
		t.Fatalf("merge: changed=%v err=%v", changed, err)
	}
	s := string(out)
	if !strings.Contains(s, "echo hi") || !strings.Contains(s, `"model": "opus"`) {
		t.Errorf("clobbered existing config:\n%s", s)
	}
	if !strings.Contains(s, claudeHookCommand) {
		t.Errorf("didn't add our entry:\n%s", s)
	}
}

// The upgrade path: an existing user whose settings.json has ONLY the old
// PreToolUse claude-edit hook (installed by a wt version before the context hook
// existed) re-runs install-claude-hook. It must ADD only the UserPromptSubmit
// entry — not duplicate the PreToolUse one — and report changed.
func TestMergeClaudeHook_AddsContextToPreToolUseOnly(t *testing.T) {
	dir := t.TempDir()
	path := dir + "/settings.json"
	existing := `{"hooks":{"PreToolUse":[{"matcher":"Edit|Write|MultiEdit","hooks":[{"type":"command","command":"wt _hook claude-edit"}]}]}}`
	if err := os.WriteFile(path, []byte(existing), 0o644); err != nil {
		t.Fatal(err)
	}
	out, changed, err := mergeClaudeHook(path)
	if err != nil || !changed {
		t.Fatalf("partial upgrade should add the context hook: changed=%v err=%v", changed, err)
	}
	s := string(out)
	if !strings.Contains(s, "UserPromptSubmit") || !strings.Contains(s, claudeContextCommand) {
		t.Errorf("did not add the UserPromptSubmit context hook:\n%s", s)
	}
	if n := strings.Count(s, claudeHookCommand); n != 1 {
		t.Errorf("PreToolUse claude-edit must appear exactly once (not duplicated), got %d:\n%s", n, s)
	}
	// second run is now a full no-op
	if err := os.WriteFile(path, out, 0o644); err != nil {
		t.Fatal(err)
	}
	if _, changed, err := mergeClaudeHook(path); err != nil || changed {
		t.Errorf("both hooks present → no-op: changed=%v err=%v", changed, err)
	}
}

// #144 upgrade path: a user whose settings.json has the two PRE-#144 hooks
// (PreToolUse claude-edit + UserPromptSubmit claude-context) re-runs
// install-claude-hook. It must ADD only the PostToolUse/TodoWrite mirror — not
// duplicate the other two — and report changed, so `wt todos` stops being
// permanently empty for everyone who installed before #144.
func TestMergeClaudeHook_AddsTodoWriteToOlderInstall(t *testing.T) {
	dir := t.TempDir()
	path := dir + "/settings.json"
	existing := `{"hooks":{` +
		`"PreToolUse":[{"matcher":"Edit|Write|MultiEdit","hooks":[{"type":"command","command":"wt _hook claude-edit"}]}],` +
		`"UserPromptSubmit":[{"hooks":[{"type":"command","command":"wt _hook claude-context"}]}]}}`
	if err := os.WriteFile(path, []byte(existing), 0o644); err != nil {
		t.Fatal(err)
	}
	out, changed, err := mergeClaudeHook(path)
	if err != nil || !changed {
		t.Fatalf("older install should gain the TodoWrite hook: changed=%v err=%v", changed, err)
	}
	s := string(out)
	if !strings.Contains(s, "PostToolUse") || !strings.Contains(s, "TodoWrite") || !strings.Contains(s, todoWriteCommand) {
		t.Errorf("did not add the PostToolUse/TodoWrite mirror:\n%s", s)
	}
	if n := strings.Count(s, claudeHookCommand); n != 1 {
		t.Errorf("PreToolUse claude-edit must stay single, got %d:\n%s", n, s)
	}
	if n := strings.Count(s, claudeContextCommand); n != 1 {
		t.Errorf("UserPromptSubmit claude-context must stay single, got %d:\n%s", n, s)
	}
	// now all three present → second run is a no-op
	if err := os.WriteFile(path, out, 0o644); err != nil {
		t.Fatal(err)
	}
	if _, changed, err := mergeClaudeHook(path); err != nil || changed {
		t.Errorf("all three present → no-op: changed=%v err=%v", changed, err)
	}
}

func TestMergeClaudeHook_RefusesGarbage(t *testing.T) {
	dir := t.TempDir()
	path := dir + "/settings.json"
	if err := os.WriteFile(path, []byte("{ not json"), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, _, err := mergeClaudeHook(path); err == nil {
		t.Error("expected refuse on unparseable JSON")
	}
}

// #144 defect 1: todoWriteHookInstalled reads .claude/settings.json so `wt todos`
// can distinguish an empty store's two causes (hook absent vs. installed-but-
// never-fired) instead of asserting "hook not installed" as fact.
func TestTodoWriteHookInstalled(t *testing.T) {
	// Isolate from the real ~/.claude: point the user-level probe at a controlled
	// dir (empty until the last case writes to it) so results depend only on what
	// the test writes, not on this machine's actual config.
	userCfg := t.TempDir()
	t.Setenv("CLAUDE_CONFIG_DIR", userCfg)

	writeSettings := func(t *testing.T, root, body string) {
		t.Helper()
		if err := os.MkdirAll(root+"/.claude", 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(root+"/.claude/settings.json", []byte(body), 0o644); err != nil {
			t.Fatal(err)
		}
	}

	writeLocal := func(t *testing.T, root, body string) {
		t.Helper()
		if err := os.MkdirAll(root+"/.claude", 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(root+"/.claude/settings.local.json", []byte(body), 0o644); err != nil {
			t.Fatal(err)
		}
	}

	// the exact shape mergeClaudeHook writes → detected
	root := t.TempDir()
	writeSettings(t, root, `{"hooks":{"PostToolUse":[{"matcher":"TodoWrite","hooks":[{"type":"command","command":"wt _hook todo-write"}]}]}}`)
	if !todoWriteHookInstalled(root) {
		t.Error("wired todo-write hook not detected")
	}

	// #144-review F1: a path-qualified command is still the same hook → detected
	root = t.TempDir()
	writeSettings(t, root, `{"hooks":{"PostToolUse":[{"matcher":"TodoWrite","hooks":[{"type":"command","command":"/usr/local/bin/wt _hook todo-write"}]}]}}`)
	if !todoWriteHookInstalled(root) {
		t.Error("path-qualified todo-write command not detected")
	}

	// #144-review F5: hook wired only in settings.local.json (Claude Code merges it) → detected
	root = t.TempDir()
	writeLocal(t, root, `{"hooks":{"PostToolUse":[{"matcher":"TodoWrite","hooks":[{"type":"command","command":"wt _hook todo-write"}]}]}}`)
	if !todoWriteHookInstalled(root) {
		t.Error("todo-write hook in settings.local.json not detected")
	}

	// other wt hooks present but NOT todo-write → false (the misleading case #144 is about)
	root = t.TempDir()
	writeSettings(t, root, `{"hooks":{"PreToolUse":[{"matcher":"Edit|Write|MultiEdit","hooks":[{"type":"command","command":"wt _hook claude-edit"}]}]}}`)
	if todoWriteHookInstalled(root) {
		t.Error("false positive: reported installed when only claude-edit is wired")
	}

	// fail-open to false: no settings file, and unparseable settings
	if todoWriteHookInstalled(t.TempDir()) {
		t.Error("absent settings.json must read as not-installed")
	}
	root = t.TempDir()
	writeSettings(t, root, `{ not json`)
	if todoWriteHookInstalled(root) {
		t.Error("garbage settings.json must read as not-installed (fail-open)")
	}

	// end-to-end: what install-claude-hook --write actually produces IS detected
	root = t.TempDir()
	merged, _, err := mergeClaudeHook(root + "/.claude/settings.json")
	if err != nil {
		t.Fatalf("mergeClaudeHook: %v", err)
	}
	if err := os.MkdirAll(root+"/.claude", 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(root+"/.claude/settings.json", merged, 0o644); err != nil {
		t.Fatal(err)
	}
	if !todoWriteHookInstalled(root) {
		t.Error("hook written by mergeClaudeHook must be detected by todoWriteHookInstalled")
	}

	// #146: hook wired ONLY at the user level (CLAUDE_CONFIG_DIR / ~/.claude) is
	// detected from a project root that lacks it — a machine-wide install counts,
	// instead of being misreported as absent and prompting a duplicate.
	if err := os.WriteFile(userCfg+"/settings.json", []byte(`{"hooks":{"PostToolUse":[{"matcher":"TodoWrite","hooks":[{"type":"command","command":"wt _hook todo-write"}]}]}}`), 0o644); err != nil {
		t.Fatal(err)
	}
	if !todoWriteHookInstalled(t.TempDir()) {
		t.Error("user-level (CLAUDE_CONFIG_DIR) todo-write hook not detected for a project without it")
	}
	// user-level settings.local.json also counts
	if err := os.Remove(userCfg + "/settings.json"); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(userCfg+"/settings.local.json", []byte(`{"hooks":{"PostToolUse":[{"matcher":"TodoWrite","hooks":[{"type":"command","command":"wt _hook todo-write"}]}]}}`), 0o644); err != nil {
		t.Fatal(err)
	}
	if !todoWriteHookInstalled(t.TempDir()) {
		t.Error("user-level settings.local.json todo-write hook not detected")
	}
}
