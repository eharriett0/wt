package gitx

import (
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"runtime"
	"strings"
	"testing"
	"time"
)

// #177 review: what `git worktree remove` deletes or refuses that git status
// does not show, and the branch delete that checks the tip it deletes.

// NestedRepos finds a repository (or a linked worktree's .git file) anywhere
// below an ignored path, and only there: a repository in a directory that is
// not ignored is untracked, which status lists already.
func TestNestedRepos(t *testing.T) {
	dir := gitRepo(t)
	t.Chdir(dir)
	writeFile(t, dir, ".gitignore", "vendor/\nbuild\n*.log\n")
	runGit(t, dir, "add", ".gitignore")
	runGit(t, dir, "commit", "-qm", "ignores")
	for _, r := range []string{"vendor/a/dep", "build", "plain"} {
		if err := os.MkdirAll(filepath.Join(dir, r), 0o755); err != nil {
			t.Fatal(err)
		}
		runGit(t, filepath.Join(dir, r), "init", "-q")
	}
	if err := os.MkdirAll(filepath.Join(dir, "vendor", "b", "wt"), 0o755); err != nil {
		t.Fatal(err)
	}
	writeFile(t, dir, "vendor/b/wt/.git", "gitdir: /elsewhere\n") // a linked worktree's .git file
	writeFile(t, dir, "vendor/b/wt/x.txt", "x\n")
	writeFile(t, dir, "debug.log", "an ignored file\n")
	if err := os.Symlink(filepath.Join(dir, "plain"), filepath.Join(dir, "vendor", "link")); err != nil {
		t.Fatal(err) // a link to a repository: git deletes only the link
	}
	got, err := NestedRepos(dir)
	if want := []string{"build", "vendor/a/dep", "vendor/b/wt"}; err != nil || !reflect.DeepEqual(got, want) {
		t.Fatalf("NestedRepos = %q, %v; want %q (plain/ is untracked, status lists it)", got, err, want)
	}
	if st, _ := StatusEntries(dir); !reflect.DeepEqual(st, []string{"?? plain/"}) {
		t.Errorf("StatusEntries = %q, want the untracked repository plain/", st)
	}
	if got, err := NestedRepos(filepath.Join(t.TempDir(), "nowhere")); err == nil {
		t.Errorf("NestedRepos of a missing directory = %q, nil; want an error", got)
	}
}

// An ignored directory it cannot read could hold a repository: an error.
func TestNestedRepos_UnreadableDirectoryIsAnError(t *testing.T) {
	if os.Geteuid() == 0 {
		t.Skip("root reads every directory")
	}
	dir := gitRepo(t)
	t.Chdir(dir)
	writeFile(t, dir, ".gitignore", "cache/\n")
	runGit(t, dir, "add", ".gitignore")
	runGit(t, dir, "commit", "-qm", "ignores")
	if err := os.MkdirAll(filepath.Join(dir, "cache", "sub"), 0o755); err != nil {
		t.Fatal(err)
	}
	writeFile(t, dir, "cache/sub/f", "x\n")
	locked := filepath.Join(dir, "cache", "sub")
	if err := os.Chmod(locked, 0); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.Chmod(locked, 0o755) })
	if got, err := NestedRepos(dir); err == nil {
		t.Errorf("NestedRepos = %q, nil; want an error for the unreadable cache/sub", got)
	}
}

// A checked-out submodule makes git refuse the removal; one that is not
// checked out does not.
func TestPopulatedSubmodules(t *testing.T) {
	sub := gitRepo(t)
	dir := gitRepo(t)
	t.Chdir(dir)
	runGit(t, dir, "-c", "protocol.file.allow=always", "submodule", "add", "-q", sub, "sub")
	runGit(t, dir, "commit", "-qm", "sub")
	wt := filepath.Join(t.TempDir(), "wt")
	runGit(t, dir, "worktree", "add", "-q", "-b", "w", wt)
	if got, err := PopulatedSubmodules(wt); err != nil || len(got) != 0 {
		t.Fatalf("submodule not checked out in the worktree: PopulatedSubmodules = %q, %v; want none", got, err)
	}
	runGit(t, wt, "-c", "protocol.file.allow=always", "submodule", "update", "-q", "--init")
	got, err := PopulatedSubmodules(wt)
	if err != nil || len(got) == 0 || got[0] != "sub" {
		t.Fatalf("PopulatedSubmodules = %q, %v; want sub", got, err)
	}
	if err := WorktreeRemove(wt, false); err == nil || !strings.Contains(err.Error(), "submodules") {
		t.Errorf("WorktreeRemove = %v; want git's refusal of a worktree with submodules, which PopulatedSubmodules predicts", err)
	}
}

