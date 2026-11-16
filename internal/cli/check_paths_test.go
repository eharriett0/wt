package cli

import (
	"encoding/json"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"reflect"
	"sort"
	"strings"
	"testing"

	"github.com/eharriett0/wt/internal/config"
)

// #181 end to end, against real worktrees: what each surface asks about when it
// is given a path. The pure layers (QueryFor, resolveCheckArgs, CheckPaths) are
// table-tested elsewhere; these pin the WIRING, which no pure test can: that
// `wt check` reads an argument from the cwd and keeps the bare-name search, that
// MCP wt_check reads one from the repo root, and that both edit hooks ask about
// exactly the file being edited. Same offline setup as behindHookRepo: a fake
// `gh` that always fails, a scratch HOME, and a cwd restored after the test.

// pathsRepo builds main with README.md, pkg/svc/README.md, docs/README.md and
// internal/foo.go (40 lines each), and two worktrees: wa (this window, no edits)
// and wb, which committed an edit to line 1 of pkg/svc/README.md and of
// internal/foo.go. Nothing touches the root README.md.
func pathsRepo(t *testing.T) (root, wa, wb string) {
	t.Helper()
	hermeticGitT(t) // the fixture's git and the code under test's (#167)
	offlineGH(t)
	t.Setenv("HOME", t.TempDir())
	t.Setenv("WT_SKIP_COLLISION", "")
	t.Setenv("HOOK_DISABLE_MULTIWINDOW_CHECK", "")
	t.Setenv("WT_CLAUDE_HOOK_BLOCK", "0")
	t.Setenv("WT_CODEX_HOOK_BLOCK", "0")
	t.Chdir(t.TempDir()) // the commands and hooks chdir; restored after

	root = t.TempDir()
	gitT(t, root, "init", "-q", "-b", "main")
	gitT(t, root, "config", "user.email", "t@t.test")
	gitT(t, root, "config", "user.name", "t")
	for _, f := range []string{"README.md", "pkg/svc/README.md", "docs/README.md", "internal/foo.go", "sparse/x.md"} {
		writeNested(t, root, f, numbered(f))
	}
	gitT(t, root, "add", "-A")
	gitT(t, root, "commit", "-qm", "base")
	gitT(t, root, "branch", "wa")
	gitT(t, root, "branch", "wb")
	wa = filepath.Join(t.TempDir(), "wa")
	wb = filepath.Join(t.TempDir(), "wb")
	gitT(t, root, "worktree", "add", "-q", wa, "wa")
	gitT(t, root, "worktree", "add", "-q", wb, "wb")
	editLine1(t, wb, "pkg/svc/README.md")
	editLine1(t, wb, "internal/foo.go")
	gitT(t, wb, "commit", "-qam", "wb edits line 1 of pkg/svc/README.md and internal/foo.go")
	return root, wa, wb
}

// offlineGH puts a fake gh first on PATH, so no test reaches GitHub. It reports
// gh as authenticated and fails every other call (no PR, no tip lookup). The
// "authenticated" matters: ghx memoizes the auth answer process-wide for a
// minute (#172), so a fake that failed `gh auth status` too left a "no" that
// the merge-pr tests' fakes (which answer yes) then read, and they failed or
// passed depending on how fast the earlier tests ran.
func offlineGH(t *testing.T) {
	t.Helper()
	bin := t.TempDir()
	script := "#!/bin/sh\n[ \"$1\" = auth ] && exit 0\nexit 1\n"
	if err := os.WriteFile(filepath.Join(bin, "gh"), []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", bin+string(os.PathListSeparator)+os.Getenv("PATH"))
}

// numbered is 40 lines naming the file, so every file's lines are distinct.
func numbered(f string) string {
	var b strings.Builder
	for i := 1; i <= 40; i++ {
		fmt.Fprintf(&b, "%s l%02d\n", f, i)
	}
	return b.String()
}

func editLine1(t *testing.T, dir, f string) {
	t.Helper()
	writeT(t, dir, f, strings.Replace(readT(t, dir, f), f+" l01\n", "EDITED "+f+" l01\n", 1))
}

func writeNested(t *testing.T, dir, rel, content string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(filepath.Join(dir, rel)), 0o755); err != nil {
		t.Fatal(err)
	}
	writeT(t, dir, rel, content)
}

