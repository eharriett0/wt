// Codex CLI UserPromptSubmit collision-awareness hook. Unlike Claude Code, Codex
// cannot gate an individual file edit: its PreToolUse hook fires on the shell
// tool only (apply_patch edits don't fire it) and only acts on "deny", not
// advisory context (openai/codex#19385). So the closest analog to the Claude
// per-edit advisory is UserPromptSubmit, which DOES inject additionalContext —
// before each turn we tell Codex which files other live windows are editing so
// it coordinates before touching them.
//
// wt's git pre-push/pre-commit guards + worktree-based collision engine are
// already agent-agnostic, so a Codex window is a first-class window (it shows in
// `wt status`, and its commits/pushes already hit wt's guards); this hook only
// adds the proactive per-prompt awareness on top. Always exits 0 and fails open —
// an awareness nicety must never disrupt the Codex session.
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

const codexMaxOverlapLines = 12

// parseCodexCwd extracts the session working directory from a Codex hook payload
// (the "cwd" common field). ok=false only when the JSON won't parse; an empty
// cwd is fine (the caller falls back to the process cwd). Pure — the testable core.
func parseCodexCwd(b []byte) (cwd string, ok bool) {
	var p struct {
		CWD string `json:"cwd"`
	}
	if err := json.Unmarshal(b, &p); err != nil {
		return "", false
	}
	return strings.TrimSpace(p.CWD), true
}

// windowsExcluding returns labels minus the current window's label (deduped,
// order preserved). Pure.
func windowsExcluding(labels []string, current string) []string {
	seen := map[string]bool{}
	var out []string
	for _, l := range labels {
		if l == current || seen[l] {
			continue
		}
		seen[l] = true
		out = append(out, l)
	}
	return out
}

// codexContextMessage builds the UserPromptSubmit additionalContext from the
// active cross-window overlaps, EXCLUDING the current window from each "also
// being edited by" list. Empty (has=false) when nothing another live window is
// touching would collide. Pure — the testable core.
func codexContextMessage(overlaps []StatusOverlap, currentLabel string) (msg string, has bool) {
	var lines []string
	for _, o := range overlaps {
		others := windowsExcluding(o.Windows, currentLabel)
		if len(others) == 0 {
			continue
		}
		grade := "same file"
		if o.Severity == "HIGH" {
			grade = "overlapping hunks — HIGH"
		}
		lines = append(lines, fmt.Sprintf("  %s — also being edited by %s (%s)", o.File, strings.Join(others, ", "), grade))
	}
	if len(lines) == 0 {
		return "", false
	}
	if len(lines) > codexMaxOverlapLines {
		extra := len(lines) - codexMaxOverlapLines
		kept := append([]string{}, lines[:codexMaxOverlapLines]...)
		lines = append(kept, fmt.Sprintf("  …and %d more", extra))
	}
	msg = "wt (multi-window coordination) — other live windows are editing files in this repo:\n" +
		strings.Join(lines, "\n") +
		"\nRun `wt check <file>` before editing any of these for line-level detail, and coordinate to avoid a duplicate PR / merge conflict. (Set WT_SKIP_COLLISION=1 to silence.)"
	return msg, true
}

// hookCodexContext implements `wt _hook codex-context` — a Codex UserPromptSubmit
// hook. Reads the payload from r, derives the repo from its cwd, and prints the
// cross-window collision awareness as additionalContext. Always exits 0 (fail-open).
func hookCodexContext(r io.Reader) int {
	if os.Getenv("WT_SKIP_COLLISION") == "1" || os.Getenv("HOOK_DISABLE_MULTIWINDOW_CHECK") == "1" {
		return 0
	}
	b, err := io.ReadAll(r)
	if err != nil {
		return 0
	}
	cwd, ok := parseCodexCwd(b)
	if !ok {
		return 0
	}
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
	ws, err := collide.Scan(c)
	if err != nil {
		return 0
	}
	ov := collide.Overlaps(ws)
	live := collide.ClassifyWindows(ws, c.Base, collide.OverlapWindowSet(ov), c.MaxAge)
	active, _ := collide.PartitionOverlaps(ov, live)
	graded := gradeStatusOverlaps(c, ws, active)
	if msg, has := codexContextMessage(graded, collide.LabelForWorktree(ws, root)); has {
		emitCodexContext(msg)
	}
	return 0
}

