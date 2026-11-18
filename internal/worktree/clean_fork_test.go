package worktree

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// #243: clean's #187 check fetches refs/pull/<n>/head when the PR's head is
// not in this clone. It fetched from origin, always: in a fork clone whose gh
// default repository is upstream, the number is upstream's PR, and PR n on
// origin is another PR, so the head never arrived and the lane was kept on
// every run, with a hint naming the wrong PR. Now clean hands the check
// another repository's PR by its URL, and the fetch goes to that repository
// (merge.prRemote): a configured remote naming it first (the user's own
// transport: an ssh-only user has no https credentials), else its URL, then
// origin (#243 review: origin can be the PR's repository under the old name
// of a renamed one, which the URL names by its new name).
//
// Real git end to end: origin is github.com/me/repo and a configured
// upstream github.com/up/repo, both reached through a fake ssh; https URLs
// through url.insteadOf, every github.com one to a local path (a catch-all
// sends the rest nowhere, so no case reaches the network). origin can't use
// insteadOf: git expands it in `remote get-url`, so wt would read a local path
// and never tell the fork apart. The lane's tip is an ancestor of the PR's
// head, which only the PR's own repository holds.
func TestClean_ForkCloneFetchesThePRHeadFromThePRsRepository(t *testing.T) {
	for _, tc := range []struct {
		name     string
		url      string // what gh gives for PR 5's URL; "" when it can't
		onOrigin bool   // origin holds the PR's head (else upstream does)
		remote   bool   // upstream is a configured remote, over ssh
		mirror   bool   // aghe, a same-path mirror on another host with its own PR 5
		https    bool   // upstream's https URL can be fetched
		reaped   bool
		says     string // in the kept lane's warning
	}{
		{"upstream's PR, over https", "https://github.com/up/repo/pull/5", false, false, false, true, true, ""},
		{"upstream's PR, from the configured remote (ssh only)", "https://github.com/up/repo/pull/5", false, true, false, false, true, ""},
		{"upstream's PR, no remote and no https: kept, naming its repository", "https://github.com/up/repo/pull/5", false, false, false, false, false,
			"not in PR https://github.com/up/repo/pull/5: wt could not compare its tip with the PR's head"},
		{"a same-path mirror sorts first, but upstream on the PR's host is asked first", "https://github.com/up/repo/pull/5", false, true, true, false, true, ""},
		{"only a mirror: its own PR 5 arrives, so the URL is asked next", "https://github.com/up/repo/pull/5", false, false, true, true, true, ""},
		{"origin under a renamed repository's old name: origin, after the URL", "https://github.com/me/newname/pull/5", true, false, false, false, true, ""},
		{"control: origin's own PR", "https://github.com/me/repo/pull/5", true, false, false, false, true, ""},
		{"no URL from gh: origin, as before, and the lane is kept", "", false, false, false, true, false,
			"not in PR #5: wt could not compare its tip with the PR's head"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f := newAdoptFixture(t)
			dir, tip := f.mergedLane()
			tmp := t.TempDir()

			// The PR was updated on GitHub after this clone last fetched, so its
			// head is a commit only the PR's repository has.
			up := filepath.Join(tmp, "up.git")
			gitW(t, tmp, "init", "-q", "--bare", "-b", "main", up)
			gitW(t, f.repo, "push", "-q", up, "feat-x")
			prRepo := up
			if tc.onOrigin {
				prRepo = f.origin
			}
			head := f.commitIn(prRepo, tip, "updated on GitHub")
			gitW(t, prRepo, "update-ref", "refs/pull/5/head", head)
			if !tc.onOrigin {
				// PR 5 on the fork is another PR.
				gitW(t, f.origin, "update-ref", "refs/pull/5/head", f.commitIn(f.origin, f.base, "the fork's PR 5"))
			}
			headGh(t, head)

			ssh := filepath.Join(tmp, "ssh")
			// A same-path mirror on another host, holding its own PR 5.
			mirror := filepath.Join(tmp, "mirror.git")
			gitW(t, tmp, "init", "-q", "--bare", "-b", "main", mirror)
			gitW(t, f.repo, "push", "-q", mirror, "main")
			gitW(t, mirror, "update-ref", "refs/pull/5/head", f.commitIn(mirror, f.base, "the mirror's PR 5"))
			script := "#!/bin/sh\ncase \"$*\" in\n" +
				"*ghe.example*) exec git upload-pack '" + mirror + "' ;;\n" +
				"*\"'me/repo.git'\"*) exec git upload-pack '" + f.origin + "' ;;\n" +
				"*\"'up/repo.git'\"*) exec git upload-pack '" + up + "' ;;\n" +
				"esac\necho \"fake ssh: $*\" >&2\nexit 1\n"
			if err := os.WriteFile(ssh, []byte(script), 0o755); err != nil {
				t.Fatal(err)
			}
			t.Setenv("GIT_SSH_COMMAND", ssh)
			t.Setenv("GIT_SSH_VARIANT", "simple")
			gitW(t, f.repo, "remote", "set-url", "origin", "git@github.com:me/repo.git")
			if tc.remote {
				gitW(t, f.repo, "remote", "add", "upstream", "git@github.com:up/repo.git")
			}
			if tc.mirror {
				gitW(t, f.repo, "remote", "add", "aghe", "git@ghe.example:up/repo.git")
			}
			gitW(t, f.repo, "config", "url."+filepath.Join(tmp, "nowhere")+"/.insteadOf", "https://github.com/")
			if tc.https {
				gitW(t, f.repo, "config", "url."+up+".insteadOf", "https://github.com/up/repo")
			}

			var asked []string
			orig := prURL
			t.Cleanup(func() { prURL = orig })
			prURL = func(pr string) string {
				asked = append(asked, pr)
				return tc.url
			}

			var err error
			out := captured(t, func() { err = Clean(f.c, true, false, false, []string{"feat-x"}) })
			if err != nil {
				t.Fatalf("Clean: %v\n%s", err, out)
			}
			if len(asked) != 1 || asked[0] != "5" {
				t.Errorf("asked gh for the URL of %q, want PR 5's, once", asked)
			}
			_, serr := os.Stat(dir)
			if reaped := serr != nil; reaped != tc.reaped {
				t.Fatalf("reaped=%v, want %v\n%s", reaped, tc.reaped, out)
			}
			if tc.reaped && branchAt(f, "feat-x") != "" {
				t.Errorf("branch kept after the reap\n%s", out)
			}
			if !tc.reaped && branchAt(f, "feat-x") != tip {
				t.Errorf("branch moved from %s\n%s", tip, out)
			}
			if tc.says != "" && !strings.Contains(out, tc.says) {
				t.Errorf("clean said:\n%s\nwant %q", out, tc.says)
			}
		})
	}
}