// checkJSON runs `wt check --json args...` in cwd and returns its exit code and
// entries as "path←window", sorted.
func checkJSON(t *testing.T, cwd string, args ...string) (int, []string) {
	t.Helper()
	t.Chdir(cwd)
	r, w, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	stdout := os.Stdout
	os.Stdout = w
	code := cmdCheck(append([]string{"--json"}, args...))
	os.Stdout = stdout
	w.Close()
	out, _ := io.ReadAll(r)
	return code, payloadEntries(t, string(out))
}

func payloadEntries(t *testing.T, text string) []string {
	t.Helper()
	var p CheckPayload
	if err := json.Unmarshal([]byte(text), &p); err != nil {
		t.Fatalf("not a check payload: %v\n%s", err, text)
	}
	var got []string
	for _, e := range p.Entries {
		got = append(got, e.Path+"←"+e.Window)
	}
	sort.Strings(got)
	return got
}

func TestCheckCommand_ReadsArgumentsFromTheCwd(t *testing.T) {
	_, wa, _ := pathsRepo(t)
	cases := []struct {
		name, cwd string
		args      []string
		code      int
		want      []string
	}{
		// the #181 report: the root README.md is not pkg/svc/README.md
		{"root file at the root", wa, []string{"README.md"}, 0, nil},
		{"the same name in pkg/svc/ is pkg/svc/README.md", filepath.Join(wa, "pkg", "svc"), []string{"README.md"}, 3, []string{"pkg/svc/README.md←wb"}},
		{"a subdirectory-relative path", filepath.Join(wa, "pkg"), []string{"svc/README.md"}, 3, []string{"pkg/svc/README.md←wb"}},
		{"./ names the same file", wa, []string{"./pkg/svc/README.md"}, 3, []string{"pkg/svc/README.md←wb"}},
		{"an absolute path", filepath.Join(wa, "docs"), []string{filepath.Join(wa, "pkg", "svc", "README.md")}, 3, []string{"pkg/svc/README.md←wb"}},
		// a bare name that is nothing here stays a search
		{"bare name of a nested file", wa, []string{"foo.go"}, 3, []string{"foo.go←wb"}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			code, got := checkJSON(t, tc.cwd, tc.args...)
			if code != tc.code || !reflect.DeepEqual(got, tc.want) {
				t.Errorf("wt check %v in %s = exit %d %v, want exit %d %v", tc.args, tc.cwd, code, got, tc.code, tc.want)
			}
		})
	}
}

// MCP wt_check reads a relative path from the repo ROOT, as its schema
// promises, wherever the client started the server; `wt check` in the same
// directory reads it from there.
func TestMCPCheck_ReadsPathsFromTheRepoRoot(t *testing.T) {
	root, wa, _ := pathsRepo(t)
	gitT(t, root, "branch", "wc")
	wc := filepath.Join(t.TempDir(), "wc")
	gitT(t, root, "worktree", "add", "-q", wc, "wc")
	editLine1(t, wc, "README.md")
	gitT(t, wc, "commit", "-qam", "wc edits line 1 of the root README.md")

	docs := filepath.Join(wa, "docs")
	t.Chdir(docs)
	c, err := config.Load()
	if err != nil {
		t.Fatal(err)
	}
	for _, tc := range []struct {
		paths []string
		want  []string
	}{
		{[]string{"README.md"}, []string{"README.md←wc"}}, // the root README.md, not docs/README.md
		{[]string{filepath.Join(wa, "README.md")}, []string{"README.md←wc"}},
		{[]string{"docs/README.md"}, nil},
		{[]string{"pkg/svc/README.md"}, []string{"pkg/svc/README.md←wb"}},
	} {
		text, isErr := mcpCheck(c, tc.paths, false, false)
		if isErr {
			t.Fatalf("wt_check %v: tool error %s", tc.paths, text)
		}
		if got := payloadEntries(t, text); !reflect.DeepEqual(got, tc.want) {
			t.Errorf("wt_check %v from docs/ = %v, want %v", tc.paths, got, tc.want)
		}
	}
	// Real paths that no window touches are not typos: whether they exist, or
	// are tracked, is read from the root too. An ignored file exists but git
	// never lists it; a skip-worktree (sparse) file is tracked but not on disk.
	writeNested(t, wa, ".gitignore", "ignored/\n")
	writeNested(t, wa, "ignored/x.md", "x\n")
	gitT(t, wa, "update-index", "--skip-worktree", "sparse/x.md")
	if err := os.Remove(filepath.Join(wa, "sparse", "x.md")); err != nil {
		t.Fatal(err)
	}
	for _, p := range []string{"ignored/x.md", "sparse/x.md"} {
		if text, isErr := mcpCheck(c, []string{p}, false, false); isErr {
			t.Errorf("wt_check %s from docs/ was refused: %s", p, text)
		}
	}
	if _, got := checkJSON(t, docs, "README.md"); got != nil {
		t.Errorf("wt check README.md in docs/ = %v, want docs/README.md, which nobody touches", got)
	}
}

