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

// claudeDecision shapes the hook's stdout JSON from the collisions kept for the
// edited file. No collision → ("", false). fileLevel=true means the overlap
// couldn't be confirmed against the agent's PENDING edit (a Write, or the edit
// region couldn't be located), so the wording is an honest FILE-LEVEL heads-up
// that never claims hunk overlap — it can't then contradict `wt check` the way
// the old always-"overlapping hunks" message did (#108). Advisory (default) →
// additionalContext; block=true → permissionDecision "deny". Pure.
func claudeDecision(file string, high []CheckEntry, block, fileLevel bool) (string, bool) {
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
	var msg string
	if fileLevel {
		msg = fmt.Sprintf("wt: %s is also being edited by %s — file-level heads-up (hunk overlap not computed; run `wt check %s` for line-level detail). "+
			"Coordinate before editing to avoid a merge conflict / duplicate PR. If you've already coordinated, set WT_SKIP_COLLISION=1.",
			file, strings.Join(who, ", "), file)
	} else {
		msg = fmt.Sprintf("wt collision: your edit OVERLAPS a region of %s that %s is also editing. "+
			"Coordinate before editing to avoid a merge conflict / duplicate PR. If you've already coordinated, set WT_SKIP_COLLISION=1.",
			file, strings.Join(who, ", "))
	}

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

	// #108: the hook fires at PRE-edit time, so buildCheckReport's "current" side
	// (this worktree's diff vs base) doesn't yet include the edit the agent is
	// ABOUT to make — an empty current side fail-safes to HIGH, so the hook
	// reported "overlapping hunks" on files it hadn't touched, contradicting
	// `wt check`. Re-grade each HIGH against the agent's ACTUAL pending edit range
	// (located from old_string): a disjoint pending edit is dropped; only a real
	// overlap fires with the "overlapping" wording. When the region can't be
	// located (Write, or old_string not uniquely found), keep it but word it as a
	// file-level heads-up instead of overstating a hunk overlap it can't compute.
	// The pending-edit re-grade is only FRAME-SAFE when this worktree's file is
	// unchanged vs base: then the agent's pending edit, located by line number in
	// the on-disk file, is in the same (base) line frame as e.OtherRanges
	// (ChangedRanges → base-side hunk coords). If the current file has already
	// diverged from base, the frames skew by the net line-delta above the edit, so
	// re-grading could DROP a real overlap — there we keep the entry and word it
	// file-level instead of silently suppressing it.
	curEmpty := len(gitx.ChangedRanges(root, c.Base, rel)) == 0
	var pending []gitx.LineRange
	pendingOK := false
	if curEmpty {
		if data, err := os.ReadFile(filepath.Join(root, rel)); err == nil {
			pending, pendingOK = claudeEditRanges(b, string(data))
		}
	}

	var high []CheckEntry
	fileLevel := false
	for _, e := range entries {
		if e.Category != CatBlocking {
			continue
		}
		if pendingOK && len(e.OtherRanges) > 0 {
			if collide.ConflictSeverity(pending, e.OtherRanges, false) != collide.SevHigh {
				continue // frame-safe (cur empty): pending disjoint from other → not a real overlap
			}
			// confirmed overlap in a shared frame → keep, "OVERLAPS" wording
		} else {
			fileLevel = true // couldn't confirm a frame-safe hunk overlap → file-level wording
		}
		high = append(high, e)
	}

	if out, has := claudeDecision(rel, high, os.Getenv("WT_CLAUDE_HOOK_BLOCK") == "1", fileLevel); has {
		fmt.Println(out)
	}
	return 0
}

// locateRange finds old in content and returns the 1-based inclusive line range
// it spans. ok=false when old is empty, absent, or occurs more than once
// (ambiguous — Edit requires a unique old_string, but be safe). Pure.
func locateRange(content, old string) (gitx.LineRange, bool) {
	if old == "" || strings.Count(content, old) != 1 {
		return gitx.LineRange{}, false
	}
	i := strings.Index(content, old)
	start := 1 + strings.Count(content[:i], "\n")
	// end = line of the LAST character: a trailing "\n" terminates its own line,
	// it doesn't extend the range into the next one ("line2\n" spans line 2 only).
	end := start + strings.Count(old, "\n")
	if strings.HasSuffix(old, "\n") {
		end--
	}
	return gitx.LineRange{Start: start, End: end}, true
}

