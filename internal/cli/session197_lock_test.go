//go:build unix

package cli

import (
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/eharriett0/wt/internal/coord"
	"github.com/eharriett0/wt/internal/lock"
)

// #197 review: in a LINKED worktree (wt's own layout) the repository's name
// is the main checkout's, not the worktree directory's; a reservation made
// there records the main checkout's name, and the readers must scope by that
// name too, or every ledger record drops out. And the advisory read never
// waits on a ledger another process holds (status --json stalled 5s each).
func TestOtherSessions_linkedWorktreeAndHeldLedger(t *testing.T) {
	repo := sessionTestRepo(t)
	lane := filepath.Join(t.TempDir(), "lane-dir")
	if out, err := exec.Command("git", "-C", repo, "worktree", "add", "-q", "-b", "lane", lane).CombinedOutput(); err != nil {
		t.Fatalf("worktree add: %v\n%s", err, out)
	}
	if filepath.Base(lane) == filepath.Base(repo) {
		t.Fatal("precondition: the lane's directory name must differ from the repository's")
	}
	t.Chdir(lane)
	if err := os.WriteFile(filepath.Join(lane, "log.md"), []byte("## NEWEST-1\nolder\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	t.Setenv("WT_SESSION", "session-A")
	var code int
	if out, errOut := captureT(t, func() { code = cmdBlockID([]string{"log.md"}) }); code != 0 {
		t.Fatalf("A reserves in the lane: code %d\n%s%s", code, out, errOut)
	}
	home, _ := os.UserHomeDir()
	ledgers, _ := filepath.Glob(filepath.Join(coord.BlocksDir(home), "*.jsonl"))
	if len(ledgers) != 1 {
		t.Fatalf("want one ledger, got %v", ledgers)
	}
	h, err := os.OpenFile(ledgers[0], os.O_RDWR, 0)
	if err != nil {
		t.Fatal(err)
	}
	defer h.Close()
	if err := lock.Exclusive(h); err != nil {
		t.Fatal(err)
	}
	defer lock.Release(h)

	t.Setenv("WT_SESSION", "session-B")
	start := time.Now()
	out, _ := captureT(t, func() { cmdStatus([]string{"--json"}) })
	if took := time.Since(start); took > 3*time.Second {
		t.Errorf("status --json took %s with the ledger held, want no wait", took)
	}
	var p StatusPayload
	if err := json.Unmarshal([]byte(out), &p); err != nil {
		t.Fatalf("status --json: %v\n%s", err, out)
	}
	if len(p.OtherSessions) != 1 || p.OtherSessions[0].Session != "session-A" {
		t.Errorf("status --json in the lane: other_sessions = %+v, want session-A", p.OtherSessions)
	}
	lock.Release(h)
	dout, _ := captureT(t, func() { cmdDoctor([]string{"--json"}) })
	if !strings.Contains(dout, `"other_sessions"`) || !strings.Contains(dout, "session-A") {
		t.Errorf("doctor --json in the lane doesn't list session-A:\n%s", dout)
	}
}