// emitCodexContext prints the UserPromptSubmit additionalContext JSON that Codex
// injects into the model's context.
func emitCodexContext(msg string) {
	var out struct {
		HookSpecificOutput struct {
			HookEventName     string `json:"hookEventName"`
			AdditionalContext string `json:"additionalContext"`
		} `json:"hookSpecificOutput"`
	}
	out.HookSpecificOutput.HookEventName = "UserPromptSubmit"
	out.HookSpecificOutput.AdditionalContext = msg
	if j, err := json.Marshal(out); err == nil {
		fmt.Println(string(j))
	}
}

// codexHookCommand is the command Codex runs for the UserPromptSubmit hook.
const codexHookCommand = "wt _hook codex-context"

// codexHookSnippet is the .codex/hooks.json entry that wires the hook (same
// nested shape Codex shares with Claude Code; UserPromptSubmit takes no matcher).
const codexHookSnippet = `{
  "hooks": {
    "UserPromptSubmit": [
      {
        "hooks": [
          { "type": "command", "command": "wt _hook codex-context" }
        ]
      }
    ]
  }
}`

// cmdInstallCodexHook prints (or, with --write, merges) the UserPromptSubmit hook
// into the project's .codex/hooks.json, and always reminds the operator of the
// one-time config.toml opt-in Codex requires.
func cmdInstallCodexHook(args []string) int {
	write := false
	for _, a := range args {
		if a == "--write" {
			write = true
		}
	}
	if !write {
		ui.Info("add this to .codex/hooks.json (project) so Codex gets multi-window collision awareness each turn:")
		fmt.Println(codexHookSnippet)
		ui.Info("or run `wt install-codex-hook --write` to merge it automatically")
		printCodexOptIn()
		return 0
	}
	return withConfig(func(c *config.Config) int {
		path := filepath.Join(c.Root, ".codex", "hooks.json")
		merged, changed, err := mergeCodexHook(path)
		if err != nil {
			ui.Err("install-codex-hook: %v (add the snippet by hand: `wt install-codex-hook`)", err)
			return 1
		}
		if !changed {
			ui.OK("Codex UserPromptSubmit hook already wired in %s", path)
			printCodexOptIn()
			return 0
		}
		if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
			ui.Err("install-codex-hook: %v", err)
			return 1
		}
		if err := os.WriteFile(path, merged, 0o644); err != nil {
			ui.Err("install-codex-hook: %v", err)
			return 1
		}
		ui.OK("wired the Codex awareness hook into %s", path)
		printCodexOptIn()
		return 0
	})
}

// printCodexOptIn reminds the operator that Codex hooks are opt-in via config.toml.
func printCodexOptIn() {
	ui.Info("Codex hooks are opt-in — enable them once in ~/.codex/config.toml:")
	fmt.Println("  [features]")
	fmt.Println("  hooks = true")
}

// mergeCodexHook reads .codex/hooks.json (a fresh {} if absent), ensures a
// UserPromptSubmit command entry running our command is present WITHOUT
// clobbering any existing hooks, and returns the pretty-printed result + whether
// it changed. An unparseable file is a refuse (err) — never overwrite blind.
func mergeCodexHook(path string) (out []byte, changed bool, err error) {
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
	ups, _ := hooks["UserPromptSubmit"].([]any)

	// already present? (any UserPromptSubmit group whose inner hooks run our command)
	for _, g := range ups {
		gm, _ := g.(map[string]any)
		inner, _ := gm["hooks"].([]any)
		for _, h := range inner {
			hm, _ := h.(map[string]any)
			if cmd, _ := hm["command"].(string); cmd == codexHookCommand {
				return nil, false, nil
			}
		}
	}
	entry := map[string]any{
		"hooks": []any{map[string]any{"type": "command", "command": codexHookCommand}},
	}
	hooks["UserPromptSubmit"] = append(ups, entry)
	root["hooks"] = hooks
	b, err := json.MarshalIndent(root, "", "  ")
	if err != nil {
		return nil, false, err
	}
	return append(b, '\n'), true, nil
}
