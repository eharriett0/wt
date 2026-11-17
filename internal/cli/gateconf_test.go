package cli

import (
	"encoding/json"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"runtime"
	"strings"
	"testing"
	"time"

	"github.com/eharriett0/wt/internal/config"
	"github.com/eharriett0/wt/internal/ghx"
)

// TestBaseGateSource pins where the base branch's gate settings come from
// (#223): GitHub's copy when it was read; else this clone's origin/<base>,
// with a warning; else, or for a forwarded -R, fail closed (deploy on for the
// whole repository, no floor invented).
func TestBaseGateSource(t *testing.T) {
	on := ghx.BaseConf{Base: "main", Commit: "0123456789abc", Found: true, Text: "merge_is_deploy = true\nmerge_is_deploy_paths = infra/**\nmerge_min_checks = 3\n"}
	unread := errors.New("HTTP 502")
	localRef := "refs/remotes/origin/main"
	cases := []struct {
		name      string
		fresh     ghx.BaseConf
		freshErr  error
		forwarded bool
		local     localBaseConf
		wantFrom  string
		want      config.Gate
		wantWarn  []string // "" = no warning
	}{
		{name: "GitHub's copy", fresh: on,
			wantFrom: "main's .wt.conf on GitHub (at 0123456)",
			want:     config.Gate{MergeIsDeploy: true, MergeIsDeployPaths: []string{"infra/**"}, MergeMinChecks: 3}},
		{name: "GitHub says the base has no .wt.conf", fresh: ghx.BaseConf{Base: "main", Commit: "abc"},
			wantFrom: "main on GitHub, which has no .wt.conf"},
		{name: "GitHub unread: this clone's copy, with a warning", freshErr: unread,
			local:    localBaseConf{ref: localRef, found: true, text: "merge_is_deploy = true\n"},
			wantFrom: "origin/main's .wt.conf as last fetched", want: config.Gate{MergeIsDeploy: true},
			wantWarn: []string{"HTTP 502", "origin/main as last fetched, which may be stale"}},
		{name: "GitHub unread, this clone's copy has no .wt.conf", freshErr: unread,
			local:    localBaseConf{ref: localRef},
			wantFrom: "origin/main as last fetched, which has no .wt.conf",
			wantWarn: []string{"which may be stale"}},
		{name: "GitHub unread, no copy here: fail closed", freshErr: unread,
			local:    localBaseConf{ref: localRef, err: errors.New("refs/remotes/origin/main does not resolve to a commit here")},
			wantFrom: "the base branch's .wt.conf, which could not be read", want: config.Gate{MergeIsDeploy: true},
			wantWarn: []string{"HTTP 502", "does not resolve", "so it gates"}},
		{name: "GitHub unread for a forwarded -R: fail closed, the clone is another repository", freshErr: unread, forwarded: true,
			local:    localBaseConf{ref: localRef, found: true, text: "merge_is_deploy = false\n"},
			wantFrom: "the base branch's .wt.conf, which could not be read", want: config.Gate{MergeIsDeploy: true},
			wantWarn: []string{"(-R)", "so it gates"}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			src, warn := baseGateSource(tc.fresh, tc.freshErr, tc.forwarded, tc.local)
			if src.from != tc.wantFrom {
				t.Errorf("from = %q, want %q", src.from, tc.wantFrom)
			}
			if !reflect.DeepEqual(src.gate, tc.want) {
				t.Errorf("gate = %+v, want %+v", src.gate, tc.want)
			}
			if (warn == "") != (len(tc.wantWarn) == 0) {
				t.Errorf("warn = %q, want one: %v", warn, len(tc.wantWarn) > 0)
			}
			for _, w := range tc.wantWarn {
				if !strings.Contains(warn, w) {
					t.Errorf("warn lacks %q: %q", w, warn)
				}
			}
		})
	}
}

