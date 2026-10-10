package cli

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
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

func TestClaudeDecision(t *testing.T) {
	// no HIGH → nothing emitted
	if out, has := claudeDecision("a.go", nil, false, false); has || out != "" {
		t.Errorf("empty: %q,%v", out, has)
	}
	high := []CheckEntry{
		{Path: "a.go", Window: "#42", Liveness: "uncommitted edits"},
		{Path: "a.go", Window: "#42", Liveness: "uncommitted edits"}, // dup window
		{Path: "a.go", Window: "feat/x", Liveness: "open PR #7"},
	}
	// advisory (confirmed overlap) → additionalContext, no permissionDecision
	out, has := claudeDecision("a.go", high, false, false)
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
	out, _ = claudeDecision("a.go", high, true, false)
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
	fl, _ := claudeDecision("a.go", high, false, true)
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
