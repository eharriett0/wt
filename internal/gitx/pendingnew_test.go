package gitx

import (
	"path/filepath"
	"reflect"
	"strings"
	"testing"
)

// ChangedRangesNewWith is the edit hooks' section measurement (#226): the
// ranges ChangedRangesNew will report once the worktree's copy of the file
// holds content, in that copy's own line numbers, measured before it does. It
// must BE that measurement, so every case writes the content and asks
// ChangedRangesNew too: up to date, with uncommitted edits of its own (where
// the NEW frame differs from the base frame ChangedRangesWith reports), and
// behind base (measured from the merge base). It never writes an object into
// the repo.
func TestChangedRangesNewWith(t *testing.T) {
	dir := gitRepo(t)
	base := twentyLineFile(t, dir)
	writeFile(t, dir, "other.txt", "x\n")
	runGit(t, dir, "add", "-A")
	runGit(t, dir, "commit", "-qm", "base")
	runGit(t, dir, "branch", "behind")

	objects := func() string { return gitOut(t, dir, "count-objects", "-v") }
	same := func(dir, file, content string, want []LineRange) {
		t.Helper()
		before := objects()
		got, ok := ChangedRangesNewWith(dir, "main", file, []byte(content))
		if after := objects(); after != before {
			t.Errorf("ChangedRangesNewWith wrote into the repo's objects:\nbefore %s\nafter %s", before, after)
		}
		if !ok || !reflect.DeepEqual(got, want) {
			t.Errorf("ChangedRangesNewWith = %v, %v; want %v, true", got, ok, want)
		}
		orig := readFileT(t, dir, file)
		writeFile(t, dir, file, content)
		defer writeFile(t, dir, file, orig)
		if wt := ChangedRangesNew(dir, "main", file); !reflect.DeepEqual(wt, got) {
			t.Errorf("ChangedRangesNew with that content written = %v, ChangedRangesNewWith said %v", wt, got)
		}
	}
	same(dir, "data.txt", replaceLine("line05", "line05 EDITED")(base), []LineRange{span(5, 5)})
	same(dir, "data.txt", base, nil) // back to base: no edits
	// Two lines inserted at the top move line05 to line 7 in the copy: the NEW
	// frame, where ChangedRangesWith reports gapAt(0), span(5, 5).
	same(dir, "data.txt", "u1\nu2\n"+replaceLine("line05", "line05 EDITED")(base), []LineRange{span(1, 2), span(7, 7)})
	writeFile(t, dir, "data.txt", "u1\nu2\n"+base)
	same(dir, "data.txt", "u1\nu2\n"+replaceLine("line05", "line05 EDITED")(base), []LineRange{span(1, 2), span(7, 7)})
	writeFile(t, dir, "data.txt", base)

	// Base then gains a top line and rewrites line 10; `behind` has neither.
	// Measured from the merge base, so the copy's own line numbers, and base's
	// moves are not this window's edits.
	writeFile(t, dir, "data.txt", "top\n"+replaceLine("line10", "line10 changed-on-main")(base))
	runGit(t, dir, "commit", "-qam", "main moves data.txt after `behind` forked")
	wt := filepath.Join(t.TempDir(), "wt-behind")
	runGit(t, dir, "worktree", "add", "-q", wt, "behind")
	same(wt, "data.txt", replaceLine("line05", "line05 EDITED")(base), []LineRange{span(5, 5)})
	same(wt, "data.txt", strings.Replace(base, "line05\n", "line05\nINS\n", 1), []LineRange{span(6, 6)})
	same(wt, "data.txt", base, nil)

	// A file git stores LF and checks out CRLF: the pending copy goes through
	// the same conversion as the working tree's.
	writeFile(t, dir, ".gitattributes", "crlf.txt text eol=crlf\n")
	writeFile(t, dir, "crlf.txt", "one\ntwo\nthree\n")
	runGit(t, dir, "add", "-A")
	runGit(t, dir, "commit", "-qm", "crlf.txt")
	runGit(t, dir, "rm", "-q", "--cached", "crlf.txt")
	runGit(t, dir, "reset", "-q", "--hard")
	if on := readFileT(t, dir, "crlf.txt"); on != "one\r\ntwo\r\nthree\r\n" {
		t.Fatalf("precondition: crlf.txt checked out as %q", on)
	}
	same(dir, "crlf.txt", "one\r\nTWO\r\nthree\r\n", []LineRange{span(2, 2)})

	// Can't be measured that way → ok=false, never "no edits": a base-less
	// repo, a file base doesn't have (as ChangedRangesWith), a git error.
	if _, ok := ChangedRangesNewWith(dir, "no-such-base", "data.txt", []byte(base)); ok {
		t.Error("base-less: want ok=false")
	}
	if _, ok := ChangedRangesNewWith(dir, "main", "new.txt", []byte("n\n")); ok {
		t.Error("a file absent from base: want ok=false")
	}
	failGit(t, func(args []string) bool { return len(args) > 0 && args[0] == "diff" })
	if _, ok := ChangedRangesNewWith(wt, "main", "other.txt", []byte("x\ny\n")); ok {
		t.Error("git diff failing: want ok=false, never \"no edits\"")
	}
}
