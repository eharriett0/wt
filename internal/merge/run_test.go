package merge

import (
	"errors"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"runtime"
	"strings"
	"testing"
)

// runGh is a fake gh for Run: it answers the PR reads Run makes from files in
// its own directory and records the merge's argv (one per line) and stdin. It
// sits FIRST and ALONE on PATH (plus the system dirs sh needs), and the test
// stops unless "gh" resolves to it, so it can never reach the real gh.
const runGh = `#!/bin/sh
d=$(dirname "$0")
case "$1 $2" in
"pr diff") echo a.txt ;;
"pr view")
	case "$*" in
	*messageHeadline*) echo "Fix the widget" ;;
	*headRefName*) echo feat-x ;;
	*title*) cat "$d/title" ;;
	esac ;;
"pr merge") printf '%s\n' "$@" > "$d/argv"; cat > "$d/stdin" ;;
esac
`

// TestRunPutsWtFlagsBeforeThePassthrough pins how Run hands the merge to gh
// (#180): wt's own flags (--admin, the WIP --subject) go in FRONT of the
// operator's passthrough, so gh's last-wins parser lets an operator's
// --subject beat the WIP strip and a dangling passthrough flag fails in gh
// instead of taking a wt flag as its value; and the forwarded body wt already
// read reaches gh's stdin.
func TestRunPutsWtFlagsBeforeThePassthrough(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("the fake gh is a shell script")
	}
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "gh"), []byte(runGh), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", dir+string(os.PathListSeparator)+"/usr/bin"+string(os.PathListSeparator)+"/bin")
	if got, err := exec.LookPath("gh"); err != nil || got != filepath.Join(dir, "gh") {
		t.Fatalf("gh resolves to %q (%v), not the fake", got, err)
	}
	read := func(name string) string {
		b, err := os.ReadFile(filepath.Join(dir, name))
		if err != nil {
			t.Fatal(err)
		}
		return string(b)
	}

	cases := []struct {
		title       string
		passthrough []string
		want        []string // gh argv after `pr merge 99999 --squash`
	}{
		{"WIP: Add the widget", []string{"--subject", "Operator's subject", "-F", "-"},
			[]string{"--subject", "Add the widget", "--admin", "--subject", "Operator's subject", "-F", "-"}},
		{"WIP: Add the widget", []string{"--subject"},
			[]string{"--subject", "Add the widget", "--admin", "--subject"}},
		{"Add the widget", []string{"-F", "-"},
			[]string{"--admin", "-F", "-"}},
	}
	for _, tc := range cases {
		if err := os.WriteFile(filepath.Join(dir, "title"), []byte(tc.title+"\n"), 0o644); err != nil {
			t.Fatal(err)
		}
		args := WithAdmin(true, tc.passthrough)
		if err := Run("99999", false, false, false, []string{"feat-x"}, args, strings.NewReader("Forwarded body."), ""); err != nil {
			t.Fatalf("%q: Run: %v", tc.passthrough, err)
		}
		got := strings.Split(strings.TrimSuffix(read("argv"), "\n"), "\n")
		if want := append([]string{"pr", "merge", "99999", "--squash"}, tc.want...); !reflect.DeepEqual(got, want) {
			t.Errorf("title %q, passthrough %q:\n gh argv = %q\n want      %q", tc.title, tc.passthrough, got, want)
		}
		if got := read("stdin"); got != "Forwarded body." {
			t.Errorf("%q: gh stdin = %q, want the forwarded body", tc.passthrough, got)
		}
	}
}

// quotaGh is gh with the account's REST quota used up (#217): every REST read
// refused the way GitHub refused them during the incident, `gh api --include`
// included (the reset probe). GraphQL answers from "count" unless "count.fail"
// exists. The merge records its argv.
const quotaGh = `#!/bin/sh
d=$(dirname "$0")
limited() { echo "$1HTTP 403: API rate limit exceeded for user ID 1. If you reach out to GitHub Support for help, please include the request ID X." >&2; exit 1; }
case "$*" in
"api --include /"*) printf 'HTTP/2.0 403 Forbidden\r\nX-Ratelimit-Remaining: 0\r\nX-Ratelimit-Reset: 4102444800\r\n\r\n{}\n'; exit 1 ;;
"pr diff"*) [ -e "$d/diff.fail" ] && { cat "$d/diff.fail" >&2; exit 1; }; limited "could not find pull request diff: " ;;
*changedFiles*) [ -e "$d/count.fail" ] && { cat "$d/count.fail" >&2; exit 1; }; cat "$d/count" ;;
*messageHeadline*) echo "Fix the widget" ;;
*headRefName*) echo feat-x ;;
*title*) echo "Fix the widget" ;;
"pr merge"*) printf '%s\n' "$@" > "$d/argv" ;;
esac
`

