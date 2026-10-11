package worktree

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// #210: an edit to a tracked file that git status never looks at is
// uncommitted work. `wt clean -y`, Remove (merge-pr's auto-clean, `release
// --clean`, claim's rollback) and `wt discard` all read it through
// gitx.StatusEntries and keep the worktree; `git worktree remove` alone deleted
// the edit with it.

var hidingMechanisms = []string{"assume-unchanged", "skip-worktree", "core.ignoreStat"}

// hideEdit edits the tracked file in the worktree at dir and hides the edit from
// git status the way mech does. For core.ignoreStat the repo must have been set
// to it before the file was checked out or added (the fixtures do that).
func (f *adoptFixture) hideEdit(dir, mech, file string) {
	f.t.Helper()
	if mech != "core.ignoreStat" {
		gitW(f.t, dir, "update-index", "--"+mech, "--", file)
	}
	if tag := gitOutW(f.t, dir, "ls-files", "-v", "--", file); tag == "" || tag[0] == 'H' {
		f.t.Fatalf("fixture: %s is not flagged (%q)", file, tag)
	}
	if err := os.WriteFile(filepath.Join(dir, file), []byte("an edit git status does not show\n"), 0o644); err != nil {
		f.t.Fatal(err)
	}
	if out := gitOutW(f.t, dir, "status", "--porcelain"); out != "" {
		f.t.Fatalf("fixture: git status shows %q; %s should hide the edit", out, mech)
	}
}

func TestClean_KeepsEditsGitStatusHides(t *testing.T) {
	for _, mech := range hidingMechanisms {
		t.Run(mech, func(t *testing.T) {
			f := newAdoptFixture(t)
			failingGh(t)
			if mech == "core.ignoreStat" {
				gitW(t, f.repo, "config", "core.ignoreStat", "true")
			}
			dir := f.shippedWorktree()
			f.hideEdit(dir, mech, ".gitignore")
			var err error
			out := captured(t, func() { err = Clean(f.c, true, false, false, []string{"shipped"}) })
			if err != nil {
				t.Fatalf("Clean: %v\n%s", err, out)
			}
			if !strings.Contains(out, "shipped — has uncommitted changes (1 file(s)), leave alone") {
				t.Errorf("clean said:\n%s\nwant it to keep the worktree for its uncommitted change", out)
			}
			if b, rerr := os.ReadFile(filepath.Join(dir, ".gitignore")); rerr != nil || !strings.Contains(string(b), "does not show") {
				t.Errorf("the hidden edit is gone (%v)", rerr)
			}
			if !f.has("refs/heads/shipped") {
				t.Error("the branch was deleted")
			}
		})
	}
}

// Remove is the gate merge-pr's auto-clean, `release --clean` and claim's
// rollback go through.
func TestRemove_RefusesEditsGitStatusHides(t *testing.T) {
	for _, mech := range hidingMechanisms {
		t.Run(mech, func(t *testing.T) {
			f := newAdoptFixture(t)
			if mech == "core.ignoreStat" {
				gitW(t, f.repo, "config", "core.ignoreStat", "true")
			}
			dir := f.worktreeOn("lane", f.base)
			if err := os.WriteFile(filepath.Join(dir, "conf.txt"), []byte("v1\n"), 0o644); err != nil {
				t.Fatal(err)
			}
			gitW(t, dir, "add", "conf.txt")
			f.commitHere(dir, "conf")
			f.hideEdit(dir, mech, "conf.txt")
			err := Remove(f.c, dir, "lane", false)
			if err == nil || !strings.Contains(err.Error(), "has uncommitted changes") {
				t.Fatalf("Remove = %v, want its refusal of uncommitted changes", err)
			}
			if b, rerr := os.ReadFile(filepath.Join(dir, "conf.txt")); rerr != nil || !strings.Contains(string(b), "does not show") || !f.has("refs/heads/lane") {
				t.Errorf("the edit (%v) or the branch is gone", rerr)
			}
		})
	}
}

func TestDiscard_RefusesEditsGitStatusHides(t *testing.T) {
	for _, mech := range hidingMechanisms {
		t.Run(mech, func(t *testing.T) {
			f := newDiscardFixture(t)
			if mech == "core.ignoreStat" {
				gitW(t, f.repo, "config", "core.ignoreStat", "true")
			}
			dir := f.worktreeOn("hid", f.base)
			if err := os.WriteFile(filepath.Join(dir, "conf.txt"), []byte("v1\n"), 0o644); err != nil {
				t.Fatal(err)
			}
			gitW(t, dir, "add", "conf.txt")
			f.commitHere(dir, "conf")
			f.hideEdit(dir, mech, "conf.txt")
			r, err := Discard(f.c, "hid", DiscardOpts{DropCommits: true})
			f.assertRefused(r, err, DiscardDirty, dir, "hid")
			if len(r.Dirty) != 1 || !strings.Contains(r.Dirty[0], "conf.txt") {
				t.Errorf("dirty entries %q, want conf.txt named", r.Dirty)
			}
			if b, rerr := os.ReadFile(filepath.Join(dir, "conf.txt")); rerr != nil || !strings.Contains(string(b), "does not show") {
				t.Errorf("the hidden edit is gone (%v)", rerr)
			}
		})
	}
}
