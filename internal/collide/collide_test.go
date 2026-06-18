package collide

import (
	"reflect"
	"testing"
)

func TestLabel(t *testing.T) {
	if got := (Window{Issue: "42", Branch: "feat-42-x", Worktree: "/w/x"}).Label(); got != "#42" {
		t.Errorf("issue label = %q, want #42", got)
	}
	if got := (Window{Branch: "feat-42-x", Worktree: "/w/x"}).Label(); got != "feat-42-x" {
		t.Errorf("branch label = %q", got)
	}
	if got := (Window{Branch: "HEAD", Worktree: "/w/detached"}).Label(); got != "detached" {
		t.Errorf("detached label = %q, want basename", got)
	}
}

func TestOverlaps(t *testing.T) {
	ws := []Window{
		{Issue: "1", Branch: "feat-1", Worktree: "/w/1", Touched: []string{"a.go", "b.go"}},
		{Issue: "2", Branch: "feat-2", Worktree: "/w/2", Touched: []string{"b.go", "c.go"}},
		{Branch: "feat-3", Worktree: "/w/3", Touched: []string{"c.go", "d.go"}}, // unclaimed window
	}
	got := Overlaps(ws)
	want := []Overlap{
		{File: "b.go", Windows: []string{"#1", "#2"}},
		{File: "c.go", Windows: []string{"#2", "feat-3"}},
	}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("Overlaps =\n%#v\nwant\n%#v", got, want)
	}
}

func TestOverlaps_NoneWhenDisjoint(t *testing.T) {
	ws := []Window{
		{Issue: "1", Worktree: "/w/1", Touched: []string{"a.go"}},
		{Issue: "2", Worktree: "/w/2", Touched: []string{"b.go"}},
	}
	if got := Overlaps(ws); len(got) != 0 {
		t.Errorf("disjoint windows should have no overlap, got %v", got)
	}
}

func TestOverlaps_DedupWithinWindow(t *testing.T) {
	// A file listed twice within one window must not self-overlap.
	ws := []Window{{Issue: "1", Worktree: "/w/1", Touched: []string{"a.go", "a.go"}}}
	if got := Overlaps(ws); len(got) != 0 {
		t.Errorf("intra-window dup must not count as overlap, got %v", got)
	}
}

func TestCheckPaths(t *testing.T) {
	ws := []Window{
		{Issue: "1", Worktree: "/w/1", Touched: []string{"internal/foo.go", "main.go"}},
		{Issue: "2", Worktree: "/w/2", Touched: []string{"internal/bar.go"}},
	}
	// From window 2, about to edit internal/foo.go + README.md.
	got := CheckPaths(ws, "/w/2", []string{"internal/foo.go", "README.md"})
	want := []Conflict{{Path: "internal/foo.go", Window: "#1"}}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("CheckPaths =\n%#v\nwant\n%#v", got, want)
	}
}

func TestCheckPaths_BasenameMatch(t *testing.T) {
	ws := []Window{
		{Issue: "1", Worktree: "/w/1", Touched: []string{"internal/foo.go"}},
		{Issue: "2", Worktree: "/w/2", Touched: nil},
	}
	got := CheckPaths(ws, "/w/2", []string{"foo.go"})
	if len(got) != 1 || got[0].Window != "#1" {
		t.Errorf("basename should match a touched path, got %v", got)
	}
}

func TestCheckPaths_ExcludesOwnWorktree(t *testing.T) {
	ws := []Window{{Issue: "1", Worktree: "/w/1", Touched: []string{"a.go"}}}
	if got := CheckPaths(ws, "/w/1", []string{"a.go"}); len(got) != 0 {
		t.Errorf("own worktree must be excluded, got %v", got)
	}
}
