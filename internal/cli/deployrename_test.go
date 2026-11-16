package cli

import (
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"
)

// renameGh is gh for a PR that renames files (#221). Like the real gh, `pr
// diff --name-only` prints only the new side of a rename; REST pulls/{n}/files
// answers from "rest" (or is refused for the quota when "rest.fail" exists)
// and the GraphQL file list from "gql". Checks are green, nothing is
// required. Its own host keeps ghx's process-wide auth memo (#172) apart.
const renameGh = `#!/bin/sh
d=$(dirname "$0")
H=renames.example.com
printf '%s\n' "$*" >> "$d/calls"
case "$*" in
"auth status --json hosts --active"*) printf '{"hosts":{"%s":[{"state":"success","active":true,"host":"%s","login":"octo"}]}}\n' $H $H; exit 0 ;;
"auth status"*) exit 0 ;;
"api --hostname $H repos/o/r/pulls/99999/files"*)
	[ -e "$d/rest.fail" ] && { printf '{"message":"API rate limit exceeded for user ID 1.","status":"403"}'; echo "gh: API rate limit exceeded for user ID 1. (HTTP 403)" >&2; exit 1; }
	cat "$d/rest"; exit 0 ;;
"api --include /"*) exit 1 ;;
"api --hostname $H repos/o/r/branches/"*) echo '` + unprotected + `'; exit 0 ;;
"api --hostname $H repos/o/r/rules/branches/"*) echo '[]'; exit 0 ;;
*"--json number,url,changedFiles"*) echo "$3 https://$H/o/r/pull/$3 $(cat "$d/nfiles")"; exit 0 ;;
*"--json number,url"*) echo "$3 https://$H/o/r/pull/$3"; exit 0 ;;
*statusCheckRollup*) echo '` + greenRollup + `'; exit 0 ;;
*"files(first:100"*) cat "$d/gql"; exit 0 ;;
esac
case "$1 $2" in
"pr view")
	case "$*" in
	*" state "*) if [ -e "$d/merged" ]; then echo MERGED; else echo OPEN; fi ;;
	*isDraft*) echo false ;;
	*messageHeadline*) echo "Move the widget" ;;
	*headRefName*) echo feat-x ;;
	*title*) echo "Move the widget" ;;
	*" body "*) echo "Refs #3" ;;
	*" url "*) echo "https://$H/o/r/pull/99999" ;;
	esac ;;
"pr diff") cat "$d/diffnames" ;;
"api graphql")
	case "$*" in
	*squashMergeCommitTitle*) echo '{"title":"COMMIT_OR_PR_TITLE","message":"COMMIT_MESSAGES"}' ;;
	*parents*) echo '{"message":"Move the widget","parents":1}' ;;
	esac ;;
"issue view") echo OPEN ;;
"pr merge") printf '%s\n' "$@" > "$d/argv"; : > "$d/merged" ;;
esac
exit 0
`

