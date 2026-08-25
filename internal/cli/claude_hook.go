// Claude Code PreToolUse collision hook (#95). `wt check` is human-invoked, and
// the failure mode that motivated the pre-push guard is that people — and now
// AGENTS — forget to run it. This wires wt's collision engine into the Claude
// Code edit loop: before an Edit/Write/MultiEdit, it runs the SAME grading as
// `wt check` on the target file and surfaces a HIGH cross-worktree overlap to
// the agent as advisory context (or a hard deny under WT_CLAUDE_HOOK_BLOCK=1).
//
// Advisory-first, and always fail-open (any error / uncertainty → allow) — a
// coordination nicety must never disrupt the editing session.
package cli

import (
	"encoding/json"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"

	"github.com/eharriett0/wt/internal/collide"
	"github.com/eharriett0/wt/internal/config"
	"github.com/eharriett0/wt/internal/gitx"
	"github.com/eharriett0/wt/internal/ui"
)

// parseClaudeEdit extracts (cwd, file, relevant) from a Claude Code PreToolUse
// payload. Relevant only for the file-editing tools that carry a file_path
// (Edit / Write / MultiEdit). Pure — the testable core.
func parseClaudeEdit(b []byte) (cwd, file string, relevant bool) {
	var p struct {
		CWD       string `json:"cwd"`
		ToolName  string `json:"tool_name"`
		ToolInput struct {
			FilePath string `json:"file_path"`
		} `json:"tool_input"`
	}
	if err := json.Unmarshal(b, &p); err != nil {
		return "", "", false
	}
	switch p.ToolName {
	case "Edit", "Write", "MultiEdit":
	default:
		return p.CWD, "", false
	}
	f := strings.TrimSpace(p.ToolInput.FilePath)
	return p.CWD, f, f != ""
}

// claudeDecision shapes the hook's stdout JSON from the HIGH collisions on the
// edited file. No collision → ("", false). Advisory (default) → additionalContext
// (the agent sees it, proceeds). block=true → permissionDecision "deny" (hard
// stop). Pure — the testable core.
func claudeDecision(file string, high []CheckEntry, block bool) (string, bool) {
	if len(high) == 0 {
		return "", false
	}
	seen := map[string]bool{}
	var who []string
	for _, e := range high {
		if seen[e.Window] {
			continue
		}
		seen[e.Window] = true
		who = append(who, fmt.Sprintf("%s [%s]", e.Window, e.Liveness))
	}
	msg := fmt.Sprintf("wt collision: %s is also being edited by %s (overlapping hunks). "+
		"Another live window is in this file — coordinate before editing to avoid a merge conflict / duplicate PR. "+
		"If you've already coordinated, set WT_SKIP_COLLISION=1.", file, strings.Join(who, ", "))

	var out struct {
		HookSpecificOutput struct {
			HookEventName            string `json:"hookEventName"`
			AdditionalContext        string `json:"additionalContext,omitempty"`
			PermissionDecision       string `json:"permissionDecision,omitempty"`
			PermissionDecisionReason string `json:"permissionDecisionReason,omitempty"`
		} `json:"hookSpecificOutput"`
	}
	out.HookSpecificOutput.HookEventName = "PreToolUse"
	if block {
		out.HookSpecificOutput.PermissionDecision = "deny"
		out.HookSpecificOutput.PermissionDecisionReason = msg
	} else {
		out.HookSpecificOutput.AdditionalContext = msg
	}
	j, err := json.Marshal(out)
	if err != nil {
		return "", false
	}
	return string(j), true
}

// repoRelativePath converts an absolute file path to a clean repo-relative one,
// resolving symlinks so a /var vs /private/var mismatch on macOS doesn't defeat
// the prefix strip. A path already relative is cleaned as-is. Returns "" when
// the file is outside root (nothing to collision-check).
func repoRelativePath(root, file string) string {
	if !filepath.IsAbs(file) {
		return filepath.Clean(file)
	}
	rr := root
	if r, err := filepath.EvalSymlinks(root); err == nil {
		rr = r
	}
	rf := file
	if r, err := filepath.EvalSymlinks(file); err == nil {
		rf = r
	}
	rel, err := filepath.Rel(rr, rf)
	if err != nil || rel == ".." || strings.HasPrefix(rel, ".."+string(filepath.Separator)) {
		return ""
	}
	return rel
}

