package cli

import (
	"reflect"
	"strings"
	"testing"

	"github.com/eharriett0/wt/internal/config"
	"github.com/eharriett0/wt/internal/coord"
)

// #234 review: while another process holds the coordination log's lock past
// the wait (a prune stopped with Ctrl-Z), what an unlocked read would see may
// be a log stopped between its truncate and its rewrite, holds gone. The
// merge gate refuses instead of reading it, and its advice never says to kill
// the holder (a prune killed mid-rewrite empties the log) or to --bypass
// (which skips more than this gate). coord pins the wait itself.
func TestMergeCoordGate_RefusesALogLockedByAnotherProcess(t *testing.T) {
	repo, home := t.TempDir(), t.TempDir()
	gitInitForTest(t, repo)
	t.Setenv("HOME", home)
	for _, k := range []string{"WT_SESSION", "CODEX_SESSION_ID", "CLAUDE_CODE_SESSION_ID", "WT_WINDOW", "WT_COORD_ISSUE"} {
		t.Setenv(k, "")
	}
	t.Chdir(repo)
	c, err := config.Load()
	if err != nil {
		t.Fatal(err)
	}
	orig := loadForGate
	t.Cleanup(func() { loadForGate = orig })
	// The seam must be the refusing read, never Load (which reads unlocked
	// after the wait): coord pins LoadForGate's behavior, this pins the wiring.
	if reflect.ValueOf(orig).Pointer() != reflect.ValueOf(coord.LoadForGate).Pointer() {
		t.Fatal("the merge gate does not read the log through coord.LoadForGate")
	}
	loadForGate = func(string) ([]coord.Record, error) { return nil, coord.ErrLogLocked }

	var code int
	_, errOut := captureOutputForTest(t, func() { code = mergeCoordGate(c) })
	if code != 1 || !strings.Contains(errOut, "stayed locked by another wt process") {
		t.Errorf("merge gate under a stuck holder = %d, want 1 with the locked-log refusal:\n%s", code, errOut)
	}
	if !strings.Contains(errOut, "kill -CONT") || strings.Contains(errOut, "--bypass") {
		t.Errorf("the refusal must say to resume the holder, and not point at --bypass:\n%s", errOut)
	}
	loadForGate = func(string) ([]coord.Record, error) { return nil, nil }
	if code := mergeCoordGate(c); code != 0 {
		t.Errorf("merge gate once the log reads = %d, want 0 (no hold)", code)
	}
}
