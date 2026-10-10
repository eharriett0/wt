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
	// Not trimmed: a file name can begin or end with a space, and the path is
	// asked about exactly (#181), as git reports it (#200).
	f := p.ToolInput.FilePath
	return p.CWD, f, strings.TrimSpace(f) != ""
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
		msg = fmt.Sprintf("wt collision: with this edit, your version of %s OVERLAPS a region that %s is also editing (`wt check %s` will grade it HIGH). "+
			"Coordinate before editing to avoid a merge conflict / duplicate PR. If you've already coordinated, set WT_SKIP_COLLISION=1.",
			file, strings.Join(who, ", "), file)
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

// repoRelativePath converts file to a clean, slash-separated repo-relative path.
// Returns "" when it is outside root, or is root itself: nothing to
// collision-check.
//
// A relative file is read relative to the CURRENT directory, whose
// repo-relative prefix is prefix (`git rev-parse --show-prefix`, "" at the
// root), not relative to root. Collision matching is exact (#181), so reading
// `README.md` typed in pkg/svc/ as the root README.md would check the wrong
// file; the old suffix match used to paper over that by accident.
//
// An absolute file is made relative to root with symlinks resolved on both
// sides, so a /var vs /private/var mismatch on macOS doesn't defeat the prefix
// strip. That includes a file that doesn't exist yet (a Write creating it, a
// path that exists only on another branch): its deepest existing ancestor is
// resolved instead (resolveExisting).
func repoRelativePath(root, prefix, file string) string {
	var rel string
	if filepath.IsAbs(file) {
		r, err := filepath.Rel(resolveExisting(root), resolveExisting(file))
		if err != nil {
			return ""
		}
		rel = r
	} else {
		rel = filepath.Clean(filepath.Join(filepath.FromSlash(prefix), file))
	}
	if rel == "." || rel == ".." || strings.HasPrefix(rel, ".."+string(filepath.Separator)) {
		return ""
	}
	return filepath.ToSlash(rel)
}

// resolveExisting returns the absolute path p with symlinks resolved in its
// deepest EXISTING ancestor and the rest re-appended. filepath.EvalSymlinks
// fails outright for a path that doesn't exist, which left such a path on the
// logical /var/… side of a comparison with git's physical /private/var/… root,
// where it read as outside the repo and went unchecked. Falls back to the
// cleaned p.
func resolveExisting(p string) string {
	p = filepath.Clean(p)
	var rest []string
	for cur := p; ; {
		if r, err := filepath.EvalSymlinks(cur); err == nil {
			return filepath.Join(append([]string{r}, rest...)...)
		}
		parent := filepath.Dir(cur)
		if parent == cur {
			return p
		}
		rest = append([]string{filepath.Base(cur)}, rest...)
		cur = parent
	}
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
	prefix := ""
	if !filepath.IsAbs(file) { // Claude sends absolute paths; only a relative one needs the cwd
		prefix, _ = gitx.ShowPrefix()
	}
	rel := repoRelativePath(root, prefix, file)
	if rel == "" {
		return 0
	}
	ws, err := collide.Scan(c)
	if err != nil {
		return 0
	}
	// #181: the edit target is a real repo-relative path, so it matches EXACTLY,
	// as the pre-push guard does. A fuzzy match flagged an edit to the root
	// README.md because another window edited pkg/svc/README.md.
	entries := buildCheckReport(c, ws, root, collide.ExactQueries([]string{rel}), false)
	if len(entries) == 0 {
		return 0
	}

	// #108/#184/#199: the hook fires at PRE-edit time, so buildCheckReport's
	// "current" side (this worktree's own edits) doesn't yet include the edit the
	// agent is ABOUT to make. Re-grade every entry the way `wt check` will once
	// it's made: this window's ranges as they will read with the file the edit
	// produces (claudePendingContent), measured by the same diff and base mapping
	// as `wt check` (gitx.ChangedRangesWith), against the other window's ranges.
	// When that can't be computed (an old_string the tool would reject, a line
	// ending it doesn't model, a new file, a git error) the entry stays as a
	// file-level heads-up instead of being dropped.
	var after []gitx.LineRange
	afterOK := false
	data, readErr := os.ReadFile(filepath.Join(root, rel))
	if content, ok := claudePendingContent(b, string(data), readErr == nil); ok {
		after, afterOK = pendingRanges(root, c.Base, rel, []byte(content))
	}
	graded := regradePending(entries, after, afterOK, func(e CheckEntry) bool {
		return subsumedByBase(e.otherWorktree, c.Base, rel) // that window's, not a namesake's (#193)
	})

	var high []CheckEntry
	fileLevel := false
	for _, g := range graded {
		high = append(high, g.entry)
		if !g.confirmed {
			fileLevel = true
		}
	}
	if out, has := claudeDecision(rel, high, os.Getenv("WT_CLAUDE_HOOK_BLOCK") == "1", fileLevel); has {
		fmt.Println(out)
	}
	return 0
}

// pendingRanges is gitx.ChangedRangesWith: this window's ranges as they will
// read once the pending edit is made, ok=false when git couldn't measure them.
// A var only so the fail-safe test can make that measurement fail and pin that
// the hooks then keep a file-level heads-up rather than read the failure as "no
// edits" (#184 review).
var pendingRanges = gitx.ChangedRangesWith

// pendingGrade is one collision entry regradePending keeps for a pending edit.
// confirmed: graded from the edit itself (the file collides once it's made);
// false: a file-level heads-up, kept because that grade couldn't be computed,
// or because the edit leaves this window no edits to overlap with (`wt check`'s
// indeterminate HIGH).
type pendingGrade struct {
	entry     CheckEntry
	confirmed bool
}

