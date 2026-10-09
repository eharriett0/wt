package cli

import (
	"encoding/json"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"reflect"
	"sort"
	"strings"
	"testing"

	"github.com/eharriett0/wt/internal/collide"
	"github.com/eharriett0/wt/internal/gitx"
)

// #193: two windows can share a label. Two worktrees that claimed one issue are
// both "#N"; two detached worktrees whose directories share a name are both that
// name. `wt check` and the edit hooks resolved a conflict's window by its label,
// so both namesakes were graded against whichever came last: a real overlap read
// as the other's disjoint hunks (or the other's already-landed change).

func TestCheckEntries_NamesakesGradeAgainstTheirOwnWorktree(t *testing.T) {
	const file = "big.txt"
	for _, order := range [][2]string{{"/p1/x", "/p2/x"}, {"/p2/x", "/p1/x"}} {
		ws := []collide.Window{{Worktree: "/wt/cur", Branch: "cur", Touched: []string{file}}}
		for _, wt := range order {
			ws = append(ws, collide.Window{Worktree: wt, Branch: "HEAD", Touched: []string{file}})
		}
		ff := newFakeFacts()
		ff.ranges[factKey{"/wt/cur", file}] = []gitx.LineRange{rng(1, 1)}
		ff.ranges[factKey{"/p1/x", file}] = []gitx.LineRange{rng(1, 1)}   // overlaps cur
		ff.ranges[factKey{"/p2/x", file}] = []gitx.LineRange{rng(30, 30)} // disjoint
		live := map[string]collide.WindowLiveness{"x": {Level: collide.LiveDirty}}
		conflicts := collide.CheckPaths(ws, "/wt/cur", collide.ExactQueries([]string{file}))
		got := map[string]Category{}
		for _, e := range checkEntries(gradeTestConfig(), ff, ws, "/wt/cur", conflicts, live) {
			got[e.otherWorktree] = e.Category
		}
		if want := map[string]Category{"/p1/x": CatBlocking, "/p2/x": CatFYI}; !reflect.DeepEqual(got, want) {
			t.Errorf("namesakes listed %v graded %v, want %v", order, got, want)
		}
	}
}

// namesakeRepo builds main with big.txt (40 lines), a branch worktree cur, and
// two DETACHED worktrees p1 and p2, both labelled "x", at the commits p1At and
// p2At. git lists worktrees sorted by path and the label lookup kept the last
// one, so p1 sorts first or last as p1First says, and every test runs both.
func namesakeRepo(t *testing.T, p1First bool, p1At, p2At string, beforeWorktrees func(root string)) (cur, p1, p2 string) {
	t.Helper()
	hermeticGitT(t)
	offlineGH(t)
	t.Setenv("HOME", t.TempDir())
	t.Setenv("WT_SKIP_COLLISION", "")
	t.Setenv("HOOK_DISABLE_MULTIWINDOW_CHECK", "")
	t.Setenv("WT_CLAUDE_HOOK_BLOCK", "0")
	t.Setenv("WT_CODEX_HOOK_BLOCK", "0")
	t.Chdir(t.TempDir())

	root := t.TempDir()
	gitT(t, root, "init", "-q", "-b", "main")
	gitT(t, root, "config", "user.email", "t@t.test")
	gitT(t, root, "config", "user.name", "t")
	writeT(t, root, "big.txt", numbered("big.txt"))
	gitT(t, root, "add", "big.txt")
	gitT(t, root, "commit", "-qm", "base")
	if beforeWorktrees != nil {
		beforeWorktrees(root)
	}
	base := t.TempDir()
	p1, p2 = filepath.Join(base, "a", "x"), filepath.Join(base, "b", "x")
	if !p1First {
		p1, p2 = p2, p1
	}
	for _, wt := range []struct{ dir, at string }{{p1, p1At}, {p2, p2At}} {
		if err := os.MkdirAll(filepath.Dir(wt.dir), 0o755); err != nil {
			t.Fatal(err)
		}
		gitT(t, root, "worktree", "add", "-q", "--detach", wt.dir, wt.at)
	}
	cur = filepath.Join(base, "cur")
	gitT(t, root, "worktree", "add", "-q", "-b", "cur", cur, "main")
	return cur, p1, p2
}

