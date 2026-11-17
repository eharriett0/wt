package merge

import (
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
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
	orig := fetchPRHead
	t.Cleanup(func() { fetchPRHead = orig })
	fetchPRHead = func(remote, pr string) error {
		asked = append(asked, pr)
		return os.ErrNotExist
	}
	msg := UnshippedLane("https://github.com/acme/repo/pull/5/files", "feat-x", strings.TrimSpace(string(tip)))
	if len(asked) != 1 || asked[0] != "5" {
		t.Errorf("fetched %q, want PR 5's head fetched once", asked)
	}
	if !strings.Contains(msg, "`git fetch origin refs/pull/5/head`") {
		t.Errorf("kept-lane message: %q, want the fetch command for PR 5", msg)
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
	if got := KeptLaneMessage(TipUnknown, "feat-x", "https://github.com/acme/repo/pull/5", -1, "t", head); !strings.Contains(got, "`git fetch origin refs/pull/5/head`") {
		t.Errorf("a PR given as a URL: %q, want the fetch command for PR 5", got)
	}
	if got := KeptLaneMessage(TipUnknown, "feat-x", "feat-x", -1, "t", head); strings.Contains(got, "refs/pull/") {
		t.Errorf("no PR number: %q, want no fetch command", got)
	}
}
