package cli

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/eharriett0/wt/internal/gitx"
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

func TestLocateRange(t *testing.T) {
	content := "line1\nline2\nline3\nline4\n" // lines 1..4
	cases := []struct {
		name      string
		old       string
		wantStart int
		wantEnd   int
		wantOK    bool
	}{
		{"single line", "line2\n", 2, 2, true},
		{"multi line span", "line2\nline3\n", 2, 3, true},
		{"first line", "line1\n", 1, 1, true},
		{"empty old_string → not localizable", "", 0, 0, false},
		{"absent → not localizable", "nope", 0, 0, false},
		{"ambiguous (appears twice) → not localizable", "line", 0, 0, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			r, ok := locateRange(content, tc.old)
			if ok != tc.wantOK || (ok && (r.Start != tc.wantStart || r.End != tc.wantEnd)) {
				t.Fatalf("locateRange(%q) = (%+v, %v), want (L%d-%d, %v)", tc.old, r, ok, tc.wantStart, tc.wantEnd, tc.wantOK)
			}
		})
	}
}

func TestClaudeEditRanges(t *testing.T) {
	content := "a\nb\nc\nd\ne\nf\n" // 6 lines
	// Edit locates its old_string
	raw := `{"tool_name":"Edit","tool_input":{"file_path":"x","old_string":"b\n","new_string":"B\n"}}`
	if r, ok := claudeEditRanges([]byte(raw), content); !ok || len(r) != 1 || r[0].Start != 2 || r[0].End != 2 {
		t.Errorf("Edit: (%+v, %v)", r, ok)
	}
	// #199: the claim is what the edit changes, not the context old_string carries
	ctx := `{"tool_name":"Edit","tool_input":{"file_path":"x","old_string":"a\nb\nc\n","new_string":"a\nB\nc\n"}}`
	if r, ok := claudeEditRanges([]byte(ctx), content); !ok || len(r) != 1 || r[0] != (gitx.LineRange{Start: 2, End: 2}) {
		t.Errorf("Edit with context lines: (%+v, %v), want line 2 only", r, ok)
	}
	// MultiEdit unions all locatable ranges
	multi := `{"tool_name":"MultiEdit","tool_input":{"file_path":"x","edits":[{"old_string":"b\n"},{"old_string":"e\n"}]}}`
	if r, ok := claudeEditRanges([]byte(multi), content); !ok || len(r) != 2 {
		t.Errorf("MultiEdit: (%+v, %v)", r, ok)
	}
	// Write has no locatable region → file-level fallback
	if _, ok := claudeEditRanges([]byte(`{"tool_name":"Write","tool_input":{"file_path":"x","content":"whole"}}`), content); ok {
		t.Error("Write should not be localizable")
	}
	// an old_string not in the file → not localizable (file-level fallback)
	if _, ok := claudeEditRanges([]byte(`{"tool_name":"Edit","tool_input":{"old_string":"zzz\n"}}`), content); ok {
		t.Error("absent old_string should not be localizable")
	}
	// a MultiEdit where one edit can't be located → whole thing falls back
	mixed := `{"tool_name":"MultiEdit","tool_input":{"edits":[{"old_string":"b\n"},{"old_string":"zzz\n"}]}}`
	if _, ok := claudeEditRanges([]byte(mixed), content); ok {
		t.Error("MultiEdit with an unlocatable edit should fall back")
	}
}

// #199: a pending Edit claims what git will report once it is made: the lines it
// changes, not the context its old_string carries to be unique (since touching
// edits conflict, claiming a context line would flag a window editing the line
// beside it), and an Edit that only adds or drops lines is that insertion or
// deletion, over every position git may slide it to.
func TestClaudeEditClaim(t *testing.T) {
	content := "a\nb\nc\nd\ne\nf\n" // lines 1..6
	gap := func(p int) gitx.LineRange { return gitx.LineRange{Start: p, End: p + 1, Gap: true} }
	chg := func(s, e int) gitx.LineRange { return gitx.LineRange{Start: s, End: e} }
	cases := []struct {
		name, content, old, new string
		want                    gitx.LineRange
	}{
		{"rewrite a line", content, "b\n", "B\n", chg(2, 2)},
		{"context lines are not claimed", content, "a\nb\nc\n", "a\nB\nc\n", chg(2, 2)},
		{"two changed lines inside context", content, "b\nc\nd\n", "b\nC\nD\n", chg(3, 4)},
		{"append after a line: an insertion", content, "c\n", "c\nX\n", gap(3)},
		{"prepend before a line: an insertion", content, "c\n", "X\nc\n", gap(2)},
		{"insert between two context lines", content, "b\nc\n", "b\nX\nc\n", gap(2)},
		{"drop a line between context lines", content, "b\nc\nd\n", "b\nd\n", chg(3, 3)},
		{"drop the whole old_string", content, "b\n", "", chg(2, 2)},
		{"a mid-line old_string claims its whole line", content, "d", "D", chg(4, 4)},
		{"joining the next line claims it too", content, "c\n", "c", chg(3, 4)},
		{"a no-op claims what it touches", content, "c\n", "c\n", chg(3, 3)},
		{"an insertion git may slide claims its slide", "a\nx\nx\nb\n", "a\nx\n", "a\nx\nx\n", chg(2, 3)},
		{"the unterminated last line: what it touches", "a\nb", "b", "B", chg(2, 2)},
	}
	for _, c := range cases {
		if got := claudeEditClaim(c.content, c.old, c.new); got != c.want {
			t.Errorf("%s: claudeEditClaim(%q -> %q) = %v, want %v", c.name, c.old, c.new, got, c.want)
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
