package gitx

import (
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
)

// #210: an edit to a tracked file that git status never looks at
// (assume-unchanged, skip-worktree, core.ignoreStat) is uncommitted work, and
// StatusEntries lists it, so IsClean and every removal gate see it.

func TestParseHiddenEntries(t *testing.T) {
	out := "H 100644 aaa 0\tclean.txt\x00" +
		"h 100644 bbb 0\tassumed.txt\x00" +
		"S 100644 ccc 0\tskipped.txt\x00" +
		"s 120000 ddd 0\tboth\x00" +
		"h 100644 eee 2\tconflicted.txt\x00" + // stage 2: status lists it as unmerged
		"h 100644 fff 0\tname with\ttab and\nnewline\x00" +
		"garbage\x00" +
		"\x00"
	want := []hiddenEntry{
		{"assumed.txt", "100644", "bbb", "assume-unchanged"},
		{"skipped.txt", "100644", "ccc", "skip-worktree"},
		{"both", "120000", "ddd", "skip-worktree and assume-unchanged"},
		{"name with\ttab and\nnewline", "100644", "fff", "assume-unchanged"},
	}
	if got := parseHiddenEntries(out); !reflect.DeepEqual(got, want) {
		t.Errorf("parseHiddenEntries =\n%+v\nwant\n%+v", got, want)
	}
}

func TestBlobID(t *testing.T) {
	for _, c := range []struct {
		content string
		idLen   int
		want    string
	}{
		{"", 40, "e69de29bb2d1d6434b8b29ae775ad8c2e48c5391"}, // git's empty blob
		{"", 64, "473a0f4c3be8a93681a267e3b1e9a7dcda1185436fe141f7749120a303721813"},
	} {
		if got, ok := blobID([]byte(c.content), c.idLen); !ok || got != c.want {
			t.Errorf("blobID(%q, %d) = %q, %v; want %q", c.content, c.idLen, got, ok, c.want)
		}
	}
	if _, ok := blobID(nil, 12); ok {
		t.Error("blobID with a 12-digit id length succeeded")
	}
}

func TestCQuote(t *testing.T) {
	for in, want := range map[string]string{
		"a.txt":        `"a.txt"`,
		`say "hi".txt`: `"say \"hi\".txt"`,
		`back\slash`:   `"back\\slash"`,
		"new\nline\r":  `"new\012line\015"`,
		"café":         "\"café\"",
	} {
		if got := cQuote(in); got != want {
			t.Errorf("cQuote(%q) = %s, want %s", in, got, want)
		}
	}
}

// hiddenFixture is a repo whose main checkout tracks the given files, and a
// worktree of it, which it returns. With ignoreStat, core.ignoreStat=true is set
// before the worktree is checked out, so every file in it is flagged.
func hiddenFixture(t *testing.T, ignoreStat bool, files map[string]string) (repo, wt string) {
	t.Helper()
	repo = gitRepo(t)
	t.Chdir(repo)
	for name, content := range files {
		writeFile(t, repo, name, content)
	}
	runGit(t, repo, "add", "-A")
	runGit(t, repo, "commit", "-qm", "files")
	if ignoreStat {
		runGit(t, repo, "config", "core.ignoreStat", "true")
	}
	wt = filepath.Join(t.TempDir(), "wt")
	runGit(t, repo, "worktree", "add", "-q", "-b", "w", wt)
	return repo, wt
}

// hide flags file in the worktree at wt the way mech does; core.ignoreStat
// flagged it at checkout (hiddenFixture).
func hide(t *testing.T, wt, mech, file string) {
	t.Helper()
	switch mech {
	case "assume-unchanged", "skip-worktree":
		runGit(t, wt, "update-index", "--"+mech, "--", file)
	case "core.ignoreStat":
	default:
		t.Fatalf("unknown mechanism %q", mech)
	}
	if tag := gitOut(t, wt, "ls-files", "-v", "--", file); tag == "" || tag[0] == 'H' {
		t.Fatalf("fixture: %s is not flagged (%q)", file, tag)
	}
}

var hidingMechanisms = []string{"assume-unchanged", "skip-worktree", "core.ignoreStat"}

