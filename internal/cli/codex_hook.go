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
	"time"

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

// agentContextOverlaps grades the cross-window overlaps for the per-turn banner
// from the CURRENT window's side (#182) — the I/O half (classify via gh/git,
// grade via git) of agentOverlaps. The current window's own liveness is never
// consulted, so it isn't classified: one gh lookup fewer per turn.
func agentContextOverlaps(c *config.Config, ws []collide.Window, root string) []StatusOverlap {
	self := collide.SelfFor(ws, root)
	ov := collide.Overlaps(ws)
	labels := collide.OverlapWindowSet(ov)
	if self.Label != "" {
		// Another window sharing self's label goes unclassified with it, and so
		// counts as live: never suppressed on ambiguity.
		delete(labels, self.Label)
	}
	live := collide.ClassifyWindows(ws, c.Base, labels, c.MaxAge)
	return agentOverlaps(c, gitFactsFor(c), ws, ov, live, self)
}

// agentOverlaps is the banner's decision (#182): which overlaps it lists, with
// whom, and how they grade. The banner used to reuse `wt status`'s window-
// neutral pipeline, so merged / dormant / closed-PR windows were listed and
// graded, and two OTHER windows overlapping each other read as HIGH on a file
// this window was editing; `wt check` from this window said low for all of them.
// Now a file this window edits lists only the windows `wt check <file>` lists
// by default, and reads HIGH only where `wt check` would block: HIGH ⇒ check
// blocks, deliberately not the converse (gradeOverlaps says why). Pure given
// newFacts.
//
// The returned Windows are display names: a window that shares a label with
// self or with another window in the same overlap is suffixed with its worktree
// (bannerWindows), so codexContextMessage, which tells self apart by label,
// neither drops it as "self" nor merges two windows into one name.
func agentOverlaps(c *config.Config, newFacts func() gradeFacts, ws []collide.Window, ov []collide.Overlap, live map[string]collide.WindowLiveness, self collide.Self) []StatusOverlap {
	active, _ := collide.PartitionOverlapsFor(ov, live, self)
	graded := gradeOverlaps(c, newFacts, ws, active, live, self)
	for i := range graded {
		graded[i].Windows = bannerWindows(active[i], self)
	}
	return graded
}

// bannerWindows names o's windows for the banner (#182). Self keeps its plain
// label. Any other window whose label is self's, or is repeated within o, gets
// the shortest tail of its worktree path that tells it apart from its namesakes,
// "#77 (fix-77-b)" or "x (dupb/x)", so no two windows render alike and none
// renders as self. Pure.
func bannerWindows(o collide.Overlap, self collide.Self) []string {
	count := map[string]int{}
	for _, l := range o.Windows {
		count[l]++
	}
	aligned := len(o.Worktrees) == len(o.Windows)
	out := make([]string, len(o.Windows))
	for i, l := range o.Windows {
		out[i] = l
		if !aligned || self.Is(o, i) || (count[l] < 2 && l != self.Label) {
			continue
		}
		var namesakes []string
		for j, wt := range o.Worktrees {
			if j != i && o.Windows[j] == l {
				namesakes = append(namesakes, wt)
			}
		}
		if l == self.Label && self.Worktree != "" {
			namesakes = append(namesakes, self.Worktree)
		}
		out[i] = l + " (" + distinctTail(o.Worktrees[i], namesakes) + ")"
	}
	return out
}

// distinctTail is the shortest run of trailing path segments of wt that none of
// others ends with: "fix-77-b", or "dupb/x" when another namesake is ".../x".
// The whole path when nothing shorter is distinct. Pure.
func distinctTail(wt string, others []string) string {
	segs := strings.Split(filepath.ToSlash(filepath.Clean(wt)), "/")
	for k := 1; k < len(segs); k++ {
		tail := strings.Join(segs[len(segs)-k:], "/")
		clash := false
		for _, o := range others {
			o = filepath.ToSlash(filepath.Clean(o))
			if o == tail || strings.HasSuffix(o, "/"+tail) {
				clash = true
				break
			}
		}
		if !clash {
			return tail
		}
	}
	return wt
}

