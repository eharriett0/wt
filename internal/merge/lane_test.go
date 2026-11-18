package merge

import (
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"testing"

	"github.com/eharriett0/wt/internal/gitx"
)

// UnshippedLane fetches a PR head this clone lacks by the PR's NUMBER, from a
// PR given as a URL too (#230 re-review: the URL went to git as is).
func TestUnshippedLane_FetchesThePRHeadByNumber(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("the fake gh is a shell script")
	}
	t.Setenv("GIT_CONFIG_GLOBAL", os.DevNull)
	t.Setenv("GIT_CONFIG_NOSYSTEM", "1")
	repo := t.TempDir()
	for _, args := range [][]string{
		{"init", "-q", "-b", "main"},
		{"-c", "user.name=t", "-c", "user.email=t@t", "commit", "-q", "--allow-empty", "-m", "tip"},
	} {
		cmd := exec.Command("git", args...)
		cmd.Dir = repo
		if out, err := cmd.CombinedOutput(); err != nil {
			t.Fatalf("git %v: %v\n%s", args, err, out)
		}
	}
	tip, err := exec.Command("git", "-C", repo, "rev-parse", "HEAD").Output()
	if err != nil {
		t.Fatal(err)
	}
	const head = "0123456789abcdef0123456789abcdef01234567" // not in this clone
	gh := t.TempDir()
	if err := os.WriteFile(filepath.Join(gh, "gh"), []byte("#!/bin/sh\ncase \"$*\" in *headRefOid*) echo "+head+" ;; *) exit 1 ;; esac\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", gh+string(os.PathListSeparator)+os.Getenv("PATH"))
	t.Chdir(repo)
	var asked []string
	orig, origRepo, origNames, origRemote := fetchPRHead, originRepo, remoteNames, remoteRepo
	t.Cleanup(func() { fetchPRHead, originRepo, remoteNames, remoteRepo = orig, origRepo, origNames, origRemote })
	remoteNames = func() []string { return []string{"origin"} }
	fetchPRHead = func(remote, pr string) error {
		asked = append(asked, remote+" "+pr)
		return os.ErrNotExist
	}
	originRepo = func() (string, string, string) { return "github.com", "acme", "repo" }
	msg := UnshippedLane("https://github.com/acme/repo/pull/5/files", "feat-x", strings.TrimSpace(string(tip)))
	if len(asked) != 1 || asked[0] != "origin 5" {
		t.Errorf("fetched %q, want PR 5's head fetched once, from origin", asked)
	}
	if !strings.Contains(msg, "`git fetch origin refs/pull/5/head`") {
		t.Errorf("kept-lane message: %q, want the fetch command for PR 5", msg)
	}
	// #228 review: origin is a fork (me/repo) and the PR is upstream's, where
	// PR 5 on origin is another PR or none.
	asked = nil
	originRepo = func() (string, string, string) { return "github.com", "me", "repo" }
	msg = UnshippedLane("https://github.com/acme/repo/pull/5", "feat-x", strings.TrimSpace(string(tip)))
	// #243 review: that fetch failing (no https credentials; origin the PR's
	// own repository under an old name), origin is asked next.
	if strings.Join(asked, ",") != "https://github.com/acme/repo 5,origin 5" {
		t.Errorf("fetched %q, want PR 5's head from acme/repo, then from origin", asked)
	}
	if !strings.Contains(msg, "`git fetch https://github.com/acme/repo refs/pull/5/head`") {
		t.Errorf("kept-lane message: %q, want the fetch from acme/repo", msg)
	}
	// A configured remote naming acme/repo is used: the user's own transport.
	asked = nil
	remoteNames = func() []string { return []string{"origin", "upstream"} }
	remoteRepo = func(r string) (string, string, string) {
		if r == "upstream" {
			return "github-work", "Acme", "repo"
		}
		return "github.com", "me", "repo"
	}
	msg = UnshippedLane("https://github.com/acme/repo/pull/5", "feat-x", strings.TrimSpace(string(tip)))
	if strings.Join(asked, ",") != "upstream 5,https://github.com/acme/repo 5,origin 5" {
		t.Errorf("fetched %q, want PR 5's head from upstream, its URL, then origin", asked)
	}
	if !strings.Contains(msg, "`git fetch upstream refs/pull/5/head`") {
		t.Errorf("kept-lane message: %q, want the fetch from upstream", msg)
	}
	// #243 round 2: a fetch that works but leaves the head missing (a
	// mirror's own PR 5, origin's other PR) goes on to the next.
	asked = nil
	fetchPRHead = func(remote, pr string) error {
		asked = append(asked, remote+" "+pr)
		return nil
	}
	UnshippedLane("https://github.com/acme/repo/pull/5", "feat-x", strings.TrimSpace(string(tip)))
	if strings.Join(asked, ",") != "upstream 5,https://github.com/acme/repo 5,origin 5" {
		t.Errorf("fetched %q, want every candidate while the head stays missing", asked)
	}
	// A fetch that ran out of time ends the walk: each costs the whole budget.
	asked = nil
	fetchPRHead = func(remote, pr string) error {
		asked = append(asked, remote+" "+pr)
		return &gitx.FetchTimeoutError{}
	}
	UnshippedLane("https://github.com/acme/repo/pull/5", "feat-x", strings.TrimSpace(string(tip)))
	if strings.Join(asked, ",") != "upstream 5" {
		t.Errorf("fetched %q, want only upstream once it timed out", asked)
	}
}

