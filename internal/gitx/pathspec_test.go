package gitx

import (
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"runtime"
	"strings"
	"testing"
)

// #204, against real git: a file's path reaches git after "--", where git reads
// it as a pathspec, a pattern. "a[1].md" also matches a1.md, "*.md" every .md
// file, "?x" ax, and a leading ':' is magic, so ":colon.md" asks about
// colon.md. Every call site passes literalPath(file); each test below gives the
// pattern reading a decoy to match, so a site that reads the path as a pattern
// answers about the wrong file.

// psNames are names pathspec syntax misreads, each with the decoy that reading
// matches. ("*.md" matches every .md name here, x.md among them.)
var psNames = []struct{ name, decoy string }{
	{"a[1].md", "a1.md"},
	{"*.md", "x.md"},
	{"?x", "ax"},
	{":colon.md", "colon.md"},
}

func nameTag(i int) string  { return fmt.Sprintf("n%d", i) }
func decoyTag(i int) string { return fmt.Sprintf("d%d", i) }

// fortyLines is a 40-line file whose every line carries tag, so no two files
// share a line (and git pairs nothing up as a rename).
func fortyLines(tag string) string {
	var b strings.Builder
	for n := 1; n <= 40; n++ {
		fmt.Fprintf(&b, "%s l%02d\n", tag, n)
	}
	return b.String()
}

func readFileT(t *testing.T, dir, rel string) string {
	t.Helper()
	b, err := os.ReadFile(filepath.Join(dir, rel))
	if err != nil {
		t.Fatal(err)
	}
	return string(b)
}

// editLineN rewrites line n of a fortyLines file.
func editLineN(t *testing.T, dir, rel, tag string, n int) {
	t.Helper()
	line := fmt.Sprintf("%s l%02d\n", tag, n)
	writeFile(t, dir, rel, strings.Replace(readFileT(t, dir, rel), line, "EDITED "+line, 1))
}

// insertTop puts three new lines above line 1, moving every line of the file.
func insertTop(t *testing.T, dir, rel string) {
	t.Helper()
	writeFile(t, dir, rel, "new 1\nnew 2\nnew 3\n"+readFileT(t, dir, rel))
}

// psRepo commits every name and its decoy on main, 40 lines each.
func psRepo(t *testing.T) string {
	t.Helper()
	if runtime.GOOS == "windows" {
		t.Skip("':', '*' and '?' are not legal in Windows file names")
	}
	dir := gitRepo(t)
	for i, p := range psNames {
		writeFile(t, dir, p.name, fortyLines(nameTag(i)))
		writeFile(t, dir, p.decoy, fortyLines(decoyTag(i)))
	}
	runGit(t, dir, "add", "-A")
	runGit(t, dir, "commit", "-qm", "names and decoys")
	return dir
}

var line5 = []LineRange{span(5, 5)}

// Up to date with base: ChangedRanges' and ChangedRangesNew's diff against base.
// Each name's own edit is line 5; every decoy's is line 30.
func TestChangedRanges_PathIsLiteral(t *testing.T) {
	dir := psRepo(t)
	runGit(t, dir, "checkout", "-qb", "feat")
	for i, p := range psNames {
		editLineN(t, dir, p.name, nameTag(i), 5)
		editLineN(t, dir, p.decoy, decoyTag(i), 30)
	}
	runGit(t, dir, "commit", "-qam", "edits")
	for _, p := range psNames {
		if got := ChangedRanges(dir, "main", p.name); !reflect.DeepEqual(got, line5) {
			t.Errorf("ChangedRanges(%q) = %v, want %v", p.name, got, line5)
		}
		if got := ChangedRangesNew(dir, "main", p.name); !reflect.DeepEqual(got, line5) {
			t.Errorf("ChangedRangesNew(%q) = %v, want %v", p.name, got, line5)
		}
	}
}