// hookAgentContext implements the per-turn UserPromptSubmit hooks
// (`wt _hook codex-context` / `wt _hook claude-context` — both agents share the
// cwd-in / additionalContext-out shape; codex says which one this is, for the
// session fallback in agentHookSession). Reads the payload from r, derives the
// repo from its cwd, and injects the multi-window awareness the window should see
// this turn: cross-window file overlaps PLUS un-acked coordination signals (holds
// + announcements) from other windows and from another session in this checkout.
// Always exits 0 (fail-open); silent when there's nothing to say, or when the
// repo has ≤1 worktree and no coordination log (hookPlan).
func hookAgentContext(r io.Reader, codex bool) int {
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
	paths, err := gitx.WorktreePaths()
	if err != nil {
		return 0
	}
	run, overlaps := hookPlan(len(paths), coordLogExists)
	if !run {
		return 0
	}
	c, err := config.Load()
	if err != nil {
		return 0
	}

	var parts []string
	if overlaps {
		if msg, has := hookOverlapMessage(c); has {
			parts = append(parts, msg)
		}
	}
	// Coordination signals — un-acked holds + announcements from other windows,
	// and from another session sharing this checkout (#163). The session is the
	// one this agent's own `wt` commands stamp on their records: the hook's env
	// (coordCtx), with Codex's payload fallback (agentHookSession). Fail-open: a
	// coord read error just omits this block (never breaks the turn).
	if logPath, self := coordCtx(c); logPath != "" {
		self.Session = agentHookSession(self.Session, codex, parseHookSessionID(b))
		if recs, rerr := coord.Load(logPath); rerr == nil {
			box := hookInbox(coord.Inbox(recs, self), self, overlaps)
			if msg, has := coordContextMessage(box, c.MaxAge, time.Now()); has {
				parts = append(parts, msg)
			}
		}
	}
	if len(parts) > 0 {
		emitAgentContext(strings.Join(parts, "\n\n"))
	}
	return 0
}

// hookPlan decides what the per-turn hook does, staying cheap on the common case.
// A repo with ≤1 worktree has no other WINDOW to collide with, so the file-overlap
// scan is skipped — but it CAN have another SESSION working in its one checkout
// (#163: two agents started in the same primary checkout), whose announcements and
// holds must still reach this one. So a single-worktree repo runs only when it has
// a coordination log (hasCoordLog is consulted ONLY then: one git call + a stat),
// where it used to return before even looking. Pure given hasCoordLog.
func hookPlan(worktrees int, hasCoordLog func() bool) (run, overlaps bool) {
	if worktrees > 1 {
		return true, true
	}
	return hasCoordLog(), false
}

// hookOverlapMessage is the per-turn cross-window file-overlap block (the
// multi-worktree half of hookAgentContext). has=false on any scan error
// (fail-open) or when nothing collides.
func hookOverlapMessage(c *config.Config) (string, bool) {
	root, err := gitx.RepoRoot()
	if err != nil {
		return "", false
	}
	ws, err := collide.Scan(c)
	if err != nil {
		return "", false
	}
	// Graded the way `wt check` grades from THIS window (#182) — the banner used
	// to run its own all-windows grade and said HIGH where check said low.
	return codexContextMessage(agentContextOverlaps(c, ws, root), collide.LabelForWorktree(ws, root))
}

// coordLogExists reports whether this repo has a coordination log yet — the
// cheap gate that lets the per-turn hook stay near-free in a single-worktree repo
// while still delivering another same-checkout session's signals (#163). It
// resolves the SAME path coordCtx does (repoNameFrom).
func coordLogExists() bool {
	home, err := os.UserHomeDir()
	if err != nil || home == "" {
		return false
	}
	common, _ := gitx.CommonDir()
	root := ""
	if common == "" || filepath.Base(common) != ".git" {
		root, _ = gitx.RepoRoot() // unusual layout: the name falls back to the toplevel
	}
	_, err = os.Stat(coord.LogPath(home, repoNameFrom(common, root)))
	return err == nil
}

