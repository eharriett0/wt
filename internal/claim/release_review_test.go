package claim

import (
	"os"
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
