package gitx

import (
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"sort"
	"strings"
)

// What `git worktree remove` would delete or refuse that git status does not
// show (#177 review): a git repository in a worktree's IGNORED files, and a
// checked-out submodule.

// NestedRepos returns the git repositories inside the ignored part of the work
// tree at dir, relative to dir with forward slashes, sorted: every directory
// below an ignored path that holds a .git (a directory, or the .git file of a
// linked worktree, another repository's included). `git worktree remove`
// deletes ignored files with the worktree, such a repository's .git with them,
// unpushed commits and all, and neither its check nor git status looks inside
// an ignored directory (measured, git 2.39). A repository in a directory that
// is NOT ignored is untracked, and StatusEntries lists it already (`?? dir/`).
// The ignored paths are `git ls-files --others --ignored --exclude-standard
// --directory`, git's own notion of ignored; symlinks are not followed, as git
// deletes only the link. Any error, an unreadable directory included, is
// returned: it could hide one.
func NestedRepos(dir string) ([]string, error) {
	out, err := runRawReadOnly(dir, "ls-files", "-z", "--others", "--ignored", "--exclude-standard", "--directory")
	if err != nil {
		return nil, err
	}
	seen := map[string]bool{}
	var found []string
	for _, p := range splitNUL(out) {
		root := filepath.Join(dir, filepath.FromSlash(strings.TrimSuffix(p, "/")))
		werr := filepath.WalkDir(root, func(path string, d fs.DirEntry, err error) error {
			if err != nil {
				return err
			}
			if d.Name() != ".git" {
				return nil
			}
			rel, rerr := filepath.Rel(dir, filepath.Dir(path))
			if rerr != nil {
				return rerr
			}
			if rel = filepath.ToSlash(rel); !seen[rel] {
				seen[rel] = true
				found = append(found, rel)
			}
			if d.IsDir() {
				return filepath.SkipDir
			}
			return nil
		})
		if werr != nil {
			return nil, werr
		}
	}
	sort.Strings(found)
	return found, nil
}

// PopulatedSubmodules returns the submodules checked out in the work tree at
// dir: gitlinks in its index whose directory holds a .git, plus "(its git dir's
// modules/)" when that directory exists. Either makes `git worktree remove`
// refuse without --force ("working trees containing submodules cannot be moved
// or removed", git's validate_no_submodules, measured on 2.39), so a removal
// can say so before it starts instead of failing halfway.
func PopulatedSubmodules(dir string) ([]string, error) {
	out, err := runRawReadOnly(dir, "ls-files", "-z", "-s")
	if err != nil {
		return nil, err
	}
	var subs []string
	for _, rec := range splitNUL(out) {
		meta, path, ok := strings.Cut(rec, "\t")
		if !ok || !strings.HasPrefix(meta, "160000 ") {
			continue
		}
		if _, err := os.Lstat(filepath.Join(dir, filepath.FromSlash(path), ".git")); err == nil {
			subs = append(subs, path)
		} else if !errors.Is(err, fs.ErrNotExist) {
			return nil, err
		}
	}
	gitDir, err := RunDir(dir, "rev-parse", "--absolute-git-dir")
	if err != nil {
		return nil, err
	}
	if fi, err := os.Stat(filepath.Join(gitDir, "modules")); err == nil && fi.IsDir() {
		subs = append(subs, "(its git dir's modules/)")
	}
	return subs, nil
}

// WorktreeRemoveIn is WorktreeRemove with git run in dir, not in the current
// directory (#177 review): `wt discard` runs it from the main checkout, which it
// never removes, so the command never runs inside the worktree it deletes.
func WorktreeRemoveIn(dir, path string, force bool) error {
	args := withUntrackedShown("worktree", "remove")
	if force {
		args = append(args, "--force")
	}
	_, err := runReporting(dir, append(args, path)...)
	return err
}

// BranchTipIn is BranchTip with git run in dir.
func BranchTipIn(dir, branch string) string {
	if branch == "" || branch == "HEAD" {
		return ""
	}
	out, err := RunDir(dir, "rev-parse", "--verify", "--quiet", "refs/heads/"+branch+"^{commit}")
	if err != nil {
		return ""
	}
	return strings.TrimSpace(out)
}

