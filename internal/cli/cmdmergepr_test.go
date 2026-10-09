package cli

import (
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"runtime"
	"strings"
	"testing"
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