// agentHookSession is the session the per-turn hook reads the log as (#163): the
// one this agent's own `wt` commands stamp. That is the hook's inherited env
// (envSession, coordCtx's resolver) — except for Codex, which exports
// CODEX_SESSION_ID to every shell command but NOT to hook processes: a Codex hook
// runs with the Codex process's own environment snapshot. Its payload's
// session_id is filled from the same Session::session_id() that feeds
// CODEX_SESSION_ID, so for codex-context ONLY a token-less env falls back to it;
// without that, every Codex turn would show the session its own holds as another
// session's. Claude Code sets CLAUDE_CODE_SESSION_ID in hook processes itself, so
// its payload is never consulted: a Claude Code too old to set the variable stamps
// SessionNone from its shells, and taking the payload id would split one session
// into two parties. Pure.
func agentHookSession(envSession string, codex bool, payloadSessionID string) string {
	if !codex || envSession != coord.SessionNone {
		return envSession
	}
	if id := strings.TrimSpace(payloadSessionID); id != "" {
		return id
	}
	return envSession
}

// parseHookSessionID extracts the agent hook payload's "session_id" ("" when
// absent or unparseable). Pure.
func parseHookSessionID(b []byte) string {
	var p struct {
		SessionID string `json:"session_id"`
	}
	if json.Unmarshal(b, &p) != nil {
		return ""
	}
	return strings.TrimSpace(p.SessionID)
}

// hookInbox picks the coordination entries for the per-turn context. With other
// worktrees present it is the whole inbox, as before #163. In a single-worktree
// repo (where the hook used to stay silent altogether) it is everything from
// another session in this same checkout (#163) PLUS every un-acked HOLD from
// another window: the merge gate enforces those whether or not that window's
// worktree still exists (a removed worktree's hold, a WT_WINDOW-pinned session in
// this very checkout, a same-named clone sharing the log), so the hook never
// hides one. Plain announcements from other windows stay out, as before. Entries
// come back labelled (labelForContext). Pure.
func hookInbox(box []coord.Record, self coord.Self, otherWorktrees bool) []coord.Record {
	if !otherWorktrees {
		var keep []coord.Record
		for _, r := range box {
			if self.SharesCheckout(r) || len(r.Hold) > 0 {
				keep = append(keep, r)
			}
		}
		box = keep
	}
	return labelForContext(box, self)
}

// labelForContext readies inbox entries for the per-turn context:
// coordContextMessage shows each entry's Window as who posted it, and an entry
// from another session in THIS checkout carries the reader's own window id — so
// it is relabelled "same checkout, another session (…)" (#163). Returns copies.
// Pure.
func labelForContext(box []coord.Record, self coord.Self) []coord.Record {
	out := make([]coord.Record, len(box))
	for i, r := range box {
		r.Window = postedBy(r, self)
		out[i] = r
	}
	return out
}

// coordMaxEntryChars caps a single announcement's free-text in the per-turn hook
// (#150). Newest-first (#147) surfaces the verbose CURRENT announcements, which
// un-truncated ballooned the injection ~5x (9KB → 44KB) — charged to every
// window's context budget every turn. The first ~300 chars (roughly the first
// sentence) almost always decide whether the reader needs the rest; the full text
// stays in `wt inbox`.
const coordMaxEntryChars = 300

// coordMaxInjectBytes bounds the coordination injection (#150) so the per-turn
// cost is capped regardless of how many announcements or how verbose. It covers
// the entry lines PLUS the fixed header/footer framing (seeded into the running
// total below); the single trailing summary line (~110B) is the only unbudgeted
// part. Entries are added NEWEST-first and the budget drops the OLDEST notes —
// never the newest (that would recreate #147). Holds are always delivered
// (safety) and are not budget-gated.
const coordMaxInjectBytes = 8192

// truncateMessage caps an announcement's free-text to coordMaxEntryChars runes,
// appending a marker pointing at `wt inbox` for the full text (#150). Pure.
func truncateMessage(m string) string {
	m = strings.TrimSpace(m)
	r := []rune(m)
	if len(r) <= coordMaxEntryChars {
		return m
	}
	return strings.TrimSpace(string(r[:coordMaxEntryChars])) + "… (truncated — `wt inbox` for full)"
}

