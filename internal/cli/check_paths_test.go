package cli

import (
	"encoding/json"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"sort"
	"strings"
	"testing"
	"time"

	"github.com/eharriett0/wt/internal/config"
	"github.com/eharriett0/wt/internal/gitx"
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
// symlink to a directory outside the repository, which holds x.md. The case
// rows of the tests that use it run only where the filesystem folds case: on a
// case-sensitive one readme.md is another file (#201 review), which
// TestCheck_CaseSensitiveVolumeNeverMergesTwoFiles covers.
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

// #201 review: a Codex patch that also touches wt's own `link_files` `.env` (an
// absolute symlink into the main checkout, as `wt new` makes it) kept no
// prediction at all once relative paths resolved through symlinks: the `.env`
// operation resolved outside this worktree, the whole patch read as
// unpredictable, and WT_CODEX_HOOK_BLOCK lost the deny for the repo file beside
// it, which `wt check` grades HIGH.
func TestCodexEdit_LinkFilesEnvDoesNotCostTheDeny(t *testing.T) {
	hermeticGitT(t)
	offlineGH(t)
	t.Setenv("HOME", t.TempDir())
	t.Setenv("WT_SKIP_COLLISION", "")
	t.Setenv("HOOK_DISABLE_MULTIWINDOW_CHECK", "")
	t.Setenv("WT_CODEX_HOOK_BLOCK", "1")
	t.Chdir(t.TempDir())

	root := t.TempDir()
	gitT(t, root, "init", "-q", "-b", "main")
	gitT(t, root, "config", "user.email", "t@t.test")
	gitT(t, root, "config", "user.name", "t")
	writeNested(t, root, ".gitignore", ".env\n")
	writeNested(t, root, "pkg/svc/a.go", numbered("pkg/svc/a.go"))
	gitT(t, root, "add", "-A")
	gitT(t, root, "commit", "-qm", "base")
	writeNested(t, root, ".env", "KEY=1\n") // the main checkout's, ignored
	parent := t.TempDir()
	wa, wb := filepath.Join(parent, "wa"), filepath.Join(parent, "wb")
	for b, dir := range map[string]string{"wa": wa, "wb": wb} {
		gitT(t, root, "branch", b)
		gitT(t, root, "worktree", "add", "-q", dir, b)
	}
	symlinkT(t, filepath.Join(root, ".env"), filepath.Join(wa, ".env")) // linkSharedFiles' shape
	writeT(t, wb, "pkg/svc/a.go", strings.Replace(readT(t, wb, "pkg/svc/a.go"), "pkg/svc/a.go l02\n", "B pkg/svc/a.go l02\n", 1))

	if code, got := checkJSON(t, wa, "pkg/svc/a.go"); code != 3 || !reflect.DeepEqual(got, []string{"pkg/svc/a.go←wb"}) {
		t.Fatalf("wt check pkg/svc/a.go = exit %d %v, want exit 3 [pkg/svc/a.go←wb]", code, got)
	}
	patch := "*** Begin Patch\n*** Update File: pkg/svc/a.go\n@@\n-pkg/svc/a.go l02\n+A pkg/svc/a.go l02\n" +
		"*** Update File: .env\n@@\n-KEY=1\n+KEY=2\n*** End Patch\n"
	p, _ := json.Marshal(map[string]any{"cwd": wa, "tool_name": "apply_patch", "tool_input": map[string]string{"command": patch}})
	if v := hookVerdict(t, runHookCapture(t, hookCodexEdit, string(p))); v != "deny" {
		t.Errorf("codex-edit under WT_CODEX_HOOK_BLOCK=1 = %s, want deny: pkg/svc/a.go overlaps wb, as `wt check` says", v)
	}
}

// #201 re-review: a Codex move ACROSS the repo's edge through an in-repo link
// to a directory outside it. Into the repo, it overwrites a file another window
// edits; out of it, it removes one (modify/delete). Read lexically before #201,
// both were in-repo moves and denied under WT_CODEX_HOOK_BLOCK; once the link
// resolved outside, the whole patch went unpredicted and only warned.
func TestCodexEdit_MoveAcrossTheRepoEdgeIsDenied(t *testing.T) {
	hermeticGitT(t)
	offlineGH(t)
	t.Setenv("HOME", t.TempDir())
	t.Setenv("WT_SKIP_COLLISION", "")
	t.Setenv("HOOK_DISABLE_MULTIWINDOW_CHECK", "")
	t.Setenv("WT_CODEX_HOOK_BLOCK", "1")
	t.Chdir(t.TempDir())

	root := t.TempDir()
	gitT(t, root, "init", "-q", "-b", "main")
	gitT(t, root, "config", "user.email", "t@t.test")
	gitT(t, root, "config", "user.name", "t")
	writeNested(t, root, "pkg/svc/a.go", numbered("pkg/svc/a.go"))
	// mid.txt and the outside ten.txt start as a.go's twins: read as they lie on
	// disk, a move of either onto a.go with an append was a pure append there,
	// disjoint from wb's line 2, and read SILENT (#201 re-review)
	writeNested(t, root, "pkg/mid.txt", numbered("pkg/svc/a.go"))
	gitT(t, root, "add", "-A")
	gitT(t, root, "commit", "-qm", "base")
	parent := t.TempDir()
	wa, wb := filepath.Join(parent, "wa"), filepath.Join(parent, "wb")
	for b, dir := range map[string]string{"wa": wa, "wb": wb} {
		gitT(t, root, "branch", b)
		gitT(t, root, "worktree", "add", "-q", dir, b)
	}
	outside := t.TempDir()
	writeT(t, outside, "note.txt", "n1\nn2\n")
	writeT(t, outside, "ten.txt", numbered("pkg/svc/a.go"))
	if f, err := os.Create(filepath.Join(outside, "big.txt")); err != nil { // over maxPredictBytes
		t.Fatal(err)
	} else if err := f.Truncate(maxPredictBytes + 1); err != nil {
		t.Fatal(err)
	} else {
		f.Close()
	}
	symlinkT(t, outside, filepath.Join(wa, "ext"))
	writeT(t, wb, "pkg/svc/a.go", strings.Replace(readT(t, wb, "pkg/svc/a.go"), "pkg/svc/a.go l02\n", "B pkg/svc/a.go l02\n", 1))

	codex := func(patch string) string {
		p, _ := json.Marshal(map[string]any{"cwd": wa, "tool_name": "apply_patch", "tool_input": map[string]string{"command": "*** Begin Patch\n" + patch + "*** End Patch\n"}})
		return hookVerdict(t, runHookWithin(t, 20*time.Second, hookCodexEdit, string(p)))
	}
	for name, tc := range map[string]struct{ patch, want string }{
		"into the repo: overwrites wb's file": {"*** Update File: ext/note.txt\n*** Move to: pkg/svc/a.go\n@@\n-n1\n+N1\n", "deny"},
		"out of the repo: removes wb's file":  {"*** Update File: pkg/svc/a.go\n*** Move to: ext/moved.go\n@@\n-pkg/svc/a.go l05\n+X\n", "deny"},
		// #201 re-review A1: what the patch made outside is what moves in
		"added outside, moved in, moved onto wb's file": {"*** Add File: ext/new.txt\n+n1\n+n2\n*** Update File: ext/new.txt\n*** Move to: pkg/x.txt\n@@\n n2\n+n3\n" +
			"*** Update File: pkg/x.txt\n*** Move to: pkg/svc/a.go\n@@\n n3\n+n4\n", "deny"},
		// #201 re-review A2: an outside file rewritten (wb's line 2 too), then moved onto wb's file
		"rewritten outside, moved onto wb's file": {"*** Update File: ext/ten.txt\n@@\n-pkg/svc/a.go l02\n+Z\n*** Update File: ext/ten.txt\n*** Move to: pkg/svc/a.go\n@@\n pkg/svc/a.go l40\n+n4\n", "deny"},
		// the same with context-less (appending) chunks: these read SILENT while a
		// moved-in source was read as it lay on disk
		"added outside, moved in, moved on (appends)": {"*** Add File: ext/new.txt\n+n1\n+n2\n*** Update File: ext/new.txt\n*** Move to: pkg/mid.txt\n@@\n+n3\n" +
			"*** Update File: pkg/mid.txt\n*** Move to: pkg/svc/a.go\n@@\n+n4\n", "deny"},
		"rewritten outside, moved on (append)": {"*** Update File: ext/ten.txt\n@@\n-pkg/svc/a.go l02\n+Z\n*** Update File: ext/ten.txt\n*** Move to: pkg/svc/a.go\n@@\n+n4\n", "deny"},
		// a source never read: what it produces is a heads-up, carried through a later move, never silent
		"unread source, moved in, moved onto wb's file": {"*** Update File: ext/big.txt\n*** Move to: pkg/x.txt\n@@\n+x\n*** Update File: pkg/x.txt\n*** Move to: pkg/svc/a.go\n@@\n+y\n", "file-level"},
	} {
		if v := codex(tc.patch); v != tc.want {
			t.Errorf("%s: codex-edit under WT_CODEX_HOOK_BLOCK=1 = %s, want %s", name, v, tc.want)
		}
	}
	// #201 re-review B: a FIFO source must not block the hook
	if out, err := exec.Command("mkfifo", filepath.Join(outside, "fifo")).CombinedOutput(); err != nil {
		t.Logf("mkfifo unavailable, FIFO row skipped: %v %s", err, out)
	} else if v := codex("*** Update File: ext/fifo\n*** Move to: pkg/svc/a.go\n@@\n+x\n"); v != "file-level" {
		t.Errorf("a FIFO moved onto wb's file: codex-edit = %s, want a file-level heads-up", v)
	} else {
		// and a FIFO in the repo, edited by Claude, where a file wb edits was (the
		// same read: it blocked the Claude hook before #201 too)
		a := filepath.Join(wa, "pkg", "svc", "a.go")
		if err := os.Remove(a); err != nil {
			t.Fatal(err)
		}
		if out, err := exec.Command("mkfifo", a).CombinedOutput(); err != nil {
			t.Fatalf("mkfifo: %v %s", err, out)
		}
		p, _ := json.Marshal(map[string]any{"cwd": wa, "tool_name": "Edit", "tool_input": map[string]string{
			"file_path": a, "old_string": "pkg/svc/a.go l03\n", "new_string": "X\n"}})
		if v := hookVerdict(t, runHookWithin(t, 20*time.Second, hookClaudeEdit, string(p))); v != "file-level" {
			t.Errorf("claude-edit of a FIFO where wb edits = %s, want a file-level heads-up", v)
		}
	}
}

// runHookWithin is runHookCapture that fails the test, instead of hanging it,
// when the hook doesn't return within d: a hook must always return promptly.
func runHookWithin(t *testing.T, d time.Duration, hook func(io.Reader) int, payload string) string {
	t.Helper()
	r, w, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	stdout := os.Stdout
	os.Stdout = w
	done := make(chan int, 1)
	go func() { done <- hook(strings.NewReader(payload)) }()
	select {
	case code := <-done:
		os.Stdout = stdout
		w.Close()
		out, _ := io.ReadAll(r)
		if code != 0 {
			t.Errorf("hook exit %d, want 0 (fail-open)", code)
		}
		return strings.TrimSpace(string(out))
	case <-time.After(d):
		os.Stdout = stdout
		t.Fatalf("hook still running after %v", d)
		return ""
	}
}

// #201 review: in a `git clone --bare` layout, worktrees sit inside the bare
// repository's own directory, which `git worktree list` lists first (marked
// `bare`). It holds no repo paths, so `../HEAD` from a worktree inside it is
// outside every worktree and refused; placed in the bare directory it read as
// the repo path HEAD, and clear. A path in a sibling worktree still maps.
func TestCheck_BareLayoutIsNotAWorktree(t *testing.T) {
	hermeticGitT(t)
	offlineGH(t)
	t.Setenv("HOME", t.TempDir())
	t.Chdir(t.TempDir())

	src := t.TempDir()
	gitT(t, src, "init", "-q", "-b", "main")
	gitT(t, src, "config", "user.email", "t@t.test")
	gitT(t, src, "config", "user.name", "t")
	writeNested(t, src, "x.md", numbered("x.md"))
	gitT(t, src, "add", "-A")
	gitT(t, src, "commit", "-qm", "base")
	parent := t.TempDir()
	bare := filepath.Join(parent, "proj.git")
	gitT(t, parent, "clone", "-q", "--bare", src, bare)
	main, feat := filepath.Join(bare, "main"), filepath.Join(bare, "feat")
	gitT(t, bare, "worktree", "add", "-q", main, "main")
	gitT(t, bare, "worktree", "add", "-q", "-b", "feat", feat, "main")
	editLine1(t, feat, "x.md")

	if got := gitx.BareWorktreesIn(main); len(got) != 1 || resolveExisting(got[0]) != resolveExisting(bare) {
		t.Fatalf("BareWorktreesIn = %q, want the bare directory %q", got, bare)
	}
	if code := checkExit(t, main, "../HEAD"); code != 64 {
		t.Errorf("wt check ../HEAD in a worktree inside a bare repo = exit %d, want 64 (outside every worktree)", code)
	}
	if code, got := checkJSON(t, main, "../feat/x.md"); code != 3 || !reflect.DeepEqual(got, []string{"x.md←feat"}) {
		t.Errorf("wt check ../feat/x.md = exit %d %v, want exit 3 [x.md←feat]", code, got)
	}
}

// caseSensitiveDir is a directory on a case-sensitive filesystem: the test temp
// dir when it is one (Linux), else WT_CASE_SENSITIVE_DIR (on macOS, a
// case-sensitive APFS image mounted somewhere), else the test is skipped. The
// returned directory is fresh, named by os.MkdirTemp with no letter in its name.
func caseSensitiveDir(t *testing.T) string {
	t.Helper()
	probe := filepath.Join(t.TempDir(), "Probe")
	if err := os.Mkdir(probe, 0o755); err != nil {
		t.Fatal(err)
	}
	base := filepath.Dir(probe)
	if _, err := os.Stat(filepath.Join(base, "pROBE")); err == nil {
		if base = os.Getenv("WT_CASE_SENSITIVE_DIR"); base == "" {
			t.Skip("the temp dir folds case; set WT_CASE_SENSITIVE_DIR to a directory on a case-sensitive volume")
		}
	}
	dir, err := os.MkdirTemp(base, "")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { os.RemoveAll(dir) })
	return dir
}