// Both edit hooks ask about exactly the file being edited (#181): an edit of the
// root README.md is not an edit of pkg/svc/README.md, and a Codex patch path is
// read from the session's cwd.
func TestEditHooks_AskAboutTheEditedFileExactly(t *testing.T) {
	_, wa, _ := pathsRepo(t)
	claude := func(cwd, file string) string {
		line := filepath.ToSlash(strings.TrimPrefix(file, wa+string(filepath.Separator))) + " l01\n"
		p, _ := json.Marshal(map[string]any{"cwd": cwd, "tool_name": "Edit", "tool_input": map[string]string{
			"file_path": file, "old_string": line, "new_string": "X " + line}})
		return hookVerdict(t, runHookCapture(t, hookClaudeEdit, string(p)))
	}
	codex := func(cwd, patchPath, repoPath string) string {
		line := repoPath + " l01"
		p, _ := json.Marshal(map[string]any{"cwd": cwd, "tool_name": "apply_patch", "tool_input": map[string]string{
			"command": "*** Begin Patch\n*** Update File: " + patchPath + "\n@@\n-" + line + "\n+X " + line + "\n*** End Patch\n"}})
		return hookVerdict(t, runHookCapture(t, hookCodexEdit, string(p)))
	}
	svc := filepath.Join(wa, "pkg", "svc", "README.md")
	for _, tc := range []struct {
		name, got, want string
	}{
		{"claude edits the root README.md", claude(wa, filepath.Join(wa, "README.md")), "silent"},
		{"claude edits pkg/svc/README.md", claude(wa, svc), "overlap"},
		{"claude edits it from a subdirectory", claude(filepath.Join(wa, "pkg"), svc), "overlap"},
		{"codex patches the root README.md", codex(wa, "README.md", "README.md"), "silent"},
		{"codex patches pkg/svc/README.md", codex(wa, "pkg/svc/README.md", "pkg/svc/README.md"), "overlap"},
		{"codex in pkg/ patches svc/README.md", codex(filepath.Join(wa, "pkg"), "svc/README.md", "pkg/svc/README.md"), "overlap"},
		{"codex in pkg/svc/ patches README.md", codex(filepath.Join(wa, "pkg", "svc"), "README.md", "pkg/svc/README.md"), "overlap"},
	} {
		if tc.got != tc.want {
			t.Errorf("%s: hook says %s, want %s", tc.name, tc.got, tc.want)
		}
	}
	// the hook's question and `wt check`'s are the same one (#92)
	if code, _ := checkJSON(t, wa, "README.md"); code != 0 {
		t.Errorf("wt check README.md = exit %d, want 0, as the hooks say", code)
	}
}