// TestCmdMergePR_renameOutOfADeployPathGates is #221 end to end: a PR that
// renames a file OUT of merge_is_deploy_paths (infra/x.yaml → docs/x.yaml)
// deletes a deployed file, yet `gh pr diff --name-only` lists only docs/x.yaml,
// so the deploy gate said "changes no deploy-path files" and merged without
// the typed confirm. The gate now sees both sides of a rename (REST), and
// gates when only GraphQL answers and a file was renamed (its old path is
// unknown). A rename that stays outside the deploy paths still skips it.
func TestCmdMergePR_renameOutOfADeployPathGates(t *testing.T) {
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
		{"remote", "add", "origin", "https://renames.example.com/o/r.git"}, // never fetched: gh is the fake
	} {
		if out, err := exec.Command(gitBin, append([]string{"-C", repo}, args...)...).CombinedOutput(); err != nil {
			t.Fatalf("git %v: %v\n%s", args, err, out)
		}
	}
	ghDir := t.TempDir()
	if err := os.WriteFile(filepath.Join(ghDir, "gh"), []byte(renameGh), 0o755); err != nil {
		t.Fatal(err)
	}
	sep := string(os.PathListSeparator)
	t.Setenv("PATH", ghDir+sep+filepath.Dir(gitBin)+sep+"/usr/bin"+sep+"/bin")
	if got, err := exec.LookPath("gh"); err != nil || got != filepath.Join(ghDir, "gh") {
		t.Fatalf("gh resolves to %q (%v), not the fake", got, err)
	}
	t.Setenv("HOME", t.TempDir())
	t.Setenv("WT_MERGE_IS_DEPLOY", "1")
	t.Setenv("WT_MERGE_IS_DEPLOY_PATHS", "infra/**")
	t.Chdir(repo)
	postMergeSleep = func(time.Duration) {}
	liveChecksReads.sleep = func(time.Duration) {}
	realTTY := stdinIsTTY
	stdinIsTTY = func() bool { return false }
	t.Cleanup(func() {
		postMergeSleep, liveChecksReads.sleep = time.Sleep, time.Sleep
		stdinIsTTY = realTTY
	})

	const out = `{"f":"docs/x.yaml","p":"infra/x.yaml","s":"renamed"}` + "\n" + `{"f":"README.md","p":null,"s":"modified"}` + "\n"
	const within = `{"f":"docs/y.md","p":"docs/x.md","s":"renamed"}` + "\n"
	cases := []struct {
		name, diffnames, rest, gql, nfiles string
		restFail                           bool
		code                               int
		merged                             bool
		want                               []string
	}{
		{name: "renamed out of infra/: the gate sees infra/x.yaml",
			diffnames: "docs/x.yaml\nREADME.md\n", rest: out, nfiles: "2", code: 1,
			want: []string{"prod deploy not confirmed"}},
		{name: "renamed within docs/: no deploy path, no gate",
			diffnames: "docs/y.md\n", rest: within, nfiles: "1", code: 0, merged: true,
			want: []string{"changes no deploy-path files"}},
		// #221 review: a page the pagination lost must not read as a PR without its file
		{name: "the REST list does not add up to the PR's count: it gates",
			diffnames: "docs/y.md\n", rest: within, nfiles: "3", code: 1,
			want: []string{"REST listed 1 of the PR's 3 changed files", "gating to be safe", "prod deploy not confirmed"}},
		{name: "REST refused, GraphQL lists the rename: its old path is unknown, so it gates",
			diffnames: "docs/x.yaml\nREADME.md\n", restFail: true, nfiles: "2", code: 1,
			gql:  `{"total":2,"paths":["docs/x.yaml","README.md"],"renamed":["docs/x.yaml"]}`,
			want: []string{"renamed file(s) (docs/x.yaml)", "gating to be safe", "prod deploy not confirmed"}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			for name, text := range map[string]string{"diffnames": tc.diffnames, "rest": tc.rest, "gql": tc.gql, "nfiles": tc.nfiles} {
				if err := os.WriteFile(filepath.Join(ghDir, name), []byte(text), 0o644); err != nil {
					t.Fatal(err)
				}
			}
			for _, name := range []string{"argv", "calls", "merged", "rest.fail"} {
				_ = os.Remove(filepath.Join(ghDir, name))
			}
			if tc.restFail {
				if err := os.WriteFile(filepath.Join(ghDir, "rest.fail"), nil, 0o644); err != nil {
					t.Fatal(err)
				}
			}
			var code int
			stdout, stderr := captureT(t, func() { code = cmdMergePR([]string{"99999", "--keep"}) })
			if code != tc.code {
				t.Errorf("cmdMergePR = %d, want %d\nstdout: %s\nstderr: %s", code, tc.code, stdout, stderr)
			}
			for _, w := range tc.want {
				if !strings.Contains(stdout+stderr, w) {
					t.Errorf("output lacks %q\nstdout: %s\nstderr: %s", w, stdout, stderr)
				}
			}
			if _, err := os.Stat(filepath.Join(ghDir, "argv")); (err == nil) != tc.merged {
				t.Errorf("gh pr merge ran = %v, want %v", err == nil, tc.merged)
			}
		})
	}
}
