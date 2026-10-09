package cli

import (
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"runtime"
	"strings"
	"testing"
	"time"
)

// mergePRGh is a fake gh for cmdMergePR: an open PR 99999 whose one commit's
// body says "Closes #7" while the PR body only says "Refs #7", answered from
// argv, with every `pr merge` argv (one per line) and stdin recorded.
const mergePRGh = `#!/bin/sh
d=$(dirname "$0")
case "$1 $2" in
"pr view")
	case "$*" in
	*" state "*) echo OPEN ;;
	*isDraft*) echo false ;;
	*messageHeadline*) echo "Fix the widget" ;;
	*join*) printf 'Fix the widget\nCloses #7\n' ;;
	*headRefName*) echo feat-x ;;
	*title*) echo "Fix the widget" ;;
	*" body "*) echo "Refs #7" ;;
	*" url "*) echo "https://github.com/o/r/pull/99999" ;;
	esac ;;
"pr diff") echo a.txt ;;
"issue view")
	case "$*" in
	*state*) echo OPEN ;;
	*) echo "an issue" ;;
	esac ;;
"pr merge") printf '%s\n' "$@" > "$d/argv"; cat > "$d/stdin" ;;
esac
exit 0
`

// TestCmdMergePR_forwardedBodyReachesGh drives the whole merge-pr command in a
// scratch repo against a fake gh, so the last link of the #180 wiring is
// pinned too: the body wt reads and judges is the body gh is fed. A refusal
// here is the pre-#180 gate (the commit body's "Closes #7" never ships); an
// empty stdin at gh is the silent empty-body merge. The fake gh sits first and
// alone on PATH (with git's own dir), the test stops unless "gh" resolves to
// it, and the repo has no GitHub remote — it can never reach a real PR.
func TestCmdMergePR_forwardedBodyReachesGh(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("the fake gh is a shell script")
	}
	gitBin, err := exec.LookPath("git")
	if err != nil {
		t.Skip("no git")
	}
	hermeticGitT(t)
	repo := t.TempDir()
	for _, args := range [][]string{
		{"init", "-q", "-b", "main"},
		{"-c", "user.name=t", "-c", "user.email=t@t", "commit", "-q", "--allow-empty", "-m", "base"},
	} {
		if out, err := exec.Command(gitBin, append([]string{"-C", repo}, args...)...).CombinedOutput(); err != nil {
			t.Fatalf("git %v: %v\n%s", args, err, out)
		}
	}
	ghDir := t.TempDir()
	if err := os.WriteFile(filepath.Join(ghDir, "gh"), []byte(mergePRGh), 0o755); err != nil {
		t.Fatal(err)
	}
	sep := string(os.PathListSeparator)
	t.Setenv("PATH", ghDir+sep+filepath.Dir(gitBin)+sep+"/usr/bin"+sep+"/bin")
	if got, err := exec.LookPath("gh"); err != nil || got != filepath.Join(ghDir, "gh") {
		t.Fatalf("gh resolves to %q (%v), not the fake", got, err)
	}
	t.Setenv("HOME", t.TempDir())
	t.Chdir(repo)
	bodyFile := filepath.Join(t.TempDir(), "body.txt")
	if err := os.WriteFile(bodyFile, []byte("File body, refs #7.\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	cases := []struct {
		name     string
		args     []string
		stdin    string
		wantArgv []string // after `pr merge 99999 --squash`
		wantBody string
	}{
		{"-F - from stdin", []string{"99999", "--keep", "--", "-F", "-"}, "Piped body, refs #7.\n",
			[]string{"-F", "-"}, "Piped body, refs #7.\n"},
		{"--admin and a body file", []string{"99999", "--keep", "--admin", "--", "--body-file", bodyFile}, "not the body",
			[]string{"--admin", "--body-file", "-"}, "File body, refs #7.\n"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			stdinFile := filepath.Join(t.TempDir(), "stdin")
			if err := os.WriteFile(stdinFile, []byte(tc.stdin), 0o644); err != nil {
				t.Fatal(err)
			}
			in, err := os.Open(stdinFile)
			if err != nil {
				t.Fatal(err)
			}
			defer in.Close()
			old := os.Stdin
			os.Stdin = in
			defer func() { os.Stdin = old }()
			_ = os.Remove(filepath.Join(ghDir, "argv"))

			if code := cmdMergePR(tc.args); code != 0 {
				t.Fatalf("cmdMergePR(%q) = %d, want 0", tc.args, code)
			}
			argv, err := os.ReadFile(filepath.Join(ghDir, "argv"))
			if err != nil {
				t.Fatalf("gh pr merge never ran: %v", err)
			}
			want := append([]string{"pr", "merge", "99999", "--squash"}, tc.wantArgv...)
			if got := strings.Split(strings.TrimSuffix(string(argv), "\n"), "\n"); !reflect.DeepEqual(got, want) {
				t.Errorf("gh argv = %q, want %q", got, want)
			}
			if got, _ := os.ReadFile(filepath.Join(ghDir, "stdin")); string(got) != tc.wantBody {
				t.Errorf("gh stdin = %q, want %q", got, tc.wantBody)
			}
		})
	}
}