// issue201Repo builds main with README.md, pkg/svc/README.md and internal/foo.go
// (40 lines each) plus two TRACKED symlinks, link.md → README.md and svclink →
// pkg/svc, and four worktrees side by side: wa (this window, no edits), wb (line
// 1 of pkg/svc/README.md and internal/foo.go, and a new docs/NEW.md), wc (line 1
// of pkg/svc/README.md, and link.md retargeted to internal/foo.go) and wd (line
// 1 of the root README.md), all committed. wa also holds ext, an untracked
// symlink to a directory outside the repository, which holds x.md.
// core.ignorecase is set as git sets it on a case-insensitive filesystem, so the
// case rows run on any filesystem (on a case-sensitive one, only the index can
// spell them).
func issue201Repo(t *testing.T) (wa, wb, outside string) {
	t.Helper()
	hermeticGitT(t)
	offlineGH(t)
	t.Setenv("HOME", t.TempDir())
	t.Setenv("WT_SKIP_COLLISION", "")
	t.Setenv("HOOK_DISABLE_MULTIWINDOW_CHECK", "")
	t.Setenv("WT_CLAUDE_HOOK_BLOCK", "0")
	t.Setenv("WT_CODEX_HOOK_BLOCK", "0")
	t.Chdir(t.TempDir())

	root := t.TempDir()
	gitT(t, root, "init", "-q", "-b", "main")
	gitT(t, root, "config", "user.email", "t@t.test")
	gitT(t, root, "config", "user.name", "t")
	gitT(t, root, "config", "core.ignorecase", "true")
	for _, f := range []string{"README.md", "pkg/svc/README.md", "internal/foo.go"} {
		writeNested(t, root, f, numbered(f))
	}
	symlinkT(t, "README.md", filepath.Join(root, "link.md"))
	symlinkT(t, "pkg/svc", filepath.Join(root, "svclink"))
	gitT(t, root, "add", "-A")
	gitT(t, root, "commit", "-qm", "base")
	parent := t.TempDir()
	dirs := map[string]string{}
	for _, b := range []string{"wa", "wb", "wc", "wd"} {
		gitT(t, root, "branch", b)
		dirs[b] = filepath.Join(parent, b)
		gitT(t, root, "worktree", "add", "-q", dirs[b], b)
	}
	wa, wb = dirs["wa"], dirs["wb"]
	editLine1(t, wb, "pkg/svc/README.md")
	editLine1(t, wb, "internal/foo.go")
	writeNested(t, wb, "docs/NEW.md", "new\n")
	gitT(t, wb, "add", "-A")
	gitT(t, wb, "commit", "-qm", "wb")
	editLine1(t, dirs["wc"], "pkg/svc/README.md")
	if err := os.Remove(filepath.Join(dirs["wc"], "link.md")); err != nil {
		t.Fatal(err)
	}
	symlinkT(t, "internal/foo.go", filepath.Join(dirs["wc"], "link.md"))
	gitT(t, dirs["wc"], "add", "-A")
	gitT(t, dirs["wc"], "commit", "-qm", "wc")
	editLine1(t, dirs["wd"], "README.md")
	gitT(t, dirs["wd"], "commit", "-qam", "wd")

	outside = t.TempDir()
	writeT(t, outside, "x.md", "x\n")
	symlinkT(t, outside, filepath.Join(wa, "ext"))
	return wa, wb, outside
}

func symlinkT(t *testing.T, target, link string) {
	t.Helper()
	if err := os.Symlink(target, link); err != nil {
		t.Skipf("symlinks unavailable: %v", err)
	}
}

// checkExit runs `wt check --json args...` in cwd and returns only its exit
// code (a refusal prints no payload).
func checkExit(t *testing.T, cwd string, args ...string) int {
	t.Helper()
	t.Chdir(cwd)
	stdout := os.Stdout
	devnull, err := os.Open(os.DevNull)
	if err != nil {
		t.Fatal(err)
	}
	defer devnull.Close()
	os.Stdout = devnull
	defer func() { os.Stdout = stdout }()
	return cmdCheck(append([]string{"--json"}, args...))
}

