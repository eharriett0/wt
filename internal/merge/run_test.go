package merge

import (
	"errors"
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