// subjectGh is a fake gh for cmdMergePR's #196 cases: open PR 99999 whose
// title, body and commits (one JSON object each, as wt's --jq prints them) are
// the files "title", "body" and "commits" in its directory, in a repo squashing
// with GitHub's default message. closingIssuesReferences is empty. Every `pr
// merge` argv is recorded, one per line, in "argv".
const subjectGh = `#!/bin/sh
d=$(dirname "$0")
case "$1 $2" in
"pr view")
	case "$*" in
	*" state "*) echo OPEN ;;
	*isDraft*) echo false ;;
	*messageHeadline*) echo "feat: one" ;;
	*headRefName*) echo feat-x ;;
	*title*) cat "$d/title" ;;
	*" body "*) cat "$d/body" ;;
	*" url "*) echo "https://github.com/o/r/pull/99999" ;;
	esac ;;
"pr diff") echo a.txt ;;
"api graphql")
	case "$*" in
	*squashMergeCommitTitle*) echo '{"title":"COMMIT_OR_PR_TITLE","message":"COMMIT_MESSAGES"}' ;;
	*parents*) cat "$d/commits" ;;
	esac ;;
"issue view")
	case "$*" in
	*state*) echo OPEN ;;
	*) echo "an issue" ;;
	esac ;;
"pr merge") printf '%s\n' "$@" > "$d/argv" ;;
esac
exit 0
`

