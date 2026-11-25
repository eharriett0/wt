package gitx

import (
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
)

// hermeticGit points every git the test runs, the fixture's and the code
// under test's, at an empty global config and no system config, so the
// runner's ~/.gitconfig (commit.gpgsign with a key it cannot use, hooksPath,
// init.defaultBranch, ...) never reaches a fixture (#167).
func hermeticGit(t *testing.T) {
	t.Helper()
	t.Setenv("GIT_CONFIG_GLOBAL", os.DevNull)
	t.Setenv("GIT_CONFIG_NOSYSTEM", "1")
}

// mustTouched is TouchedFiles for a worktree git reads: an error fails the test.
func mustTouched(t *testing.T, dir, base string) []string {
	t.Helper()
	files, err := TouchedFiles(dir, base)
	if err != nil {
		t.Fatalf("TouchedFiles(%s): %v", dir, err)
	}
	return files
}

func runGit(t *testing.T, dir string, args ...string) {
	t.Helper()
	hermeticGit(t)
	cmd := exec.Command("git", args...)
	cmd.Dir = dir
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("git %v: %v\n%s", args, err, out)
	}
}

func gitRepo(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	runGit(t, dir, "init", "-q", "-b", "main")
	runGit(t, dir, "config", "user.email", "t@t.test")
	runGit(t, dir, "config", "user.name", "t")
	runGit(t, dir, "commit", "--allow-empty", "-qm", "init")
	return dir
}

func touchedContains(ss []string, want string) bool {
	for _, s := range ss {
		if s == want {
			return true
		}
	}
	return false
}

