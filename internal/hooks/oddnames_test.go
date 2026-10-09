package hooks

import (
	"fmt"
	"io"
	"os"
	"path/filepath"
	"reflect"
	"runtime"
	"sort"
	"strings"
	"testing"

	"github.com/eharriett0/wt/internal/collide"
	"github.com/eharriett0/wt/internal/config"
	"github.com/eharriett0/wt/internal/gitx"
)

// #200 through the git hooks, against real worktrees. git printed `café.md` as
// `"caf\303\251.md"` in every path list it was read from, so the pre-push
// outgoing set, the pre-commit staged set and each window's touched set held
// quoted names, and the porcelain read (quotes stripped, escapes kept) did not
// even agree with the diff reads. A push colliding on a non-ASCII name went
// through.

var (
	// oddPushNames: cur commits an edit of line 5, other edits line 5 too.
	oddPushNames = []string{"café.md", "dír é/naïve.md", "a b.md", `q"uote.md`, "new\nline.md", " lead.md", "trail.md "}
	// oddDisjoint: cur edits line 5, other line 30. Advisory, so a block on
	// oddPushNames comes from ranges read through the odd name, not from
	// unmeasured (indeterminate) ones.
	oddDisjoint = []string{"dís joint.md", "dis\"joint\nb.md"}
	// oddSolo: only cur edits it.
	oddSolo = "sólo.md"
)

// oddHookRepo commits every odd name on main (40 lines each), then adds two
// worktrees: cur COMMITS an edit of line 5 of every one, and other edits line 5
// of oddPushNames and line 30 of oddDisjoint, uncommitted. The cwd is cur.
func oddHookRepo(t *testing.T) (cur, other string) {
	t.Helper()
	if runtime.GOOS == "windows" {
		t.Skip("quotes and newlines are not legal in Windows file names")
	}
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

	all := append(append(append([]string(nil), oddPushNames...), oddDisjoint...), oddSolo)
	line := func(i, n int) string { return fmt.Sprintf("f%d l%02d\n", i, n) }
	edit := func(dir string, i, n int) {
		writeOddH(t, dir, all[i], strings.Replace(readOddH(t, dir, all[i]), line(i, n), "EDITED "+line(i, n), 1))
	}
	root := t.TempDir()
	runGitH(t, root, "init", "-q", "-b", "main")
	runGitH(t, root, "config", "user.email", "t@t.test")
	runGitH(t, root, "config", "user.name", "t")
	for i, f := range all {
		var b strings.Builder
		for n := 1; n <= 40; n++ {
			b.WriteString(line(i, n))
		}
		writeOddH(t, root, f, b.String())
	}
	runGitH(t, root, "add", "-A")
	runGitH(t, root, "commit", "-qm", "base")
	base := t.TempDir()
	cur, other = filepath.Join(base, "cur"), filepath.Join(base, "other")
	runGitH(t, root, "worktree", "add", "-q", "-b", "cur", cur, "main")
	runGitH(t, root, "worktree", "add", "-q", "-b", "other", other, "main")
	for i, f := range all {
		edit(cur, i, 5)
		switch {
		case f == oddSolo:
		case i < len(oddPushNames):
			edit(other, i, 5)
		default:
			edit(other, i, 30)
		}
	}
	runGitH(t, cur, "commit", "-qam", "cur edits line 5 of every file")
	t.Chdir(cur)
	return cur, other
}

func writeOddH(t *testing.T, dir, rel, content string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(filepath.Join(dir, rel)), 0o755); err != nil {
		t.Fatal(err)
	}
	writeFileH(t, dir, rel, content)
}

func readOddH(t *testing.T, dir, rel string) string {
	t.Helper()
	b, err := os.ReadFile(filepath.Join(dir, rel))
	if err != nil {
		t.Fatal(err)
	}
	return string(b)
}

// The pre-push gate over cur's outgoing commits, then the pre-commit notice in
// other once it stages its edits: one fixture, both hooks.
func TestGitHooks_OddNamesCollide(t *testing.T) {
	cur, other := oddHookRepo(t)
	c, err := config.Load()
	if err != nil {
		t.Fatal(err)
	}
	sha := gitOutH(t, cur, "rev-parse", "HEAD")

	// the outgoing set names every file as it is
	want := append(append(append([]string(nil), oddPushNames...), oddDisjoint...), oddSolo)
	sort.Strings(want)
	got := outgoingPaths(cur, "main", sha)
	sort.Strings(got)
	if !reflect.DeepEqual(got, want) {
		t.Errorf("outgoing paths = %q,\nwant %q", got, want)
	}

	// graded the way pushCollisionBlocks grades them: every same-line edit is a
	// hard overlap, every disjoint one advisory, and the file only cur edits is
	// no conflict
	ws, err := collide.Scan(c)
	if err != nil {
		t.Fatal(err)
	}
	root := repoRootOrEmpty()
	conflicts := pathConflicts(ws, root, got)
	live := collide.ClassifyWindows(ws, c.Base, collide.ConflictWindowSet(conflicts), c.MaxAge)
	active, _ := collide.PartitionConflicts(conflicts, live)
	hard, soft := gradeConflicts(c, active, root, ws, gitx.ChangedRanges, gitx.ChangedRangesNew)
	for _, g := range []struct {
		name      string
		got, want []string
	}{{"hard", distinctPaths(hard), oddPushNames}, {"advisory", distinctPaths(soft), oddDisjoint}} {
		sort.Strings(g.got)
		w := append([]string(nil), g.want...)
		sort.Strings(w)
		if !reflect.DeepEqual(g.got, w) {
			t.Errorf("%s overlaps = %q,\nwant %q", g.name, g.got, w)
		}
	}
	// and the hook itself, fed git's pre-push protocol line
	in := "refs/heads/cur " + sha + " refs/heads/cur " + strings.Repeat("0", 40) + "\n"
	if code := HookPrePush(c, strings.NewReader(in)); code != 1 {
		t.Errorf("HookPrePush = %d, want 1 (blocked)", code)
	}

	// pre-commit reads the staged set: other stages its edits and is told
	// which files cur edited the same lines of, by name
	runGitH(t, other, "add", "-A")
	t.Chdir(other)
	if c, err = config.Load(); err != nil {
		t.Fatal(err)
	}
	r, w, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	stderr := os.Stderr
	os.Stderr = w
	code := HookPreCommit(c)
	os.Stderr = stderr
	w.Close()
	b, _ := io.ReadAll(r)
	out := string(b)
	if code != 0 {
		t.Errorf("HookPreCommit = %d, want 0 (it never blocks)", code)
	}
	if want := fmt.Sprintf("%d staged file(s) have an OVERLAPPING edit", len(oddPushNames)); !strings.Contains(out, want) {
		t.Errorf("pre-commit notice lacks %q:\n%s", want, out)
	}
	for _, f := range oddPushNames {
		if !strings.Contains(out, f) {
			t.Errorf("pre-commit notice does not name %q:\n%s", f, out)
		}
	}
}
