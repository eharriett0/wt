package cli

import (
	"path/filepath"
	"runtime"
	"strings"
	"testing"
)

// #204 end to end, against real worktrees: names git reads as patterns or
// magic when they follow "--". wa and wb both commit an edit of line 5 of every
// name, and wb also holds the UNTRACKED files those readings match (a1.md, bx,
// colon.md; "*.md" matches the .md ones). Read as patterns, wb's committed
// files looked untracked, and `wt check` downgraded every real collision on
// them to advisory (#113).
func TestCheckCommand_PathspecNames(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("':', '*' and '?' are not legal in Windows file names")
	}
	names := []string{"a[1].md", "*.md", "?x", ":colon.md"}
	hermeticGitT(t)
	offlineGH(t)
	t.Setenv("HOME", t.TempDir())
	t.Setenv("WT_SKIP_COLLISION", "")
	t.Setenv("HOOK_DISABLE_MULTIWINDOW_CHECK", "")
	t.Chdir(t.TempDir())

	root := t.TempDir()
	gitT(t, root, "init", "-q", "-b", "main")
	gitT(t, root, "config", "user.email", "t@t.test")
	gitT(t, root, "config", "user.name", "t")
	for i, f := range append(append([]string(nil), names...), "sub/y.md") {
		var b strings.Builder
		for n := 1; n <= 40; n++ {
			b.WriteString(oddLine(i, n))
		}
		writeNested(t, root, f, b.String())
	}
	gitT(t, root, "add", "-A")
	gitT(t, root, "commit", "-qm", "base")
	base := t.TempDir()
	wa, wb := filepath.Join(base, "wa"), filepath.Join(base, "wb")
	for _, w := range []string{wa, wb} {
		gitT(t, root, "branch", filepath.Base(w))
		gitT(t, root, "worktree", "add", "-q", w, filepath.Base(w))
		for i, f := range names {
			writeT(t, w, f, strings.Replace(readT(t, w, f), oddLine(i, 5), "EDITED "+oddLine(i, 5), 1))
		}
		gitT(t, w, "commit", "-qam", "edit line 5 of every name")
	}
	for _, f := range []string{"a1.md", "bx", "colon.md"} {
		writeT(t, wb, f, "untracked\n")
	}

	code, p := checkPayloadIn(t, wa, names...)
	if code != 3 || len(p.Entries) != len(names) {
		t.Errorf("wt check <every name> in wa = exit %d, entries %+v; want exit 3 and one per name", code, p.Entries)
	}
	got := map[string]CheckEntry{}
	for _, e := range p.Entries {
		got[e.Path] = e
	}
	for _, f := range names {
		if e, ok := got[f]; !ok || e.Window != "wb" || e.Severity != "HIGH" || e.Untracked || !coversLine(e.OverlapSpans, 5) {
			t.Errorf("wt check %q: entry %+v (listed %v), want wb HIGH overlapping line 5, not untracked", f, e, ok)
		}
	}

	// sub/[y].md names nothing; read as a pattern it was "tracked" (sub/y.md is),
	// and `wt check` reported it clear instead of refusing the typo (#93)
	t.Chdir(wa)
	if code := cmdCheck([]string{"sub/[y].md"}); code != 64 {
		t.Errorf("wt check sub/[y].md = exit %d, want 64 (no such path)", code)
	}
}