// #201 review, on a real case-sensitive filesystem: the index tracks README.md,
// window B committed a NEW readme.md, and window A is about to create readme.md:
// an add/add conflict (`git merge-tree` says so). Folding readme.md into the
// tracked README.md read clear in `wt check` and both hooks. Run with git's own
// core.ignorecase (false: exercises the probe, which must look at the worktree's
// own `.git`, here under letterless names below the volume) and with it set
// true by hand (exercises acceptFold: two existing different spellings are two
// files), and with it set by hand and README.md deleted in A (#201 re-review:
// neither spelling on disk; core.ignorecase alone must not fold readme.md into
// the index's README.md).
func TestCheck_CaseSensitiveVolumeNeverMergesTwoFiles(t *testing.T) {
	for _, v := range []struct {
		name, ignorecase string
		rmTracked        bool
	}{
		{"core.ignorecase as git leaves it", "", false},
		{"core.ignorecase=true by hand", "true", false},
		{"core.ignorecase=true by hand, README.md deleted here", "true", true},
	} {
		ignorecase := v.ignorecase
		t.Run(v.name, func(t *testing.T) {
			hermeticGitT(t)
			offlineGH(t)
			t.Setenv("HOME", t.TempDir())
			t.Setenv("WT_SKIP_COLLISION", "")
			t.Setenv("HOOK_DISABLE_MULTIWINDOW_CHECK", "")
			t.Chdir(t.TempDir())
			base := caseSensitiveDir(t)
			root, wa, wb := filepath.Join(base, "1"), filepath.Join(base, "2"), filepath.Join(base, "3")
			gitT(t, base, "init", "-q", "-b", "main", "1")
			gitT(t, root, "config", "user.email", "t@t.test")
			gitT(t, root, "config", "user.name", "t")
			writeNested(t, root, "README.md", numbered("README.md"))
			gitT(t, root, "add", "-A")
			gitT(t, root, "commit", "-qm", "base")
			gitT(t, root, "worktree", "add", "-q", "-b", "wa", wa)
			gitT(t, root, "worktree", "add", "-q", "-b", "wb", wb)
			writeNested(t, wb, "readme.md", "B's new file\n")
			gitT(t, wb, "add", "-A")
			gitT(t, wb, "commit", "-qm", "wb adds readme.md")
			if ignorecase != "" { // set by hand afterwards: under it, B's add would have been refused
				gitT(t, root, "config", "core.ignorecase", ignorecase)
			}
			if v.rmTracked { // deleted, not staged: still in A's index
				if err := os.Remove(filepath.Join(wa, "README.md")); err != nil {
					t.Fatal(err)
				}
			}

			if code, got := checkJSON(t, wa, "readme.md"); code != 3 || !reflect.DeepEqual(got, []string{"readme.md←wb"}) {
				t.Errorf("wt check readme.md = exit %d %v, want exit 3 [readme.md←wb]: B created that file", code, got)
			}
			t.Setenv("WT_CLAUDE_HOOK_BLOCK", "1")
			p, _ := json.Marshal(map[string]any{"cwd": wa, "tool_name": "Write", "tool_input": map[string]string{
				"file_path": filepath.Join(wa, "readme.md"), "content": "A's new file\n"}})
			if v := hookVerdict(t, runHookCapture(t, hookClaudeEdit, string(p))); v != "deny" {
				t.Errorf("claude-edit Write readme.md under WT_CLAUDE_HOOK_BLOCK=1 = %s, want deny", v)
			}
			p, _ = json.Marshal(map[string]any{"cwd": wa, "tool_name": "apply_patch", "tool_input": map[string]string{
				"command": "*** Begin Patch\n*** Add File: readme.md\n+A's new file\n*** End Patch\n"}})
			if out := runHookCapture(t, hookCodexEdit, string(p)); !strings.Contains(out, "readme.md") || !strings.Contains(out, "wb") {
				t.Errorf("codex-edit Add File readme.md must warn about wb's readme.md, got %q", out)
			}
		})
	}
}