func TestStrictestFloor(t *testing.T) {
	src := func(n int, bad string) gateSource {
		return gateSource{gate: config.Gate{MergeMinChecks: n, MergeMinChecksBad: bad}}
	}
	cases := []struct {
		name string
		srcs []gateSource
		n    int
		bad  string
	}{
		{"none", []gateSource{src(0, ""), src(0, "")}, 0, ""},
		{"the checkout lowers the base's floor: the base's", []gateSource{src(0, ""), src(3, "")}, 3, ""},
		{"the checkout raises it: its own", []gateSource{src(5, ""), src(3, "")}, 5, ""},
		{"a value that is not a count, in the base: refused", []gateSource{src(2, ""), src(0, "three")}, 0, "three"},
		{"a value that is not a count, in the checkout: refused", []gateSource{src(0, "x"), src(3, "")}, 0, "x"},
	}
	for _, tc := range cases {
		if n, bad := strictestFloor(tc.srcs); n != tc.n || bad != tc.bad {
			t.Errorf("%s: strictestFloor = (%d, %q), want (%d, %q)", tc.name, n, bad, tc.n, tc.bad)
		}
	}
}

// TestDeployDecision pins the deploy gate over two sources (#223): ANY source
// that turns merge_is_deploy on and gates the PR fires it, so neither side can
// loosen the other's gate.
func TestDeployDecision(t *testing.T) {
	src := func(from string, deploy bool, paths ...string) gateSource {
		return gateSource{from: from, gate: config.Gate{MergeIsDeploy: deploy, MergeIsDeployPaths: paths}}
	}
	cases := []struct {
		name      string
		srcs      []gateSource
		files     []string
		filesErr  error
		needFiles bool
		fires     bool
		by        string
	}{
		{name: "both off", srcs: []gateSource{src("co", false), src("base", false)}, files: []string{"infra/x"}},
		{name: "the checkout turns it off, the base on: fires",
			srcs: []gateSource{src("co", false, "infra/**"), src("base", true, "infra/**")}, files: []string{"infra/x"},
			needFiles: true, fires: true, by: "base"},
		{name: "a glob only the base has: fires",
			srcs: []gateSource{src("co", true, "infra/**"), src("base", true, "infra/**", "newtree/**")}, files: []string{"newtree/a.yaml"},
			needFiles: true, fires: true, by: "base"},
		{name: "a glob only the checkout has: fires (it tightens)",
			srcs: []gateSource{src("co", true, "infra/**", "extra/**"), src("base", true, "infra/**")}, files: []string{"extra/a"},
			needFiles: true, fires: true, by: "co"},
		{name: "the checkout's exclusion can't carve a file out of the base's gate",
			srcs: []gateSource{src("co", true, "infra/**", "!infra/**/*.yaml"), src("base", true, "infra/**")}, files: []string{"infra/x.yaml"},
			needFiles: true, fires: true, by: "base"},
		{name: "the base on for the whole repository: fires without reading files",
			srcs: []gateSource{src("co", true, "infra/**"), src("base", true)}, files: nil, fires: true, by: "base"},
		{name: "no source matches: skips",
			srcs: []gateSource{src("co", true, "infra/**"), src("base", true, "infra/**")}, files: []string{"docs/a.md"},
			needFiles: true},
		{name: "files unreadable: fires (fail closed)",
			srcs: []gateSource{src("co", false), src("base", true, "infra/**")}, filesErr: errors.New("x"),
			needFiles: true, fires: true},
		{name: "no files listed: fires (fail closed)",
			srcs: []gateSource{src("co", false), src("base", true, "infra/**")}, files: []string{},
			needFiles: true, fires: true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := gateNeedsFiles(tc.srcs); got != tc.needFiles {
				t.Errorf("gateNeedsFiles = %v, want %v", got, tc.needFiles)
			}
			fires, by := deployDecision(tc.srcs, tc.files, tc.filesErr)
			if fires != tc.fires || by != tc.by {
				t.Errorf("deployDecision = (%v, %q), want (%v, %q)", fires, by, tc.fires, tc.by)
			}
		})
	}
}