// Behind base: the branch's own diff from the merge base (it also edited every
// decoy, at line 30) and base's diff since it (base inserted three lines atop
// every decoy). Read as patterns, the own diff gains line 30 and base's
// insertions move line 5 to line 8.
func TestChangedRanges_BehindBranchPathIsLiteral(t *testing.T) {
	dir := psRepo(t)
	runGit(t, dir, "checkout", "-qb", "feat")
	for i, p := range psNames {
		editLineN(t, dir, p.name, nameTag(i), 5)
		editLineN(t, dir, p.decoy, decoyTag(i), 30)
	}
	runGit(t, dir, "commit", "-qam", "feat")
	runGit(t, dir, "checkout", "-q", "main")
	for _, p := range psNames {
		insertTop(t, dir, p.decoy)
	}
	runGit(t, dir, "commit", "-qam", "base moves every decoy")
	runGit(t, dir, "checkout", "-q", "feat")
	for _, p := range psNames {
		if got := ChangedRanges(dir, "main", p.name); !reflect.DeepEqual(got, line5) {
			t.Errorf("ChangedRanges(%q), behind = %v, want %v", p.name, got, line5)
		}
		if got := ChangedRangesNew(dir, "main", p.name); !reflect.DeepEqual(got, line5) {
			t.Errorf("ChangedRangesNew(%q), behind = %v, want %v", p.name, got, line5)
		}
	}
}

// LinesToBase maps a pending edit through this worktree's own diff of the file.
// The names are as base has them, so line 5 on disk is base line 5; only the
// decoys moved (three lines inserted atop each). Read as patterns, line 5 maps
// to line 2. Base-less, the mapping holds only while the file has no
// uncommitted change; the decoys' unstaged, then staged, changes must not count.
func TestLinesToBase_PathIsLiteral(t *testing.T) {
	dir := psRepo(t)
	for _, p := range psNames {
		insertTop(t, dir, p.decoy)
	}
	check := func(state string) {
		t.Helper()
		for _, p := range psNames {
			for _, base := range []string{"main", "no-such-base"} {
				if got, ok := LinesToBase(dir, base, p.name, line5); !ok || !reflect.DeepEqual(got, line5) {
					t.Errorf("LinesToBase(%q, base %s), decoys %s = %v, %v; want %v, true", p.name, base, state, got, ok, line5)
				}
			}
		}
	}
	check("unstaged")
	runGit(t, dir, "add", "-A")
	check("staged")
}

// Base-less, ChangedRanges reads the unstaged and the staged diff. Each name's
// line 5 is edited, unstaged; each decoy's line 30, unstaged, then staged.
func TestChangedRanges_BaselessPathIsLiteral(t *testing.T) {
	dir := psRepo(t)
	var decoys []string
	for i, p := range psNames {
		editLineN(t, dir, p.name, nameTag(i), 5)
		editLineN(t, dir, p.decoy, decoyTag(i), 30)
		decoys = append(decoys, p.decoy)
	}
	check := func(state string) {
		t.Helper()
		for _, p := range psNames {
			if got := ChangedRanges(dir, "no-such-base", p.name); !reflect.DeepEqual(got, line5) {
				t.Errorf("ChangedRanges(%q), no base, decoys %s = %v, want %v", p.name, state, got, line5)
			}
		}
	}
	check("unstaged")
	runGit(t, dir, append([]string{"add", "--"}, decoys...)...)
	check("staged")
}

// The #113 downgrade: a committed, unchanged file is not untracked, whatever
// untracked file its name reads as a pattern for.
func TestIsUntracked_PathIsLiteral(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("':', '*' and '?' are not legal in Windows file names")
	}
	dir := gitRepo(t)
	for i, p := range psNames {
		writeFile(t, dir, p.name, fortyLines(nameTag(i)))
	}
	runGit(t, dir, "add", "-A")
	runGit(t, dir, "commit", "-qm", "names")
	for i, p := range psNames {
		writeFile(t, dir, p.decoy, fortyLines(decoyTag(i))) // untracked
	}
	for _, p := range psNames {
		if IsUntracked(dir, p.name) {
			t.Errorf("IsUntracked(%q) = true: it is committed and unchanged; %s is the untracked one", p.name, p.decoy)
		}
	}
	writeFile(t, dir, "n[2].md", "new\n")
	if !IsUntracked(dir, "n[2].md") {
		t.Error("IsUntracked(n[2].md) = false for an untracked file")
	}
}

