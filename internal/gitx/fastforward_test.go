package gitx

import (
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

func TestParseLeftRight(t *testing.T) {
	cases := []struct {
		out  string
		l, r int
		ok   bool
	}{
		{"3\t7", 3, 7, true},
		{"0\t0\n", 0, 0, true},
		{"", 0, 0, false},
		{"3", 0, 0, false},
		{"x\t1", 0, 0, false},
		{"1\ty", 0, 0, false},
		{"1\t2\t3", 0, 0, false},
	}
	for _, c := range cases {
		l, r, err := parseLeftRight(c.out)
		if (err == nil) != c.ok || l != c.l || r != c.r {
			t.Errorf("parseLeftRight(%q) = (%d, %d, %v), want (%d, %d, ok=%v)", c.out, l, r, err, c.l, c.r, c.ok)
		}
	}
}

// #167: `wt adopt` fast-forwards a stale-but-behind local branch to the PR head.
// FastForwardBranch must only ever move a branch FORWARD, and only from the
// commit the caller saw: backwards, sideways, and a stale old value all leave
// the branch where it is.
func TestFastForwardBranch(t *testing.T) {
	dir := gitRepo(t)
	t.Chdir(dir) // FastForwardBranch runs git in cwd
	base := gitOut(t, dir, "rev-parse", "HEAD")
	runGit(t, dir, "branch", "feat", base)
	runGit(t, dir, "commit", "--allow-empty", "-qm", "two")
	two := gitOut(t, dir, "rev-parse", "HEAD")
	runGit(t, dir, "commit", "--allow-empty", "-qm", "three")
	three := gitOut(t, dir, "rev-parse", "HEAD")
	runGit(t, dir, "switch", "-qc", "side", base)
	runGit(t, dir, "commit", "--allow-empty", "-qm", "side")
	side := gitOut(t, dir, "rev-parse", "HEAD")
	runGit(t, dir, "switch", "-q", "main")

	if err := FastForwardBranch("feat", base, two); err != nil {
		t.Fatalf("forward move refused: %v", err)
	}
	if got := BranchTip("feat"); got != two {
		t.Fatalf("feat = %s after a fast-forward, want %s", got, two)
	}
	for _, c := range []struct{ name, from, to string }{
		{"backwards", two, base},
		{"sideways (diverged)", two, side},
		{"stale old value (the branch is not at from any more)", base, three},
		{"missing commit", two, strings.Repeat("1", 40)},
		{"empty", "", two},
	} {
		if err := FastForwardBranch("feat", c.from, c.to); err == nil {
			t.Errorf("%s: FastForwardBranch succeeded, want a refusal", c.name)
		}
		if got := BranchTip("feat"); got != two {
			t.Fatalf("%s: feat moved to %s, want it left at %s", c.name, got, two)
		}
	}
}

// #167 review: FastForwardBranch must not move a branch another worktree is
// using. The mid-rebase case is the one `git worktree list` cannot see (the
// rebasing worktree's HEAD is detached), and an update-ref moved the branch
// underneath the rebase; git branch -f refuses it.
func TestFastForwardBranch_RefusesABranchAnotherWorktreeIsUsing(t *testing.T) {
	dir := gitRepo(t)
	t.Chdir(dir)
	writeFile(t, dir, "f", "base\n")
	runGit(t, dir, "add", "f")
	runGit(t, dir, "commit", "-qm", "base")
	runGit(t, dir, "switch", "-qc", "b")
	writeFile(t, dir, "f", "on b\n")
	runGit(t, dir, "commit", "-qam", "b edits f")
	old := gitOut(t, dir, "rev-parse", "b")
	runGit(t, dir, "switch", "-qc", "newer")
	runGit(t, dir, "commit", "--allow-empty", "-qm", "newer")
	newer := gitOut(t, dir, "rev-parse", "newer")
	runGit(t, dir, "switch", "-q", "main")
	writeFile(t, dir, "f", "on main\n")
	runGit(t, dir, "commit", "-qam", "main edits f") // so rebasing b onto main stops on a conflict

	wt := filepath.Join(t.TempDir(), "wt-b")
	runGit(t, dir, "worktree", "add", "-q", wt, "b")
	if err := FastForwardBranch("b", old, newer); err == nil {
		t.Error("checked out in another worktree: FastForwardBranch moved it")
	}
	if got := BranchTip("b"); got != old {
		t.Fatalf("checked out: b moved to %s, want it left at %s", got, old)
	}

	rebase := exec.Command("git", "rebase", "main")
	rebase.Dir = wt
	if out, err := rebase.CombinedOutput(); err == nil {
		t.Fatalf("expected the rebase to stop on a conflict, got success:\n%s", out)
	}
	if got := gitOut(t, wt, "rev-parse", "--abbrev-ref", "HEAD"); got != "HEAD" {
		t.Fatalf("mid-rebase worktree should be detached, is on %q", got)
	}
	if err := FastForwardBranch("b", old, newer); err == nil {
		t.Error("mid-rebase in another worktree: FastForwardBranch moved it")
	}
	if got := BranchTip("b"); got != old {
		t.Errorf("mid-rebase: b moved to %s, want it left at %s", got, old)
	}
}

// The read side of the #167 refusal: where a local branch stands against the
// target, and which commits only it has.
func TestAheadBehindAndOnelineLog(t *testing.T) {
	dir := gitRepo(t)
	t.Chdir(dir)
	runGit(t, dir, "switch", "-qc", "stale")
	for _, m := range []string{"old 1", "old 2", "old 3"} {
		runGit(t, dir, "commit", "--allow-empty", "-qm", m)
	}
	runGit(t, dir, "switch", "-q", "main")
	runGit(t, dir, "commit", "--allow-empty", "-qm", "new 1")
	runGit(t, dir, "commit", "--allow-empty", "-qm", "new 2")

	ahead, behind, err := AheadBehind("stale", "main")
	if err != nil || ahead != 3 || behind != 2 {
		t.Errorf("AheadBehind(stale, main) = (%d, %d, %v), want (3, 2, nil)", ahead, behind, err)
	}
	lines, err := OnelineLog("main", "stale", 2)
	if err != nil || len(lines) != 2 || !strings.HasSuffix(lines[0], " old 3") || !strings.HasSuffix(lines[1], " old 2") {
		t.Errorf("OnelineLog(main, stale, 2) = (%q, %v), want the two newest stale-only commits", lines, err)
	}
	if got := HeadCommit(dir); got != gitOut(t, dir, "rev-parse", "main") {
		t.Errorf("HeadCommit = %q, want main's tip", got)
	}
	if got := HeadCommit(t.TempDir()); got != "" {
		t.Errorf("HeadCommit outside a repo = %q, want empty", got)
	}
}

// RemoteTrackingTip reads origin/<branch> as last fetched, and "" for no ref.
func TestRemoteTrackingTip(t *testing.T) {
	remote := t.TempDir()
	runGit(t, remote, "init", "-q", "--bare", "-b", "main")
	repo := gitRepo(t)
	runGit(t, repo, "remote", "add", "origin", remote)
	runGit(t, repo, "push", "-q", "-u", "origin", "main")
	t.Chdir(repo)
	if got, want := RemoteTrackingTip("main"), gitOut(t, repo, "rev-parse", "origin/main"); got != want {
		t.Errorf("RemoteTrackingTip(main) = %q, want %q", got, want)
	}
	if got := RemoteTrackingTip("nope"); got != "" {
		t.Errorf("RemoteTrackingTip(nope) = %q, want empty", got)
	}
	if got := RemoteTrackingTip(""); got != "" {
		t.Errorf("RemoteTrackingTip(\"\") = %q, want empty", got)
	}
}