// branchFixture is a repo on main with a branch x one commit ahead, not checked
// out anywhere, carrying an upstream config section.
func branchFixture(t *testing.T) (dir, tip string) {
	t.Helper()
	dir = gitRepo(t)
	t.Chdir(dir)
	runGit(t, dir, "branch", "x")
	runGit(t, dir, "checkout", "-q", "x")
	runGit(t, dir, "commit", "--allow-empty", "-qm", "on x")
	runGit(t, dir, "checkout", "-q", "main")
	runGit(t, dir, "config", "branch.x.remote", "origin")
	runGit(t, dir, "config", "branch.x.merge", "refs/heads/x")
	return dir, gitOut(t, dir, "rev-parse", "x")
}

func TestDeleteBranchAt(t *testing.T) {
	dir, tip := branchFixture(t)
	elsewhere := t.TempDir()
	t.Chdir(elsewhere) // git runs in dir, never in the current directory
	if err := DeleteBranchAt(dir, "x", gitOut(t, dir, "rev-parse", "main")); err == nil || !strings.Contains(err.Error(), "expected") {
		t.Errorf("DeleteBranchAt at a tip x is not at = %v, want update-ref's refusal", err)
	}
	if BranchTipIn(dir, "x") != tip {
		t.Fatal("a refused delete moved or deleted x")
	}
	if err := DeleteBranchAt(dir, "x", tip); err != nil {
		t.Fatalf("DeleteBranchAt: %v", err)
	}
	if BranchTipIn(dir, "x") != "" {
		t.Error("x is still there")
	}
	if out, _ := RunDir(dir, "config", "--get-regexp", `^branch\.x\.`); out != "" {
		t.Errorf("x's config section is still there: %q", out)
	}
	for _, c := range [][2]string{{"", tip}, {"main", ""}} {
		if err := DeleteBranchAt(dir, c[0], c[1]); err == nil {
			t.Errorf("DeleteBranchAt(%q, %q) = nil, want an error", c[0], c[1])
		}
	}
}

// What `git branch -D` refuses, DeleteBranchAt refuses too: a branch a worktree
// has checked out, or is rebasing (HEAD detached meanwhile, so `git worktree
// list` shows no branch there).
func TestDeleteBranchAt_RefusesABranchInUse(t *testing.T) {
	dir, tip := branchFixture(t)
	wt := filepath.Join(t.TempDir(), "wt")
	runGit(t, dir, "worktree", "add", "-q", wt, "x")
	if err := DeleteBranchAt(dir, "x", tip); err == nil || !strings.Contains(err.Error(), "checked out in") {
		t.Errorf("DeleteBranchAt on a checked-out branch = %v, want a refusal", err)
	}
	runGit(t, wt, "commit", "--allow-empty", "-qm", "two")
	runGit(t, wt, "commit", "--allow-empty", "-qm", "three")
	cmd := exec.Command("git", "rebase", "-q", "-i", "HEAD~2")
	cmd.Dir = wt
	cmd.Env = append(os.Environ(), "GIT_SEQUENCE_EDITOR=sed -i.bak -e 1s/^pick/edit/")
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("rebase: %v\n%s", err, out)
	}
	if b := gitOut(t, wt, "worktree", "list", "--porcelain"); strings.Contains(b, "branch refs/heads/x") {
		t.Fatalf("fixture: worktree list still shows x checked out:\n%s", b)
	}
	now := gitOut(t, dir, "rev-parse", "x")
	if err := DeleteBranchAt(dir, "x", now); err == nil || !strings.Contains(err.Error(), "rebased or bisected") {
		t.Errorf("DeleteBranchAt on a branch being rebased = %v, want a refusal", err)
	}
	if BranchTipIn(dir, "x") != now {
		t.Error("x went")
	}
}