// IsTracked (cwd-relative) and IsTrackedIn (dir-relative): a tracked name is
// tracked; a name that is nothing, but whose pattern reading matches a tracked
// file, is not (`wt check` relies on it to refuse a typo, #93).
func TestIsTracked_PathIsLiteral(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("':', '*' and '?' are not legal in Windows file names")
	}
	dir := gitRepo(t)
	tracked := []string{"a[1].md", "*.md", "?x", ":colon.md"} // colon.md does not exist
	for _, f := range append(append([]string(nil), tracked...), "b1.md", "x.txt", "ay") {
		writeFile(t, dir, f, f+"\n")
	}
	runGit(t, dir, "add", "-A")
	runGit(t, dir, "commit", "-qm", "files")
	nothing := []string{"b[1].md", "*.txt", "?y", ":x.txt"} // read as patterns: b1.md, x.txt, ay, x.txt
	t.Chdir(dir)
	for _, fn := range []struct {
		name string
		f    func(string) bool
	}{
		{"IsTracked", IsTracked},
		{"IsTrackedIn", func(p string) bool { return IsTrackedIn(dir, p) }},
	} {
		for _, p := range tracked {
			if !fn.f(p) {
				t.Errorf("%s(%q) = false, want true", fn.name, p)
			}
		}
		for _, p := range nothing {
			if fn.f(p) {
				t.Errorf("%s(%q) = true for a path that names nothing", fn.name, p)
			}
		}
	}
}

// git exports GIT_LITERAL_PATHSPECS to the hooks of `git --literal-pathspecs
// push`. Under it git reads ":(literal)" as part of the file name, so the
// scoped env strips it.
func TestPathspecs_AmbientLiteralSwitch(t *testing.T) {
	dir := psRepo(t)
	for i, p := range psNames {
		editLineN(t, dir, p.name, nameTag(i), 5)
	}
	t.Setenv("GIT_LITERAL_PATHSPECS", "1")
	for _, p := range psNames {
		if !IsTrackedIn(dir, p.name) {
			t.Errorf("IsTrackedIn(%q) under GIT_LITERAL_PATHSPECS = false", p.name)
		}
		if got := ChangedRanges(dir, "main", p.name); !reflect.DeepEqual(got, line5) {
			t.Errorf("ChangedRanges(%q) under GIT_LITERAL_PATHSPECS = %v, want %v", p.name, got, line5)
		}
	}
}

// RefBlob's staged form names stage 0 outright: in the short ":path" form,
// "1:x.md" reads as stage 1 of x.md.
func TestRefBlob_StagedPathIsLiteral(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("':' is not legal in Windows file names")
	}
	dir := gitRepo(t)
	writeFile(t, dir, "1:x.md", "one\n")
	writeFile(t, dir, "x.md", "two\n")
	writeFile(t, dir, ":c.md", "three\n")
	runGit(t, dir, "add", "-A")
	for f, content := range map[string]string{"1:x.md": "one\n", ":c.md": "three\n", "x.md": "two\n"} {
		cmd := exec.Command("git", "hash-object", "--stdin")
		cmd.Stdin = strings.NewReader(content)
		want, err := cmd.Output()
		if err != nil {
			t.Fatal(err)
		}
		if got, ok := RefBlob(dir, "", f); !ok || got != strings.TrimSpace(string(want)) {
			t.Errorf("RefBlob(staged %q) = %q, %v; want %s", f, got, ok, want)
		}
	}
}
