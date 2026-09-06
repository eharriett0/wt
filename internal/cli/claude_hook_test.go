package cli

import (
	"encoding/json"
	"os"
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

func TestRepoRelativePath(t *testing.T) {
	// relative stays relative (cleaned)
	if got := repoRelativePath("/repo", "internal/a.go"); got != "internal/a.go" {
		t.Errorf("relative = %q", got)
	}
	// outside the repo → ""
	if got := repoRelativePath("/repo", "/elsewhere/x.go"); got != "" {
		t.Errorf("outside = %q, want empty", got)
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
}
