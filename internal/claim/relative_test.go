package claim

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// #238: a claim made under a relative worktree_root, before #238, recorded a
// relative Worktree path. `release --clean` reads it from the main checkout,
// as the root now is, so it finds the lane from a subdirectory too; read from
// the cwd, it was "already gone" and the lane stayed.
func TestRelease_CleanReadsARelativeRecordFromTheMainCheckout(t *testing.T) {
	f := newClaimFixture(t)
	if err := f.claim(true); err != nil {
		t.Fatalf("claim: %v", err)
	}
	rel, err := filepath.Rel(f.repo, f.dir())
	if err != nil || filepath.IsAbs(rel) {
		t.Fatalf("precondition: %q relative to %q: %q, %v", f.dir(), f.repo, rel, err)
	}
	b, err := os.ReadFile(f.c.ActiveWork)
	if err != nil {
		t.Fatal(err)
	}
	recorded := strings.Replace(string(b), "`"+f.dir()+"`", "`"+rel+"`", 1)
	if recorded == string(b) {
		t.Fatalf("precondition: active-work doesn't record %s:\n%s", f.dir(), b)
	}
	if err := os.WriteFile(f.c.ActiveWork, []byte(recorded), 0o644); err != nil {
		t.Fatal(err)
	}
	f.c.MainRoot = f.repo
	sub := filepath.Join(f.repo, "sub")
	if err := os.MkdirAll(sub, 0o755); err != nil {
		t.Fatal(err)
	}
	t.Chdir(sub)

	out, err := capturedRelease(t, f)
	if err != nil {
		t.Fatalf("Release: %v\n%s", err, out)
	}
	if _, serr := os.Stat(f.dir()); serr == nil {
		t.Errorf("the lane was kept (read from the cwd?)\n%s", out)
	}
	if strings.Contains(out, "already gone") {
		t.Errorf("release read the relative record from the cwd:\n%s", out)
	}
}

// recordRelative rewrites the claim's active-work Worktree to rel, as a claim
// made before #238 under a relative worktree_root recorded it.
func recordRelative(t *testing.T, f *claimFixture, rel string) {
	t.Helper()
	b, err := os.ReadFile(f.c.ActiveWork)
	if err != nil {
		t.Fatal(err)
	}
	recorded := strings.Replace(string(b), "`"+f.dir()+"`", "`"+rel+"`", 1)
	if recorded == string(b) {
		t.Fatalf("precondition: active-work doesn't record %s:\n%s", f.dir(), b)
	}
	if err := os.WriteFile(f.c.ActiveWork, []byte(recorded), 0o644); err != nil {
		t.Fatal(err)
	}
	f.c.MainRoot = f.repo
}

