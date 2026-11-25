package gitx

import (
	"os"
	"path/filepath"
	"reflect"
	"slices"
	"strings"
	"testing"
	"time"
)

// #256: read at a commit from the main repository, a window's committed
// changes measure exactly as they do in its own directory (when it has no
// uncommitted edits): its files, its ranges and its structured-doc edits,
// ahead of base and behind it. A head git can't read is no measurement.
func TestChangedAt_matchesTheWorktreeReads(t *testing.T) {
	main := gitRepo(t)
	var x strings.Builder
	for i := 1; i <= 10; i++ {
		x.WriteString("x" + string(rune('0'+i%10)) + "\n")
	}
	const plan = "# Plan\n## Alpha\na1\na2\n## Beta\nb1\nb2\n"
	writeFile(t, main, "x.go", x.String())
	writeFile(t, main, "PLAN.md", plan)
	runGit(t, main, "add", "-A")
	runGit(t, main, "commit", "-qm", "base")
	lane := filepath.Join(t.TempDir(), "lane")
	runGit(t, main, "worktree", "add", "-q", "-b", "lane", lane, "main")
	writeFile(t, lane, "x.go", strings.Replace(x.String(), "x3\n", "LANE3\n", 1))
	writeFile(t, lane, "PLAN.md", strings.Replace(plan, "b1\n", "Lb1\n", 1))
	writeFile(t, lane, "new.txt", "n\n")
	runGit(t, lane, "add", "-A")
	runGit(t, lane, "commit", "-qm", "lane")
	head := gitOut(t, lane, "rev-parse", "HEAD")
	t.Chdir(main)

	check := func(t *testing.T) {
		t.Helper()
		at, err := CommittedFilesAt("main", head)
		in, terr := TouchedFiles(lane, "main")
		slices.Sort(at)
		slices.Sort(in)
		if err != nil || terr != nil || !reflect.DeepEqual(at, in) {
			t.Errorf("CommittedFilesAt = %v, %v; TouchedFiles in its directory = %v, %v", at, err, in, terr)
		}
		ra, ok := ChangedRangesAt("main", head, "x.go")
		if rd := ChangedRanges(lane, "main", "x.go"); !ok || !reflect.DeepEqual(ra, rd) {
			t.Errorf("ChangedRangesAt = %v (ok %v); ChangedRanges in its directory = %v", ra, ok, rd)
		}
		da, dd := ChangedDocEditsAt("main", head, "PLAN.md"), ChangedDocEdits(lane, "main", "PLAN.md")
		if !reflect.DeepEqual(da, dd) {
			t.Errorf("ChangedDocEditsAt = %+v; ChangedDocEdits in its directory = %+v", da, dd)
		}
	}
	t.Run("ahead of base", check)
	// Base moves on: a line far from the lane's and a renamed heading.
	writeFile(t, main, "x.go", strings.Replace(x.String(), "x9\n", "MAIN9\n", 1))
	writeFile(t, main, "PLAN.md", strings.Replace(plan, "## Alpha\n", "## Alpha two\n", 1))
	runGit(t, main, "commit", "-qam", "base moves on")
	t.Run("behind base", check)
	// From a subdirectory: a `-- <path>` pathspec is read from git's working
	// directory, so the at-commit reads must run at the top (#259 review).
	if err := os.MkdirAll(filepath.Join(main, "sub"), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Chdir(filepath.Join(main, "sub"))
	t.Run("from a subdirectory", check)
	t.Chdir(main)
	// Subsumed, read at the commit, answers as in its directory.
	sa, ka := FileChangeSubsumedAt("main", head, "x.go")
	if sd, kd := FileChangeSubsumed(lane, "main", "x.go"); sa != sd || ka != kd {
		t.Errorf("FileChangeSubsumedAt = %v, %v; in its directory %v, %v", sa, ka, sd, kd)
	}

	missing := strings.Repeat("1", 40)
	if files, err := CommittedFilesAt("main", missing); err == nil {
		t.Errorf("CommittedFilesAt(a missing object) = %v, want an error", files)
	}
	if r, ok := ChangedRangesAt("main", missing, "x.go"); ok {
		t.Errorf("ChangedRangesAt(a missing object) = %v, ok; want no measurement", r)
	}
	if d := ChangedDocEditsAt("main", missing, "PLAN.md"); !d.All {
		t.Errorf("ChangedDocEditsAt(a missing object) = %+v, want every section", d)
	}
	if age, err := CommitAge(head, time.Now()); err != nil || age < 0 || age > time.Hour {
		t.Errorf("CommitAge(head) = %v, %v; want a few seconds", age, err)
	}
	if _, err := os.Stat(lane); err != nil {
		t.Fatal(err)
	}
}