// The tip check and the delete are one step: a commit that lands on the branch
// after the caller read it, even right before the delete, keeps the branch.
// A git shim moves x just before `update-ref -d` runs, as another process
// would; `git branch -D` deleted the moved branch anyway (#177 review, s11).
func TestDeleteBranchAt_ARaceBeforeTheDeleteKeepsTheBranch(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("the git shim is a shell script")
	}
	dir, tip := branchFixture(t)
	moved := gitOut(t, dir, "commit-tree", "-p", tip, "-m", "made during the delete", gitOut(t, dir, "rev-parse", tip+"^{tree}"))
	realGit, err := exec.LookPath("git")
	if err != nil {
		t.Fatal(err)
	}
	shim := t.TempDir()
	script := "#!/bin/sh\ncase \" $* \" in *\" update-ref \"*\" -d refs/heads/x \"*) \"" + realGit + "\" update-ref refs/heads/x " + moved + " ;; esac\nexec \"" + realGit + "\" \"$@\"\n"
	if err := os.WriteFile(filepath.Join(shim, "git"), []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", shim+string(os.PathListSeparator)+os.Getenv("PATH"))
	err = DeleteBranchAt(dir, "x", tip)
	if err == nil || !strings.Contains(err.Error(), "expected") {
		t.Errorf("DeleteBranchAt across a concurrent move = %v, want update-ref's refusal", err)
	}
	if got := BranchTipIn(dir, "x"); got != moved {
		t.Errorf("x is at %q, want the moved %s kept", got, moved)
	}
}

// RefTipsExcept is what keeps a commit reachable once a branch and its
// worktree go: every other ref (tags, other branches, the stash) and every
// other worktree's HEAD, detached ones included.
func TestRefTipsExcept(t *testing.T) {
	dir, tip := branchFixture(t)
	runGit(t, dir, "tag", "t1", "main")
	det := filepath.Join(t.TempDir(), "det")
	runGit(t, dir, "worktree", "add", "-q", "--detach", det, tip)
	got, err := RefTipsExcept(dir, "refs/heads/x", "")
	if err != nil {
		t.Fatal(err)
	}
	joined := " " + strings.Join(got, " ") + " "
	if !strings.Contains(joined, " "+tip+" ") {
		t.Errorf("RefTipsExcept = %q; want the detached worktree's HEAD %s", got, tip)
	}
	for _, skip := range []string{realPathT(t, det), det} { // det: /var/... names /private/var/... on macOS
		if got, _ := RefTipsExcept(dir, "refs/heads/x", skip); strings.Contains(" "+strings.Join(got, " ")+" ", " "+tip+" ") {
			t.Errorf("RefTipsExcept skipping %s = %q; want x's tip absent", skip, got)
		}
	}
}

func realPathT(t *testing.T, p string) string {
	t.Helper()
	r, err := filepath.EvalSymlinks(p)
	if err != nil {
		t.Fatal(err)
	}
	return r
}

// FetchRemote never prunes, whatever fetch.prune says: a stale remote-tracking
// ref can be the only copy of a branch's commits in this clone.
func TestFetchRemote_NeverPrunes(t *testing.T) {
	origin := gitRepo(t)
	runGit(t, origin, "branch", "gone")
	dir := filepath.Join(t.TempDir(), "clone")
	runGit(t, filepath.Dir(dir), "clone", "-q", origin, dir)
	t.Chdir(dir)
	runGit(t, dir, "config", "fetch.prune", "true")
	runGit(t, origin, "branch", "-D", "gone")
	if err := FetchRemote("origin"); err != nil {
		t.Fatal(err)
	}
	if RemoteTrackingTip("gone") == "" {
		t.Error("FetchRemote pruned origin/gone (fetch.prune=true)")
	}
}

// StatusEntries reports a submodule's changes whatever submodule.<name>.ignore
// says, as `git worktree remove`'s own check does.
func TestStatusEntries_SubmoduleIgnoreConfigIsOverridden(t *testing.T) {
	sub := gitRepo(t)
	writeFile(t, sub, "a.txt", "a\n")
	runGit(t, sub, "add", "a.txt")
	runGit(t, sub, "commit", "-qm", "a")
	dir := gitRepo(t)
	t.Chdir(dir)
	runGit(t, dir, "-c", "protocol.file.allow=always", "submodule", "add", "-q", sub, "sub")
	runGit(t, dir, "config", "-f", ".gitmodules", "submodule.sub.ignore", "all")
	runGit(t, dir, "add", ".gitmodules")
	runGit(t, dir, "commit", "-qm", "sub, ignore=all")
	writeFile(t, dir, "sub/a.txt", "an edit inside the submodule\n")
	if out := gitOut(t, dir, "status", "--porcelain"); out != "" {
		t.Fatalf("fixture: git status shows %q; ignore=all should hide it", out)
	}
	if got, err := StatusEntries(dir); err != nil || len(got) != 1 || !strings.HasSuffix(got[0], "sub") {
		t.Errorf("StatusEntries = %q, %v; want the submodule's change listed", got, err)
	}
}

