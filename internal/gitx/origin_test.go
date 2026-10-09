package gitx

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// #198 review: in a single-branch clone (`--single-branch`, which every
// `--depth` clone is) `git fetch origin <branch>` fetched into FETCH_HEAD only,
// so origin/<branch> never existed and a stale local branch read as never
// pushed. Fetch spells the refspec out, so the remote-tracking ref is written.
func TestFetch_WritesTheTrackingRefInASingleBranchClone(t *testing.T) {
	origin := t.TempDir()
	runGit(t, origin, "init", "-q", "--bare", "-b", "main")
	seed := gitRepo(t)
	runGit(t, seed, "remote", "add", "origin", origin)
	runGit(t, seed, "push", "-q", "origin", "main")
	runGit(t, seed, "switch", "-qc", "feat/x")
	runGit(t, seed, "commit", "--allow-empty", "-qm", "x")
	runGit(t, seed, "push", "-q", "origin", "feat/x")
	want := gitOut(t, seed, "rev-parse", "HEAD")

	clone := filepath.Join(t.TempDir(), "clone")
	runGit(t, t.TempDir(), "clone", "-q", "--single-branch", "-b", "main", origin, clone)
	t.Chdir(clone)
	if err := Fetch("origin", "feat/x"); err != nil {
		t.Fatalf("Fetch: %v", err)
	}
	if got := RemoteTrackingTip("feat/x"); got != want {
		t.Errorf("origin/feat/x after Fetch = %q, want %s", got, want)
	}

	// A branch origin lacks fails, and the error says why instead of a bare exit status.
	err := Fetch("origin", "nope")
	if err == nil || !strings.Contains(err.Error(), "nope") {
		t.Errorf("Fetch of a missing branch = %v, want git's own reason", err)
	}
}

// RemoteTrackingTip answers for a ref of EXACTLY that name (#198 review): not one
// below it (a for-each-ref pattern matches refs/remotes/origin/feat/x for
// "feat"), and not one differing only in case, which a case-insensitive
// filesystem resolves to the same loose file.
func TestRemoteTrackingTip_MatchesTheNameExactly(t *testing.T) {
	dir := gitRepo(t)
	t.Chdir(dir)
	head := gitOut(t, dir, "rev-parse", "HEAD")
	runGit(t, dir, "update-ref", "refs/remotes/origin/feat/x", head)
	runGit(t, dir, "update-ref", "refs/remotes/origin/Other/y", head)
	if got := RemoteTrackingTip("feat"); got != "" {
		t.Errorf("RemoteTrackingTip(feat) = %q, want empty: only feat/x exists", got)
	}
	if got := RemoteTrackingTip("feat/x"); got != head {
		t.Errorf("RemoteTrackingTip(feat/x) = %q, want %s", got, head)
	}
	if got := RemoteTrackingTip("other/y"); got != "" {
		t.Errorf("RemoteTrackingTip(other/y) = %q, want empty: only Other/y exists", got)
	}
}

func TestPickExactRef(t *testing.T) {
	out := "aaa commit refs/remotes/origin/feat/x\nbbb commit refs/remotes/origin/feat/x/sub\nccc tag refs/remotes/origin/t\n"
	cases := []struct{ ref, want string }{
		{"refs/remotes/origin/feat/x", "aaa"},
		{"refs/remotes/origin/feat/x/sub", "bbb"},
		{"refs/remotes/origin/feat", ""},
		{"refs/remotes/origin/Feat/x", ""},
		{"refs/remotes/origin/t", ""}, // not a commit
	}
	for _, c := range cases {
		if got := pickExactRef(out, c.ref); got != c.want {
			t.Errorf("pickExactRef(%s) = %q, want %q", c.ref, got, c.want)
		}
	}
}