// #243 round 2: `git remote` lists alphabetically, so a mirror elsewhere with
// the same owner/name (a GHE copy, Codeberg) was picked before `upstream` on
// the PR's own host, and the URL never tried. Remotes on the URL's host come
// first, then the others (an ssh alias), then the URL. A remote with the same
// owner and another name, or the same name and another owner, is no match.
func TestPRRemotes_order(t *testing.T) {
	orig, origNames, origRemote := originRepo, remoteNames, remoteRepo
	t.Cleanup(func() { originRepo, remoteNames, remoteRepo = orig, origNames, origRemote })
	originRepo = func() (string, string, string) { return "github.com", "me", "repo" }
	repos := map[string][3]string{
		"aghe":       {"ghe.example", "up", "repo"},
		"alias":      {"github-work", "Up", "Repo"},
		"origin":     {"github.com", "me", "repo"},
		"othername":  {"github.com", "up", "other"},
		"otherowner": {"github.com", "down", "repo"},
		"upstream":   {"github.com", "up", "repo"},
	}
	remoteNames = func() []string { return []string{"aghe", "alias", "origin", "othername", "otherowner", "upstream"} }
	remoteRepo = func(r string) (string, string, string) { x := repos[r]; return x[0], x[1], x[2] }
	got := strings.Join(prRemotes("https://github.com/up/repo/pull/5"), ",")
	if want := "upstream,aghe,alias,https://github.com/up/repo"; got != want {
		t.Errorf("prRemotes = %s, want %s", got, want)
	}
	if got := prRemote("https://github.com/up/repo/pull/5"); got != "upstream" {
		t.Errorf("prRemote = %s, want upstream (the hint)", got)
	}
}

// #243: `wt clean` passes the URL only for another repository's PR, so its
// message says "#5" for origin's own, as every other clean line does.
func TestLaneSelector(t *testing.T) {
	orig, origNames := originRepo, remoteNames
	t.Cleanup(func() { originRepo, remoteNames = orig, origNames })
	originRepo = func() (string, string, string) { return "github.com", "me", "repo" }
	remoteNames = func() []string { return nil }
	for _, tc := range []struct{ url, want string }{
		{"https://github.com/me/repo/pull/5", "5"},
		{"https://github.com/up/repo/pull/5", "https://github.com/up/repo/pull/5"},
		{"", "5"},
	} {
		if got := LaneSelector("5", tc.url); got != tc.want {
			t.Errorf("LaneSelector(5, %q) = %q, want %q", tc.url, got, tc.want)
		}
	}
}

// A message names a PR "#5", or by the selector given, never "#<url>" (#228
// review).
func TestPRLabel(t *testing.T) {
	for pr, want := range map[string]string{
		"5":                                   "#5",
		"#5":                                  "#5",
		"https://github.com/acme/repo/pull/5": "https://github.com/acme/repo/pull/5",
		"feat-x":                              "feat-x",
		"+5":                                  "+5",
		"0":                                   "0",
		"":                                    "",
	} {
		if got := PRLabel(pr); got != want {
			t.Errorf("PRLabel(%q) = %q, want %q", pr, got, want)
		}
	}
}