// #238 re-review: when the branch lookup misses (the lane detached, mid
// rebase), the record is read from the main checkout, where a decoy worktree
// on another branch can sit. --clean removes only a worktree on the claim's
// branch, so the decoy stays, ignored files and all.
func TestRelease_CleanKeepsADecoyWhenTheLookupMisses(t *testing.T) {
	f := newClaimFixture(t)
	if err := f.claim(true); err != nil {
		t.Fatalf("claim: %v", err)
	}
	f.git(f.dir(), "switch", "-q", "--detach")
	rel := filepath.Join("wts", claimBranch)
	decoy := filepath.Join(f.repo, rel)
	f.git(f.repo, "worktree", "add", "-q", "-b", "decoy-work", decoy)
	if err := os.WriteFile(filepath.Join(decoy, ".gitignore"), []byte("cache/\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	f.git(decoy, "add", ".gitignore")
	f.git(decoy, "commit", "-qm", "ignore cache")
	recordRelative(t, f, rel)

	out, err := capturedRelease(t, f)
	if err != nil {
		t.Fatalf("Release: %v\n%s", err, out)
	}
	if _, serr := os.Stat(decoy); serr != nil {
		t.Errorf("the decoy worktree on another branch was removed\n%s", out)
	}
	if !strings.Contains(out, "is not on "+claimBranch) {
		t.Errorf("release said:\n%s\nwant it to refuse the decoy as not on the claim's branch", out)
	}
}

// The branch lookup never returns the main checkout (#238 re-review): a
// claim's lane is never the main worktree, and with the claim's branch checked
// out there only git's own refusal stood between it and the removal.
func TestRelease_CleanNeverPicksTheMainCheckout(t *testing.T) {
	f := newClaimFixture(t)
	if err := f.claim(true); err != nil {
		t.Fatalf("claim: %v", err)
	}
	f.git(f.repo, "worktree", "remove", f.dir())
	f.git(f.repo, "switch", "-q", claimBranch)
	// Another lane, as a real repo has: the lookup reads the whole list.
	f.git(f.repo, "worktree", "add", "-q", "-b", "other-work", filepath.Join(t.TempDir(), "other"))
	recordRelative(t, f, filepath.Join("wts", claimBranch))

	out, err := capturedRelease(t, f)
	if err != nil {
		t.Fatalf("Release: %v\n%s", err, out)
	}
	if strings.Contains(out, f.repo+" ") || strings.Contains(out, "couldn't remove worktree") {
		t.Errorf("release went for the main checkout:\n%s", out)
	}
	if !strings.Contains(out, "already gone") {
		t.Errorf("release said:\n%s\nwant the lane read as already gone", out)
	}
}

// With no active-work file where it looks, release says where it looked
// (#238 re-review: a relative active_work used to be a file per cwd, and an
// entry recorded in another one was skipped without a word).
func TestRelease_SaysWhereItLookedForActiveWork(t *testing.T) {
	f := newClaimFixture(t)
	if err := f.claim(true); err != nil {
		t.Fatalf("claim: %v", err)
	}
	if err := os.Remove(f.c.ActiveWork); err != nil {
		t.Fatal(err)
	}
	out, err := capturedRelease(t, f)
	if err != nil {
		t.Fatalf("Release: %v\n%s", err, out)
	}
	if !strings.Contains(out, "no active-work file at "+f.c.ActiveWork) {
		t.Errorf("release said:\n%s\nwant it to name the active-work file it looked for", out)
	}
}

// #238 review: a relative record was relative to whatever cwd the claim ran
// in, so read from the main checkout it could name a DIFFERENT worktree that
// happens to sit there (a decoy on another branch), and release removed that.
// The lane is found by the claim's branch instead.
func TestRelease_CleanFindsARelativeRecordsLaneByItsBranch(t *testing.T) {
	f := newClaimFixture(t)
	if err := f.claim(true); err != nil {
		t.Fatalf("claim: %v", err)
	}
	// The record says wts/<lane>, as a claim made in a subdirectory with
	// worktree_root = wts wrote it; read from the main checkout that path is
	// a decoy worktree on another branch.
	rel := filepath.Join("wts", claimBranch)
	decoy := filepath.Join(f.repo, rel)
	f.git(f.repo, "worktree", "add", "-q", "-b", "decoy-work", decoy)
	b, err := os.ReadFile(f.c.ActiveWork)
	if err != nil {
		t.Fatal(err)
	}
	recorded := strings.Replace(string(b), "`"+f.dir()+"`", "`"+rel+"`", 1)
	if recorded == string(b) {
		t.Fatalf("precondition: active-work doesn't record %s:\n%s", f.dir(), b)
	}
	if err := os.WriteFile(f.c.ActiveWork, []byte(recorded), 0o644); err != nil {
		t.Fatal(err)
	}
	f.c.MainRoot = f.repo

	out, err := capturedRelease(t, f)
	if err != nil {
		t.Fatalf("Release: %v\n%s", err, out)
	}
	if _, serr := os.Stat(decoy); serr != nil {
		t.Errorf("the decoy worktree on another branch was removed\n%s", out)
	}
	if _, serr := os.Stat(f.dir()); serr == nil {
		t.Errorf("the claim's own lane was kept\n%s", out)
	}
}
