package cli

import (
	"fmt"
	"os"
	"strings"
	"testing"
)

func TestParseCodexCwd(t *testing.T) {
	if cwd, ok := parseCodexCwd([]byte(`{"cwd":"/repo","session_id":"x","prompt":"hi"}`)); !ok || cwd != "/repo" {
		t.Errorf("valid: (%q,%v)", cwd, ok)
	}
	// empty cwd is still ok (caller falls back to process cwd)
	if cwd, ok := parseCodexCwd([]byte(`{"session_id":"x"}`)); !ok || cwd != "" {
		t.Errorf("no cwd: (%q,%v)", cwd, ok)
	}
	// whitespace trimmed
	if cwd, _ := parseCodexCwd([]byte(`{"cwd":"  /r  "}`)); cwd != "/r" {
		t.Errorf("trim: %q", cwd)
	}
	// garbage → not ok
	if _, ok := parseCodexCwd([]byte(`not json`)); ok {
		t.Error("garbage should not parse")
	}
}

func TestWindowsExcluding(t *testing.T) {
	got := windowsExcluding([]string{"#1", "feat/x", "#1", "feat/y"}, "#1")
	if len(got) != 2 || got[0] != "feat/x" || got[1] != "feat/y" {
		t.Fatalf("windowsExcluding = %v, want [feat/x feat/y]", got)
	}
}

func TestCodexContextMessage(t *testing.T) {
	// no overlaps → nothing
	if _, has := codexContextMessage(nil, "#1"); has {
		t.Error("no overlaps → no message")
	}
	// an overlap whose only window IS the current one → skipped (nothing to say)
	if _, has := codexContextMessage([]StatusOverlap{{File: "a", Windows: []string{"#1"}}}, "#1"); has {
		t.Error("overlap containing only the current window should be skipped")
	}

	ov := []StatusOverlap{
		{File: "foo.go", Windows: []string{"#1", "feat/x"}, Severity: "HIGH"},
		{File: "bar.go", Windows: []string{"feat/y", "#1"}, Severity: "low"},
	}
	msg, has := codexContextMessage(ov, "#1")
	if !has {
		t.Fatal("expected a message")
	}
	if strings.Contains(msg, "#1") {
		t.Errorf("current window #1 must be excluded from the message: %q", msg)
	}
	if !strings.Contains(msg, "feat/x") || !strings.Contains(msg, "feat/y") {
		t.Errorf("other windows missing: %q", msg)
	}
	if !strings.Contains(msg, "foo.go") || !strings.Contains(msg, "HIGH") {
		t.Errorf("HIGH overlap not surfaced: %q", msg)
	}
	if !strings.Contains(msg, "bar.go") || !strings.Contains(msg, "same file") {
		t.Errorf("non-HIGH overlap not surfaced / mislabeled: %q", msg)
	}
	if !strings.Contains(msg, "wt check") {
		t.Errorf("missing the `wt check` reminder: %q", msg)
	}
}

func TestCodexContextMessage_Caps(t *testing.T) {
	var ov []StatusOverlap
	for i := 0; i < codexMaxOverlapLines+5; i++ {
		ov = append(ov, StatusOverlap{File: fmt.Sprintf("f%d.go", i), Windows: []string{"other"}, Severity: "HIGH"})
	}
	msg, has := codexContextMessage(ov, "#1")
	if !has {
		t.Fatal("expected a message")
	}
	if !strings.Contains(msg, "…and 5 more") {
		t.Errorf("expected the cap summary line: %q", msg)
	}
}

func TestMergeCodexHook_FreshAndIdempotent(t *testing.T) {
	dir := t.TempDir()
	path := dir + "/hooks.json"
	out, changed, err := mergeCodexHook(path)
	if err != nil || !changed {
		t.Fatalf("fresh: changed=%v err=%v", changed, err)
	}
	if !strings.Contains(string(out), codexHookCommand) || !strings.Contains(string(out), "UserPromptSubmit") {
		t.Errorf("fresh output missing entry:\n%s", out)
	}
	if err := os.WriteFile(path, out, 0o644); err != nil {
		t.Fatal(err)
	}
	if _, changed, err := mergeCodexHook(path); err != nil || changed {
		t.Errorf("idempotent second merge: changed=%v err=%v", changed, err)
	}
}

func TestMergeCodexHook_PreservesExisting(t *testing.T) {
	dir := t.TempDir()
	path := dir + "/hooks.json"
	existing := `{"hooks":{"PostToolUse":[{"hooks":[{"type":"command","command":"other-tool"}]}],` +
		`"UserPromptSubmit":[{"hooks":[{"type":"command","command":"my-existing"}]}]}}`
	if err := os.WriteFile(path, []byte(existing), 0o644); err != nil {
		t.Fatal(err)
	}
	out, changed, err := mergeCodexHook(path)
	if err != nil || !changed {
		t.Fatalf("changed=%v err=%v", changed, err)
	}
	s := string(out)
	if !strings.Contains(s, "other-tool") || !strings.Contains(s, "my-existing") {
		t.Errorf("existing hooks clobbered:\n%s", s)
	}
	if !strings.Contains(s, codexHookCommand) {
		t.Errorf("our command not added:\n%s", s)
	}
}

func TestMergeCodexHook_RefusesGarbage(t *testing.T) {
	dir := t.TempDir()
	path := dir + "/hooks.json"
	if err := os.WriteFile(path, []byte("not json"), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, _, err := mergeCodexHook(path); err == nil {
		t.Error("expected a refuse (error) on non-JSON, not a blind overwrite")
	}
}
