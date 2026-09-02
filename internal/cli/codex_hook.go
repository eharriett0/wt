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
	"slices"
	"strings"

	"github.com/eharriett0/wt/internal/collide"
	"github.com/eharriett0/wt/internal/config"
	"github.com/eharriett0/wt/internal/coord"
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
		// "also" only when THIS window is one of the participants; otherwise it's a
		// heads-up about a file two OTHER windows are contesting (don't imply this
		// window is editing it — and if the current window can't be identified,
		// currentLabel is "" and we stay with the neutral phrasing).
		verb := "being edited by"
		if currentLabel != "" && slices.Contains(o.Windows, currentLabel) {
			verb = "also being edited by"
		}
		lines = append(lines, fmt.Sprintf("  %s — %s %s (%s)", o.File, verb, strings.Join(others, ", "), grade))
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

// hookAgentContext implements the per-turn UserPromptSubmit hooks
// (`wt _hook codex-context` / `wt _hook claude-context` — both agents share the
// cwd-in / additionalContext-out shape). Reads the payload from r, derives the
// repo from its cwd, and injects the multi-window awareness the window should see
// this turn: cross-window file overlaps PLUS un-acked coordination signals (holds
// + announcements) from other windows. Always exits 0 (fail-open); silent when
// there's nothing to say or the repo has ≤1 worktree.
func hookAgentContext(r io.Reader) int {
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
	// Cheap on the common case: a repo with ≤1 worktree is not multi-window, so
	// there are no other windows to collide with OR to have posted coordination.
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

	var parts []string
	if msg, has := codexContextMessage(graded, collide.LabelForWorktree(ws, root)); has {
		parts = append(parts, msg)
	}
	// Coordination signals — un-acked holds + announcements from other windows.
	// Fail-open: a coord read error just omits this block (never breaks the turn).
	if logPath, self := coordCtx(c); logPath != "" {
		if recs, rerr := coord.Load(logPath); rerr == nil {
			if msg, has := coordContextMessage(coord.Inbox(recs, self)); has {
				parts = append(parts, msg)
			}
		}
	}
	if len(parts) > 0 {
		emitAgentContext(strings.Join(parts, "\n\n"))
	}
	return 0
}

// coordContextMessage renders the un-acked coordination signals from OTHER
// windows for the per-turn context: active HOLDS (an op another window asked you
// to avoid until all-clear) + plain announcements. inbox is already self-excluded
// and un-acked (coord.Inbox). Holds come first (they survive the cap); the full
// record id is shown so `wt ack <id>` copy-pastes. Pure; has=false when empty.
func coordContextMessage(inbox []coord.Record) (msg string, has bool) {
	var holds, notes []string
	for _, r := range inbox {
		who := r.Window
		if who == "" {
			who = "another window"
		}
		suffix := ""
		if m := strings.TrimSpace(r.Message); m != "" {
			suffix = " — " + m
		}
		if len(r.Hold) > 0 {
			holds = append(holds, fmt.Sprintf("  ⚠ HOLD %s [%s]%s (wt ack %s)", who, strings.Join(r.Hold, ","), suffix, r.ID))
		} else {
			notes = append(notes, fmt.Sprintf("  %s%s (wt ack %s)", who, suffix, r.ID))
		}
	}
	if len(holds) == 0 && len(notes) == 0 {
		return "", false
	}
	lines := append([]string{}, holds...)
	lines = append(lines, notes...)
	if len(lines) > codexMaxOverlapLines {
		extra := len(lines) - codexMaxOverlapLines
		lines = append(lines[:codexMaxOverlapLines:codexMaxOverlapLines], fmt.Sprintf("  …and %d more", extra))
	}
	msg = "wt coordination — un-acked signals from other windows (respect any HOLD before that op):\n" +
		strings.Join(lines, "\n") +
		"\nSee `wt inbox` for detail; `wt ack <id>` to acknowledge. (Set WT_SKIP_COLLISION=1 to silence.)"
	return msg, true
}

