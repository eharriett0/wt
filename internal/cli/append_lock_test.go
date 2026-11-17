//go:build unix

package cli

import (
	"os"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/eharriett0/wt/internal/lock"
)

// #235 review: `wt append` locked the doc with a wait that had no limit, so an
// append stopped with Ctrl-Z held every later one forever, silently. Now it
// says it is waiting, gives up at appendLockWait naming the doc, and writes
// nothing.
func TestAppend_givesUpOnAStuckHolder(t *testing.T) {
	repo := t.TempDir()
	gitInitForTest(t, repo)
	t.Chdir(repo)
	doc := filepath.Join(repo, "PLAN.md")
	const body = "# Plan\n\n## Alpha\na1\n"
	if err := os.WriteFile(doc, []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}
	ow, on := appendLockWait, appendLockNotice
	appendLockWait, appendLockNotice = 300*time.Millisecond, 50*time.Millisecond
	t.Cleanup(func() { appendLockWait, appendLockNotice = ow, on })
	holder, err := os.OpenFile(doc, os.O_RDWR, 0)
	if err != nil {
		t.Fatal(err)
	}
	defer holder.Close()
	if err := lock.Exclusive(holder); err != nil {
		t.Fatal(err)
	}
	defer lock.Release(holder)

	var code int
	_, errOut := captureOutputForTest(t, func() {
		code = cmdAppend([]string{"PLAN.md", "--section", "Alpha", "a2"})
	})
	if code != 1 || !strings.Contains(errOut, "waiting for the lock on PLAN.md") ||
		!strings.Contains(errOut, "PLAN.md stayed locked by another process") || !strings.Contains(errOut, "kill -CONT") ||
		!strings.Contains(errOut, "Don't kill it") {
		t.Errorf("cmdAppend = %d, want 1 with the waiting line and the refusal:\n%s", code, errOut)
	}
	if got, _ := os.ReadFile(doc); string(got) != body {
		t.Errorf("the doc changed under another process's lock:\n%s", got)
	}
}

// The documented bounds, and no unlocked fallback for a read-modify-write: on
// a filesystem that can't lock, `wt append` refuses (#235 review).
func TestAppend_lockDefaultsAndNoFallback(t *testing.T) {
	if appendLockWait != 30*time.Second || appendLockNotice != time.Second {
		t.Errorf("appendLockWait %s, appendLockNotice %s; the docs say 30s and 1s", appendLockWait, appendLockNotice)
	}
	repo := t.TempDir()
	gitInitForTest(t, repo)
	t.Chdir(repo)
	doc := filepath.Join(repo, "PLAN.md")
	const body = "# Plan\n\n## Alpha\na1\n"
	if err := os.WriteFile(doc, []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}
	orig := appendLock
	t.Cleanup(func() { appendLock = orig })
	appendLock = func(*os.File, time.Duration, time.Duration, func()) error { return syscall.ENOTSUP }
	var code int
	_, errOut := captureOutputForTest(t, func() {
		code = cmdAppend([]string{"PLAN.md", "--section", "Alpha", "a2"})
	})
	if code != 1 || !strings.Contains(errOut, "lock PLAN.md") {
		t.Errorf("cmdAppend = %d, want 1 when the doc can't be locked:\n%s", code, errOut)
	}
	if got, _ := os.ReadFile(doc); string(got) != body {
		t.Errorf("the doc was written without its lock:\n%s", got)
	}
}
