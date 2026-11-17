package cli

import (
	"os"
	"runtime"
	"strings"
	"testing"
	"time"

	"github.com/eharriett0/wt/internal/config"
	"github.com/eharriett0/wt/internal/coord"
	"github.com/eharriett0/wt/internal/lock"
)

// #234 review: while another process holds the coordination log's lock past
// the wait (a prune stopped with Ctrl-Z), what an unlocked read would see may
// be a log stopped between its truncate and its rewrite, holds gone. The
// merge gate refuses instead of reading it, and passes once the lock is let go.
func TestMergeCoordGate_RefusesALogLockedByAnotherProcess(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("the advisory lock is a no-op off unix")
	}
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
	logPath, _ := coordCtx(c)
	if err := coord.Append(logPath, coord.Record{ID: "note", Kind: coord.KindAnnounce, Repo: "repo", Window: "another",
		Session: "s", Message: "no hold", TS: time.Now().UTC().Format(time.RFC3339)}); err != nil {
		t.Fatal(err)
	}
	holder, err := os.OpenFile(logPath, os.O_RDWR, 0)
	if err != nil {
		t.Fatal(err)
	}
	defer holder.Close()
	if err := lock.Exclusive(holder); err != nil {
		t.Fatal(err)
	}
	var code int
	_, errOut := captureOutputForTest(t, func() { code = mergeCoordGate(c) })
	if code != 1 || !strings.Contains(errOut, "stayed locked by another wt process") {
		t.Errorf("merge gate under a stuck holder = %d, want 1 with the locked-log refusal:\n%s", code, errOut)
	}
	lock.Release(holder)
	if code := mergeCoordGate(c); code != 0 {
		t.Errorf("merge gate once the holder let go = %d, want 0 (no hold)", code)
	}
}
