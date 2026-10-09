package hooks

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/eharriett0/wt/internal/collide"
	"github.com/eharriett0/wt/internal/config"
	"github.com/eharriett0/wt/internal/gitx"
)

// #193: two windows can share a label (two worktrees that claimed one issue are
// both "#N"; two detached worktrees whose directories share a name are both that
// name). gradeConflicts resolved a conflict's window by its label, so both
// namesakes were graded against whichever came last, and a real overlap read as
// the other one's disjoint hunks: the push went through.

func TestGradeConflicts_NamesakesGradeAgainstTheirOwnWorktree(t *testing.T) {
	ranges := func(dir, base, file string) []gitx.LineRange {
		switch dir {
		case testRoot, "/p1/x":
			return []gitx.LineRange{rng(1, 1)}
		case "/p2/x":
			return []gitx.LineRange{rng(30, 30)}
		}
		return nil
	}
	for _, order := range [][2]string{{"/p1/x", "/p2/x"}, {"/p2/x", "/p1/x"}} {
		ws := []collide.Window{{Branch: "self", Worktree: testRoot, Touched: []string{"big.txt"}}}
		for _, wt := range order {
			ws = append(ws, collide.Window{Branch: "HEAD", Worktree: wt, Touched: []string{"big.txt"}})
		}
		conflicts := pathConflicts(ws, testRoot, []string{"big.txt"})
		hard, soft := gradeConflicts(&config.Config{Base: "main"}, conflicts, testRoot, ws, ranges, ranges)
		if len(hard) != 1 || hard[0].Worktree != "/p1/x" || len(soft) != 1 || soft[0].Worktree != "/p2/x" {
			t.Errorf("namesakes listed %v: hard %+v soft %+v, want p1/x hard and p2/x soft", order, hard, soft)
		}
	}
}

// The #193 repro through the pre-push decision, against real worktrees: p1 edits
// big.txt line 1, its namesake p2 line 30, and the pushing window committed line
// 1. The push must block on p1 whichever namesake git lists last.
func TestPushCollisionBlocks_NamesakesAreGradedApart(t *testing.T) {
	for _, p1First := range []bool{true, false} {
		t.Run(fmt.Sprintf("p1First=%v", p1First), func(t *testing.T) {
			hermeticGitH(t)
			bin := t.TempDir() // a fake gh: authenticated, every other call fails; no GitHub
			if err := os.WriteFile(filepath.Join(bin, "gh"), []byte("#!/bin/sh\n[ \"$1\" = auth ] && exit 0\nexit 1\n"), 0o755); err != nil {
				t.Fatal(err)
			}
			t.Setenv("PATH", bin+string(os.PathListSeparator)+os.Getenv("PATH"))
			t.Setenv("HOME", t.TempDir())
			t.Setenv("WT_SKIP_COLLISION", "")
			t.Setenv("HOOK_DISABLE_MULTIWINDOW_CHECK", "")
			t.Chdir(t.TempDir())

			root := t.TempDir()
			runGitH(t, root, "init", "-q", "-b", "main")
			runGitH(t, root, "config", "user.email", "t@t.test")
			runGitH(t, root, "config", "user.name", "t")
			var b strings.Builder
			for i := 1; i <= 40; i++ {
				fmt.Fprintf(&b, "big.txt l%02d\n", i)
			}
			writeFileH(t, root, "big.txt", b.String())
			runGitH(t, root, "add", "big.txt")
			runGitH(t, root, "commit", "-qm", "base")
			// git lists worktrees sorted by path, and the label lookup kept the last
			// one: p1 (the overlap) sorts first or last
			base := t.TempDir()
			p1, p2 := filepath.Join(base, "a", "x"), filepath.Join(base, "b", "x")
			if !p1First {
				p1, p2 = p2, p1
			}
			for _, wt := range []string{p1, p2} {
				if err := os.MkdirAll(filepath.Dir(wt), 0o755); err != nil {
					t.Fatal(err)
				}
				runGitH(t, root, "worktree", "add", "-q", "--detach", wt, "main")
			}
			cur := filepath.Join(base, "cur")
			runGitH(t, root, "worktree", "add", "-q", "-b", "cur", cur, "main")
			edit := func(dir string, n int) {
				line := fmt.Sprintf("big.txt l%02d\n", n)
				writeFileH(t, dir, "big.txt", strings.Replace(b.String(), line, "EDITED "+line, 1))
			}
			edit(p1, 1)
			edit(p2, 30)
			edit(cur, 1)
			runGitH(t, cur, "commit", "-qam", "cur edits line 1")

			t.Chdir(cur)
			c, err := config.Load()
			if err != nil {
				t.Fatal(err)
			}
			ws, err := collide.Scan(c)
			if err != nil {
				t.Fatal(err)
			}
			if !pushCollisionBlocks(c, ws, repoRootOrEmpty(), []string{"big.txt"}) {
				t.Error("pre-push let big.txt through: p1 edits the same line")
			}
		})
	}
}