// hookClaudeEdit implements `wt _hook claude-edit`. Always exits 0 (the JSON
// permissionDecision drives any block); every failure path allows.
func hookClaudeEdit(r io.Reader) int {
	if os.Getenv("WT_SKIP_COLLISION") == "1" || os.Getenv("HOOK_DISABLE_MULTIWINDOW_CHECK") == "1" {
		return 0
	}
	b, err := io.ReadAll(r)
	if err != nil {
		return 0
	}
	cwd, file, relevant := parseClaudeEdit(b)
	if !relevant {
		return 0
	}
	// Resolve everything from the agent's cwd (which may be a native subagent
	// worktree), so the collision scan + config load target the right repo.
	if cwd != "" {
		if err := os.Chdir(cwd); err != nil {
			return 0
		}
	}
	// Cheap on the common case: a repo with ≤1 worktree can't collide.
	if paths, err := gitx.WorktreePaths(); err != nil || len(paths) <= 1 {
		return 0
	}
	c, err := config.Load()
	if err != nil {
		return 0
	}
	root, err := gitx.RepoRoot()
	if err != nil {
		return 0
	}
	rel := repoRelativePath(root, file)
	if rel == "" {
		return 0
	}
	ws, err := collide.Scan(c)
	if err != nil {
		return 0
	}
	entries := buildCheckReport(c, ws, root, []string{rel}, false)
	var high []CheckEntry
	for _, e := range entries {
		if e.Category == CatBlocking {
			high = append(high, e)
		}
	}
	if out, has := claudeDecision(rel, high, os.Getenv("WT_CLAUDE_HOOK_BLOCK") == "1"); has {
		fmt.Println(out)
	}
	return 0
}

// claudeHookSnippet is the .claude/settings.json entry that wires the hook.
const claudeHookSnippet = `{
  "hooks": {
    "PreToolUse": [
      {
        "matcher": "Edit|Write|MultiEdit",
        "hooks": [
          { "type": "command", "command": "wt _hook claude-edit" }
        ]
      }
    ]
  }
}`

const claudeHookCommand = "wt _hook claude-edit"

// cmdInstallClaudeHook prints (or, with --write, merges) the PreToolUse hook
// entry into the project's .claude/settings.json (#95).
func cmdInstallClaudeHook(args []string) int {
	write := false
	for _, a := range args {
		if a == "--write" {
			write = true
		}
	}
	if !write {
		ui.Info("add this to .claude/settings.json (project) to collision-check every agent edit (#95):")
		fmt.Println(claudeHookSnippet)
		ui.Info("or run `wt install-claude-hook --write` to merge it automatically")
		ui.Info("advisory by default; WT_CLAUDE_HOOK_BLOCK=1 makes a HIGH collision a hard deny; WT_SKIP_COLLISION=1 bypasses")
		return 0
	}
	return withConfig(func(c *config.Config) int {
		path := filepath.Join(c.Root, ".claude", "settings.json")
		merged, changed, err := mergeClaudeHook(path)
		if err != nil {
			ui.Err("install-claude-hook: %v (add the snippet by hand: `wt install-claude-hook`)", err)
			return 1
		}
		if !changed {
			ui.OK("Claude Code PreToolUse hook already wired in %s", path)
			return 0
		}
		if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
			ui.Err("install-claude-hook: %v", err)
			return 1
		}
		if err := os.WriteFile(path, merged, 0o644); err != nil {
			ui.Err("install-claude-hook: %v", err)
			return 1
		}
		ui.OK("wired the collision hook into %s", path)
		ui.Info("advisory by default; WT_CLAUDE_HOOK_BLOCK=1 makes a HIGH collision a hard deny")
		return 0
	})
}

// mergeClaudeHook reads .claude/settings.json (a fresh {} if absent), ensures a
// PreToolUse Edit|Write|MultiEdit entry running our command is present WITHOUT
// clobbering any existing hooks, and returns the pretty-printed result + whether
// it changed. An unparseable file is a refuse (err) — never overwrite blind.
func mergeClaudeHook(path string) (out []byte, changed bool, err error) {
	root := map[string]any{}
	if b, rerr := os.ReadFile(path); rerr == nil {
		if jerr := json.Unmarshal(b, &root); jerr != nil {
			return nil, false, fmt.Errorf("%s is not valid JSON", path)
		}
	}
	hooks, _ := root["hooks"].(map[string]any)
	if hooks == nil {
		hooks = map[string]any{}
	}
	pre, _ := hooks["PreToolUse"].([]any)

	// already present? (any PreToolUse group whose inner hooks run our command)
	for _, g := range pre {
		gm, _ := g.(map[string]any)
		inner, _ := gm["hooks"].([]any)
		for _, h := range inner {
			hm, _ := h.(map[string]any)
			if cmd, _ := hm["command"].(string); cmd == claudeHookCommand {
				return nil, false, nil
			}
		}
	}
	entry := map[string]any{
		"matcher": "Edit|Write|MultiEdit",
		"hooks":   []any{map[string]any{"type": "command", "command": claudeHookCommand}},
	}
	hooks["PreToolUse"] = append(pre, entry)
	root["hooks"] = hooks
	b, err := json.MarshalIndent(root, "", "  ")
	if err != nil {
		return nil, false, err
	}
	return append(b, '\n'), true, nil
}