// StatusEntries reads another window's worktree, and must not rewrite its
// index (which takes index.lock and can fail that window's own commit).
func TestStatusEntries_LeavesTheIndexAlone(t *testing.T) {
	dir := gitRepo(t)
	writeFile(t, dir, "f.txt", "x\n")
	runGit(t, dir, "add", "f.txt")
	runGit(t, dir, "commit", "-qm", "f")
	later := time.Now().Add(2 * time.Hour)
	if err := os.Chtimes(filepath.Join(dir, "f.txt"), later, later); err != nil {
		t.Fatal(err)
	}
	index := filepath.Join(dir, ".git", "index")
	before, err := os.Stat(index)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := StatusEntries(dir); err != nil {
		t.Fatal(err)
	}
	if after, _ := os.Stat(index); !after.ModTime().Equal(before.ModTime()) {
		t.Fatalf("StatusEntries rewrote the index: it must run with GIT_OPTIONAL_LOCKS=0")
	}
	runGit(t, dir, "status", "--porcelain")
	if after, _ := os.Stat(index); after.ModTime().Equal(before.ModTime()) {
		t.Skip("this git did not refresh the index on a plain status; the check above proves nothing here")
	}
}

// Under core.ignorecase a branch differing only in case is one loose ref file
// (#167): a worktree on X has x checked out, and x is not deleted.
func TestDeleteBranchAt_RefusesACaseTwinInUse(t *testing.T) {
	dir, tip := branchFixture(t)
	if !ignoreCaseIn(dir) {
		t.Skip("core.ignorecase is off: X and x are two refs here")
	}
	wt := filepath.Join(t.TempDir(), "wt")
	runGit(t, dir, "worktree", "add", "-q", wt, "X")
	if err := DeleteBranchAt(dir, "x", tip); err == nil || !strings.Contains(err.Error(), "checked out in") {
		t.Errorf("DeleteBranchAt(x) with X checked out = %v, want a refusal", err)
	}
}

// A hidden symlink entry that is no longer a link on disk is a change.
func TestStatusEntries_HiddenSymlinkTypeChange(t *testing.T) {
	_, wt := hiddenFixture(t, false, map[string]string{"a.txt": "a\n"})
	if err := os.Symlink("a.txt", filepath.Join(wt, "link")); err != nil {
		t.Fatal(err)
	}
	runGit(t, wt, "add", "link")
	runGit(t, wt, "commit", "-qm", "link")
	hide(t, wt, "assume-unchanged", "link")
	if got, err := StatusEntries(wt); err != nil || len(got) != 0 {
		t.Fatalf("flagged, unchanged: StatusEntries = %q, %v", got, err)
	}
	if err := os.Remove(filepath.Join(wt, "link")); err != nil {
		t.Fatal(err)
	}
	writeFile(t, wt, "link", "a.txt")
	if got, err := StatusEntries(wt); err != nil || len(got) != 1 || !strings.HasPrefix(got[0], " M link  (") {
		t.Errorf("a link that became a file: StatusEntries = %q, %v", got, err)
	}
}

// git refuses a worktree whose git dir has a modules/ directory even once no
// submodule is checked out (deinit leaves the module's repository there), and
// PopulatedSubmodules says so.
func TestPopulatedSubmodules_ModulesDirAlone(t *testing.T) {
	sub := gitRepo(t)
	dir := gitRepo(t)
	t.Chdir(dir)
	runGit(t, dir, "-c", "protocol.file.allow=always", "submodule", "add", "-q", sub, "sub")
	runGit(t, dir, "commit", "-qm", "sub")
	wt := filepath.Join(t.TempDir(), "wt")
	runGit(t, dir, "worktree", "add", "-q", "-b", "w", wt)
	runGit(t, wt, "-c", "protocol.file.allow=always", "submodule", "update", "-q", "--init")
	runGit(t, wt, "submodule", "deinit", "-q", "-f", "sub")
	got, err := PopulatedSubmodules(wt)
	if err != nil || !reflect.DeepEqual(got, []string{"(its git dir's modules/)"}) {
		t.Fatalf("PopulatedSubmodules after deinit = %q, %v; want only the modules/ directory", got, err)
	}
	if err := WorktreeRemove(wt, false); err == nil || !strings.Contains(err.Error(), "submodules") {
		t.Errorf("WorktreeRemove = %v; want git's refusal, which the modules/ directory alone triggers", err)
	}
}