// emitAgentContext prints the UserPromptSubmit additionalContext JSON that Codex
// and Claude Code both inject into the model's context.
func emitAgentContext(msg string) {
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

// ---- edit-time hook (#117) ---------------------------------------------------

// parseCodexEdit extracts (cwd, patch, relevant) from a Codex PreToolUse payload.
// Relevant only for apply_patch, whose tool_input.command carries the patch text;
// Bash/other tools return relevant=false (git commit/push already hit wt's git
// guards, and parsing arbitrary shell for edit targets is unreliable). Pure.
func parseCodexEdit(b []byte) (cwd, patch string, relevant bool) {
	var p struct {
		CWD       string `json:"cwd"`
		ToolName  string `json:"tool_name"`
		ToolInput struct {
			Command string `json:"command"`
		} `json:"tool_input"`
	}
	if err := json.Unmarshal(b, &p); err != nil {
		return "", "", false
	}
	if p.ToolName != "apply_patch" {
		return p.CWD, "", false
	}
	return strings.TrimSpace(p.CWD), p.ToolInput.Command, p.ToolInput.Command != ""
}

// codexHunk is one apply_patch hunk. preImage is the ordered context+removed
// lines (all present in the CURRENT file — used to locate the hunk via
// locateRange); removed holds the offsets into preImage that are actually
// removed (leading '-'), so we can range the MODIFIED lines precisely and NOT
// the surrounding context (which anchors the hunk but isn't a change — including
// it would false-flag edits merely adjacent to another window's, defeating wt's
// -U0 exact-hunk grading; #117 review).
type codexHunk struct {
	preImage []string
	removed  []int
}

// codexPatchFile is one file section of an apply_patch payload: its repo-relative
// path (+ move destination) and its Update hunks.
type codexPatchFile struct {
	path    string
	newPath string
	hunks   []codexHunk
}

// parseCodexPatch parses an apply_patch payload into its file sections. Pure —
// the testable core. Best-effort: it never errors, it just extracts what it can.
func parseCodexPatch(patch string) []codexPatchFile {
	var files []codexPatchFile
	var cur *codexPatchFile
	var pre []string
	var rem []int
	flushHunk := func() {
		if cur != nil && len(pre) > 0 {
			cur.hunks = append(cur.hunks, codexHunk{preImage: pre, removed: rem})
		}
		pre, rem = nil, nil
	}
	flushFile := func() {
		flushHunk()
		if cur != nil {
			files = append(files, *cur)
			cur = nil
		}
	}
	start := func(ln, prefix string) {
		flushFile()
		cur = &codexPatchFile{path: strings.TrimSpace(strings.TrimPrefix(ln, prefix))}
	}
	lines := strings.Split(patch, "\n")
	// Drop the single trailing "" a terminating newline produces, so it isn't
	// mistaken for a blank context line appended to the last open hunk.
	if n := len(lines); n > 0 && lines[n-1] == "" {
		lines = lines[:n-1]
	}
	for _, ln := range lines {
		switch {
		case strings.HasPrefix(ln, "*** Update File: "):
			start(ln, "*** Update File: ")
		case strings.HasPrefix(ln, "*** Add File: "):
			start(ln, "*** Add File: ")
		case strings.HasPrefix(ln, "*** Delete File: "):
			start(ln, "*** Delete File: ")
		case strings.HasPrefix(ln, "*** Move File: "):
			start(ln, "*** Move File: ")
		case strings.HasPrefix(ln, "*** Move to: "):
			if cur != nil {
				cur.newPath = strings.TrimSpace(strings.TrimPrefix(ln, "*** Move to: "))
			}
		case strings.HasPrefix(ln, "***"):
			// Begin/End Patch + any other control line — not file content
		case ln == "@@" || strings.HasPrefix(ln, "@@ "):
			flushHunk() // hunk boundary — the @@ header isn't file content
		case cur == nil:
			// preamble noise
		case strings.HasPrefix(ln, "+"):
			// added line — NOT in the current file; skip
		case strings.HasPrefix(ln, "-"):
			rem = append(rem, len(pre))
			pre = append(pre, ln[1:]) // removed line — present in the current file
		case strings.HasPrefix(ln, " "):
			pre = append(pre, ln[1:]) // context line — present in the current file
		case ln == "":
			// blank context line emitted WITHOUT a leading space (an apply_patch
			// quirk); reached only inside an open hunk (cur==nil is caught above).
			pre = append(pre, "")
		}
	}
	flushFile()
	return files
}

// patchPaths returns every repo-relative path an apply_patch touches (update /
// add / delete targets + move destinations), deduped. Pure.
func patchPaths(files []codexPatchFile) []string {
	seen := map[string]bool{}
	var out []string
	add := func(p string) {
		if p != "" && !seen[p] {
			seen[p] = true
			out = append(out, p)
		}
	}
	for _, f := range files {
		add(f.path)
		add(f.newPath)
	}
	return out
}

// contiguousRuns groups increasing offsets into [first,last] runs. Pure.
func contiguousRuns(offsets []int) [][2]int {
	if len(offsets) == 0 {
		return nil
	}
	var runs [][2]int
	s, e := offsets[0], offsets[0]
	for _, o := range offsets[1:] {
		if o == e+1 {
			e = o
			continue
		}
		runs = append(runs, [2]int{s, e})
		s, e = o, o
	}
	return append(runs, [2]int{s, e})
}

// patchRangesInFile locates each hunk's pre-image in content, then ranges only
// the REMOVED lines within it — matching wt's -U0 exact-hunk grading. Pure-add
// hunks (no removed lines) modify no existing line, so they contribute no range
// (an insertion adjacent to another window's edit isn't a conflict). ok=false
// when a hunk can't be uniquely located, or nothing is a real modification — the
// caller then falls back to a file-level advisory. Frame-safety (content ==
// base) is the caller's job (the #108 lesson). Reuses locateRange.
func patchRangesInFile(f codexPatchFile, content string) ([]gitx.LineRange, bool) {
	var ranges []gitx.LineRange
	for _, h := range f.hunks {
		if len(h.preImage) == 0 {
			continue
		}
		r, ok := locateRange(content, strings.Join(h.preImage, "\n"))
		if !ok {
			return nil, false
		}
		if len(h.removed) == 0 {
			continue // pure addition — no existing line modified
		}
		for _, run := range contiguousRuns(h.removed) {
			ranges = append(ranges, gitx.LineRange{Start: r.Start + run[0], End: r.Start + run[1]})
		}
	}
	if len(ranges) == 0 {
		return nil, false
	}
	return ranges, true
}

// hookCodexEdit implements `wt _hook codex-edit` — a Codex PreToolUse hook on
// apply_patch. It grades the patch's target files with the SAME engine as
// `wt check`, re-graded against the patch's actual hunks when frame-safe (the
// #108 lesson), and emits additionalContext on a HIGH overlap — or, under
// WT_CODEX_HOOK_BLOCK=1, a `deny` for a CONFIRMED HIGH only. Always exits 0
// (fail-open); disjoint / no-overlap / ≤1-worktree stay silent.
func hookCodexEdit(r io.Reader) int {
	if os.Getenv("WT_SKIP_COLLISION") == "1" || os.Getenv("HOOK_DISABLE_MULTIWINDOW_CHECK") == "1" {
		return 0
	}
	b, err := io.ReadAll(r)
	if err != nil {
		return 0
	}
	cwd, patch, relevant := parseCodexEdit(b)
	if !relevant {
		return 0
	}
	if cwd != "" {
		if err := os.Chdir(cwd); err != nil {
			return 0
		}
	}
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
	files := parseCodexPatch(patch)
	paths := patchPaths(files)
	if len(paths) == 0 {
		return 0
	}
	byPath := map[string]codexPatchFile{}
	for _, f := range files {
		byPath[f.path] = f
		if f.newPath != "" {
			byPath[f.newPath] = f // a move grades the destination too (#117 review)
		}
	}

	entries := buildCheckReport(c, ws, root, paths, false)
	var high []codexGradedEntry
	anyConfirmed := false
	for _, e := range entries {
		if e.Category != CatBlocking {
			continue
		}
		// Re-grade against the patch's ACTUAL hunks, but only when frame-safe: the
		// hunk line numbers (located in the on-disk file) match e.OtherRanges'
		// base frame only when this worktree's file is unchanged vs base (#108).
		conf := false
		if pending, ok := pendingPatchRanges(byPath, e.Path, root, c.Base); ok && len(e.OtherRanges) > 0 {
			if collide.ConflictSeverity(pending, e.OtherRanges, false) != collide.SevHigh {
				continue // patch hunks are disjoint from the other window — no overlap
			}
			conf = true // frame-safe, real overlapping hunks
			anyConfirmed = true
		}
		high = append(high, codexGradedEntry{entry: e, confirmed: conf})
	}
	if out, has := codexEditDecision(high, os.Getenv("WT_CODEX_HOOK_BLOCK") == "1" && anyConfirmed); has {
		fmt.Println(out)
	}
	return 0
}

// codexGradedEntry pairs a blocking CheckEntry with whether its overlap was
// frame-safe hunk-CONFIRMED (vs a file-level heads-up), so a multi-file patch
// can word each line accurately (#117 review).
type codexGradedEntry struct {
	entry     CheckEntry
	confirmed bool
}

// pendingPatchRanges returns the patch's edited ranges for relPath, but ONLY when
// this worktree's file is unchanged vs base (frame-safe) — otherwise the located
// line numbers don't share e.OtherRanges' base frame. ok=false → the caller keeps
// the entry as a file-level advisory rather than risk a wrong grade.
func pendingPatchRanges(byPath map[string]codexPatchFile, relPath, root, base string) ([]gitx.LineRange, bool) {
	f, ok := byPath[relPath]
	if !ok || len(f.hunks) == 0 {
		return nil, false
	}
	if len(gitx.ChangedRanges(root, base, relPath)) != 0 {
		return nil, false // this worktree already diverged for the path — not frame-safe
	}
	data, err := os.ReadFile(filepath.Join(root, relPath))
	if err != nil {
		return nil, false
	}
	return patchRangesInFile(f, string(data))
}

// codexEditDecision shapes the PreToolUse stdout JSON. deny=true → permissionDecision
// "deny" (only ever passed when the batch has ≥1 CONFIRMED HIGH); else
// additionalContext. Each file is tagged per-entry — "overlapping hunks" (frame-safe
// confirmed) vs "hunk overlap not computed" (file-level heads-up) — so a mixed
// multi-file patch never overstates hunk overlap on an unverified file (#117 review).
// Pure.
func codexEditDecision(high []codexGradedEntry, deny bool) (string, bool) {
	if len(high) == 0 {
		return "", false
	}
	seen := map[string]bool{}
	var lines []string
	anyConfirmed, anyFileLevel := false, false
	for _, g := range high {
		e := g.entry
		if seen[e.Path] {
			continue
		}
		seen[e.Path] = true
		tag := "hunk overlap not computed — run `wt check`"
		if g.confirmed {
			tag = "overlapping hunks"
			anyConfirmed = true
		} else {
			anyFileLevel = true
		}
		lines = append(lines, fmt.Sprintf("  %s — also being edited by %s [%s] (%s)", e.Path, e.Window, e.Liveness, tag))
	}
	header := "wt: your apply_patch touches file(s) another live window is editing:"
	if anyConfirmed && !anyFileLevel {
		header = "wt collision: your apply_patch OVERLAPS hunks another live window is editing:"
	}
	msg := header + "\n" + strings.Join(lines, "\n") +
		"\nCoordinate before applying to avoid a merge conflict / duplicate PR. (Set WT_SKIP_COLLISION=1 to silence.)"
	var out struct {
		HookSpecificOutput struct {
			HookEventName            string `json:"hookEventName"`
			AdditionalContext        string `json:"additionalContext,omitempty"`
			PermissionDecision       string `json:"permissionDecision,omitempty"`
			PermissionDecisionReason string `json:"permissionDecisionReason,omitempty"`
		} `json:"hookSpecificOutput"`
	}
	out.HookSpecificOutput.HookEventName = "PreToolUse"
	if deny {
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

// codexHookCommand is the command Codex runs for the UserPromptSubmit hook.
const codexHookCommand = "wt _hook codex-context"

// codexEditHookCommand is the command Codex runs for the PreToolUse edit hook.
const codexEditHookCommand = "wt _hook codex-edit"

// codexHookSnippet is the .codex/hooks.json entry that wires both hooks (same
// nested shape Codex shares with Claude Code): UserPromptSubmit for the per-turn
// coordination snapshot, PreToolUse (matcher apply_patch) for edit-time checks.
const codexHookSnippet = `{
  "hooks": {
    "UserPromptSubmit": [
      {
        "hooks": [
          { "type": "command", "command": "wt _hook codex-context" }
        ]
      }
    ],
    "PreToolUse": [
      {
        "matcher": "apply_patch",
        "hooks": [
          { "type": "command", "command": "wt _hook codex-edit" }
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
		ui.Info("add this to .codex/hooks.json (project) so Codex gets multi-window collision awareness:")
		ui.Info("  • UserPromptSubmit — per-turn snapshot of who's touching what")
		ui.Info("  • PreToolUse (apply_patch) — edit-time overlap check before each patch")
		fmt.Println(codexHookSnippet)
		ui.Info("or run `wt install-codex-hook --write` to merge both automatically")
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
			ui.OK("Codex hooks (UserPromptSubmit + PreToolUse) already wired in %s", path)
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
		ui.OK("wired the Codex awareness hooks (UserPromptSubmit + PreToolUse) into %s", path)
		printCodexOptIn()
		return 0
	})
}

// printCodexOptIn reminds the operator that Codex hooks are opt-in via config.toml.
func printCodexOptIn() {
	ui.Info("Codex hooks are enabled by default (definitions still require trust/review on first run).")
	ui.Info("To turn them off entirely, set in ~/.codex/config.toml:")
	fmt.Println("  [features]")
	fmt.Println("  hooks = false")
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
	c1 := ensureHookEntry(hooks, "UserPromptSubmit", "", codexHookCommand)
	c2 := ensureHookEntry(hooks, "PreToolUse", "apply_patch", codexEditHookCommand)
	if !c1 && !c2 {
		return nil, false, nil
	}
	root["hooks"] = hooks
	b, err := json.MarshalIndent(root, "", "  ")
	if err != nil {
		return nil, false, err
	}
	return append(b, '\n'), true, nil
}

// ensureHookEntry adds a {matcher?, hooks:[{type,command}]} group under event
// (shared by the codex + claude installs)
// iff no existing group already runs command (idempotent; preserves whatever
// else the operator has wired). Returns whether it mutated hooks.
func ensureHookEntry(hooks map[string]any, event, matcher, command string) bool {
	list, _ := hooks[event].([]any)
	for _, g := range list {
		gm, _ := g.(map[string]any)
		inner, _ := gm["hooks"].([]any)
		for _, h := range inner {
			hm, _ := h.(map[string]any)
			if cmd, _ := hm["command"].(string); cmd == command {
				return false
			}
		}
	}
	entry := map[string]any{
		"hooks": []any{map[string]any{"type": "command", "command": command}},
	}
	if matcher != "" {
		entry["matcher"] = matcher
	}
	hooks[event] = append(list, entry)
	return true
}
