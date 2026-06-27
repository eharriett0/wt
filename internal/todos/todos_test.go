package todos

import (
	"testing"
)

func TestKeyIsStableAndSanitized(t *testing.T) {
	k := Key("/Users/me/engineering/repo-worktrees/feat/x")
	if k == "" {
		t.Fatal("empty key")
	}
	for _, r := range k {
		ok := (r >= 'a' && r <= 'z') || (r >= 'A' && r <= 'Z') || (r >= '0' && r <= '9') || r == '-' || r == '.' || r == '_'
		if !ok {
			t.Fatalf("key has unsafe char %q in %q", r, k)
		}
	}
	if Key("/Users/me/repo") != Key("/Users/me/repo") {
		t.Fatal("key not deterministic")
	}
	if Key("/a/b") == Key("/a/c") {
		t.Fatal("distinct paths collided")
	}
}

func TestCountsAndActive(t *testing.T) {
	r := Record{Todos: []Todo{
		{Content: "a", Status: "completed"},
		{Content: "b", Status: "in_progress", ActiveForm: "Doing b"},
		{Content: "c", Status: "pending"},
		{Content: "d", Status: "pending"},
	}}
	p, ip, done := r.Counts()
	if p != 2 || ip != 1 || done != 1 {
		t.Fatalf("counts = %d/%d/%d, want 2/1/1", p, ip, done)
	}
	a, ok := r.Active()
	if !ok || a.ActiveForm != "Doing b" {
		t.Fatalf("active = %+v ok=%v, want 'Doing b'", a, ok)
	}
}

func TestActiveNoneWhenNoInProgress(t *testing.T) {
	r := Record{Todos: []Todo{{Status: "pending"}, {Status: "completed"}}}
	if _, ok := r.Active(); ok {
		t.Fatal("expected no active todo")
	}
}

func TestWriteReadRoundTrip(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	wt := "/Users/me/engineering/repo-worktrees/feat-x"
	items := []Todo{{Content: "task", Status: "in_progress", ActiveForm: "Tasking"}}
	if err := Write(wt, "feat-x", "2026-06-27T03:00:00Z", items); err != nil {
		t.Fatalf("write: %v", err)
	}
	got, err := ForWorktree(wt)
	if err != nil || got == nil {
		t.Fatalf("read: %v rec=%v", err, got)
	}
	if got.Branch != "feat-x" || len(got.Todos) != 1 || got.Todos[0].Content != "task" {
		t.Fatalf("round-trip mismatch: %+v", got)
	}
	all, err := All()
	if err != nil || len(all) != 1 {
		t.Fatalf("All() = %d records (%v), want 1", len(all), err)
	}
	if err := Remove(wt); err != nil {
		t.Fatalf("remove: %v", err)
	}
	if got, _ := ForWorktree(wt); got != nil {
		t.Fatal("record persisted after Remove")
	}
	// Remove of a missing record is a no-op, not an error.
	if err := Remove(wt); err != nil {
		t.Fatalf("remove(missing): %v", err)
	}
}
