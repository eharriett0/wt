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
// every run, with a hint naming the wrong PR. Now clean hands the check the
// PR's URL, and the fetch goes to the PR's own repository (merge.prRemote).
//
// Real git end to end: origin is github.com/me/repo, reached through a fake
// ssh; upstream is github.com/up/repo, reached through url.insteadOf (git
// expands insteadOf in `remote get-url`, so origin can't use it: wt would read
// a local path and never tell the fork apart). The lane's tip is an ancestor
// of the PR's head, which only the PR's own repository holds.
func TestClean_ForkCloneFetchesThePRHeadFromThePRsRepository(t *testing.T) {
	for _, tc := range []struct {
		name   string
		url    string // what gh gives for PR 5's URL; "" when it can't
		onFork bool   // the PR is origin's own, so origin holds its head
		reaped bool
		hint   string // in the kept lane's warning
	}{
		{"upstream's PR, fetched from upstream", "https://github.com/up/repo/pull/5", false, true, ""},
		{"control: origin's own PR, fetched from origin", "https://github.com/me/repo/pull/5", true, true, ""},
		{"no URL from gh: origin, as before, and the lane is kept", "", false, false, "`git fetch origin refs/pull/5/head`"},
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
			if tc.onFork {
				prRepo = f.origin
			}
			head := f.commitIn(prRepo, tip, "updated on GitHub")
			gitW(t, prRepo, "update-ref", "refs/pull/5/head", head)
			if !tc.onFork {
				// PR 5 on the fork is another PR.
				gitW(t, f.origin, "update-ref", "refs/pull/5/head", f.commitIn(f.origin, f.base, "the fork's PR 5"))
			}
			headGh(t, head)

			ssh := filepath.Join(tmp, "ssh")
			script := "#!/bin/sh\ncase \"$*\" in\n*\"'me/repo.git'\"*) exec git upload-pack '" + f.origin + "' ;;\nesac\necho \"fake ssh: $*\" >&2\nexit 1\n"
			if err := os.WriteFile(ssh, []byte(script), 0o755); err != nil {
				t.Fatal(err)
			}
			t.Setenv("GIT_SSH_COMMAND", ssh)
			t.Setenv("GIT_SSH_VARIANT", "simple")
			gitW(t, f.repo, "remote", "set-url", "origin", "git@github.com:me/repo.git")
			gitW(t, f.repo, "config", "url."+up+".insteadOf", "https://github.com/up/repo")

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
			if tc.hint != "" && !strings.Contains(out, tc.hint) {
				t.Errorf("clean said:\n%s\nwant the hint %s", out, tc.hint)
			}
		})
	}
}