// A hint's command never spells a PR "#5": a shell reads "#" as a comment.
func TestPRArg(t *testing.T) {
	for pr, want := range map[string]string{
		"#5": "5", "5": "5", "https://x/o/r/pull/5": "https://x/o/r/pull/5",
		"#hotfix": "#hotfix", "#0": "#0", "#+5": "#+5", "feat-x": "feat-x",
	} {
		if got := PRArg(pr); got != want {
			t.Errorf("PRArg(%q) = %q, want %q", pr, got, want)
		}
	}
}

// gh takes a PR as a number or a URL; the PR-head fetch needs the number
// (#230 re-review: a URL got no fetch and a broken fetch command).
func TestPRNumber(t *testing.T) {
	for _, tc := range []struct{ pr, want string }{
		{"5", "5"},
		{"#5", "5"},
		{"05", "5"},
		{"+5", ""},
		{"0", ""},
		{"https://github.com/acme/repo/pull/5", "5"},
		{"https://github.com/acme/repo/pull/5/", "5"},
		{"https://github.com/acme/repo/pull/5/files", "5"},
		{"https://github.com/acme/repo/pull/5?w=1", "5"},
		{"https://github.com/acme/repo/pull/5#issuecomment-1", "5"},
		{"https://git.acme.test/acme/repo/pull/12", "12"},
		{"feat-x", ""},
		{"", ""},
	} {
		if got := PRNumber(tc.pr); got != tc.want {
			t.Errorf("PRNumber(%q) = %q, want %q", tc.pr, got, tc.want)
		}
	}
	const head = "1111111111111111111111111111111111111111"
	if got := KeptLaneMessage(TipUnknown, "feat-x", "https://github.com/acme/repo/pull/5", "origin", -1, "t", head); !strings.Contains(got, "`git fetch origin refs/pull/5/head`") {
		t.Errorf("a PR given as a URL: %q, want the fetch command for PR 5", got)
	}
	if got := KeptLaneMessage(TipUnknown, "feat-x", "feat-x", "origin", -1, "t", head); strings.Contains(got, "refs/pull/") {
		t.Errorf("no PR number: %q, want no fetch command", got)
	}
}

// #228 review: with origin a fork and -R naming upstream, PR n on origin is
// another PR or none, so the head is fetched, and the hint fetches it, from
// the PR's own repository.
func TestPRRemote(t *testing.T) {
	orig, origNames, origRemote := originRepo, remoteNames, remoteRepo
	t.Cleanup(func() { originRepo, remoteNames, remoteRepo = orig, origNames, origRemote })
	originRepo = func() (string, string, string) { return "github.com", "me", "repo" }
	// A remote naming no repository wt can read (a local path) is never picked.
	remoteNames = func() []string { return []string{"origin", "mirror"} }
	remoteRepo = func(string) (string, string, string) { return "", "", "" }
	for pr, want := range map[string]string{
		"5":                                    "origin",
		"https://github.com/me/repo/pull/5":    "origin",
		"https://github.com/ME/Repo/pull/5":    "origin",
		"https://github.com/up/repo/pull/5":    "https://github.com/up/repo",
		"https://git.acme.test/up/svc/pull/12": "https://git.acme.test/up/svc",
	} {
		if got := prRemote(pr); got != want {
			t.Errorf("prRemote(%q) = %q, want %q", pr, got, want)
		}
	}
	originRepo = func() (string, string, string) { return "", "", "" }
	if got := prRemote("https://github.com/up/repo/pull/5"); got != "origin" {
		t.Errorf("an origin wt can't read: %q, want origin", got)
	}
	const head = "1111111111111111111111111111111111111111"
	if got := KeptLaneMessage(TipUnknown, "feat-x", "https://github.com/up/repo/pull/5", "https://github.com/up/repo", -1, "t", head); !strings.Contains(got, "`git fetch https://github.com/up/repo refs/pull/5/head`") {
		t.Errorf("the hint must fetch from the PR's repository: %q", got)
	}
}