// coordContextMessage renders the un-acked coordination signals from OTHER
// windows for the per-turn context: active HOLDS (an op another window asked you
// to avoid until all-clear) + plain announcements. inbox is already self-excluded
// and un-acked (coord.Inbox), arriving OLDEST-first (log order).
//
// NEWEST-first delivery (#147): a fresh announcement must always be visible even
// when a window's backlog is deep. Holds come first (always delivered, never aged
// out — an un-cleared hold is a standing safety request); plain announcements are
// aged out of delivery when maxAge>0 (they remain in `wt inbox`).
//
// Bounded cost (#150): each entry's free-text is truncated (coordMaxEntryChars)
// and the whole injection is held under a byte budget (coordMaxInjectBytes),
// filled newest-first so the OLDEST notes drop, never the newest. Holds are always
// included; notes fill the remaining budget. The summary line names the count and
// the size so the cost is visible. Pure; has=false when empty.
func coordContextMessage(inbox []coord.Record, maxAge time.Duration, now time.Time) (msg string, has bool) {
	// reverse to newest-first (inbox is oldest-first log order)
	ordered := make([]coord.Record, len(inbox))
	for i, r := range inbox {
		ordered[len(inbox)-1-i] = r
	}
	render := func(r coord.Record) string {
		who := r.Window
		if who == "" {
			who = "another window"
		}
		suffix := ""
		if m := truncateMessage(r.Message); m != "" {
			suffix = " — " + m
		}
		if len(r.Hold) > 0 {
			return fmt.Sprintf("  ⚠ HOLD %s [%s]%s (wt ack %s)", who, strings.Join(r.Hold, ","), suffix, r.ID)
		}
		return fmt.Sprintf("  %s%s (wt ack %s)", who, suffix, r.ID)
	}
	var holds, notes []string
	for _, r := range ordered {
		if len(r.Hold) > 0 {
			holds = append(holds, render(r))
			continue
		}
		if maxAge > 0 && coord.Age(r, now) > maxAge {
			continue // stale plain announcement — drop from delivery (still in `wt inbox`)
		}
		notes = append(notes, render(r))
	}
	if len(holds) == 0 && len(notes) == 0 {
		return "", false
	}

	// header/footer are fixed framing; seed the running total with them so the byte
	// budget bounds the WHOLE injection, not just the entry lines (#150 review).
	const header = "wt coordination — un-acked signals from other windows (respect any HOLD before that op):"
	const footer = "See `wt inbox` for detail; `wt ack <id>` to acknowledge (`wt ack --all` clears the backlog). (Set WT_SKIP_COLLISION=1 to silence.)"

	// Assemble under the byte budget, newest-first. Holds are always included;
	// notes fill the remaining budget (the newest note is always kept even if it
	// alone would exceed — dropping the newest would recreate #147). Dropped =
	// oldest notes.
	body := append([]string{}, holds...)
	used := len(header) + len(footer) + 2 // + the two newlines joining header/body/footer
	for _, l := range holds {
		used += len(l) + 1
	}
	kept := 0
	for _, l := range notes {
		if kept > 0 && used+len(l)+1 > coordMaxInjectBytes {
			break
		}
		body = append(body, l)
		used += len(l) + 1
		kept++
	}
	// The summary ALWAYS names count + size so the per-turn cost is visible (#150
	// review), with the dropped-count clause when the budget dropped older notes.
	shownCount := len(holds) + kept
	if dropped := len(notes) - kept; dropped > 0 {
		body = append(body, fmt.Sprintf("  …and %d older not shown (%d shown, ~%.1f KB; `wt inbox` for all, `wt ack --all` to clear)",
			dropped, shownCount, float64(used)/1024))
	} else {
		body = append(body, fmt.Sprintf("  (%d shown, ~%.1f KB)", shownCount, float64(used)/1024))
	}
	msg = header + "\n" + strings.Join(body, "\n") + "\n" + footer
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
// -U0 exact-hunk grading; #117 review). added holds each run of added lines
// ('+') and where it goes, so a run that only INSERTS (no removed line next to
// it) is graded as the insertion git will see (#199).
type codexHunk struct {
	preImage []string
	removed  []int
	added    []codexAdd
}

// codexAdd is one run of consecutive added lines, inserted before preImage[at]
// (at == len(preImage): after the hunk's last line).
type codexAdd struct {
	at    int
	lines []string
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
	var adds []codexAdd
	adding := false // the open section is an Add File: its '+' lines are the file, not a hunk
	flushHunk := func() {
		// An update hunk with added lines but no pre-image is kept too: it has
		// nothing to locate it by, which patchRangesInFile must report, not skip
		// (#199).
		if cur != nil && (len(pre) > 0 || (len(adds) > 0 && !adding)) {
			cur.hunks = append(cur.hunks, codexHunk{preImage: pre, removed: rem, added: adds})
		}
		pre, rem, adds = nil, nil, nil
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
		adding = prefix == "*** Add File: "
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
			// added line — NOT in the current file, so not pre-image; recorded
			// with where it goes (a run continues while no pre-image line comes
			// between)
			if n := len(adds); n > 0 && adds[n-1].at == len(pre) {
				adds[n-1].lines = append(adds[n-1].lines, ln[1:])
			} else {
				adds = append(adds, codexAdd{at: len(pre), lines: []string{ln[1:]}})
			}
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

// repoRelativePatch rewrites each section's path (and move destination) from
// the form apply_patch uses, relative to Codex's cwd (or absolute), to a
// repo-relative one, via repoRelativePath. prefix is that cwd relative to the
// repo root (`git rev-parse --show-prefix`). Matching is exact (#181), so a
// Codex session started in pkg/ that patches "svc/README.md" must be checked as
// pkg/svc/README.md; the old suffix match only reached it by accident, and the
// pending-hunk read joined the wrong file onto root. A path outside the repo
// becomes "", which patchPaths drops. Pure for relative paths.
func repoRelativePatch(files []codexPatchFile, root, prefix string) []codexPatchFile {
	out := make([]codexPatchFile, len(files))
	for i, f := range files {
		f.path = repoRelativePath(root, prefix, f.path)
		if f.newPath != "" {
			f.newPath = repoRelativePath(root, prefix, f.newPath)
		}
		out[i] = f
	}
	return out
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

// patchRangesInFile locates each hunk's pre-image in content and returns the
// edits the patch makes there, the way git's -U0 diff will report them once it is
// applied (#199): each run of REMOVED lines is a change of those lines (the
// context around it anchors the hunk but isn't a change: ranging it would flag
// edits merely next to another window's, #117 review), and each run of added
// lines with no removed line beside it is an INSERTION (a Gap) between its two
// pre-image neighbours. An insertion meets another window's change of either
// neighbour in git, so skipping it (as this did before #199) let a patch that
// only inserts next to another window's edit read as disjoint. A pure insertion
// or deletion git may slide (lines that repeat the ones beside it, which git
// shifts to align) claims every position it can slide to (insertionClaim,
// deletionClaim), so the grade can't miss where git puts it.
//
// ok=false when a hunk can't be uniquely located (or has added lines and no
// pre-image to locate them by), or the patch edits nothing — the caller then
// falls back to a file-level advisory. The ranges are on-disk line numbers;
// moving them into base numbers is the caller's job (pendingPatchRanges →
// gitx.LinesToBase, the #108 lesson). Reuses locateRange. Pure.
func patchRangesInFile(f codexPatchFile, content string) ([]gitx.LineRange, bool) {
	file := fileLines(content)
	var ranges []gitx.LineRange
	for _, h := range f.hunks {
		if len(h.preImage) == 0 {
			if len(h.added) > 0 {
				return nil, false // an insertion with nothing to place it by
			}
			continue
		}
		r, ok := locateRange(content, strings.Join(h.preImage, "\n"))
		if !ok {
			return nil, false
		}
		addedAt := func(lo, hi int) bool { // an added run goes in at an offset in [lo, hi]
			return slices.ContainsFunc(h.added, func(a codexAdd) bool { return lo <= a.at && a.at <= hi })
		}
		for _, run := range contiguousRuns(h.removed) {
			start, end := r.Start+run[0], r.Start+run[1]
			if addedAt(run[0], run[1]+1) {
				ranges = append(ranges, gitx.LineRange{Start: start, End: end}) // a replacement
			} else {
				ranges = append(ranges, deletionClaim(file, start, end))
			}
		}
		for _, a := range h.added {
			if slices.Contains(h.removed, a.at-1) || slices.Contains(h.removed, a.at) {
				continue // replaces removed lines: their change covers it
			}
			ranges = append(ranges, insertionClaim(file, r.Start+a.at-1, a.lines))
		}
	}
	if len(ranges) == 0 {
		return nil, false
	}
	return ranges, true
}

// hookCodexEdit implements `wt _hook codex-edit` — a Codex PreToolUse hook on
// apply_patch. It grades the patch's target files with the SAME engine as
// `wt check`, re-graded against the patch's actual hunks in base line numbers
// (regradePending, the #108/#184 lesson), and emits additionalContext on a HIGH
// overlap — or, under WT_CODEX_HOOK_BLOCK=1, a `deny` for a CONFIRMED HIGH only.
// Always exits 0 (fail-open); disjoint / no-overlap / ≤1-worktree stay silent.
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
	prefix, _ := gitx.ShowPrefix()
	files := repoRelativePatch(parseCodexPatch(patch), root, prefix)
	paths := patchPaths(files)
	if len(paths) == 0 {
		return 0
	}
	byPath := map[string]codexPatchFile{}
	for _, f := range files {
		if f.path != "" {
			byPath[f.path] = f
		}
		if f.newPath != "" {
			byPath[f.newPath] = f // a move grades the destination too (#117 review)
		}
	}

	// #181: the patch's targets are real repo-relative paths, so they match
	// EXACTLY, as the pre-push guard does; never by suffix or basename.
	entries := buildCheckReport(c, ws, root, collide.ExactQueries(paths), false)
	// Re-grade each path's entries against the patch's ACTUAL hunks the way `wt
	// check` will grade the file once the patch is applied (regradePending, the
	// same rule as the Claude hook): this window's own ranges plus the patch's,
	// moved into base line numbers through this worktree's own diff (#108/#184).
	byEntryPath := map[string][]CheckEntry{}
	var order []string
	for _, e := range entries {
		if _, seen := byEntryPath[e.Path]; !seen {
			order = append(order, e.Path)
		}
		byEntryPath[e.Path] = append(byEntryPath[e.Path], e)
	}
	var high []codexGradedEntry
	anyConfirmed := false
	for _, path := range order {
		cur, curOK := ownRanges(root, c.Base, path)
		pending, pendingOK := pendingPatchRanges(byPath, path, root, c.Base)
		graded := regradePending(byEntryPath[path], cur, curOK, pending, pendingOK, func(e CheckEntry) bool {
			return subsumedByBase(e.otherWorktree, c.Base, path) // that window's, not a namesake's (#193)
		})
		for _, g := range graded {
			high = append(high, codexGradedEntry{entry: g.entry, confirmed: g.confirmed})
			anyConfirmed = anyConfirmed || g.confirmed
		}
	}
	if out, has := codexEditDecision(high, os.Getenv("WT_CODEX_HOOK_BLOCK") == "1" && anyConfirmed); has {
		fmt.Println(out)
	}
	return 0
}

// codexGradedEntry pairs a blocking CheckEntry with whether its overlap was
// hunk-CONFIRMED in base line numbers (vs a file-level heads-up), so a multi-file patch
// can word each line accurately (#117 review).
type codexGradedEntry struct {
	entry     CheckEntry
	confirmed bool
}

// pendingPatchRanges returns the patch's edited ranges for relPath in BASE line
// numbers: located in the on-disk file, then moved through this worktree's own
// diff against base (gitx.LinesToBase), so they share e.OtherRanges' frame even
// when this worktree already differs from base, by its own edits or by base's it
// is behind on (#108/#184). ok=false (unlocatable hunks, an add/delete, a git
// error) → the caller keeps the entry as a file-level advisory rather than risk a
// wrong grade.
func pendingPatchRanges(byPath map[string]codexPatchFile, relPath, root, base string) ([]gitx.LineRange, bool) {
	f, ok := byPath[relPath]
	if !ok || len(f.hunks) == 0 {
		return nil, false
	}
	data, err := os.ReadFile(filepath.Join(root, relPath))
	if err != nil {
		return nil, false
	}
	onDisk, ok := patchRangesInFile(f, string(data))
	if !ok {
		return nil, false
	}
	return gitx.LinesToBase(root, base, relPath, onDisk)
}

// codexEditDecision shapes the PreToolUse stdout JSON. deny=true → permissionDecision
// "deny" (only ever passed when the batch has ≥1 CONFIRMED HIGH); else
// additionalContext. Each file is tagged per-entry — "overlapping hunks" (computed
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
		header = "wt collision: with this apply_patch, your version OVERLAPS hunks another live window is editing (`wt check` will grade it HIGH):"
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