// ls-remote matches a pattern against the tail of a name, so the first line it
// prints need not be the branch asked about (#198 review). Pure.
func TestPickLsRemote(t *testing.T) {
	out := "aaa\trefs/remotes/x/refs/heads/feat\nbbb\trefs/heads/feat\n"
	if got := pickLsRemote(out, "refs/heads/feat"); got != "bbb" {
		t.Errorf("pickLsRemote = %q, want bbb", got)
	}
	if got := pickLsRemote(out, "refs/heads/nope"); got != "" {
		t.Errorf("pickLsRemote(nope) = %q, want empty", got)
	}
	if got := pickLsRemote("", "refs/heads/feat"); got != "" {
		t.Errorf("pickLsRemote(empty) = %q, want empty", got)
	}
}

func TestGitStderr(t *testing.T) {
	in := "To /o.git\n ! [rejected]        feat -> feat (fetch first)\nerror: failed to push some refs to '/o.git'\nhint: Updates were rejected because the remote contains work that you do not\nhint: have locally.\n\n"
	want := "To /o.git; ! [rejected]        feat -> feat (fetch first); error: failed to push some refs to '/o.git'"
	if got := gitStderr(in); got != want {
		t.Errorf("gitStderr = %q, want %q", got, want)
	}
	if got := gitStderr("\nhint: x\n"); got != "" {
		t.Errorf("gitStderr(hints only) = %q, want empty", got)
	}
}

// A rejected push says why (#198 review): it used to be "exit status 1".
func TestPushSetUpstream_ErrorCarriesGitsReason(t *testing.T) {
	origin := t.TempDir()
	runGit(t, origin, "init", "-q", "--bare", "-b", "main")
	if err := os.WriteFile(filepath.Join(origin, "hooks", "pre-receive"), []byte("#!/bin/sh\necho no pushes today >&2\nexit 1\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	repo := gitRepo(t)
	runGit(t, repo, "remote", "add", "origin", origin)
	err := PushSetUpstream(repo, "main")
	if err == nil || !strings.Contains(err.Error(), "no pushes today") {
		t.Errorf("PushSetUpstream = %v, want the remote's reason in it", err)
	}
}

// WorktreeToplevel is the top of the checkout holding dir, which is how wt
// tells its worktree from a directory nested inside the main checkout (#198
// review).
func TestWorktreeToplevel(t *testing.T) {
	dir := gitRepo(t)
	nested := filepath.Join(dir, ".worktrees", "feat-x")
	if err := os.MkdirAll(nested, 0o755); err != nil {
		t.Fatal(err)
	}
	real, err := filepath.EvalSymlinks(dir)
	if err != nil {
		t.Fatal(err)
	}
	for _, d := range []string{dir, nested} {
		got, err := filepath.EvalSymlinks(WorktreeToplevel(d))
		if err != nil || got != real {
			t.Errorf("WorktreeToplevel(%s) = %q (%v), want %s", d, got, err, real)
		}
	}
	if got := WorktreeToplevel(t.TempDir()); got != "" {
		t.Errorf("WorktreeToplevel outside a repo = %q, want empty", got)
	}
}

// BranchUpstream reads the upstream config with no worktree on the branch, and
// still after origin deleted it (#198 review).
func TestBranchUpstream(t *testing.T) {
	dir := gitRepo(t)
	t.Chdir(dir)
	runGit(t, dir, "branch", "feat/x")
	if r, m := BranchUpstream("feat/x"); r != "" || m != "" {
		t.Errorf("BranchUpstream(feat/x) with none set = (%q, %q), want empty", r, m)
	}
	runGit(t, dir, "config", "branch.feat/x.remote", "origin")
	runGit(t, dir, "config", "branch.feat/x.merge", "refs/heads/feat/x")
	if r, m := BranchUpstream("feat/x"); r != "origin" || m != "refs/heads/feat/x" {
		t.Errorf("BranchUpstream(feat/x) = (%q, %q), want (origin, refs/heads/feat/x)", r, m)
	}
}