// #201 end to end: each gap in how an argument resolves to a repo path, read
// against real worktrees, symlinks and an outside directory, for `wt check`,
// MCP wt_check and both edit hooks.
func TestCheck_Issue201_ArgumentsResolveToTheRepoPath(t *testing.T) {
	wa, wb, outside := issue201Repo(t)
	svc := []string{"pkg/svc/README.md←wb", "pkg/svc/README.md←wc"}
	all := []string{"README.md←wd", "docs/NEW.md←wb", "internal/foo.go←wb", "link.md←wc", "pkg/svc/README.md←wb", "pkg/svc/README.md←wc"}
	relToWb, err := filepath.Rel(wa, wb)
	if err != nil {
		t.Fatal(err)
	}
	cases := []struct {
		name, cwd string
		arg       string
		code      int
		want      []string
	}{
		// 1: a relative argument names what its absolute spelling does
		{"through a symlinked directory, relative", wa, "svclink/README.md", 3, svc},
		{"through a symlinked directory, absolute", wa, filepath.Join(wa, "svclink", "README.md"), 3, svc},
		{"a tracked symlink file: the link AND its target", wa, "link.md", 3, []string{"README.md←wd", "link.md←wc"}},
		{"a tracked symlink file, absolute", wa, filepath.Join(wa, "link.md"), 3, []string{"README.md←wd", "link.md←wc"}},
		{"a symlink out of the repository, itself", wa, "ext", 0, nil},
		// 2: the top level is the whole repository, from wherever it is named
		{"`.` at the top", wa, ".", 3, all},
		{"`./` at the top", wa, "./", 3, all},
		{"$PWD at the top", wa, wa, 3, all},
		{"`..` from a subdirectory", filepath.Join(wa, "pkg"), "..", 3, all},
		// 3: a path in another worktree is that repo path
		{"a file in another worktree, absolute", wa, filepath.Join(wb, "pkg", "svc", "README.md"), 3, svc},
		{"a file in another worktree, relative", wa, filepath.Join(relToWb, "pkg", "svc", "README.md"), 3, svc},
		{"another worktree's top level", wa, wb, 3, all},
		// 4: case-insensitive names: the spelling git tracks / a window touches
		{"a case variant of a tracked file", wa, "pkg/svc/readme.md", 3, svc},
		{"a case variant of a directory", wa, "PKG/svc/README.md", 3, svc},
		{"a case variant of a file only another window has", wa, "docs/new.md", 3, []string{"docs/NEW.md←wb"}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			code, got := checkJSON(t, tc.cwd, tc.arg)
			if code != tc.code || !reflect.DeepEqual(got, tc.want) {
				t.Errorf("wt check %s in %s = exit %d %v, want exit %d %v", tc.arg, tc.cwd, code, got, tc.code, tc.want)
			}
		})
	}
	// 3: a path outside every worktree is refused, never "clear"
	for _, arg := range []string{filepath.Join(outside, "x.md"), "ext/x.md", filepath.Join(wa, "ext", "x.md"), "../../.."} {
		if code := checkExit(t, wa, arg); code != 64 {
			t.Errorf("wt check %s = exit %d, want 64 (outside the repository: refused)", arg, code)
		}
		if code := checkExit(t, wa, "--allow-missing", arg); code != 64 {
			t.Errorf("wt check --allow-missing %s = exit %d, want 64 (nothing to check)", arg, code)
		}
	}
	// the OS reads svclink/../README.md as pkg/README.md, which is nothing
	if code := checkExit(t, wa, "svclink/../README.md"); code != 64 {
		t.Errorf("wt check svclink/../README.md = exit %d, want 64: it is pkg/README.md, which doesn't exist", code)
	}

	// MCP wt_check: the same answers, absolute paths included
	t.Chdir(filepath.Join(wa, "pkg"))
	c, err := config.Load()
	if err != nil {
		t.Fatal(err)
	}
	for _, tc := range []struct {
		paths []string
		want  []string
	}{
		{[]string{"svclink/README.md"}, svc},
		{[]string{"."}, all},
		{[]string{filepath.Join(wb, "pkg", "svc", "README.md")}, svc},
		{[]string{"pkg/svc/readme.md"}, svc},
	} {
		text, isErr := mcpCheck(c, tc.paths, false, false)
		if isErr {
			t.Errorf("wt_check %v: tool error %s", tc.paths, text)
			continue
		}
		if got := payloadEntries(t, text); !reflect.DeepEqual(got, tc.want) {
			t.Errorf("wt_check %v = %v, want %v", tc.paths, got, tc.want)
		}
	}
	for _, p := range []string{filepath.Join(outside, "x.md"), "ext/x.md"} {
		if text, isErr := mcpCheck(c, []string{p}, false, true); !isErr || !strings.Contains(text, "not in any worktree") {
			t.Errorf("wt_check %s (include_missing) = %v %s, want refused as outside", p, isErr, text)
		}
	}

	// the edit hooks ask what `wt check` asks (#92), and the Codex hook names
	// every window on the file (#201 item 6)
	codexOut := func(cwd, patchPath, line string) string {
		p, _ := json.Marshal(map[string]any{"cwd": cwd, "tool_name": "apply_patch", "tool_input": map[string]string{
			"command": "*** Begin Patch\n*** Update File: " + patchPath + "\n@@\n-" + line + "\n+X " + line + "\n*** End Patch\n"}})
		return runHookCapture(t, hookCodexEdit, string(p))
	}
	claudeOut := func(cwd, file, line string) string {
		p, _ := json.Marshal(map[string]any{"cwd": cwd, "tool_name": "Edit", "tool_input": map[string]string{
			"file_path": file, "old_string": line + "\n", "new_string": "X " + line + "\n"}})
		return runHookCapture(t, hookClaudeEdit, string(p))
	}
	const svcLine = "pkg/svc/README.md l01"
	for _, tc := range []struct{ name, out string }{
		{"codex patches svclink/README.md", codexOut(wa, "svclink/README.md", svcLine)},
		{"codex in pkg/ patches ../svclink/README.md", codexOut(filepath.Join(wa, "pkg"), "../svclink/README.md", svcLine)},
		{"codex patches pkg/svc/readme.md", codexOut(wa, "pkg/svc/readme.md", svcLine)},
		{"claude edits through svclink", claudeOut(wa, filepath.Join(wa, "svclink", "README.md"), svcLine)},
		{"claude edits pkg/svc/readme.md", claudeOut(wa, filepath.Join(wa, "pkg", "svc", "readme.md"), svcLine)},
	} {
		if v := hookVerdict(t, tc.out); v != "overlap" {
			t.Errorf("%s: hook says %s, want overlap, as `wt check` grades it HIGH", tc.name, v)
		}
	}
	ctx, _, _ := decodeDecision(t, codexOut(wa, "svclink/README.md", svcLine))
	if !strings.Contains(ctx, "pkg/svc/README.md — also being edited by wb [") || !strings.Contains(ctx, "), wc [") {
		t.Errorf("the Codex hook must name both windows on pkg/svc/README.md:\n%s", ctx)
	}
	// a tracked symlink file: apply_patch writes THROUGH it (its target,
	// README.md, which wd edits) but deletes the LINK (which wc retargeted).
	// `wt check link.md` asks about both; each hook op asks about the one it
	// changes.
	ctx, _, _ = decodeDecision(t, codexOut(wa, "link.md", "README.md l01"))
	if !strings.Contains(ctx, "README.md — also being edited by wd [") || strings.Contains(ctx, "link.md") {
		t.Errorf("an Update through link.md changes README.md (wd's), not the link:\n%s", ctx)
	}
	del, _ := json.Marshal(map[string]any{"cwd": wa, "tool_name": "apply_patch", "tool_input": map[string]string{
		"command": "*** Begin Patch\n*** Delete File: link.md\n*** End Patch\n"}})
	delOut := runHookCapture(t, hookCodexEdit, string(del))
	if v := hookVerdict(t, delOut); v != "overlap" {
		t.Errorf("a Delete of link.md: hook says %s, want overlap with wc's retarget", v)
	}
	if ctx, _, _ = decodeDecision(t, delOut); !strings.Contains(ctx, "link.md — also being edited by wc [") || strings.Contains(ctx, "wd") {
		t.Errorf("a Delete of link.md removes the link (wc's), not README.md:\n%s", ctx)
	}
}
