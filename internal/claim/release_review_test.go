package claim

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// #230 review: `wt release 42 --clean` run INSIDE the claim's worktree (c.Root
// is then that worktree) removed it, and then read origin from the removed
// directory. The read failed, was taken for "origin's branch is already gone",
// and origin's placeholder stayed: the #159 state, where a re-claim of the
// issue is refused. The remote is read from the main checkout, resolved
// before the removal, and a failed read is reported.
func TestRelease_CleanFromInsideTheLane(t *testing.T) {
	for _, inside := range []bool{false, true} {
		name := map[bool]string{false: "control: from the main checkout", true: "from inside the lane"}[inside]
		t.Run(name, func(t *testing.T) {
			f := newClaimFixture(t)
			if err := f.claim(true); err != nil {
				t.Fatalf("claim: %v", err)
			}
			if inside {
				t.Chdir(f.dir())
				f.c.Root = f.dir() // what config.Load reads there
			}
			out, err := capturedRelease(t, f)
			if err != nil {
				t.Fatalf("Release: %v\n%s", err, out)
			}
			_, statErr := os.Stat(f.dir())
			for what, kept := range map[string]bool{
				"the worktree":         statErr == nil,
				"the local branch":     f.ref(f.repo, "refs/heads/"+claimBranch) != "",
				"origin's placeholder": f.ref(f.origin, "refs/heads/"+claimBranch) != "",
			} {
				if kept {
					t.Errorf("%s kept\n%s", what, out)
				}
			}
			if !strings.Contains(out, "--clean: deleted remote branch origin/"+claimBranch) {
				t.Errorf("release said:\n%s\nwant origin's placeholder deleted", out)
			}
		})
	}
}

// #230 re-review: a tag with the branch's name turned --abbrev-ref's answer
// into "heads/<branch>", and release refused the claim's own lane. The full ref
// is compared.
func TestRelease_CleanWithATagOfTheBranchsName(t *testing.T) {
	f := newClaimFixture(t)
	if err := f.claim(true); err != nil {
		t.Fatalf("claim: %v", err)
	}
	f.git(f.repo, "tag", claimBranch)
	out, err := capturedRelease(t, f)
	if err != nil {
		t.Fatalf("Release: %v\n%s", err, out)
	}
	if _, serr := os.Stat(f.dir()); serr == nil {
		t.Errorf("the lane was kept\n%s", out)
	}
}

// The claim rollback deletes the branch it cut only at the commit it left
// there (#230 re-review): a commit landing before the rollback keeps the
// branch. origin's hook makes that commit in the lane, then rejects the push.
func TestClaim_RollbackKeepsABranchACommitLandedOn(t *testing.T) {
	f := newClaimFixture(t)
	hook := filepath.Join(f.origin, "hooks", "pre-receive")
	script := "#!/bin/sh\nenv -u GIT_DIR -u GIT_OBJECT_DIRECTORY -u GIT_ALTERNATE_OBJECT_DIRECTORIES -u GIT_QUARANTINE_PATH " +
		"git -C '" + f.dir() + "' commit -q --allow-empty -m 'landed during the claim'\necho rejected by the test >&2\nexit 1\n"
	if err := os.WriteFile(hook, []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := f.claim(true); err == nil {
		t.Fatal("Claim succeeded although origin rejects every push")
	}
	tip := f.ref(f.repo, "refs/heads/"+claimBranch)
	if tip == "" {
		t.Fatal("the branch was deleted with the commit that landed on it")
	}
	if subj := f.git(f.repo, "log", "-1", "--format=%s", tip); subj != "landed during the claim" {
		t.Errorf("branch at %q (%s), want it kept at the commit that landed", subj, tip)
	}
}

// ...and when the placeholder commit itself fails, the branch is still at
// the commit it was cut at, and goes.
func TestClaim_RollbackAfterAFailedPlaceholderCommit(t *testing.T) {
	f := newClaimFixture(t)
	hook := filepath.Join(f.repo, ".git", "hooks", "commit-msg")
	if err := os.WriteFile(hook, []byte("#!/bin/sh\necho no placeholder >&2\nexit 1\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := f.claim(true); err == nil {
		t.Fatal("Claim succeeded although the placeholder commit is refused")
	}
	if got := f.ref(f.repo, "refs/heads/"+claimBranch); got != "" {
		t.Errorf("the branch the claim cut is still at %s; the rollback deletes it", got)
	}
	if _, serr := os.Stat(f.dir()); !os.IsNotExist(serr) {
		t.Errorf("the worktree the claim made must be removed (stat: %v)", serr)
	}
}

// #230 review: release --clean proves the CLAIM's branch abandoned, so a lane
// switched to other work is not its to remove: the removal took that work's
// ignored files.
func TestRelease_CleanKeepsALaneSwitchedToOtherWork(t *testing.T) {
	f := newClaimFixture(t)
	if err := f.claim(true); err != nil {
		t.Fatalf("claim: %v", err)
	}
	f.git(f.dir(), "switch", "-q", "-c", "real-work")
	f.git(f.dir(), "commit", "-q", "--allow-empty", "-m", "real work")
	out, err := capturedRelease(t, f)
	if err != nil {
		t.Fatalf("Release: %v\n%s", err, out)
	}
	if _, serr := os.Stat(f.dir()); serr != nil {
		t.Errorf("the lane was removed: %v\n%s", serr, out)
	}
	if !strings.Contains(out, "is not on "+claimBranch) {
		t.Errorf("release said:\n%s\nwant it to say the lane is on another branch", out)
	}
	if f.ref(f.origin, "refs/heads/"+claimBranch) == "" {
		t.Errorf("origin's placeholder was deleted\n%s", out)
	}
}