// #27: a fully-untracked new directory must list its files individually, not
// collapse to "dir/" — otherwise a collision under it never matches.
func TestTouchedFiles_UntrackedNewDirListsFiles(t *testing.T) {
	dir := gitRepo(t)
	if err := os.MkdirAll(filepath.Join(dir, "newpkg"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "newpkg", "foo.go"), []byte("package newpkg\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	got := mustTouched(t, dir, "main")
	if !touchedContains(got, "newpkg/foo.go") {
		t.Errorf("expected full path newpkg/foo.go in touched set, got %v", got)
	}
	if touchedContains(got, "newpkg/") {
		t.Errorf("dir-collapsed newpkg/ should NOT appear with -uall, got %v", got)
	}
}

// #28: a staged rename must record BOTH the old and new paths so an edit to the
// pre-rename name in another window still overlaps.
func TestTouchedFiles_RenameRecordsBothPaths(t *testing.T) {
	dir := gitRepo(t)
	if err := os.WriteFile(filepath.Join(dir, "old.go"), []byte("package x\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	runGit(t, dir, "add", "old.go")
	runGit(t, dir, "commit", "-qm", "add old")
	runGit(t, dir, "mv", "old.go", "new.go") // staged rename

	got := mustTouched(t, dir, "main")
	if !touchedContains(got, "old.go") {
		t.Errorf("expected OLD path old.go in touched set, got %v", got)
	}
	if !touchedContains(got, "new.go") {
		t.Errorf("expected NEW path new.go in touched set, got %v", got)
	}
}

// #181 review: a COMMITTED move must list both paths too, in every name source
// the collision engine reads. With git's default rename detection, `git diff
// --name-only` printed only the new path, so a window that committed README.md →
// pkg/README.md never matched another window's edit of README.md. Fuzzy suffix
// matching hid that ("README.md" is a suffix of "pkg/README.md"); exact matching
// relies on these. The file is large enough for git to detect the rename, or the
// test would pass for the wrong reason (movedRepo asserts it).

// movedRepo commits a 40-line README.md on main, then moves it to pkg/README.md
// on branch feat; staged=true leaves the move staged instead of committed.
func movedRepo(t *testing.T, staged bool) string {
	t.Helper()
	dir := gitRepo(t)
	writeFile(t, dir, "README.md", strings.Repeat("readme line\n", 40))
	runGit(t, dir, "add", "README.md")
	runGit(t, dir, "commit", "-qm", "add readme")
	runGit(t, dir, "checkout", "-qb", "feat")
	if err := os.MkdirAll(filepath.Join(dir, "pkg"), 0o755); err != nil {
		t.Fatal(err)
	}
	runGit(t, dir, "mv", "README.md", "pkg/README.md")
	if !staged {
		runGit(t, dir, "commit", "-qm", "move readme")
	}
	// precondition: git itself sees a rename, so rename detection would hide the old path
	args := []string{"diff", "--name-status", "-M", "main"}
	if staged {
		args = append(args, "--cached")
	} else {
		args = append(args, "HEAD")
	}
	cmd := exec.Command("git", args...)
	cmd.Dir = dir
	out, err := cmd.Output()
	if err != nil || !strings.HasPrefix(string(out), "R") {
		t.Fatalf("precondition: git should detect the move as a rename, got %q (%v)", out, err)
	}
	return dir
}

func TestTouchedFiles_CommittedMoveRecordsBothPaths(t *testing.T) {
	dir := movedRepo(t, false)
	got := mustTouched(t, dir, "main")
	for _, want := range []string{"README.md", "pkg/README.md"} {
		if !touchedContains(got, want) {
			t.Errorf("committed move: %s missing from the touched set %v", want, got)
		}
	}
}

// The other direction: THIS window made the move. Pre-push reads the outgoing
// set (RangeChangedPaths), pre-commit the staged set (StagedFiles); both must
// list the old path, or another window's edit of it is never checked.
func TestRangeChangedPaths_MoveListsBothPaths(t *testing.T) {
	dir := movedRepo(t, false)
	got, err := RangeChangedPaths(dir, "main", "HEAD")
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{"README.md", "pkg/README.md"} {
		if !touchedContains(got, want) {
			t.Errorf("outgoing move: %s missing from %v", want, got)
		}
	}
}

func TestStagedFiles_MoveListsBothPaths(t *testing.T) {
	dir := movedRepo(t, true)
	t.Chdir(dir) // StagedFiles reads the invoking worktree's own index
	got, err := StagedFiles()
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{"README.md", "pkg/README.md"} {
		if !touchedContains(got, want) {
			t.Errorf("staged move: %s missing from %v", want, got)
		}
	}
}

// writeFile is a tiny helper for the range-scope tests below.
func writeFile(t *testing.T, dir, rel, content string) {
	t.Helper()
	if err := os.WriteFile(filepath.Join(dir, rel), []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
}

// The pre-push outgoing scope must be THREE-dot (diff since the merge-base), so a
// branch BEHIND base — you branched, base moved on, you did NOT rebase — is scoped
// to only what the branch itself added, never the files base advanced by. Two-dot
// (base..head) diffs the two trees directly and reports base's divergent files as
// outgoing (reversals), which is exactly the pre-push guard's false-HIGH block on
// files the pusher never touched.
func TestRangeChangedPaths_BehindBaseScopesToBranchContributionOnly(t *testing.T) {
	dir := gitRepo(t) // main @ empty init
	writeFile(t, dir, "base.txt", "base\n")
	runGit(t, dir, "add", "-A")
	runGit(t, dir, "commit", "-qm", "base") // <- the eventual merge-base

	runGit(t, dir, "checkout", "-q", "-b", "feature")
	writeFile(t, dir, "feature.txt", "feat\n")
	runGit(t, dir, "add", "-A")
	runGit(t, dir, "commit", "-qm", "feature")

	// main advances with an UNRELATED file; feature does NOT rebase → it is behind.
	runGit(t, dir, "checkout", "-q", "main")
	writeFile(t, dir, "main-advanced.txt", "adv\n")
	runGit(t, dir, "add", "-A")
	runGit(t, dir, "commit", "-qm", "advance")

	paths, err := RangeChangedPaths(dir, "main", "feature")
	if err != nil {
		t.Fatalf("RangeChangedPaths: %v", err)
	}
	if !touchedContains(paths, "feature.txt") {
		t.Errorf("branch's own file missing from outgoing scope: %v", paths)
	}
	if touchedContains(paths, "main-advanced.txt") {
		t.Errorf("two-dot regression: base's advanced file must NOT be attributed to the push: %v", paths)
	}
	if len(paths) != 1 {
		t.Errorf("outgoing scope should be exactly [feature.txt], got %v", paths)
	}
}

// The common case (branch AHEAD of base, base is the merge-base) is unchanged by
// three-dot: it still reports exactly the branch's commits.
func TestRangeChangedPaths_AheadOfBaseUnchanged(t *testing.T) {
	dir := gitRepo(t)
	writeFile(t, dir, "base.txt", "base\n")
	runGit(t, dir, "add", "-A")
	runGit(t, dir, "commit", "-qm", "base")

	runGit(t, dir, "checkout", "-q", "-b", "feature")
	writeFile(t, dir, "a.go", "package a\n")
	writeFile(t, dir, "b.go", "package b\n")
	runGit(t, dir, "add", "-A")
	runGit(t, dir, "commit", "-qm", "feature work")

	paths, err := RangeChangedPaths(dir, "main", "feature")
	if err != nil {
		t.Fatalf("RangeChangedPaths: %v", err)
	}
	if !touchedContains(paths, "a.go") || !touchedContains(paths, "b.go") {
		t.Errorf("ahead-of-base scope should list the branch's files, got %v", paths)
	}
	if len(paths) != 2 {
		t.Errorf("expected exactly [a.go b.go], got %v", paths)
	}
}

func rangeCovers(rs []LineRange, line int) bool {
	for _, r := range rs {
		if r.Start <= line && line <= r.End {
			return true
		}
	}
	return false
}

// ChangedRangesNew reports a change at its CURRENT-file line (new frame); the
// same change appears at a DIFFERENT (base-file) line via ChangedRanges. A late
// edit whose line shifted because of an earlier insert must not be confused
// across frames — the #123 section-grade bug used base-frame ranges against
// current-frame section spans.
func TestChangedRangesNew_NewFrameDiffersFromBaseFrame(t *testing.T) {
	dir := gitRepo(t)
	writeFile(t, dir, "doc.md", "line1\nline2\nline3\nTARGET\n")
	runGit(t, dir, "add", "-A")
	runGit(t, dir, "commit", "-qm", "base")
	// Insert 2 lines early (shifts TARGET from base line 4 → current line 6) and
	// change TARGET.
	writeFile(t, dir, "doc.md", "line1\nINS-A\nINS-B\nline2\nline3\nTARGET-EDIT\n")

	newF := ChangedRangesNew(dir, "main", "doc.md")
	baseF := ChangedRanges(dir, "main", "doc.md")

	if !rangeCovers(newF, 6) {
		t.Errorf("ChangedRangesNew should cover current line 6 (TARGET's new position): %v", newF)
	}
	if rangeCovers(newF, 4) {
		t.Errorf("ChangedRangesNew should NOT report base line 4 (unchanged 'line2' in the current file): %v", newF)
	}
	if !rangeCovers(baseF, 4) {
		t.Errorf("ChangedRanges should cover base line 4 (TARGET's base position): %v", baseF)
	}
}

// FileChangeSubsumed: a branch whose change to a file already landed on base (via
// a different branch's squash) contributes nothing new → subsumed; a branch that
// changes a line base didn't → not subsumed; an unresolvable base → known=false
// (fail-safe: stays a collision). #122.
func TestFileChangeSubsumed(t *testing.T) {
	dir := gitRepo(t)
	writeFile(t, dir, "f.txt", "a\nb\nc\nd\ne\n")
	runGit(t, dir, "add", "-A")
	runGit(t, dir, "commit", "-qm", "C0")
	c0, _ := Run("-C", dir, "rev-parse", "HEAD")

	// orphan forks at C0 and changes line 3 (its real work).
	runGit(t, dir, "checkout", "-q", "-b", "orphan")
	writeFile(t, dir, "f.txt", "a\nb\nC-ORPHAN\nd\ne\n")
	runGit(t, dir, "add", "-A")
	runGit(t, dir, "commit", "-qm", "orphan")

	// main gains orphan's line-3 change (landed elsewhere) AND changes line 5.
	runGit(t, dir, "checkout", "-q", "main")
	writeFile(t, dir, "f.txt", "a\nb\nC-ORPHAN\nd\nE-LANDED\n")
	runGit(t, dir, "add", "-A")
	runGit(t, dir, "commit", "-qm", "landed")

	// Evaluate from the orphan worktree.
	runGit(t, dir, "checkout", "-q", "orphan")
	if subsumed, known := FileChangeSubsumed(dir, "main", "f.txt"); !known || !subsumed {
		t.Errorf("orphan's landed change should be subsumed: subsumed=%v known=%v", subsumed, known)
	}

	// A branch that changes line 2 (base did NOT) is genuinely contested.
	runGit(t, dir, "checkout", "-q", "-b", "real", strings.TrimSpace(c0))
	writeFile(t, dir, "f.txt", "a\nB-REAL\nc\nd\ne\n")
	runGit(t, dir, "add", "-A")
	runGit(t, dir, "commit", "-qm", "real")
	if subsumed, known := FileChangeSubsumed(dir, "main", "f.txt"); !known || subsumed {
		t.Errorf("real branch adds a line-2 change → not subsumed: subsumed=%v known=%v", subsumed, known)
	}

	// Fail-safe: an unresolvable base ref → known=false (stays a collision).
	if subsumed, known := FileChangeSubsumed(dir, "no-such-base", "f.txt"); known || subsumed {
		t.Errorf("unresolvable base must be known=false: subsumed=%v known=%v", subsumed, known)
	}
	// Base lacks the file (branch adds a brand-new one) → known=false.
	writeFile(t, dir, "new-only.txt", "x\n")
	runGit(t, dir, "add", "-A")
	runGit(t, dir, "commit", "-qm", "add new")
	if subsumed, known := FileChangeSubsumed(dir, "main", "new-only.txt"); known || subsumed {
		t.Errorf("base lacking the file must be known=false: subsumed=%v known=%v", subsumed, known)
	}
}

// A worktree whose uncommitted changes git can't read is an error, never "no
// changes" (#253), measured on real git: a branch holding an object the repo
// lacks, an unreadable index, a directory that is gone or can't be entered.
// An index.lock (a `git add` running in that window) does not fail git
// status, so it is no error; nor is a bare repository, which has no work
// tree. A --separate-git-dir repository's git directory is ErrGitDirOnly.
func TestTouchedFiles_UnreadIsAnError(t *testing.T) {
	t.Setenv("GIT_DEFAULT_REF_FORMAT", "files") // the branch case writes a ref file
	branch := func(t *testing.T) string {
		dir := gitRepo(t)
		runGit(t, dir, "checkout", "-q", "-b", "feat")
		writeFile(t, dir, "committed.txt", "c\n")
		runGit(t, dir, "add", "committed.txt")
		runGit(t, dir, "commit", "-qm", "feat commits")
		writeFile(t, dir, "dirty.txt", "d\n") // uncommitted, untracked
		return dir
	}
	t.Run("healthy and with index.lock: read", func(t *testing.T) {
		dir := branch(t)
		for _, lock := range []bool{false, true} {
			if lock {
				writeFile(t, dir, ".git/index.lock", "")
			}
			files, err := TouchedFiles(dir, "main")
			if err != nil || !touchedContains(files, "dirty.txt") || !touchedContains(files, "committed.txt") {
				t.Errorf("index.lock=%v: %v, %v; want both files and no error", lock, files, err)
			}
		}
	})
	// unreadOf is err as TouchedFiles' *UnreadError, failing the test if it
	// is not one.
	unreadOf := func(t *testing.T, err error) *UnreadError {
		t.Helper()
		var ue *UnreadError
		if !errors.As(err, &ue) {
			t.Fatalf("err = %v (%T), want an *UnreadError", err, err)
		}
		return ue
	}
	t.Run("branch holds a missing object", func(t *testing.T) {
		dir := branch(t)
		writeFile(t, dir, ".git/refs/heads/feat", strings.Repeat("1", 40)+"\n")
		_, err := TouchedFiles(dir, "main")
		if err == nil || !strings.Contains(err.Error(), "git status: ") || !strings.Contains(err.Error(), "bad object HEAD") {
			t.Errorf("err = %v, want git status's own reason (bad object HEAD)", err)
		}
		if ue := unreadOf(t, err); ue.CommitsRead || ue.Partial {
			t.Errorf("%+v: want its commits unread too (HEAD is the missing object), not partial", ue)
		}
	})
	t.Run("main checkout moved without repair: git can't find the repository", func(t *testing.T) {
		base := t.TempDir()
		main, linked := filepath.Join(base, "main"), filepath.Join(base, "linked")
		runGit(t, base, "init", "-q", "-b", "main", main)
		runGit(t, main, "config", "user.email", "t@t.test")
		runGit(t, main, "config", "user.name", "t")
		runGit(t, main, "commit", "--allow-empty", "-qm", "init")
		runGit(t, main, "worktree", "add", "-q", "-b", "linked", linked, "main")
		writeFile(t, linked, "c.txt", "c\n")
		runGit(t, linked, "add", "c.txt")
		runGit(t, linked, "commit", "-qm", "linked commits")
		if err := os.Rename(main, filepath.Join(base, "moved")); err != nil {
			t.Fatal(err)
		}
		_, err := TouchedFiles(linked, "main")
		if ue := unreadOf(t, err); ue.CommitsRead || errors.Is(err, ErrNoDir) || !strings.Contains(err.Error(), "git status: ") {
			t.Errorf("err = %v %+v: want git's own reason, its commits unread too", err, ue)
		}
	})
	t.Run("git status exits 0 but names paths it could not read: partial", func(t *testing.T) {
		if os.Geteuid() == 0 {
			t.Skip("root reads mode-restricted directories")
		}
		dir := branch(t)
		if err := os.MkdirAll(filepath.Join(dir, "d"), 0o755); err != nil {
			t.Fatal(err)
		}
		writeFile(t, dir, "d/k.txt", "k\n")
		runGit(t, dir, "add", "d/k.txt")
		runGit(t, dir, "commit", "-qm", "d")
		writeFile(t, dir, "d/k.txt", "k2\n") // a tracked edit under d/
		d := filepath.Join(dir, "d")
		t.Cleanup(func() { _ = os.Chmod(d, 0o755) })
		// 000: d/k.txt's edit is left out of git status's output (measured; it
		// is still listed here, as the branch committed it). 300: d/k.txt is
		// read, but d/ can't be listed for untracked files.
		for _, c := range []struct {
			mode   os.FileMode
			stderr string
		}{{0, "d/k.txt: Permission denied"}, {0o300, "could not open directory 'd/'"}} {
			if err := os.Chmod(d, c.mode); err != nil {
				t.Fatal(err)
			}
			files, err := TouchedFiles(dir, "main")
			ue := unreadOf(t, err)
			if !ue.Partial || !ue.CommitsRead || !strings.Contains(err.Error(), c.stderr) {
				t.Errorf("mode %o: %v %+v, want partial with %q, its commits read", c.mode, err, ue, c.stderr)
			}
			if !touchedContains(files, "dirty.txt") || !touchedContains(files, "committed.txt") {
				t.Errorf("mode %o: files = %v, want what git read kept", c.mode, files)
			}
		}
		if err := os.Chmod(d, 0o755); err != nil {
			t.Fatal(err)
		}
		if files, err := TouchedFiles(dir, "main"); err != nil || !touchedContains(files, "d/k.txt") {
			t.Errorf("control, d/ readable: %v, %v; want d/k.txt and no error", files, err)
		}
	})
	t.Run("a start failure that is not the directory keeps its own reason", func(t *testing.T) {
		dir := branch(t)
		orig := gitOutputStderr
		t.Cleanup(func() { gitOutputStderr = orig })
		gitOutputStderr = func(dir string, args ...string) ([]byte, []byte, error) {
			return nil, nil, &os.SyscallError{Syscall: "pipe", Err: syscall.EMFILE}
		}
		if _, err := TouchedFiles(dir, "main"); errors.Is(err, ErrNoDir) || !strings.Contains(err.Error(), "git status: pipe: too many open files") {
			t.Errorf("err = %v, want its own reason, not ErrNoDir", err)
		}
	})
	t.Run("directory gone: its own error (Scan leaves out the prunable)", func(t *testing.T) {
		dir := branch(t)
		gone := filepath.Join(t.TempDir(), "gone")
		runGit(t, dir, "worktree", "add", "-q", "-b", "gone", gone, "main")
		if err := os.RemoveAll(gone); err != nil {
			t.Fatal(err)
		}
		if _, err := TouchedFiles(gone, "main"); !errors.Is(err, ErrNoDir) || !strings.Contains(err.Error(), "no such file or directory") || unreadOf(t, err).CommitsRead {
			t.Errorf("TouchedFiles(gone) err = %v, want its directory's own error", err)
		}
	})
	t.Run("directory can't be entered: its own error, not a failed exec", func(t *testing.T) {
		if os.Geteuid() == 0 {
			t.Skip("root enters a mode-000 directory")
		}
		// 000: it can't be opened. 600: it can be opened (read), not entered.
		for _, mode := range []os.FileMode{0, 0o600} {
			dir := branch(t)
			if err := os.Chmod(dir, mode); err != nil {
				t.Fatal(err)
			}
			t.Cleanup(func() { _ = os.Chmod(dir, 0o755) })
			if _, err := TouchedFiles(dir, "main"); !errors.Is(err, ErrNoDir) || !strings.Contains(err.Error(), "permission denied") || strings.Contains(err.Error(), "exec") || unreadOf(t, err).CommitsRead {
				t.Errorf("mode %o: err = %v, want ErrNoDir with the directory's own error (permission denied), not git's exec", mode, err)
			}
		}
	})
	t.Run("separate git dir: ErrGitDirOnly, its work tree read", func(t *testing.T) {
		base := t.TempDir()
		sep, work := filepath.Join(base, "sep.git"), filepath.Join(base, "main")
		runGit(t, base, "init", "-q", "-b", "main", "--separate-git-dir", sep, work)
		runGit(t, work, "config", "user.email", "t@t.test")
		runGit(t, work, "config", "user.name", "t")
		runGit(t, work, "commit", "--allow-empty", "-qm", "init")
		writeFile(t, work, "dirty.txt", "d\n")
		if _, err := TouchedFiles(sep, "main"); !errors.Is(err, ErrGitDirOnly) {
			t.Errorf("TouchedFiles(its git dir) err = %v, want ErrGitDirOnly", err)
		}
		if files, err := TouchedFiles(work, "main"); err != nil || !touchedContains(files, "dirty.txt") {
			t.Errorf("TouchedFiles(its work tree) = %v, %v; want dirty.txt and no error", files, err)
		}
	})
	t.Run("bare repository: ErrGitDirOnly (Scan leaves it out); a directory inside it is not", func(t *testing.T) {
		bare := filepath.Join(t.TempDir(), "r.git")
		runGit(t, t.TempDir(), "clone", "-q", "--bare", branch(t), bare)
		wa := filepath.Join(t.TempDir(), "wa")
		runGit(t, bare, "worktree", "add", "-q", wa, "feat")
		if out := gitOut(t, bare, "worktree", "list", "--porcelain"); !strings.Contains(out, "\nbare\n") {
			t.Fatalf("precondition: git lists the bare repository as a worktree:\n%s", out)
		}
		if _, err := TouchedFiles(bare, "main"); !errors.Is(err, ErrGitDirOnly) {
			t.Errorf("TouchedFiles(bare) err = %v, want ErrGitDirOnly", err)
		}
		if files, err := TouchedFiles(wa, "main"); err != nil || !touchedContains(files, "committed.txt") {
			t.Errorf("TouchedFiles(its worktree) = %v, %v; want its committed file and no error", files, err)
		}
		// A worktree nested in the bare repository that lost its .git file:
		// git status walks up into the bare repository and fails there, but
		// this directory is no git directory, and it holds an edit.
		nested := filepath.Join(bare, "nested")
		runGit(t, bare, "worktree", "add", "-q", "-b", "nested", nested, "feat")
		if err := os.Remove(filepath.Join(nested, ".git")); err != nil {
			t.Fatal(err)
		}
		writeFile(t, nested, "dirty.txt", "d\n")
		if _, err := TouchedFiles(nested, "main"); err == nil || errors.Is(err, ErrGitDirOnly) {
			t.Errorf("TouchedFiles(a directory inside a bare repository) err = %v, want git's own failure", err)
		}
	})
	t.Run("index unreadable: committed files still listed", func(t *testing.T) {
		if os.Geteuid() == 0 {
			t.Skip("root reads a mode-000 file")
		}
		dir := branch(t)
		index := filepath.Join(dir, ".git", "index")
		if err := os.Chmod(index, 0); err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { _ = os.Chmod(index, 0o644) })
		files, err := TouchedFiles(dir, "main")
		if err == nil || !strings.Contains(err.Error(), "index") {
			t.Errorf("err = %v, want git status's reason (the index)", err)
		}
		if !touchedContains(files, "committed.txt") {
			t.Errorf("files = %v, want the committed file still listed", files)
		}
		if ue := unreadOf(t, err); !ue.CommitsRead || ue.Partial {
			t.Errorf("%+v: want its commits read, not partial", ue)
		}
	})
}

// statusMissed keeps only what a successful git status says it could not
// read (#254 review): measured lines kept, unrelated warnings dropped.
func TestStatusMissed(t *testing.T) {
	dir := t.TempDir() // the window: d/, a file "a: b.txt", and nothing else
	if err := os.MkdirAll(filepath.Join(dir, "d"), 0o755); err != nil {
		t.Fatal(err)
	}
	writeFile(t, dir, "a: b.txt", "x\n")
	statusMissed := func(stderr string) string { return statusMissed(dir, stderr) }
	got := statusMissed("warning: in the working copy of 'a.txt', CRLF will be replaced by LF the next time Git touches it\n" +
		"warning: unable to access '/home/u/.config/git/ignore': Permission denied\n" +
		"d/k.txt: Permission denied\n" +
		"warning: could not open directory 'd/': Permission denied\n" +
		"hint: something\n\n")
	if want := "d/k.txt: Permission denied; warning: could not open directory 'd/': Permission denied"; got != want {
		t.Errorf("statusMissed = %q, want %q", got, want)
	}
	if got := statusMissed("warning: in the working copy of 'a.txt', CRLF will be replaced by LF\n"); got != "" {
		t.Errorf("a line-ending warning only: %q, want none", got)
	}
	// Measured in review: stderr a successful status carries that names no
	// missed path (an fsmonitor hook without watchman, GIT_TRACE, a hook).
	for _, noise := range []string{
		"open2: exec of watchman -j --no-pretty failed: No such file or directory at .git/hooks/fsmonitor-watchman line 78.",
		"15:04:05.123456 git.c:460               trace: built-in: git status --porcelain",
		"post-index-change: index rewritten",
		"note: Permission denied for nothing in particular",
		"cat: " + dir + "/.git/last-sync: No such file or directory", // a hook's own error (round 6)
		dir + "/d/k.txt: Permission denied",                          // absolute: not git's work tree form
		"../d/k.txt: Permission denied",
		".git/hooks/post-index-change: line 2: nosuchcmd: command not found", // a failing hook (round 7)
	} {
		if got := statusMissed(noise + "\n"); got != "" {
			t.Errorf("statusMissed(%q) = %q, want none", noise, got)
		}
	}
	// Inside a submodule git prints the path relative to it (round 7).
	if err := os.MkdirAll(filepath.Join(dir, "sub", "e"), 0o755); err != nil {
		t.Fatal(err)
	}
	writeFile(t, dir, ".gitmodules", "[submodule \"sub\"]\n\tpath = sub\n\turl = ../sub\n")
	if got := statusMissed("e/k.txt: Permission denied\n"); got != "e/k.txt: Permission denied" {
		t.Errorf("a path inside a submodule: %q, want the line", got)
	}
	if got := statusMissed("nowhere/k.txt: Permission denied\n"); got != "" {
		t.Errorf("a path in neither the window nor a submodule: %q, want none", got)
	}
	// musl words errors differently (round 6): the path decides, not the text.
	if got := statusMissed("d/k.txt: Symbolic link loop\n"); got != "d/k.txt: Symbolic link loop" {
		t.Errorf("musl's wording: %q, want the line", got)
	}
	// A path holding ": " is still one path; many lines are capped.
	if got := statusMissed("a: b.txt: Permission denied\n"); got != "a: b.txt: Permission denied" {
		t.Errorf("a path with \": \" in it: %q", got)
	}
	var many strings.Builder
	for i := 0; i < 300; i++ {
		fmt.Fprintf(&many, "d/f%d.txt: Permission denied\n", i)
	}
	if got := statusMissed(many.String()); !strings.HasSuffix(got, "; and 297 more") || strings.Count(got, "Permission denied") != 3 {
		t.Errorf("300 lines: %q, want 3 and a count", got)
	}
}

// A failed git call's reason is capped like a partial read's (#254 review):
// it rides on every hook and status line.
func TestExitText_capped(t *testing.T) {
	var stderr strings.Builder
	for i := 0; i < 50; i++ {
		fmt.Fprintf(&stderr, "error: d/f%d.txt: Permission denied\n", i)
	}
	stderr.WriteString("fatal: cannot read the index\n")
	got := exitText(&exec.ExitError{Stderr: []byte(stderr.String())})
	if !strings.HasSuffix(got, "; and 48 more") || strings.Count(got, "Permission denied") != 3 {
		t.Errorf("exitText of 51 lines = %q, want 3 and a count", got)
	}
	// One line holding "; " (a path) is one line, never cut (round 6).
	one := "fatal: /r; a; b; c/.git/worktrees/w/index: index file open failed: Permission denied"
	if got := exitText(&exec.ExitError{Stderr: []byte(one + "\n")}); got != one {
		t.Errorf("exitText of one line with \"; \" = %q, want it whole", got)
	}
}