// regradePending keeps exactly the entries `wt check` will grade HIGH once an
// agent's pending edit is made, the pre-edit hooks' block predicate (it MUST
// equal `wt check`'s, #92/#108). after is this window's ranges as `wt check`
// will read them then: the file the edit produces, measured by the same diff and
// base mapping (gitx.ChangedRangesWith, #199 review), so the grade is `wt
// check`'s by construction, not a guess at which lines the edit changes. A
// hunk-graded entry, HIGH or FYI alike, is HIGH afterwards iff after conflicts
// with the other window's ranges by git's rule (collide.ConflictSeverity: they
// overlap, or touch with no unchanged line between, #199). An FYI entry is
// re-graded too: a window whose earlier edits were disjoint can still be about
// to overlap (#184 review). A newly-HIGH entry is then put through the #122
// subsumed check `wt check` applies to every HIGH (subsumed is only called for
// those). An edit that leaves this window's copy as base's (after empty) reads
// HIGH, as `wt check` reads an empty side (indeterminate), but no hunk overlap
// is computed, so it is a heads-up, not a confirmed overlap.
//
// When that grade can't be computed (afterOK false, or an entry graded without
// line ranges: an indeterminate side, a structured-doc section) the entry is
// kept as a file-level heads-up if it could become HIGH: never dropped on an
// unknown. Left out: entries no edit of this window can make HIGH (stale,
// already merged, untracked, subsumed, append-only), and shared-doc advisories,
// which the hooks have never graded per edit (a structured doc's section grade
// is `wt check`'s and pre-push's). Pure.
func regradePending(entries []CheckEntry, after []gitx.LineRange, afterOK bool, subsumed func(CheckEntry) bool) []pendingGrade {
	var out []pendingGrade
	for _, e := range entries {
		hunkGraded := len(e.OtherRanges) > 0 && !e.Subsumed && (e.Category == CatBlocking || e.Category == CatFYI)
		if e.Category != CatBlocking && !hunkGraded {
			continue
		}
		if !hunkGraded || !afterOK {
			out = append(out, pendingGrade{entry: e})
			continue
		}
		if collide.ConflictSeverity(after, e.OtherRanges, false) != collide.SevHigh {
			continue
		}
		if e.Category != CatBlocking && subsumed(e) {
			continue
		}
		out = append(out, pendingGrade{entry: e, confirmed: len(after) > 0})
	}
	return out
}

// claudePendingContent is the edited file's content once a Claude Code Edit,
// MultiEdit or Write lands, computed the way Claude Code computes it (#199
// review): a Write's content as given; an Edit's replacement of old_string in
// the file (applyClaudeEdit), each MultiEdit edit on the previous one's result.
// Claude Code matches a CRLF file as LF and writes it back as CRLF; an edit's
// own CRLFs are read as LF there too (if the tool didn't, it would find no match
// and make no edit, so nothing is graded wrongly). ok=false when it can't be
// told: an edit the tool would reject (old_string absent, or not unique without
// replace_all), a missing file, a file mixing line endings, a lone CR, an
// unknown tool. The hook then keeps its file-level heads-up. Pure.
func claudePendingContent(raw []byte, content string, exists bool) (string, bool) {
	type edit struct {
		OldString  string `json:"old_string"`
		NewString  string `json:"new_string"`
		ReplaceAll bool   `json:"replace_all"`
	}
	var p struct {
		ToolName  string `json:"tool_name"`
		ToolInput struct {
			edit
			Content *string `json:"content"`
			Edits   []edit  `json:"edits"`
		} `json:"tool_input"`
	}
	if err := json.Unmarshal(raw, &p); err != nil {
		return "", false
	}
	var edits []edit
	switch p.ToolName {
	case "Write":
		if p.ToolInput.Content == nil {
			return "", false
		}
		return *p.ToolInput.Content, true
	case "Edit":
		edits = []edit{p.ToolInput.edit}
	case "MultiEdit":
		edits = p.ToolInput.Edits
	}
	if !exists || len(edits) == 0 {
		return "", false
	}
	crlf := strings.Contains(content, "\r")
	if crlf {
		// Only a file whose every line ends CRLF has one reading as LF.
		if n := strings.Count(content, "\r\n"); n != strings.Count(content, "\n") || n != strings.Count(content, "\r") {
			return "", false
		}
		content = strings.ReplaceAll(content, "\r\n", "\n")
	}
	for _, e := range edits {
		if crlf {
			e.OldString = strings.ReplaceAll(e.OldString, "\r\n", "\n")
			e.NewString = strings.ReplaceAll(e.NewString, "\r\n", "\n")
		}
		if strings.Contains(e.OldString+e.NewString, "\r") {
			return "", false
		}
		next, ok := applyClaudeEdit(content, e.OldString, e.NewString, e.ReplaceAll)
		if !ok {
			return "", false
		}
		content = next
	}
	if crlf {
		content = strings.ReplaceAll(content, "\n", "\r\n")
	}
	return content, true
}

// applyClaudeEdit is Claude Code's replacement of old with new in content:
// refused (ok=false) when old is empty, unchanged by new, absent, or found more
// than once without replaceAll; the first match replaced, or every one under
// replaceAll. Deleting (new empty) an old that doesn't end its line takes the
// newline after it too, when one follows. Pure.
func applyClaudeEdit(content, old, new string, replaceAll bool) (string, bool) {
	n := strings.Count(content, old)
	if old == "" || old == new || n == 0 || (n > 1 && !replaceAll) {
		return "", false
	}
	if new == "" && !strings.HasSuffix(old, "\n") && strings.Contains(content, old+"\n") {
		old += "\n"
	}
	if replaceAll {
		return strings.ReplaceAll(content, old, new), true
	}
	return strings.Replace(content, old, new, 1), true
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