// baseGateGh is gh for #223's end-to-end test: open PR 99999 in o/r on its own
// host (so ghx's process-wide auth memo, #172, stays apart), base main. The
// base's .wt.conf is the JSON in "baseconf.json" (ReadBaseConf's --jq form),
// and its read fails when "base.fail" exists. The PR's files are "rest" (REST
// pulls/{n}/files lines), "nfiles" and "diffnames". One check passed, nothing
// is required. Every call goes to "calls", a merge's argv to "argv".
const baseGateGh = `#!/bin/sh
d=$(dirname "$0")
H=basegate.example.com
printf '%s\n' "$*" >> "$d/calls"
case "$*" in
"auth status --json hosts --active"*) printf '{"hosts":{"%s":[{"state":"success","active":true,"host":"%s","login":"octo"}]}}\n' $H $H; exit 0 ;;
"auth status"*) exit 0 ;;
*"--json number,url,baseRefName"*) echo "$3 https://$H/o/r/pull/$3 main"; exit 0 ;;
*"expr=refs/heads/main:.wt.conf"*)
	[ -e "$d/base.fail" ] && { echo "gh: Something went wrong (HTTP 502)" >&2; exit 1; }
	cat "$d/baseconf.json"; exit 0 ;;
"api --hostname $H repos/o/r/pulls/99999/files"*) cat "$d/rest"; exit 0 ;;
"api --include /"*) exit 1 ;;
"api --hostname $H repos/o/r/branches/"*) echo '` + unprotected + `'; exit 0 ;;
"api --hostname $H repos/o/r/rules/branches/"*) echo '[]'; exit 0 ;;
*"--json number,url,changedFiles"*) echo "$3 https://$H/o/r/pull/$3 $(cat "$d/nfiles")"; exit 0 ;;
*"--json number,url"*) echo "$3 https://$H/o/r/pull/$3"; exit 0 ;;
*statusCheckRollup*) echo '` + greenRollup + `'; exit 0 ;;
esac
case "$1 $2" in
"pr view")
	case "$*" in
	*" state "*) if [ -e "$d/merged" ]; then echo MERGED; else echo OPEN; fi ;;
	*isDraft*) echo false ;;
	*messageHeadline*) echo "Change the widget" ;;
	*headRefName*) echo feat-x ;;
	*title*) echo "Change the widget" ;;
	*" body "*) echo "Refs #3" ;;
	*" url "*) echo "https://$H/o/r/pull/99999" ;;
	esac ;;
"pr diff") cat "$d/diffnames" ;;
"api graphql")
	case "$*" in
	*squashMergeCommitTitle*) echo '{"title":"COMMIT_OR_PR_TITLE","message":"COMMIT_MESSAGES"}' ;;
	*parents*) echo '{"message":"Change the widget","parents":1}' ;;
	esac ;;
"issue view") echo OPEN ;;
"pr merge") printf '%s\n' "$@" > "$d/argv"; : > "$d/merged" ;;
esac
exit 0
`