func editLine(t *testing.T, dir, f string, n int) {
	t.Helper()
	line := fmt.Sprintf("%s l%02d\n", f, n)
	writeT(t, dir, f, strings.Replace(readT(t, dir, f), line, "EDITED "+line, 1))
}

// The #193 repro: p1 edits big.txt line 1, its namesake p2 line 30, and this
// window line 1. `wt check big.txt` must block on p1 and list p2 as disjoint,
// whichever namesake git lists last.
func TestCheckCommand_NamesakesAreGradedApart(t *testing.T) {
	for _, p1First := range []bool{true, false} {
		t.Run(fmt.Sprintf("p1First=%v", p1First), func(t *testing.T) {
			cur, p1, p2 := namesakeRepo(t, p1First, "main", "main", nil)
			editLine(t, p1, "big.txt", 1)
			editLine(t, p2, "big.txt", 30)
			editLine(t, cur, "big.txt", 1)

			t.Chdir(cur)
			r, w, err := os.Pipe()
			if err != nil {
				t.Fatal(err)
			}
			stdout := os.Stdout
			os.Stdout = w
			code := cmdCheck([]string{"--json", "big.txt"})
			os.Stdout = stdout
			w.Close()
			out, _ := io.ReadAll(r)
			var p CheckPayload
			if err := json.Unmarshal(out, &p); err != nil {
				t.Fatalf("not a check payload: %v\n%s", err, out)
			}
			var got []string
			for _, e := range p.Entries {
				got = append(got, e.Window+":"+string(e.Category))
			}
			sort.Strings(got)
			if want := []string{"x:" + string(CatBlocking), "x:" + string(CatFYI)}; code != 3 || !reflect.DeepEqual(got, want) {
				t.Errorf("wt check big.txt = exit %d %v, want exit 3 %v", code, got, want)
			}
		})
	}
}

// The pre-edit hooks re-check a newly-HIGH entry against #122 (the other
// window's change already on base). That check read the label's worktree too, so
// a namesake whose OWN change had landed could vouch for the window that really
// collides, and the hook dropped a real overlap. Here p2 holds the line-30 change
// main has since landed (subsumed); p1 edits line 1, disjoint from this window's
// line-10 edit until the agent edits line 1 as well.
func TestEditHooks_NamesakeDoesNotVouchForItsTwin(t *testing.T) {
	for _, p1First := range []bool{true, false} {
		t.Run(fmt.Sprintf("p1First=%v", p1First), func(t *testing.T) {
			land30 := func(root string) {
				editLine(t, root, "big.txt", 30)
				gitT(t, root, "commit", "-qam", "main lands line 30")
			}
			cur, p1, p2 := namesakeRepo(t, p1First, "main", "main~1", land30)
			editLine(t, p2, "big.txt", 30) // the same change main landed
			editLine(t, p1, "big.txt", 1)
			editLine(t, cur, "big.txt", 10)

			file := filepath.Join(cur, "big.txt")
			claude, _ := json.Marshal(map[string]any{"cwd": cur, "tool_name": "Edit", "tool_input": map[string]string{
				"file_path": file, "old_string": "big.txt l01\n", "new_string": "X big.txt l01\n"}})
			codex, _ := json.Marshal(map[string]any{"cwd": cur, "tool_name": "apply_patch", "tool_input": map[string]string{
				"command": "*** Begin Patch\n*** Update File: big.txt\n@@\n-big.txt l01\n+X big.txt l01\n*** End Patch\n"}})
			for _, h := range []struct {
				kind    string
				hook    func(io.Reader) int
				payload []byte
			}{{"claude", hookClaudeEdit, claude}, {"codex", hookCodexEdit, codex}} {
				if got := hookVerdict(t, runHookCapture(t, h.hook, string(h.payload))); got != "overlap" {
					t.Errorf("%s editing line 1 (p1 edits it too): hook says %s, want overlap", h.kind, got)
				}
			}
		})
	}
}
