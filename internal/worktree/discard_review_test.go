package worktree

import (
	"errors"
	"io/fs"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"syscall"
	"testing"

	"github.com/eharriett0/wt/internal/activework"
)

// #177 review: pins for what the first round left open.

// apply never forces: a file that appeared after the status was read (the
// fetch runs in between) stops the removal, by git's own check.
func TestDiscard_ApplyNeverForces(t *testing.T) {
	f := newDiscardFixture(t)
	dir := f.worktreeOn("late", f.base)
	r := DiscardResult{Dir: realPath(dir), Branch: "late", Tip: f.tip(dir, "HEAD"), root: realPath(f.repo)}
	file := filepath.Join(dir, "written-during-the-fetch.txt")
	if err := os.WriteFile(file, []byte("new work\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := r.apply(f.c); err == nil {
		t.Fatal("apply removed a worktree holding an untracked file made after the listing")
	}
	if _, err := os.Stat(file); err != nil || !f.has("refs/heads/late") {
		t.Fatalf("the file (%v) or the branch is gone", err)
	}
}

// Offline, no remote-tracking ref but origin/<base> is trusted: #177's canary
// pushed under another name (canary:canary-test) and deleted there, or pushed
// to another remote and deleted there, left a stale ref that hid its commit,
// and the branch went without --drop-commits (review s17, s5).
func TestDiscard_OfflineTrustsOnlyTheBase(t *testing.T) {
	cases := map[string]func(f *adoptFixture, dir string){
		"pushed under another name, deleted on origin": func(f *adoptFixture, dir string) {
			gitW(f.t, dir, "push", "-q", "origin", "canary:canary-test")
			gitW(f.t, f.repo, "fetch", "-q", "origin")
			gitW(f.t, f.origin, "update-ref", "-d", "refs/heads/canary-test")
		},
		"pushed to another remote, deleted there": func(f *adoptFixture, dir string) {
			fork := filepath.Join(f.t.TempDir(), "fork.git")
			gitW(f.t, f.repo, "init", "-q", "--bare", "-b", "main", fork)
			gitW(f.t, f.repo, "remote", "add", "fork", fork)
			gitW(f.t, dir, "push", "-q", "-u", "fork", "canary")
			gitW(f.t, fork, "update-ref", "-d", "refs/heads/canary")
		},
	}
	for name, setup := range cases {
		t.Run(name, func(t *testing.T) {
			f := newDiscardFixture(t)
			dir := f.worktreeOn("canary", f.base)
			tip := f.commitHere(dir, "canary: never merge")
			setup(f, dir)
			f.offline()
			r, err := Discard(f.c, "canary", DiscardOpts{})
			f.assertRefused(r, err, DiscardNeedsDropCommits, dir, "canary")
			if r.OriginAsked || r.OfflineBase != f.base || len(r.Unique) != 1 || r.Unique[0].SHA != tip {
				t.Errorf("asked %v, offline base %q, listed %+v; want origin unasked, origin/main %s trusted, %s listed", r.OriginAsked, r.OfflineBase, r.Unique, f.base, tip)
			}
			if !strings.Contains(err.Error(), "origin may not have") {
				t.Errorf("refusal %q does not say origin couldn't be asked", err)
			}
		})
	}
}

// Offline with no origin/<base> here at all, every commit on the branch counts.
func TestDiscard_OfflineWithoutTheBaseCountsEverything(t *testing.T) {
	f := newDiscardFixture(t)
	dir := f.worktreeOn("canary", f.base)
	f.commitHere(dir, "canary")
	gitW(t, f.repo, "update-ref", "-d", "refs/remotes/origin/main")
	f.offline()
	r, err := Discard(f.c, "canary", DiscardOpts{})
	f.assertRefused(r, err, DiscardNeedsDropCommits, dir, "canary")
	if r.OfflineBase != "" || len(r.Unique) != 2 {
		t.Errorf("offline base %q, listed %d; want none and both commits (the base's too)", r.OfflineBase, len(r.Unique))
	}
}

// holdsCwd compares directories, not path text (review s4): the worktree
// holds the cwd by any path that reaches it, a differently-cased one (macOS's
// filesystem ignores case) or a symlink included. An unreadable cwd counts as
// held (fail closed); a directory that does not exist holds nothing.
func TestHoldsCwd(t *testing.T) {
	root := realPath(t.TempDir())
	wt := filepath.Join(root, "repo-worktrees", "here")
	sub := filepath.Join(wt, "sub")
	other := filepath.Join(root, "other")
	for _, d := range []string{sub, other} {
		if err := os.MkdirAll(d, 0o755); err != nil {
			t.Fatal(err)
		}
	}
	link := filepath.Join(root, "link")
	if err := os.Symlink(wt, link); err != nil {
		t.Fatal(err)
	}
	cases := map[string]bool{wt: true, sub: true, link: true, filepath.Join(link, "sub"): true, other: false, root: false}
	if up := filepath.Join(root, "REPO-WORKTREES", "HERE", "sub"); dirExists(up) {
		cases[up] = true // a case-insensitive filesystem: the same directory
	}
	for cwd, want := range cases {
		t.Chdir(cwd)
		if got := holdsCwd(wt); got != want {
			t.Errorf("cwd %s: holdsCwd(%s) = %v, want %v", cwd, wt, got, want)
		}
	}
	t.Chdir(wt)
	if holdsCwd(filepath.Join(root, "nope")) {
		t.Error("a directory that does not exist held the cwd")
	}
	// A worktree that can't be read might hold it (fail closed).
	if os.Geteuid() != 0 {
		parent := filepath.Join(root, "repo-worktrees")
		t.Chdir(other)
		if err := os.Chmod(parent, 0); err != nil {
			t.Fatal(err)
		}
		held := holdsCwd(wt)
		if err := os.Chmod(parent, 0o755); err != nil {
			t.Fatal(err)
		}
		if !held {
			t.Error("an unreadable worktree read as not holding the cwd; it must count as held (fail closed)")
		}
	}
	// A cwd inside it that was deleted since is still inside it.
	doomed := filepath.Join(wt, "doomed")
	if err := os.Mkdir(doomed, 0o755); err != nil {
		t.Fatal(err)
	}
	t.Chdir(doomed)
	if err := os.Remove(doomed); err != nil {
		t.Fatal(err)
	}
	if !holdsCwd(wt) {
		t.Error("a deleted cwd inside the worktree read as outside it")
	}
	// A cwd that can't be read counts as held, wherever the worktree is.
	if os.Geteuid() != 0 {
		sealed := filepath.Join(root, "sealed")
		if err := os.Mkdir(sealed, 0o755); err != nil {
			t.Fatal(err)
		}
		t.Chdir(sealed)
		if err := os.Chmod(sealed, 0); err != nil {
			t.Fatal(err)
		}
		held := holdsCwd(other)
		if err := os.Chmod(sealed, 0o755); err != nil {
			t.Fatal(err)
		}
		if !held {
			t.Error("an unreadable cwd read as outside the worktree; it must count as held (fail closed)")
		}
	}
	if err := os.Chdir(root); err != nil {
		t.Fatal(err)
	}
}

func dirExists(p string) bool { fi, err := os.Stat(p); return err == nil && fi.IsDir() }

// The window discard runs in is refused by any path to it (review s4).
func TestDiscard_RefusesTheWindowItRunsInByAnyPath(t *testing.T) {
	f := newDiscardFixture(t)
	dir := f.worktreeOn("here", f.base)
	up := filepath.Join(filepath.Dir(dir), strings.ToUpper(filepath.Base(dir)))
	if !dirExists(up) {
		t.Skip("case-sensitive filesystem")
	}
	t.Chdir(up)
	r, err := Discard(f.c, "here", DiscardOpts{DropCommits: true})
	f.assertRefused(r, err, DiscardHoldsCwd, dir, "here")
}

// A stat error that is not "no such file" is not a gone directory: its status
// counts as unreadable (dirty) instead, so it is never discarded as having
// nothing on disk to lose.
func TestDirGone(t *testing.T) {
	for _, c := range []struct {
		err  error
		want bool
	}{
		{nil, false},
		{fs.ErrNotExist, true},
		{&fs.PathError{Op: "stat", Path: "/x", Err: syscall.ENOENT}, true},
		{&fs.PathError{Op: "stat", Path: "/x", Err: syscall.EACCES}, false},
		{&fs.PathError{Op: "stat", Path: "/x", Err: syscall.ENOTDIR}, false},
		{errors.New("anything else"), false},
	} {
		if got := dirGone(c.err); got != c.want {
			t.Errorf("dirGone(%v) = %v, want %v", c.err, got, c.want)
		}
	}
}

// A git repository in the worktree's ignored files is refused, and listed:
// `git worktree remove` deleted it, unpushed commits and all, while git status
// (and discard's plan) said "no uncommitted changes" (review s6).
func TestDiscard_RefusesARepositoryInItsIgnoredFiles(t *testing.T) {
	f := newDiscardFixture(t)
	dir := f.worktreeOn("nst", f.base)
	if err := os.WriteFile(filepath.Join(dir, ".gitignore"), []byte("vendor/\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	gitW(t, dir, "add", ".gitignore")
	f.commitHere(dir, "ignore vendor")
	gitW(t, dir, "push", "-q", "-u", "origin", "nst")
	dep := filepath.Join(dir, "vendor", "dep")
	gitW(t, f.repo, "init", "-q", dep)
	f.commitHere(dep, "unpushed work in a nested clone")
	r, err := Discard(f.c, "nst", DiscardOpts{DropCommits: true, AllRoots: true})
	f.assertRefused(r, err, DiscardNestedRepo, dir, "nst")
	if !reflect.DeepEqual(r.Nested, []string{"vendor/dep"}) || !strings.Contains(err.Error(), "vendor/dep") {
		t.Errorf("nested %q, err %v; want vendor/dep listed", r.Nested, err)
	}
	if !dirExists(filepath.Join(dep, ".git")) {
		t.Error("the nested repository was deleted")
	}
}

// A checked-out submodule is refused up front, dry run included: git refuses
// to remove the worktree, and the dry run used to promise it would (review s7).
func TestDiscard_RefusesASubmoduleUpFront(t *testing.T) {
	f := newDiscardFixture(t)
	sub := filepath.Join(t.TempDir(), "sub")
	gitW(t, f.repo, "init", "-q", "-b", "main", sub)
	f.commitHere(sub, "sub base")
	dir := f.worktreeOn("sm", f.base)
	gitW(t, dir, "-c", "protocol.file.allow=always", "submodule", "add", "-q", sub, "sub")
	f.commitHere(dir, "add a submodule")
	gitW(t, dir, "push", "-q", "-u", "origin", "sm")
	for _, dry := range []bool{true, false} {
		r, err := Discard(f.c, "sm", DiscardOpts{DryRun: dry})
		f.assertRefused(r, err, DiscardSubmodule, dir, "sm")
		if len(r.Submodules) == 0 || r.Submodules[0] != "sub" || !strings.Contains(err.Error(), "submodules") {
			t.Errorf("dry=%v: submodules %q, err %v; want sub named", dry, r.Submodules, err)
		}
		if dry != strings.Contains(err.Error(), "--dry-run") {
			t.Errorf("dry=%v: err %q", dry, err)
		}
	}
}

// The plan tells "not on origin" from "unreachable": a commit also on a local
// branch, a tag or the stash is not on origin, but deleting the branch loses
// none of them (review s6).
func TestDiscard_CountsWhatGoesUnreachable(t *testing.T) {
	f := newDiscardFixture(t)
	dir := f.worktreeOn("rch", f.base)
	c1 := f.commitHere(dir, "also on a local branch")
	gitW(t, f.repo, "branch", "keepme", c1)
	c2 := f.commitHere(dir, "tagged")
	gitW(t, f.repo, "tag", "t-only", c2)
	f.commitHere(dir, "only on rch")
	r, err := Discard(f.c, "rch", DiscardOpts{DryRun: true, DropCommits: true})
	if err != nil || len(r.Unique) != 3 || !r.OrphansCounted || r.Orphans != 1 {
		t.Fatalf("dry run: listed %d, orphans %d (counted %v), err %v; want 3 listed, 1 unreachable", len(r.Unique), r.Orphans, r.OrphansCounted, err)
	}
	if err := os.WriteFile(filepath.Join(dir, "s.txt"), []byte("stashed\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	gitW(t, dir, "add", "s.txt")
	gitW(t, dir, "stash", "-q")
	r, err = Discard(f.c, "rch", DiscardOpts{DropCommits: true})
	f.assertDiscarded(r, err, dir, "rch")
	if r.Orphans != 0 {
		t.Errorf("with a stash on the tip, orphans = %d, want 0 (the stash keeps them)", r.Orphans)
	}
	for _, sha := range []string{c1, c2} {
		if !f.has(sha + "^{commit}") {
			t.Errorf("%s went", sha)
		}
	}
}

// `wt release <issue>` drops claims BY ISSUE, so discard suggests it only when
// no other worktree still claims that issue (review s16).
func TestClaimNotes(t *testing.T) {
	left := []activework.Entry{{Issue: "7", Branch: "feat-7"}, {Issue: "8", Branch: "feat-8"}}
	got := claimNotes([]string{"7", "9"}, left)
	if len(got) != 2 || !strings.Contains(got[0], "#7 is still claimed on feat-7") || strings.Contains(got[0], "`wt release 7` unassigns") ||
		got[1] != "`wt release 9` unassigns #9" {
		t.Errorf("claimNotes = %q", got)
	}
}

// The shared removal gate (clean -y, merge-pr's auto-clean, release --clean,
// claim's rollback) keeps a worktree whose ignored files hold a git repository,
// and names a checked-out submodule before git refuses; clean's listing agrees.
func TestRemove_RefusesWhatGitStatusDoesNotShow(t *testing.T) {
	f := newAdoptFixture(t)
	lane := f.worktreeOn("lane", f.base)
	if err := os.WriteFile(filepath.Join(lane, ".gitignore"), []byte("vendor/\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	gitW(t, lane, "add", ".gitignore")
	f.commitHere(lane, "ignore vendor")
	dep := filepath.Join(lane, "vendor", "dep")
	gitW(t, f.repo, "init", "-q", dep)
	f.commitHere(dep, "unpushed")
	if err := Remove(f.c, lane, "lane", false); err == nil || !strings.Contains(err.Error(), "git repository in its ignored files (vendor/dep)") {
		t.Errorf("Remove = %v, want the nested repository named", err)
	}
	if !dirExists(filepath.Join(dep, ".git")) || !f.has("refs/heads/lane") {
		t.Error("the nested repository or the branch is gone")
	}

	sub := filepath.Join(t.TempDir(), "sub")
	gitW(t, f.repo, "init", "-q", "-b", "main", sub)
	f.commitHere(sub, "sub base")
	sm := f.worktreeOn("sm", f.base)
	gitW(t, sm, "-c", "protocol.file.allow=always", "submodule", "add", "-q", sub, "sub")
	f.commitHere(sm, "add a submodule")
	if err := Remove(f.c, sm, "sm", false); err == nil || !strings.Contains(err.Error(), "checked-out submodules (sub") {
		t.Errorf("Remove = %v, want the submodule named before git refuses", err)
	}
}

func TestClean_KeepsARepositoryInIgnoredFiles(t *testing.T) {
	f := newAdoptFixture(t)
	failingGh(t)
	dir := f.shippedWorktree() // .gitignore: *.log
	dep := filepath.Join(dir, "cache.log", "dep")
	gitW(t, f.repo, "init", "-q", dep)
	f.commitHere(dep, "unpushed work in an ignored clone")
	for _, apply := range []bool{false, true} {
		var err error
		out := captured(t, func() { err = Clean(f.c, apply, false, false, []string{"shipped"}) })
		if err != nil {
			t.Fatalf("Clean: %v\n%s", err, out)
		}
		if !strings.Contains(out, "shipped — shipped, but it holds a git repository in its ignored files (cache.log/dep)") || strings.Contains(out, "safe to remove") {
			t.Errorf("apply=%v: clean said:\n%s\nwant it to keep the worktree for the nested repository", apply, out)
		}
	}
	if !dirExists(filepath.Join(dep, ".git")) || !f.has("refs/heads/shipped") {
		t.Error("the nested repository or the branch is gone")
	}
}
