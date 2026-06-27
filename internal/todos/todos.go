// Package todos extends wt's file-level collision awareness to the task level:
// it mirrors each window's Claude Code TODO list to a small per-worktree JSON
// file so `wt todos` / `wt status` can show, across all windows, what every
// other window is currently working on.
//
// The store is written by a Claude Code PostToolUse hook on the TodoWrite tool
// (wired as `wt _hook todo-write`) and read by the CLI. The key is the worktree
// ROOT path, not the branch: it's stable across branch renames and is the
// reliable join key against collide.Window.Worktree.
package todos

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
)

// Todo is one task item, mirroring Claude Code's TodoWrite schema.
type Todo struct {
	Content    string `json:"content"`
	Status     string `json:"status"` // pending | in_progress | completed
	ActiveForm string `json:"activeForm"`
}

// Record is the persisted todo state for one worktree (one window).
type Record struct {
	Worktree string `json:"worktree"` // absolute worktree root
	Branch   string `json:"branch"`
	Updated  string `json:"updated"` // RFC3339 UTC
	Todos    []Todo `json:"todos"`
}

// Dir returns the store directory (~/.wt/todos), creating it if needed.
func Dir() (string, error) {
	home, err := os.UserHomeDir()
	if err != nil {
		return "", err
	}
	d := filepath.Join(home, ".wt", "todos")
	if err := os.MkdirAll(d, 0o755); err != nil {
		return "", err
	}
	return d, nil
}

// Key sanitizes a worktree path into a stable filename stem.
func Key(worktree string) string {
	abs := worktree
	if a, err := filepath.Abs(worktree); err == nil {
		abs = a
	}
	repl := func(r rune) rune {
		switch {
		case r >= 'a' && r <= 'z', r >= 'A' && r <= 'Z', r >= '0' && r <= '9', r == '-', r == '.':
			return r
		default:
			return '_'
		}
	}
	stem := strings.Map(repl, strings.TrimPrefix(abs, string(filepath.Separator)))
	if stem == "" {
		stem = "root"
	}
	return stem
}

// Write persists todos for a worktree. Best-effort by contract: the hook
// ignores the returned error so a write failure never disrupts the session.
func Write(worktree, branch, updated string, items []Todo) error {
	d, err := Dir()
	if err != nil {
		return err
	}
	rec := Record{Worktree: worktree, Branch: branch, Updated: updated, Todos: items}
	b, err := json.MarshalIndent(rec, "", "  ")
	if err != nil {
		return err
	}
	return os.WriteFile(filepath.Join(d, Key(worktree)+".json"), b, 0o644)
}

// ForWorktree loads the record for a worktree path; (nil, nil) if none exists.
func ForWorktree(worktree string) (*Record, error) {
	d, err := Dir()
	if err != nil {
		return nil, err
	}
	return load(filepath.Join(d, Key(worktree)+".json"))
}

// All loads every record currently in the store.
func All() ([]Record, error) {
	d, err := Dir()
	if err != nil {
		return nil, err
	}
	entries, err := os.ReadDir(d)
	if err != nil {
		return nil, err
	}
	var out []Record
	for _, e := range entries {
		if e.IsDir() || !strings.HasSuffix(e.Name(), ".json") {
			continue
		}
		if r, err := load(filepath.Join(d, e.Name())); err == nil && r != nil {
			out = append(out, *r)
		}
	}
	return out, nil
}

// Remove deletes the stored record for a worktree (used by `wt todos --prune`).
func Remove(worktree string) error {
	d, err := Dir()
	if err != nil {
		return err
	}
	if err := os.Remove(filepath.Join(d, Key(worktree)+".json")); err != nil && !os.IsNotExist(err) {
		return err
	}
	return nil
}

func load(path string) (*Record, error) {
	b, err := os.ReadFile(path)
	if err != nil {
		if os.IsNotExist(err) {
			return nil, nil
		}
		return nil, err
	}
	var r Record
	if err := json.Unmarshal(b, &r); err != nil {
		return nil, err
	}
	return &r, nil
}

// Counts tallies the record's todos by status.
func (r Record) Counts() (pending, inProgress, completed int) {
	for _, t := range r.Todos {
		switch t.Status {
		case "in_progress":
			inProgress++
		case "completed":
			completed++
		default:
			pending++
		}
	}
	return pending, inProgress, completed
}

// Active returns the first in-progress todo — the window's current focus.
func (r Record) Active() (Todo, bool) {
	for _, t := range r.Todos {
		if t.Status == "in_progress" {
			return t, true
		}
	}
	return Todo{}, false
}
