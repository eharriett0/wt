package claim

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// #208: `wt release <issue> --clean` removes the claim's worktree only when it
// is clean and abandoned (WIP-only, no live PR). With status.showUntrackedFiles=no
// a worktree holding only untracked work read as clean, and git's own check let
// `git worktree remove` delete the files, then the branch and the pushed
// placeholder went too. Release runs here on a real claim, with the fake gh.
func TestRelease_CleanKeepsUntrackedFilesTheConfigHides(t *testing.T) {
	for _, tc := range []struct {
		name, file string // file: written into the claim's worktree before the release
		kept       bool
	}{
		{"control: nothing uncommitted, the abandoned claim is removed", "", false},
		{"an untracked file the config hides: kept, file and all", "notes.txt", true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f := newClaimFixture(t)
			if err := f.claim(true); err != nil {
				t.Fatalf("claim: %v", err)
			}
			f.git(f.repo, "config", "status.showUntrackedFiles", "no")
			file := filepath.Join(f.dir(), tc.file)
			if tc.file != "" {
				if err := os.WriteFile(file, []byte("not committed\n"), 0o644); err != nil {
					t.Fatal(err)
				}
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
				if kept != tc.kept {
					t.Fatalf("%s kept=%v, want %v\n%s", what, kept, tc.kept, out)
				}
			}
			if !tc.kept {
				return
			}
			if _, err := os.Stat(file); err != nil {
				t.Errorf("the untracked file is gone: %v", err)
			}
			// release's own check refuses, before git is asked to remove anything.
			if want := "--clean: worktree " + f.dir() + " has uncommitted changes — left in place"; !strings.Contains(out, want) {
				t.Errorf("release said:\n%s\nwant %q", out, want)
			}
		})
	}
}

// capturedRelease runs `wt release 42 --clean` with stdout and stderr going to
// a file, and returns them.
func capturedRelease(t *testing.T, f *claimFixture) (string, error) {
	t.Helper()
	out, err := os.Create(filepath.Join(t.TempDir(), "out"))
	if err != nil {
		t.Fatal(err)
	}
	stdout, stderr := os.Stdout, os.Stderr
	os.Stdout, os.Stderr = out, out
	rerr := Release(f.c, claimIssue, true)
	os.Stdout, os.Stderr = stdout, stderr
	out.Close()
	b, _ := os.ReadFile(out.Name())
	return string(b), rerr
}
