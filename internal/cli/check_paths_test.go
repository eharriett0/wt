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
