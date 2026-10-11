package doctor

import (
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"testing"

	"github.com/eharriett0/wt/internal/collide"
	"github.com/eharriett0/wt/internal/config"
)

// #208: doctor's stale-base-checkout report reads cleanliness through IsClean
// and counts through the same reader, so a far-behind base checkout whose only
// uncommitted work is an untracked file the config hides is reported, with that
// file counted. A plain porcelain status read it as clean and said nothing.
func TestStaleCheckouts_CountsUntrackedFilesTheConfigHides(t *testing.T) {
	t.Setenv("GIT_CONFIG_GLOBAL", os.DevNull)
	t.Setenv("GIT_CONFIG_NOSYSTEM", "1")
	for _, k := range []string{"GIT_AUTHOR_NAME", "GIT_COMMITTER_NAME", "GIT_AUTHOR_EMAIL", "GIT_COMMITTER_EMAIL"} {
		t.Setenv(k, "t@t.test")
	}
	git := func(dir string, args ...string) string {
		t.Helper()
		cmd := exec.Command("git", args...)
		cmd.Dir = dir
		out, err := cmd.CombinedOutput()
		if err != nil {
			t.Fatalf("git %v: %v\n%s", args, err, out)
		}
		return strings.TrimSpace(string(out))
	}
	tmp := t.TempDir()
	origin, repo := filepath.Join(tmp, "origin.git"), filepath.Join(tmp, "repo")
	git(tmp, "init", "-q", "--bare", "-b", "main", origin)
	git(tmp, "init", "-q", "-b", "main", repo)
	git(repo, "remote", "add", "origin", origin)
	git(repo, "commit", "-q", "--allow-empty", "-m", "base")
	git(repo, "push", "-q", "origin", "main")
	tip, tree := git(repo, "rev-parse", "HEAD"), git(repo, "rev-parse", "HEAD^{tree}")
	for i := 0; i < collide.StaleBaseBehindThreshold; i++ { // origin's main moves on
		tip = git(origin, "commit-tree", "-p", tip, "-m", "main "+strconv.Itoa(i), tree)
	}
	git(origin, "update-ref", "refs/heads/main", tip)
	git(repo, "fetch", "-q", "origin")
	git(repo, "config", "status.showUntrackedFiles", "no")
	if err := os.WriteFile(filepath.Join(repo, "notes.txt"), []byte("not committed\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	t.Chdir(repo)

	got := staleCheckouts(&config.Config{Root: repo, Base: "main"})
	if len(got) != 1 || got[0].DirtyFiles != 1 || got[0].BehindBase != collide.StaleBaseBehindThreshold {
		t.Fatalf("staleCheckouts = %+v, want the base checkout reported with 1 dirty file, %d behind", got, collide.StaleBaseBehindThreshold)
	}
}