// claudeEditRanges returns the line ranges a pending Edit/MultiEdit will touch,
// by locating each old_string in the CURRENT (pre-edit) file content. ok=false
// for Write (whole-file, no region), or any old_string that can't be uniquely
// located — the caller then falls back to a file-level heads-up rather than claim
// a hunk overlap it can't compute. Pure — the testable core.
func claudeEditRanges(raw []byte, content string) ([]gitx.LineRange, bool) {
	var p struct {
		ToolName  string `json:"tool_name"`
		ToolInput struct {
			OldString string `json:"old_string"`
			Edits     []struct {
				OldString string `json:"old_string"`
			} `json:"edits"`
		} `json:"tool_input"`
	}
	if err := json.Unmarshal(raw, &p); err != nil {
		return nil, false
	}
	var olds []string
	switch p.ToolName {
	case "Edit":
		olds = []string{p.ToolInput.OldString}
	case "MultiEdit":
		for _, e := range p.ToolInput.Edits {
			olds = append(olds, e.OldString)
		}
	default:
		return nil, false // Write / unknown → no locatable region
	}
	if len(olds) == 0 {
		return nil, false
	}
	var ranges []gitx.LineRange
	for _, old := range olds {
		r, ok := locateRange(content, old)
		if !ok {
			return nil, false // ambiguous / not found → file-level fallback
		}
		ranges = append(ranges, r)
	}
	return ranges, true
}

// claudeHookSnippet is the .claude/settings.json entry that wires all three hooks:
// PreToolUse (per-edit collision check) + PostToolUse/TodoWrite (mirror each
// window's TODO list so `wt todos` can show it) + UserPromptSubmit (per-turn
// multi-window awareness — overlaps + un-acked coordination holds/announcements).
const claudeHookSnippet = `{
  "hooks": {
    "PreToolUse": [
      {
        "matcher": "Edit|Write|MultiEdit",
        "hooks": [
          { "type": "command", "command": "wt _hook claude-edit" }
        ]
      }
    ],
    "PostToolUse": [
      {
        "matcher": "TodoWrite",
        "hooks": [
          { "type": "command", "command": "wt _hook todo-write" }
        ]
      }
    ],
    "UserPromptSubmit": [
      {
        "hooks": [
          { "type": "command", "command": "wt _hook claude-context" }
        ]
      }
    ]
  }
}`

const claudeHookCommand = "wt _hook claude-edit"

// claudeContextCommand is the UserPromptSubmit hook — the per-turn multi-window
// awareness snapshot (shares hookAgentContext with codex-context).
const claudeContextCommand = "wt _hook claude-context"

// todoWriteCommand is the PostToolUse/TodoWrite hook that mirrors each window's
// Claude Code TODO list into the wt todo store (the data source for `wt todos`).
// Nothing wired it before #144, so `wt todos` was permanently empty even though
// the recording hook (`wt _hook todo-write`) already existed.
const todoWriteCommand = "wt _hook todo-write"

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
		ui.Info("add this to .claude/settings.json (project) so Claude Code gets multi-window collision awareness:")
		ui.Info("  • PreToolUse — collision-check every agent edit (#95)")
		ui.Info("  • PostToolUse — mirror each window's TodoWrite list so `wt todos` shows what every window is on")
		ui.Info("  • UserPromptSubmit — per-turn snapshot of file overlaps + coordination holds/announcements")
		fmt.Println(claudeHookSnippet)
		ui.Info("or run `wt install-claude-hook --write` to merge all three automatically")
		ui.Info("advisory by default; WT_CLAUDE_HOOK_BLOCK=1 makes a HIGH collision a hard deny; WT_SKIP_COLLISION=1 bypasses")
		ui.Info("tip: set WT_MAX_AGE (e.g. 5d) to keep the per-turn hook from flagging stale/abandoned worktrees as HIGH")
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
			ui.OK("Claude Code hooks (PreToolUse + PostToolUse + UserPromptSubmit) already wired in %s", path)
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
		ui.OK("wired the Claude hooks (PreToolUse + PostToolUse + UserPromptSubmit) into %s", path)
		ui.Info("advisory by default; WT_CLAUDE_HOOK_BLOCK=1 makes a HIGH collision a hard deny")
		ui.Info("`wt todos` will populate once the agent uses the TodoWrite tool in a window")
		return 0
	})
}

// mergeClaudeHook reads .claude/settings.json (a fresh {} if absent), ensures the
// PreToolUse (Edit|Write|MultiEdit), PostToolUse (TodoWrite) and UserPromptSubmit
// entries running our commands are present WITHOUT clobbering any existing hooks,
// and returns the pretty-printed result + whether it changed. An unparseable file
// is a refuse (err) — never overwrite blind.
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
	c1 := ensureHookEntry(hooks, "PreToolUse", "Edit|Write|MultiEdit", claudeHookCommand)
	c2 := ensureHookEntry(hooks, "UserPromptSubmit", "", claudeContextCommand)
	c3 := ensureHookEntry(hooks, "PostToolUse", "TodoWrite", todoWriteCommand)
	if !c1 && !c2 && !c3 {
		return nil, false, nil
	}
	root["hooks"] = hooks
	b, err := json.MarshalIndent(root, "", "  ")
	if err != nil {
		return nil, false, err
	}
	return append(b, '\n'), true, nil
}
