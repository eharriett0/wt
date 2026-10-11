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

// #199: a window editing a line TOUCHING another window's edit (no unchanged line
// between) is one conflict region in git's merge, so the push must block, the
// same grade `wt check` gives (#92). The pre-push guard let it through. Real
// worktrees: wa holds an uncommitted edit, the pushing window cur committed its
// own; the verdicts are git merge-tree's on the two branches.
func TestPushCollisionBlocks_TouchingEdits(t *testing.T) {
	change := func(n int) func([]string) []string {
		return func(ls []string) []string { ls[n-1] = "changed " + ls[n-1]; return ls }
	}
	insertAfter := func(n int) func([]string) []string {
		return func(ls []string) []string { return append(ls[:n:n], append([]string{"inserted"}, ls[n:]...)...) }
	}
	for _, tc := range []struct {
		name    string
		wa, cur func([]string) []string
		blocks  bool
	}{
		{"wa edits L10, push edits L11", change(10), change(11), true},
		{"one unchanged line between", change(10), change(12), false},
		{"wa inserts after L10, push edits L11", insertAfter(10), change(11), true},
		{"wa inserts after L10, push inserts after L11", insertAfter(10), insertAfter(11), false},
	} {
		t.Run(tc.name, func(t *testing.T) {
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
			var lines []string
			for i := 1; i <= 20; i++ {
				lines = append(lines, fmt.Sprintf("l%02d", i))
			}
			writeFileH(t, root, "data.txt", strings.Join(lines, "\n")+"\n")
			runGitH(t, root, "add", "data.txt")
			runGitH(t, root, "commit", "-qm", "base")
			base := t.TempDir()
			wa, cur := filepath.Join(base, "wa"), filepath.Join(base, "cur")
			runGitH(t, root, "worktree", "add", "-q", "-b", "wa", wa, "main")
			runGitH(t, root, "worktree", "add", "-q", "-b", "cur", cur, "main")
			write := func(dir string, edit func([]string) []string) {
				writeFileH(t, dir, "data.txt", strings.Join(edit(append([]string(nil), lines...)), "\n")+"\n")
			}
			write(wa, tc.wa) // uncommitted
			write(cur, tc.cur)
			runGitH(t, cur, "commit", "-qam", "cur's edit")

			t.Chdir(cur)
			c, err := config.Load()
			if err != nil {
				t.Fatal(err)
			}
			ws, err := collide.Scan(c)
			if err != nil {
				t.Fatal(err)
			}
			if got := pushCollisionBlocks(c, ws, repoRootOrEmpty(), []string{"data.txt"}); got != tc.blocks {
				t.Errorf("pushCollisionBlocks = %v, want %v", got, tc.blocks)
			}
			conflicts := pathConflicts(ws, repoRootOrEmpty(), []string{"data.txt"})
			hard, _ := gradeConflicts(c, conflicts, repoRootOrEmpty(), ws, gitx.ChangedRanges, gitx.ChangedRangesNew)
			if got := len(hard) > 0; got != tc.blocks {
				t.Errorf("pre-commit's gradeConflicts: hard=%v, want %v", got, tc.blocks)
			}
		})
	}
}
