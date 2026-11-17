package cli

import (
	"fmt"
	"reflect"
	"strings"
	"syscall"
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
	// #235 review: a lock that failed for another reason (EIO) has no holder
	// to hunt for: refused, with a retry, not the resume advice.
	loadForGate = func(string) ([]coord.Record, error) {
		return nil, fmt.Errorf("%w: input/output error", coord.ErrLogUnlockable)
	}
	_, errOut = captureOutputForTest(t, func() { code = mergeCoordGate(c) })
	if code != 1 || !strings.Contains(errOut, "could not lock the coordination log") || !strings.Contains(errOut, "Run the merge again") || strings.Contains(errOut, "kill -CONT") {
		t.Errorf("merge gate when the lock failed = %d, want 1 with a retry, no holder advice:\n%s", code, errOut)
	}
	// ENOLCK that keeps coming back is this machine's lock service down: say
	// so, since running the merge again won't help.
	loadForGate = func(string) ([]coord.Record, error) {
		return nil, fmt.Errorf("%w: %w", coord.ErrLogUnlockable, syscall.ENOLCK)
	}
	out, errOut := captureOutputForTest(t, func() { code = mergeCoordGate(c) })
	if code != 1 || !strings.Contains(out+errOut, "lock service (NFS lockd/statd) is down") {
		t.Errorf("merge gate on ENOLCK = %d, want 1 naming the lock service:\n%s%s", code, out, errOut)
	}
	loadForGate = func(string) ([]coord.Record, error) { return nil, nil }
	if code := mergeCoordGate(c); code != 0 {
		t.Errorf("merge gate once the log reads = %d, want 0 (no hold)", code)
	}
}
