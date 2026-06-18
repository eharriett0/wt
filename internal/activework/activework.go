// Package activework manages the cross-window "active work" state file — the
// portable replacement for awesome-o's Claude-memory project_active_work.md.
//
// The file lives in $GIT_COMMON_DIR (shared across every worktree of a repo on
// one machine, not committed), so collision detection works across parallel
// windows with no Claude Code dependency. Each claim is one markdown section:
//
//	## #123 — claimed 2026-06-18T15:04:05Z
//	- Title: ...
//	- Branch: `feat-123-...`
//	- Worktree: `...`
//	- Draft PR: https://...
//	- Window: `...`
//	- Last seen: 2026-06-18T15:04:05Z
package activework

import (
	"fmt"
	"os"
	"strings"
	"time"
)

const header = `---
name: Active work across windows
description: What each window is actively working on. Updated by 'wt claim N' and 'wt release N'. Read at session start / surfaced at commit time.
type: project
---

# Active work

Each entry below is one claim — one window working on one issue. Stale entries
(last seen long ago with no commits to the branch) are dead; clear with
'wt release N'.
`

// Entry is a single claim.
type Entry struct {
	Issue    string
	Title    string
	Branch   string
	Worktree string
	PRURL    string
	Window   string
	When     time.Time
}

// AppendSection returns content with a new claim section appended. If content
// is empty/whitespace, the file header is created first.
func AppendSection(content string, e Entry) string {
	if strings.TrimSpace(content) == "" {
		content = header
	}
	ts := e.When.UTC().Format(time.RFC3339)

	var b strings.Builder
	b.WriteString(content)
	if !strings.HasSuffix(content, "\n") {
		b.WriteByte('\n')
	}
	fmt.Fprintf(&b, "\n## #%s — claimed %s\n", e.Issue, ts)
	fmt.Fprintf(&b, "- Title: %s\n", e.Title)
	fmt.Fprintf(&b, "- Branch: `%s`\n", e.Branch)
	fmt.Fprintf(&b, "- Worktree: `%s`\n", e.Worktree)
	if e.PRURL != "" {
		fmt.Fprintf(&b, "- Draft PR: %s\n", e.PRURL)
	}
	fmt.Fprintf(&b, "- Window: `%s`\n", e.Window)
	fmt.Fprintf(&b, "- Last seen: %s\n", ts)
	return b.String()
}

// RemoveSection removes the "## #<issue> — ..." section through the line
// before the next "## " (or EOF). Returns the new content and whether anything
// changed.
func RemoveSection(content, issue string) (string, bool) {
	want := "#" + issue
	lines := strings.Split(content, "\n")
	out := make([]string, 0, len(lines))
	skip := false
	for _, ln := range lines {
		if strings.HasPrefix(ln, "## ") {
			fields := strings.Fields(strings.TrimPrefix(ln, "## "))
			if len(fields) > 0 && fields[0] == want {
				skip = true
				continue
			}
			skip = false
		}
		if !skip {
			out = append(out, ln)
		}
	}
	res := strings.Join(out, "\n")
	return res, res != content
}

// OtherClaims returns the issue tokens (e.g. "#123") of claims that are NOT
// currentIssue. When currentIssue is empty every claim is "other" (matches the
// bash hook behavior on a non-feat branch).
func OtherClaims(content, currentIssue string) []string {
	want := "#" + currentIssue
	var others []string
	for _, ln := range strings.Split(content, "\n") {
		if !strings.HasPrefix(ln, "## ") {
			continue
		}
		fields := strings.Fields(strings.TrimPrefix(ln, "## "))
		if len(fields) > 0 && strings.HasPrefix(fields[0], "#") && fields[0] != want {
			others = append(others, fields[0])
		}
	}
	return others
}

// Parse extracts structured claim entries from the file content. Used by the
// collision layer to enrich worktrees with their issue/title/window.
func Parse(content string) []Entry {
	var entries []Entry
	var cur *Entry
	flush := func() {
		if cur != nil {
			entries = append(entries, *cur)
			cur = nil
		}
	}
	for _, ln := range strings.Split(content, "\n") {
		if strings.HasPrefix(ln, "## ") {
			flush()
			fields := strings.Fields(strings.TrimPrefix(ln, "## "))
			if len(fields) > 0 && strings.HasPrefix(fields[0], "#") {
				cur = &Entry{Issue: strings.TrimPrefix(fields[0], "#")}
			}
			continue
		}
		if cur == nil {
			continue
		}
		switch {
		case strings.HasPrefix(ln, "- Title: "):
			cur.Title = strings.TrimPrefix(ln, "- Title: ")
		case strings.HasPrefix(ln, "- Branch: "):
			cur.Branch = unbacktick(strings.TrimPrefix(ln, "- Branch: "))
		case strings.HasPrefix(ln, "- Worktree: "):
			cur.Worktree = unbacktick(strings.TrimPrefix(ln, "- Worktree: "))
		case strings.HasPrefix(ln, "- Draft PR: "):
			cur.PRURL = strings.TrimPrefix(ln, "- Draft PR: ")
		case strings.HasPrefix(ln, "- Window: "):
			cur.Window = unbacktick(strings.TrimPrefix(ln, "- Window: "))
		}
	}
	flush()
	return entries
}

func unbacktick(s string) string { return strings.Trim(strings.TrimSpace(s), "`") }

// Read returns the file content, or "" if the file doesn't exist.
func Read(path string) string {
	b, err := os.ReadFile(path)
	if err != nil {
		return ""
	}
	return string(b)
}

// Write writes content to path (0644).
func Write(path, content string) error {
	return os.WriteFile(path, []byte(content), 0o644)
}