// RefTipsExcept returns the object of every ref in the repository at dir but
// the one named exactly except, and the HEAD commit of every worktree but the
// one at the path skipWorktree (#177 review): what keeps a commit reachable
// once that branch and that worktree are gone. Reflogs are not included.
func RefTipsExcept(dir, except, skipWorktree string) ([]string, error) {
	out, err := RunDir(dir, "for-each-ref", "--format=%(objectname) %(refname)")
	if err != nil {
		return nil, err
	}
	var tips []string
	for _, ln := range strings.Split(out, "\n") {
		if sha, ref, ok := strings.Cut(strings.TrimSpace(ln), " "); ok && sha != "" && ref != except {
			tips = append(tips, sha)
		}
	}
	wts, err := WorktreeListIn(dir)
	if err != nil {
		return nil, err
	}
	skip := resolvedPath(skipWorktree)
	for _, w := range wts {
		if w.Head != "" && (skipWorktree == "" || resolvedPath(w.Path) != skip) {
			tips = append(tips, w.Head)
		}
	}
	return tips, nil
}

// resolvedPath is p with its symlinks resolved when it exists (/var and
// /private/var on macOS name one directory), else p as given.
func resolvedPath(p string) string {
	if r, err := filepath.EvalSymlinks(p); err == nil {
		return r
	}
	return p
}

// DeleteBranchAt deletes local branch, and only while it still points at tip
// (#177): `wt discard` listed the branch's commits at tip, and a commit made
// since is not one the operator was shown. The delete is `git update-ref -d
// refs/heads/<branch> <tip>`, which checks the old value and deletes in one
// step (`git branch -D` deletes whatever the branch points at by then; a
// commit landing between a separate check and it was dropped, #177 review).
// Before it, branchBusy does `git branch -D`'s own check, which update-ref
// does not make: no worktree has the branch checked out, or is rebasing or
// bisecting it. After it, the branch's config section (upstream, description)
// goes, as `git branch -D` drops it; best-effort, a leftover section is
// harmless. git runs in dir. The error carries git's stderr.
func DeleteBranchAt(dir, branch, tip string) error {
	if branch == "" || tip == "" {
		return fmt.Errorf("delete needs a branch and its tip, got %q %q", branch, tip)
	}
	if where, err := branchBusy(dir, branch); err != nil {
		return fmt.Errorf("could not check that no worktree is using %s: %w", branch, err)
	} else if where != "" {
		return fmt.Errorf("%s is %s", branch, where)
	}
	if _, err := runReporting(dir, "update-ref", "-m", "wt discard", "-d", "refs/heads/"+branch, tip); err != nil {
		return err
	}
	_, _ = runReporting(dir, "config", "--remove-section", "branch."+branch)
	return nil
}

// branchBusy says where branch is in use in the repository at dir, "" when it
// is not, as `git branch -D` refuses it: checked out in a worktree (also as a
// case twin under core.ignorecase, one loose ref file, #167), or being rebased
// or bisected there, whose HEAD is detached meanwhile (rebase-merge/head-name,
// rebase-apply/head-name, BISECT_START in that worktree's git dir).
func branchBusy(dir, branch string) (string, error) {
	fold := ignoreCaseIn(dir)
	same := func(name string) bool {
		return name == branch || (fold && name != "" && strings.EqualFold(name, branch))
	}
	wts, err := WorktreeListIn(dir)
	if err != nil {
		return "", err
	}
	for _, w := range wts {
		if same(w.Branch) {
			return "checked out in " + w.Path, nil
		}
	}
	common, err := CommonDirIn(dir)
	if err != nil {
		return "", err
	}
	gitDirs := []string{common}
	if linked, err := filepath.Glob(filepath.Join(common, "worktrees", "*")); err == nil {
		gitDirs = append(gitDirs, linked...)
	}
	for _, gd := range gitDirs {
		for _, f := range []string{"rebase-merge/head-name", "rebase-apply/head-name", "BISECT_START"} {
			b, err := os.ReadFile(filepath.Join(gd, filepath.FromSlash(f)))
			if err != nil {
				continue
			}
			if name := strings.TrimPrefix(strings.TrimSpace(string(b)), "refs/heads/"); same(name) {
				return "being rebased or bisected in a worktree (" + gd + ")", nil
			}
		}
	}
	return "", nil
}

// ignoreCaseIn is IgnoreCase for the repository at dir.
func ignoreCaseIn(dir string) bool {
	out, err := RunDir(dir, "config", "--type=bool", "--get", "core.ignorecase")
	return err == nil && strings.TrimSpace(out) == "true"
}