// #201 re-review, on a real case-sensitive filesystem with core.ignorecase set
// by hand: a bare name that is nothing here stays the search it is (#181), not
// a fold to the spelling another window touches. B created NOTES.md, C edits
// other/notes.md; `wt check notes.md` finds C by basename, as before #201.
// Folding to B's NOTES.md (a different file on this volume) dropped C.
func TestCheck_CaseSensitiveVolumeKeepsTheSearch(t *testing.T) {
	hermeticGitT(t)
	offlineGH(t)
	t.Setenv("HOME", t.TempDir())
	t.Chdir(t.TempDir())
	base := caseSensitiveDir(t)
	root := filepath.Join(base, "1")
	gitT(t, base, "init", "-q", "-b", "main", "1")
	gitT(t, root, "config", "user.email", "t@t.test")
	gitT(t, root, "config", "user.name", "t")
	writeNested(t, root, "base.md", "x\n")
	gitT(t, root, "add", "-A")
	gitT(t, root, "commit", "-qm", "base")
	wa, wb, wc := filepath.Join(base, "2"), filepath.Join(base, "3"), filepath.Join(base, "4")
	for b, dir := range map[string]string{"wa": wa, "wb": wb, "wc": wc} {
		gitT(t, root, "worktree", "add", "-q", "-b", b, dir)
	}
	writeNested(t, wb, "NOTES.md", "B\n")
	gitT(t, wb, "add", "-A")
	gitT(t, wb, "commit", "-qm", "wb adds NOTES.md")
	writeNested(t, wc, "other/notes.md", "C\n")
	gitT(t, wc, "add", "-A")
	gitT(t, wc, "commit", "-qm", "wc adds other/notes.md")
	gitT(t, root, "config", "core.ignorecase", "true")
	if code, got := checkJSON(t, wa, "notes.md"); code != 3 || !reflect.DeepEqual(got, []string{"notes.md←wc"}) {
		t.Errorf("wt check notes.md = exit %d %v, want exit 3 [notes.md←wc]: a search, which reaches other/notes.md", code, got)
	}
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
	}
	folds := foldsCase(wa)
	if folds {
		cases = append(cases, []struct {
			name, cwd string
			arg       string
			code      int
			want      []string
		}{
			// 4: case-insensitive names: the spelling git tracks / a window touches
			{"a case variant of a tracked file", wa, "pkg/svc/readme.md", 3, svc},
			{"a case variant of a directory", wa, "PKG/svc/README.md", 3, svc},
			{"a case variant of a file only another window has", wa, "docs/new.md", 3, []string{"docs/NEW.md←wb"}},
		}...)
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
	mcpCases := [][]string{{"svclink/README.md"}, {"."}, {filepath.Join(wb, "pkg", "svc", "README.md")}}
	if folds {
		mcpCases = append(mcpCases, []string{"pkg/svc/readme.md"})
	}
	for _, paths := range mcpCases {
		tc := struct{ paths, want []string }{paths, svc}
		if paths[0] == "." {
			tc.want = all
		}
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
	hookCases := []struct{ name, out string }{
		{"codex patches svclink/README.md", codexOut(wa, "svclink/README.md", svcLine)},
		{"codex in pkg/ patches ../svclink/README.md", codexOut(filepath.Join(wa, "pkg"), "../svclink/README.md", svcLine)},
		{"claude edits through svclink", claudeOut(wa, filepath.Join(wa, "svclink", "README.md"), svcLine)},
	}
	if folds {
		hookCases = append(hookCases, []struct{ name, out string }{
			{"codex patches pkg/svc/readme.md", codexOut(wa, "pkg/svc/readme.md", svcLine)},
			{"claude edits pkg/svc/readme.md", claudeOut(wa, filepath.Join(wa, "pkg", "svc", "readme.md"), svcLine)},
		}...)
	}
	for _, tc := range hookCases {
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