// TestRunUnreadDiffIsNotEmpty pins #217: with the REST quota used up, merge-pr
// read the diff through GraphQL instead of calling it EMPTY; and when GraphQL
// could not answer either, it refused as UNREAD, said why, and did not point
// at --bypass (an exhausted quota is no reason to spend a safety flag). An
// explicit --bypass still goes through, saying what it skipped.
func TestRunUnreadDiffIsNotEmpty(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("the fake gh is a shell script")
	}
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "gh"), []byte(quotaGh), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", dir+string(os.PathListSeparator)+"/usr/bin"+string(os.PathListSeparator)+"/bin")
	if got, err := exec.LookPath("gh"); err != nil || got != filepath.Join(dir, "gh") {
		t.Fatalf("gh resolves to %q (%v), not the fake", got, err)
	}
	write := func(name, s string) {
		t.Helper()
		if err := os.WriteFile(filepath.Join(dir, name), []byte(s), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	run := func(dryRun, bypass bool) (stdout, stderr string, err error) {
		t.Helper()
		_ = os.Remove(filepath.Join(dir, "argv"))
		out, oerr := os.CreateTemp(dir, "stdout")
		errF, eerr := os.CreateTemp(dir, "stderr")
		if oerr != nil || eerr != nil {
			t.Fatal(oerr, eerr)
		}
		saved, savedErr := os.Stdout, os.Stderr
		os.Stdout, os.Stderr = out, errF
		err = Run("99999", dryRun, bypass, false, []string{"feat-x"}, nil, nil, "")
		os.Stdout, os.Stderr = saved, savedErr
		o, _ := os.ReadFile(out.Name())
		s, _ := os.ReadFile(errF.Name())
		return string(o), string(s), err
	}
	merged := func() bool { _, err := os.Stat(filepath.Join(dir, "argv")); return err == nil }

	write("count", "6\n")
	if stdout, stderr, err := run(false, false); err != nil || !merged() || strings.Contains(stderr, "EMPTY") || !strings.Contains(stdout, "6 changed file(s)") {
		t.Errorf("REST refused, GraphQL fine: err=%v merged=%v\nstdout: %s\nstderr: %s\nwant GraphQL's 6 files and a merge", err, merged(), stdout, stderr)
	}

	write("count.fail", "GraphQL: API rate limit exceeded for user ID 1.\n")
	stdout, stderr, err := run(false, false)
	if err == nil || merged() {
		t.Fatalf("both reads refused: err=%v merged=%v, want a refusal and no merge", err, merged())
	}
	for _, want := range []string{"could not be read", "REST API rate limit", "it lifts at", "GraphQL API rate limit", "Run the merge again once the rate limit lifts."} {
		if !strings.Contains(stderr, want) {
			t.Errorf("both reads refused: stderr lacks %q:\n%s", want, stderr)
		}
	}
	if strings.Contains(stderr, "EMPTY") || strings.Contains(stderr, "--bypass") {
		t.Errorf("both reads refused: stderr calls the diff EMPTY or points at --bypass:\n%s", stderr)
	}
	// A dry run refuses at the guard too, as it does for an empty diff.
	if _, stderr, err := run(true, false); err == nil || merged() || !strings.Contains(stderr, "could not be read") {
		t.Errorf("dry run: err=%v merged=%v stderr=%q, want the same refusal", err, merged(), stderr)
	}
	if stdout, _, err := run(true, true); err != nil || merged() || !strings.Contains(stdout, "verdict=block:diff_unread file_count=unread") {
		t.Errorf("dry run with --bypass: err=%v merged=%v stdout=%q, want the unread verdict and count, no merge", err, merged(), stdout)
	}
	if stdout, stderr, err = run(false, true); err != nil || !merged() || !strings.Contains(stderr, "--bypass set: proceeding without the empty-diff check") || !strings.Contains(stdout, "changed files unread") {
		t.Errorf("--bypass: err=%v merged=%v\nstdout: %s\nstderr: %s\nwant the merge, saying what it skipped", err, merged(), stdout, stderr)
	}
	// A diff unread for some other reason does not promise a rate limit lifting.
	write("diff.fail", "GraphQL: Could not resolve to a PullRequest with the number of 99999. (repository.pullRequest)\n")
	write("count.fail", "GraphQL: Could not resolve to a PullRequest with the number of 99999. (repository.pullRequest)\n")
	if _, stderr, err := run(false, false); err == nil || merged() || strings.Contains(stderr, "rate limit") || !strings.Contains(stderr, "Check that gh can read it (gh pr diff 99999 --name-only)") {
		t.Errorf("unread, not rate limited: err=%v merged=%v stderr=%q, want a refusal that sends the operator to gh, not to a reset", err, merged(), stderr)
	}
}

// TestRunMarksGhFailures: an error from `gh pr merge` itself wraps
// ErrMergeCommand, so merge-pr reads the PR state before calling it "not
// merged" (gh can fail AFTER merging: `-d` cannot delete a local branch a wt
// worktree has checked out, #196); a guard's refusal never does, so it exits 1
// without a state read.
func TestRunMarksGhFailures(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("the fake gh is a shell script")
	}
	dir := t.TempDir()
	const failingGh = `#!/bin/sh
d=$(dirname "$0")
case "$1 $2" in
"pr diff") cat "$d/files" ;;
"pr view")
	case "$*" in
	*messageHeadline*) echo "Fix the widget" ;;
	*headRefName*) echo feat-x ;;
	*title*) echo "Fix the widget" ;;
	esac ;;
"pr merge") echo "failed to delete local branch feat-x" >&2; exit 1 ;;
esac
`
	if err := os.WriteFile(filepath.Join(dir, "gh"), []byte(failingGh), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", dir+string(os.PathListSeparator)+"/usr/bin"+string(os.PathListSeparator)+"/bin")
	if got, err := exec.LookPath("gh"); err != nil || got != filepath.Join(dir, "gh") {
		t.Fatalf("gh resolves to %q (%v), not the fake", got, err)
	}
	for _, tc := range []struct {
		files  string
		ghFail bool
	}{
		{"a.txt\n", true}, // gh ran and failed
		{"", false},       // the empty-diff guard refused: gh never ran
	} {
		if err := os.WriteFile(filepath.Join(dir, "files"), []byte(tc.files), 0o644); err != nil {
			t.Fatal(err)
		}
		err := Run("99999", false, false, false, []string{"feat-x"}, nil, nil, "")
		if err == nil {
			t.Fatalf("files %q: Run succeeded, want an error", tc.files)
		}
		if got := errors.Is(err, ErrMergeCommand); got != tc.ghFail {
			t.Errorf("files %q: errors.Is(%v, ErrMergeCommand) = %v, want %v", tc.files, err, got, tc.ghFail)
		}
	}
}

// hintGh is a fake gh for Run's refusals: an empty diff when "$d/empty"
// exists, head branch feat-y.
const hintGh = `#!/bin/sh
d=$(dirname "$0")
case "$1 $2" in
"pr diff") [ -e "$d/empty" ] || echo a.txt ;;
"pr view")
	case "$*" in
	*messageHeadline*) echo "Fix the widget" ;;
	*headRefName*) echo feat-y ;;
	*title*) echo "Fix the widget" ;;
	esac ;;
"pr merge") exit 1 ;;
esac
`

// Run's refusals print commands to paste; a "#5" in them is a shell comment,
// so they spell the PR without it (#228 review), and name it "#5".
func TestRunHintsSpellThePRForAShell(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("the fake gh is a shell script")
	}
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "gh"), []byte(hintGh), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", dir+string(os.PathListSeparator)+"/usr/bin"+string(os.PathListSeparator)+"/bin")
	if got, err := exec.LookPath("gh"); err != nil || got != filepath.Join(dir, "gh") {
		t.Fatalf("gh resolves to %q (%v), not the fake", got, err)
	}
	stderr := func(f func()) string {
		t.Helper()
		r, w, err := os.Pipe()
		if err != nil {
			t.Fatal(err)
		}
		old := os.Stderr
		os.Stderr = w
		done := make(chan string)
		go func() { b, _ := io.ReadAll(r); done <- string(b) }()
		f()
		os.Stderr = old
		_ = w.Close()
		return <-done
	}
	foreign := stderr(func() { _ = Run("#5", false, false, false, []string{"feat-x"}, nil, nil, "") })
	if !strings.Contains(foreign, "If intended: wt merge-pr 5 --merge-foreign") || !strings.Contains(foreign, "REFUSING to merge PR #5") {
		t.Errorf("foreign-branch refusal:\n%s", foreign)
	}
	if err := os.WriteFile(filepath.Join(dir, "empty"), nil, 0o644); err != nil {
		t.Fatal(err)
	}
	empty := stderr(func() { _ = Run("#5", false, false, false, []string{"feat-y"}, nil, nil, "") })
	if !strings.Contains(empty, "wt merge-pr 5 --bypass") || strings.Contains(empty, "#5 --bypass") {
		t.Errorf("empty-diff refusal:\n%s", empty)
	}
}