func TestStatusEntries_ListsEditsGitStatusHides(t *testing.T) {
	for _, mech := range hidingMechanisms {
		t.Run(mech, func(t *testing.T) {
			_, wt := hiddenFixture(t, mech == "core.ignoreStat", map[string]string{"conf.txt": "v1\n", "other.txt": "o\n"})
			hide(t, wt, mech, "conf.txt")
			if got, err := StatusEntries(wt); err != nil || len(got) != 0 || !IsClean(wt) {
				t.Fatalf("flagged but unchanged: StatusEntries = %q, %v; want clean", got, err)
			}
			writeFile(t, wt, "conf.txt", "v2: a local edit\n")
			if out := gitOut(t, wt, "status", "--porcelain"); out != "" {
				t.Fatalf("fixture: git status shows %q; the flag should hide the edit", out)
			}
			got, err := StatusEntries(wt)
			if err != nil || len(got) != 1 || !strings.HasPrefix(got[0], " M conf.txt  (") {
				t.Fatalf("StatusEntries = %q, %v; want the hidden edit of conf.txt", got, err)
			}
			if IsClean(wt) {
				t.Error("IsClean read a hidden edit as clean")
			}
			if err := os.Remove(filepath.Join(wt, "conf.txt")); err != nil {
				t.Fatal(err)
			}
			if got, err := StatusEntries(wt); err != nil || len(got) != 0 {
				t.Errorf("flagged file gone from disk: StatusEntries = %q, %v; want clean (nothing on disk to lose)", got, err)
			}
		})
	}
}

// The comparison is git's own (`hash-object --stdin-paths`, filters applied), so
// a file that differs from the index only by what a clean filter normalizes
// away is not an edit; a symlink is compared by its target; a file that became a
// directory, or can't be read, counts as changed (fail closed).
func TestStatusEntries_HiddenEditsCompareAsGitWould(t *testing.T) {
	repo, wt := hiddenFixture(t, false, map[string]string{"crlf.txt": "x\n", "unreadable.txt": "u\n", "dir-now.txt": "d\n"})
	runGit(t, repo, "config", "core.autocrlf", "true")
	if err := os.Symlink("crlf.txt", filepath.Join(repo, "link")); err != nil {
		t.Fatal(err)
	}
	runGit(t, repo, "add", "link")
	runGit(t, repo, "commit", "-qm", "link")
	runGit(t, wt, "merge", "-q", "--ff-only", "main")
	for _, f := range []string{"crlf.txt", "unreadable.txt", "dir-now.txt", "link"} {
		hide(t, wt, "assume-unchanged", f)
	}
	writeFile(t, wt, "crlf.txt", "x\r\n") // autocrlf normalizes it back to the index's blob
	if got, err := StatusEntries(wt); err != nil || len(got) != 0 {
		t.Fatalf("only line endings autocrlf normalizes away: StatusEntries = %q, %v; want clean", got, err)
	}
	link := filepath.Join(wt, "link")
	if err := os.Remove(link); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink("elsewhere.txt", link); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(filepath.Join(wt, "unreadable.txt"), 0); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.Chmod(filepath.Join(wt, "unreadable.txt"), 0o644) })
	if err := os.Remove(filepath.Join(wt, "dir-now.txt")); err != nil {
		t.Fatal(err)
	}
	if err := os.Mkdir(filepath.Join(wt, "dir-now.txt"), 0o755); err != nil {
		t.Fatal(err)
	}
	got, err := StatusEntries(wt)
	if err != nil {
		t.Fatal(err)
	}
	joined := strings.Join(got, "\n")
	for _, want := range []string{" M link  (", " M dir-now.txt  (", " ? unreadable.txt  ("} {
		if !strings.Contains(joined, want) {
			t.Errorf("StatusEntries =\n%s\nwant a line starting %q", joined, want)
		}
	}
	if len(got) != 3 {
		t.Errorf("StatusEntries = %q, want 3 entries", got)
	}
}

// A name git would C-quote on a line (a newline, a leading quote) is still
// hashed as that one file.
func TestStatusEntries_HiddenEditsWithOddNames(t *testing.T) {
	names := []string{"new\nline.txt", `"quoted.txt`, "café.txt"}
	files := map[string]string{}
	for _, n := range names {
		files[n] = "v1\n"
	}
	_, wt := hiddenFixture(t, false, files)
	for _, n := range names {
		hide(t, wt, "skip-worktree", n)
	}
	if got, err := StatusEntries(wt); err != nil || len(got) != 0 {
		t.Fatalf("flagged, unchanged: StatusEntries = %q, %v; want clean", got, err)
	}
	for _, n := range names {
		writeFile(t, wt, n, "v2\n")
	}
	got, err := StatusEntries(wt)
	if err != nil || len(got) != len(names) {
		t.Fatalf("StatusEntries = %q, %v; want the %d edits", got, err, len(names))
	}
}
