//go:build unix

package cli

import (
	"os"
	"path/filepath"
	"strings"
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
		!strings.Contains(errOut, "PLAN.md stayed locked by another process") || !strings.Contains(errOut, "kill -CONT") {
		t.Errorf("cmdAppend = %d, want 1 with the waiting line and the refusal:\n%s", code, errOut)
	}
	if got, _ := os.ReadFile(doc); string(got) != body {
		t.Errorf("the doc changed under another process's lock:\n%s", got)
	}
}
