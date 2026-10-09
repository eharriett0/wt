package gitx

import (
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"runtime"
	"sort"
	"strings"
	"testing"
)

// #200, against real git: every path lister the collision engine reads must
// return a name exactly as it is on disk. Read line by line, git C-quoted any
// name holding a non-ASCII byte (core.quotePath, on by default), a quote, a
// backslash or a control character, so `café.md` became `"caf\303\251.md"`, the
// touched path every check then compared against; and a trimmed read dropped a
// leading or trailing space.

// oddNames are names a quoted, line-split or trimmed path list gets wrong.
var oddNames = []string{
	"café.md",        // non-ASCII: C-quoted under core.quotePath
	"dír é/naïve.md", // a non-ASCII directory with a space in it
	"a b.md",         // a space: porcelain status quotes it
	`q"uote.md`,      // a double quote
	`back\slash.md`,  // a backslash
	"tab\tname.md",   // a tab
	"new\nline.md",   // a newline: a line read splits it in two
	" lead.md",       // a trim drops the leading space
	"trail.md ",      // and the trailing one
}

// oddRepo commits every odd name on main, each holding its own name (distinct
// content, so git pairs a rename with the right source).
func oddRepo(t *testing.T) string {
	t.Helper()
	if runtime.GOOS == "windows" {
		t.Skip("quotes, tabs and newlines are not legal in Windows file names")
	}
	dir := gitRepo(t)
	for _, n := range oddNames {
		writeOdd(t, dir, n, n+"\n")
	}
	runGit(t, dir, "add", "-A")
	runGit(t, dir, "commit", "-qm", "odd names")
	return dir
}

func writeOdd(t *testing.T, dir, rel, content string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(filepath.Join(dir, rel)), 0o755); err != nil {
		t.Fatal(err)
	}
	writeFile(t, dir, rel, content)
}

func sortedNames(ss ...[]string) []string {
	var out []string
	for _, s := range ss {
		out = append(out, s...)
	}
	sort.Strings(out)
	return out
}

// The committed side: TouchedFiles' diff against base (what `wt check` and the
// edit hooks compare with) and RangeChangedPaths (the pre-push outgoing set).
func TestPathListers_CommittedNamesVerbatim(t *testing.T) {
	dir := oddRepo(t)
	runGit(t, dir, "checkout", "-qb", "feat")
	for _, n := range oddNames {
		writeOdd(t, dir, n, n+"\nfeat\n")
	}
	runGit(t, dir, "commit", "-qam", "feat edits every odd name")
	want := sortedNames(oddNames)

	if got := sortedNames(TouchedFiles(dir, "main")); !reflect.DeepEqual(got, want) {
		t.Errorf("TouchedFiles (committed) = %q,\nwant %q", got, want)
	}
	got, err := RangeChangedPaths(dir, "main", "HEAD")
	if err != nil {
		t.Fatal(err)
	}
	if got = sortedNames(got); !reflect.DeepEqual(got, want) {
		t.Errorf("RangeChangedPaths = %q,\nwant %q", got, want)
	}
}

// The uncommitted side: TouchedFiles' porcelain read, through every record
// shape: modified, untracked, a staged rename and an intent-to-add rename in the
// worktree (both "XY <new>\0<orig>\0" under -z), all of odd names.
func TestPathListers_UncommittedNamesVerbatim(t *testing.T) {
	dir := oddRepo(t)
	staged, ita := `q"uote.md`, "tab\tname.md"
	for _, n := range oddNames {
		if n != staged && n != ita {
			writeOdd(t, dir, n, n+"\nedited\n")
		}
	}
	runGit(t, dir, "mv", staged, "dír é/moved ü.md")
	if err := os.Rename(filepath.Join(dir, ita), filepath.Join(dir, "it ä.md")); err != nil {
		t.Fatal(err)
	}
	runGit(t, dir, "add", "-N", "it ä.md")
	untracked := []string{"untr é.md", "dír é/new\nfile.md"}
	for _, n := range untracked {
		writeOdd(t, dir, n, n+"\n")
	}
	// precondition: git reports the staged move as a rename record, so the
	// two-record shape is what the parser reads here
	cmd := exec.Command("git", "status", "--porcelain", "-z")
	cmd.Dir = dir
	if out, err := cmd.Output(); err != nil || !strings.Contains(string(out), "R  dír é/moved ü.md\x00"+staged+"\x00") {
		t.Fatalf("precondition: want a staged rename record, got %q (%v)", out, err)
	}

	want := sortedNames(oddNames, []string{"dír é/moved ü.md", "it ä.md"}, untracked)
	if got := sortedNames(TouchedFiles(dir, "main")); !reflect.DeepEqual(got, want) {
		t.Errorf("TouchedFiles (uncommitted) = %q,\nwant %q", got, want)
	}
}

// The pre-commit hook's read of the invoking worktree's own index.
func TestStagedFiles_NamesVerbatim(t *testing.T) {
	dir := oddRepo(t)
	for _, n := range oddNames {
		writeOdd(t, dir, n, n+"\nstaged\n")
	}
	runGit(t, dir, "add", "-A")
	t.Chdir(dir)
	got, err := StagedFiles()
	if err != nil {
		t.Fatal(err)
	}
	if got, want := sortedNames(got), sortedNames(oddNames); !reflect.DeepEqual(got, want) {
		t.Errorf("StagedFiles = %q,\nwant %q", got, want)
	}
}

// The #78 base-drift warning names the conflicting paths: as they are.
func TestMergeTreeConflicts_NamesVerbatim(t *testing.T) {
	dir := oddRepo(t)
	runGit(t, dir, "checkout", "-qb", "side")
	for _, n := range oddNames {
		writeOdd(t, dir, n, n+"\nside\n")
	}
	runGit(t, dir, "commit", "-qam", "side")
	runGit(t, dir, "checkout", "-q", "main")
	for _, n := range oddNames {
		writeOdd(t, dir, n, n+"\nmain\n")
	}
	runGit(t, dir, "commit", "-qam", "main")
	t.Chdir(dir)
	paths, conflicted, err := MergeTreeConflicts("main", "side")
	if err != nil {
		t.Skipf("git merge-tree --write-tree unavailable (git < 2.38): %v", err)
	}
	if got, want := sortedNames(paths), sortedNames(oddNames); !conflicted || !reflect.DeepEqual(got, want) {
		t.Errorf("MergeTreeConflicts = %q (conflicted=%v),\nwant %q", got, conflicted, want)
	}
}
