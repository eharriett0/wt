package gitx

import (
	"os"
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
	sameFrom := func(base, dir, file, content string, want []LineRange) {
		t.Helper()
		before := objects()
		got, ok := ChangedRangesNewWith(dir, base, file, []byte(content))
		if after := objects(); after != before {
			t.Errorf("ChangedRangesNewWith wrote into the repo's objects:\nbefore %s\nafter %s", before, after)
		}
		if !ok || !reflect.DeepEqual(got, want) {
			t.Errorf("ChangedRangesNewWith(%s) = %v, %v; want %v, true", file, got, ok, want)
		}
		orig, had := "", true
		if b, err := os.ReadFile(filepath.Join(dir, file)); err == nil {
			orig = string(b)
		} else {
			had = false
		}
		writeFile(t, dir, file, content)
		defer func() {
			if had {
				writeFile(t, dir, file, orig)
			} else {
				os.Remove(filepath.Join(dir, file))
			}
		}()
		if wt := ChangedRangesNew(dir, base, file); !reflect.DeepEqual(wt, got) {
			t.Errorf("ChangedRangesNew(%s) with that content written = %v, ChangedRangesNewWith said %v", file, wt, got)
		}
	}
	same := func(dir, file, content string, want []LineRange) {
		t.Helper()
		sameFrom("main", dir, file, content, want)
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

	// ChangedRangesNew's fallbacks (#226 review: each read as "unmeasurable"
	// here, and the hook dropped an entry `wt check` then graded HIGH).
	// A file base doesn't have: a tracked one is all new, an untracked one is
	// no edits, as `git diff <base> -- file` reads them.
	same(dir, "new.txt", "n1\nn2\n", nil)
	writeFile(t, dir, "new.txt", "n1\n")
	runGit(t, dir, "add", "new.txt")
	same(dir, "new.txt", "n1\nn2\n", []LineRange{span(1, 2)})
	// A file base has that the index no longer tracks reads as deleted.
	runGit(t, dir, "rm", "-q", "--cached", "other.txt")
	same(dir, "other.txt", "x\ny\n", []LineRange{span(0, 1)}) // NEW frame: never a Gap
	runGit(t, dir, "add", "other.txt")
	// No base: the uncommitted ranges, index to the copy plus what is staged.
	sameFrom("no-such-base", dir, "new.txt", "n1\nEDIT\n", []LineRange{span(2, 2), span(1, 1)})
	// A clean filter applies to the copy, never to the empty side, git's
	// /dev/null (#226 re-review: one adding a header moved every range).
	runGit(t, dir, "config", "filter.hdr.clean", "sh -c 'echo HDR; cat'")
	writeFile(t, dir, ".gitattributes", "crlf.txt text eol=crlf\nhdr.txt filter=hdr\n")
	writeFile(t, dir, "hdr.txt", "a\n")
	runGit(t, dir, "add", ".gitattributes", "hdr.txt")
	same(dir, "hdr.txt", "a\nb\n", []LineRange{span(1, 3)})
	// A diff that fails: the whole file, never "no edits".
	failGit(t, func(args []string) bool { return len(args) > 0 && args[0] == "diff" })
	if got, ok := ChangedRangesNewWith(wt, "main", "other.txt", []byte("x\ny\n")); !ok || !reflect.DeepEqual(got, wholeFile) {
		t.Errorf("git diff failing: %v, %v; want the whole file, as ChangedRangesNew", got, ok)
	}
	// Only a copy that can't be put in as a blob is unmeasurable.
	failGit(t, func(args []string) bool { return len(args) > 0 && args[0] == "hash-object" })
	if _, ok := ChangedRangesNewWith(wt, "main", "other.txt", []byte("x\ny\n")); ok {
		t.Error("hash-object failing: want ok=false")
	}
}