// TestCmdMergePR_judgesTheShippedSubject drives merge-pr end to end over the
// squash SUBJECT (#196): a closing PR title on a two-commit PR used to merge
// with no warning (the subject was never read), and a closing headline that a
// forwarded --subject and --body kept from shipping still refused the merge.
// Same safety as the other fake-gh tests: the fake is first and alone on PATH
// (with git's dir), and the repo has no GitHub remote.
func TestCmdMergePR_judgesTheShippedSubject(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("the fake gh is a shell script")
	}
	gitBin, err := exec.LookPath("git")
	if err != nil {
		t.Skip("no git")
	}
	hermeticGitT(t)
	repo := t.TempDir()
	for _, args := range [][]string{
		{"init", "-q", "-b", "main"},
		{"-c", "user.name=t", "-c", "user.email=t@t", "commit", "-q", "--allow-empty", "-m", "base"},
	} {
		if out, err := exec.Command(gitBin, append([]string{"-C", repo}, args...)...).CombinedOutput(); err != nil {
			t.Fatalf("git %v: %v\n%s", args, err, out)
		}
	}
	ghDir := t.TempDir()
	if err := os.WriteFile(filepath.Join(ghDir, "gh"), []byte(subjectGh), 0o755); err != nil {
		t.Fatal(err)
	}
	sep := string(os.PathListSeparator)
	t.Setenv("PATH", ghDir+sep+filepath.Dir(gitBin)+sep+"/usr/bin"+sep+"/bin")
	if got, err := exec.LookPath("gh"); err != nil || got != filepath.Join(ghDir, "gh") {
		t.Fatalf("gh resolves to %q (%v), not the fake", got, err)
	}
	t.Setenv("HOME", t.TempDir())
	t.Chdir(repo)
	postMergeSleep = func(time.Duration) {} // the PR stays OPEN: no merge to wait for
	t.Cleanup(func() { postMergeSleep = time.Sleep })

	const two = `{"message":"feat: one","parents":1}
{"message":"feat: two","parents":1}`
	const flakeOfTwo = `{"message":"Fix #6 flake\n\nthe fix","parents":1}
{"message":"feat: other","parents":1}`
	cases := []struct {
		name     string
		title    string
		commits  string
		args     []string
		code     int
		merged   []string // gh's argv after `pr merge 99999 --squash`; nil = gh never merged
		stderrIs string
	}{
		{"a closing PR title on two commits is refused", "Fixes #5 the thing", two,
			[]string{"99999", "--keep"}, 1, nil, "the squash SUBJECT (the PR title) will close #5"},
		{"--dry-run says so, and merges nothing", "Fixes #5 the thing", two,
			[]string{"99999", "--dry-run"}, 0, nil, "a real merge would REFUSE here"},
		{"a headline that a forwarded subject and body keep out does not refuse", "Two changes", flakeOfTwo,
			[]string{"99999", "--keep", "--", "--subject", "clean", "--body", "clean"}, 0,
			[]string{"--subject", "clean", "--body", "clean"}, ""},
		{"a forwarded closing --subject is refused", "Clean title", two,
			[]string{"99999", "--keep", "--", "-t", "Fixes #8 via subject"}, 1, nil, "the squash SUBJECT (the forwarded -t) will close #8"},
		{"--close-ok lets it through", "Clean title", two,
			[]string{"99999", "--keep", "--close-ok", "--", "-t", "Fixes #8 via subject"}, 0,
			[]string{"-t", "Fixes #8 via subject"}, ""},
		{"the WIP strip's subject is judged too", "WIP: Fixes #5 the thing", two,
			[]string{"99999", "--keep"}, 1, nil, `the squash SUBJECT (the PR title without "WIP:") will close #5`},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			for name, text := range map[string]string{"title": tc.title + "\n", "body": "Refs #3\n", "commits": tc.commits + "\n"} {
				if err := os.WriteFile(filepath.Join(ghDir, name), []byte(text), 0o644); err != nil {
					t.Fatal(err)
				}
			}
			_ = os.Remove(filepath.Join(ghDir, "argv"))
			stderr, err := os.Create(filepath.Join(t.TempDir(), "stderr"))
			if err != nil {
				t.Fatal(err)
			}
			old := os.Stderr
			os.Stderr = stderr
			code := cmdMergePR(tc.args)
			os.Stderr = old
			stderr.Close()
			if code != tc.code {
				t.Errorf("cmdMergePR(%q) = %d, want %d", tc.args, code, tc.code)
			}
			warned, _ := os.ReadFile(stderr.Name())
			if !strings.Contains(string(warned), tc.stderrIs) {
				t.Errorf("stderr = %q, want it to say %q", warned, tc.stderrIs)
			}
			argv, err := os.ReadFile(filepath.Join(ghDir, "argv"))
			switch {
			case tc.merged == nil && err == nil:
				t.Errorf("gh pr merge ran (%q), want no merge", argv)
			case tc.merged != nil && err != nil:
				t.Errorf("gh pr merge never ran: %v", err)
			case tc.merged != nil:
				want := append([]string{"pr", "merge", "99999", "--squash"}, tc.merged...)
				if got := strings.Split(strings.TrimSuffix(string(argv), "\n"), "\n"); !reflect.DeepEqual(got, want) {
					t.Errorf("gh argv = %q, want %q", got, want)
				}
			}
		})
	}
}
