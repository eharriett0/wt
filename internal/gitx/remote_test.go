package gitx

import (
	"strings"
	"testing"
)

// #159: release --clean must delete the remote placeholder branch that claim
// pushed, or re-claiming the same issue pushes a fresh divergent placeholder and
// is rejected non-fast-forward. This pins the mechanism: the rejection happens,
// and DeleteRemoteBranch clears it so the re-push succeeds.
func TestDeleteRemoteBranch(t *testing.T) {
	remote := t.TempDir()
	runGit(t, remote, "init", "-q", "--bare", "-b", "main")

	repo := gitRepo(t) // an init commit on main
	runGit(t, repo, "remote", "add", "origin", remote)
	runGit(t, repo, "push", "-q", "-u", "origin", "main")

	// claim-style: a placeholder branch off main, pushed to origin
	runGit(t, repo, "switch", "-qc", "feat-1")
	runGit(t, repo, "commit", "--allow-empty", "-qm", "WIP: claim #1")
	if err := PushSetUpstream(repo, "feat-1"); err != nil {
		t.Fatalf("initial push: %v", err)
	}

	// re-claim after a local-only clean: a fresh divergent placeholder on the SAME
	// branch name is rejected while the remote still holds the old one.
	runGit(t, repo, "switch", "-q", "main")
	runGit(t, repo, "branch", "-qD", "feat-1")
	runGit(t, repo, "switch", "-qc", "feat-1")
	runGit(t, repo, "commit", "--allow-empty", "-qm", "WIP: claim #1 (again)")
	if err := PushSetUpstream(repo, "feat-1"); err == nil {
		t.Fatal("expected non-fast-forward rejection re-pushing a divergent branch (the #159 bug)")
	}

	// the fix: delete the remote branch, then the re-push succeeds cleanly
	if err := DeleteRemoteBranch(repo, "feat-1"); err != nil {
		t.Fatalf("DeleteRemoteBranch: %v", err)
	}
	if out, _ := RunDir(repo, "ls-remote", "--heads", "origin", "feat-1"); strings.TrimSpace(out) != "" {
		t.Errorf("remote feat-1 still present after delete: %q", out)
	}
	if err := PushSetUpstream(repo, "feat-1"); err != nil {
		t.Fatalf("re-push after remote delete should succeed: %v", err)
	}

	// deleting an already-absent remote branch is an error the caller treats as
	// best-effort (release --clean logs and moves on).
	runGit(t, repo, "push", "-q", "origin", "--delete", "feat-1")
	if err := DeleteRemoteBranch(repo, "feat-1"); err == nil {
		t.Error("deleting an absent remote branch should error (caller treats best-effort)")
	}
}