// TestCmdMergePR_gateSettingsComeFromTheBaseToo is #223 end to end: merge-pr
// runs in the PR's own worktree, whose .wt.conf is the branch being merged.
// It read its gates from that file alone, so a PR turning merge_is_deploy off,
// lowering merge_min_checks, or cut before the base added a deploy glob merged
// without the gate. The base branch's .wt.conf (GitHub's copy, else this
// clone's origin/main, else fail closed) now counts too, and the stricter
// applies.
func TestCmdMergePR_gateSettingsComeFromTheBaseToo(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("the fake gh is a shell script")
	}
	gitBin, err := exec.LookPath("git")
	if err != nil {
		t.Skip("no git")
	}
	hermeticGitT(t)
	repo := t.TempDir()
	git := func(args ...string) string {
		t.Helper()
		out, err := exec.Command(gitBin, append([]string{"-C", repo}, args...)...).CombinedOutput()
		if err != nil {
			t.Fatalf("git %v: %v\n%s", args, err, out)
		}
		return strings.TrimSpace(string(out))
	}
	git("init", "-q", "-b", "main")
	git("remote", "add", "origin", "https://basegate.example.com/o/r.git") // never fetched: gh is the fake
	// Two commits a test can point origin/main at: one whose .wt.conf gates
	// infra/**, one without a .wt.conf.
	git("-c", "user.name=t", "-c", "user.email=t@t", "commit", "-q", "--allow-empty", "-m", "no conf")
	noConf := git("rev-parse", "HEAD")
	if err := os.WriteFile(filepath.Join(repo, ".wt.conf"), []byte("merge_is_deploy = true\nmerge_is_deploy_paths = infra/**\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	git("add", ".wt.conf")
	git("-c", "user.name=t", "-c", "user.email=t@t", "commit", "-q", "-m", "gate infra")
	gateInfra := git("rev-parse", "HEAD")

	ghDir := t.TempDir()
	if err := os.WriteFile(filepath.Join(ghDir, "gh"), []byte(baseGateGh), 0o755); err != nil {
		t.Fatal(err)
	}
	sep := string(os.PathListSeparator)
	t.Setenv("PATH", ghDir+sep+filepath.Dir(gitBin)+sep+"/usr/bin"+sep+"/bin")
	if got, err := exec.LookPath("gh"); err != nil || got != filepath.Join(ghDir, "gh") {
		t.Fatalf("gh resolves to %q (%v), not the fake", got, err)
	}
	t.Setenv("HOME", t.TempDir())
	for _, k := range []string{"WT_MERGE_IS_DEPLOY", "WT_MERGE_IS_DEPLOY_PATHS", "WT_MERGE_MIN_CHECKS", "WT_BASE"} {
		t.Setenv(k, "")
		os.Unsetenv(k) // the _PATHS key counts when set at all, even empty
	}
	t.Chdir(repo)
	postMergeSleep = func(time.Duration) {}
	liveChecksReads.sleep = func(time.Duration) {}
	realTTY := stdinIsTTY
	stdinIsTTY = func() bool { return false }
	t.Cleanup(func() {
		postMergeSleep, liveChecksReads.sleep = time.Sleep, time.Sleep
		stdinIsTTY = realTTY
	})

	baseJSON := func(text string, found bool) string {
		v := map[string]any{"oid": fakeHead, "conf": nil}
		if found {
			v["conf"] = map[string]any{"__typename": "Blob", "text": text, "isBinary": false, "isTruncated": false}
		}
		b, err := json.Marshal(v)
		if err != nil {
			t.Fatal(err)
		}
		return string(b)
	}
	const gateOn = "merge_is_deploy = true\nmerge_is_deploy_paths = infra/**\n"
	cases := []struct {
		name      string
		checkout  string // this checkout's .wt.conf; "" = none
		base      string // the base's on GitHub; "" = none
		baseFail  bool   // GitHub's read fails
		originAt  string // what origin/main points at; "" = no origin/main
		file      string // the PR's one changed file
		dryRun    bool
		code      int
		merged    bool
		want, not []string
	}{
		{name: "the checkout turns the deploy gate off, the base has it on: gates",
			checkout: "merge_is_deploy = false\nmerge_is_deploy_paths = infra/**\n", base: gateOn, file: "infra/x.yaml", code: 1,
			want: []string{"the stricter of the two applies", "this checkout's settings alone would skip the prod gate", "prod deploy not confirmed"}},
		{name: "a branch older than the base's newest deploy glob: gates",
			checkout: gateOn, base: "merge_is_deploy = true\nmerge_is_deploy_paths = infra/**,newtree/**\n", file: "newtree/a.yaml", code: 1,
			want: []string{"main's .wt.conf on GitHub (at 0123456): merge_is_deploy on (2 merge_is_deploy_paths glob(s))", "prod deploy not confirmed"}},
		{name: "the checkout lowers the checks floor: held to the base's",
			checkout: "merge_min_checks = off\n", base: "merge_min_checks = 3\n", file: "docs/a.md", code: 1,
			want: []string{"fewer than merge_min_checks = 3"}},
		{name: "the checkout tightens the gate: held to its own",
			checkout: "merge_is_deploy = true\n", file: "docs/a.md", code: 1,
			want: []string{"prod deploy not confirmed"}, not: []string{"alone would skip"}},
		{name: "both scope the gate and the PR is outside it: merges",
			checkout: gateOn, base: gateOn, file: "docs/a.md", code: 0, merged: true,
			want: []string{"changes no deploy-path files"}, not: []string{"differ"}},
		{name: "GitHub unread, no origin/main here: fails closed",
			baseFail: true, file: "docs/a.md", code: 1,
			want: []string{"couldn't read the base branch's .wt.conf from GitHub", "HTTP 502", "so it gates", "prod deploy not confirmed"}},
		{name: "GitHub unread: this clone's origin/main copy decides, with a warning",
			checkout: "merge_is_deploy = false\n", baseFail: true, originAt: "gateInfra", file: "infra/x.yaml", code: 1,
			want: []string{"which may be stale", "origin/main's .wt.conf as last fetched", "prod deploy not confirmed"}},
		{name: "GitHub unread, origin/main has no .wt.conf: merges, with a warning",
			baseFail: true, originAt: "noConf", file: "infra/x.yaml", code: 0, merged: true,
			want: []string{"which may be stale"}, not: []string{"prod deploy not confirmed"}},
		{name: "--dry-run names both sources",
			checkout: "merge_is_deploy = false\n", base: gateOn, file: "infra/x.yaml", dryRun: true, code: 0,
			want: []string{"this checkout's .wt.conf and WT_* environment: merge_is_deploy off, merge_min_checks off",
				"main's .wt.conf on GitHub (at 0123456): merge_is_deploy on (1 merge_is_deploy_paths glob(s)), merge_min_checks off",
				"a real merge stops at the deploy gate"}},
		{name: "--dry-run with nothing set still says where the settings came from",
			file: "infra/x.yaml", dryRun: true, code: 0,
			want: []string{"merge-pr's gate settings:", "main on GitHub, which has no .wt.conf: merge_is_deploy off"}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			switch tc.originAt {
			case "":
				_ = exec.Command(gitBin, "-C", repo, "update-ref", "-d", "refs/remotes/origin/main").Run()
			case "gateInfra":
				git("update-ref", "refs/remotes/origin/main", gateInfra)
			case "noConf":
				git("update-ref", "refs/remotes/origin/main", noConf)
			}
			// this checkout's .wt.conf, on disk (config.Load reads the file)
			conf := filepath.Join(repo, ".wt.conf")
			if tc.checkout == "" {
				_ = os.Remove(conf)
			} else if err := os.WriteFile(conf, []byte(tc.checkout), 0o644); err != nil {
				t.Fatal(err)
			}
			for _, name := range []string{"argv", "calls", "merged", "base.fail"} {
				_ = os.Remove(filepath.Join(ghDir, name))
			}
			rest, _ := json.Marshal(map[string]any{"f": tc.file, "p": nil, "s": "modified"})
			files := map[string]string{"baseconf.json": baseJSON(tc.base, tc.base != ""), "rest": string(rest) + "\n",
				"nfiles": "1", "diffnames": tc.file + "\n"}
			if tc.baseFail {
				files["base.fail"] = ""
			}
			for name, text := range files {
				if err := os.WriteFile(filepath.Join(ghDir, name), []byte(text), 0o644); err != nil {
					t.Fatal(err)
				}
			}
			args := []string{"99999", "--keep"}
			if tc.dryRun {
				args = append(args, "--dry-run")
			}
			var code int
			stdout, stderr := captureT(t, func() { code = cmdMergePR(args) })
			if code != tc.code {
				t.Errorf("cmdMergePR = %d, want %d\nstdout: %s\nstderr: %s", code, tc.code, stdout, stderr)
			}
			for _, w := range tc.want {
				if !strings.Contains(stdout+stderr, w) {
					t.Errorf("output lacks %q\nstdout: %s\nstderr: %s", w, stdout, stderr)
				}
			}
			for _, w := range tc.not {
				if strings.Contains(stdout+stderr, w) {
					t.Errorf("output has %q\nstdout: %s\nstderr: %s", w, stdout, stderr)
				}
			}
			if _, err := os.Stat(filepath.Join(ghDir, "argv")); (err == nil) != tc.merged {
				t.Errorf("gh pr merge ran = %v, want %v", err == nil, tc.merged)
			}
		})
	}
}
