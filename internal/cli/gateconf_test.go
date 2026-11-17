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
	"github.com/eharriett0/wt/internal/coord"
	"github.com/eharriett0/wt/internal/ghx"
)

// TestBaseGateSource pins where the base branch's gate settings come from
// (#223): GitHub's copy when it was read; else this clone's origin/<base>,
// fetched first, but only for the base branch GitHub named and only when the
// PR's repository is this clone's origin; else fail closed.
func TestBaseGateSource(t *testing.T) {
	on := ghx.BaseConf{Base: "main", Owner: "o", Name: "r", Commit: "0123456789abc", Found: true,
		Text: "merge_is_deploy = true\nmerge_is_deploy_paths = infra/**\nmerge_min_checks = 3\n"}
	resolved := ghx.BaseConf{Base: "release", Owner: "o", Name: "r", Host: "github.com"} // gh named the PR; the conf read failed
	unread := errors.New("HTTP 502")
	ref := "refs/remotes/origin/release"
	defaults := config.GateFromConf("")
	cases := []struct {
		name         string
		fresh        ghx.BaseConf
		freshErr     error
		originIsBase bool
		local        localBaseConf
		wantFrom     string
		want         config.Gate
		wantUnread   bool
		wantWarn     []string // none = no warning
	}{
		{name: "GitHub's copy", fresh: on,
			wantFrom: "main's .wt.conf on GitHub (at 0123456)",
			want: config.Gate{MergeIsDeploy: true, MergeIsDeployPaths: []string{"infra/**"}, MergeMinChecks: 3,
				HoldMaxAge: config.DefaultHoldMaxAge}},
		{name: "GitHub says the base has no .wt.conf: defaults", fresh: ghx.BaseConf{Base: "main", Commit: "abc"},
			wantFrom: "main on GitHub, which has no .wt.conf", want: defaults},
		{name: "GitHub unread: the PR's base, fetched just now", fresh: resolved, freshErr: unread, originIsBase: true,
			local:    localBaseConf{ref: ref, found: true, text: "merge_is_deploy = true\n"},
			wantFrom: "origin/release's .wt.conf fetched just now", want: config.Gate{MergeIsDeploy: true, HoldMaxAge: config.DefaultHoldMaxAge},
			wantWarn: []string{"HTTP 502", "origin/release, fetched just now"}},
		{name: "GitHub unread and the fetch failed: as last fetched, may be stale", fresh: resolved, freshErr: unread, originIsBase: true,
			local:    localBaseConf{ref: ref, fetchErr: errors.New("offline"), found: true, text: "merge_is_deploy = true\n"},
			wantFrom: "origin/release's .wt.conf as last fetched", want: config.Gate{MergeIsDeploy: true, HoldMaxAge: config.DefaultHoldMaxAge},
			wantWarn: []string{"offline", "which may be stale"}},
		{name: "GitHub unread, this clone's copy has no .wt.conf", fresh: resolved, freshErr: unread, originIsBase: true,
			local:    localBaseConf{ref: ref},
			wantFrom: "origin/release fetched just now, which has no .wt.conf", want: defaults,
			wantWarn: []string{"fetched just now"}},
		{name: "the base branch was never resolved: fail closed, never a guess", freshErr: unread,
			wantUnread: true, want: config.Gate{MergeIsDeploy: true},
			wantWarn: []string{"the PR's base branch could not be resolved", "checks gate refuses without --checks-ok"}},
		{name: "the PR's repository is not origin (-R, a URL, GH_REPO, a fork): fail closed", fresh: resolved, freshErr: unread,
			local:      localBaseConf{ref: ref, found: true, text: "merge_is_deploy = false\n"},
			wantUnread: true, want: config.Gate{MergeIsDeploy: true},
			wantWarn: []string{"o/r is not this clone's origin"}},
		{name: "this clone's copy can't be read (a symlink, an unfetched blob): fail closed", fresh: resolved, freshErr: unread, originIsBase: true,
			local:      localBaseConf{ref: ref, err: errors.New(".wt.conf in refs/remotes/origin/release is a symlink")},
			wantUnread: true, want: config.Gate{MergeIsDeploy: true},
			wantWarn: []string{"is a symlink"}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			src, warn := baseGateSource(tc.fresh, tc.freshErr, tc.originIsBase, tc.local)
			if tc.wantFrom != "" && src.from != tc.wantFrom {
				t.Errorf("from = %q, want %q", src.from, tc.wantFrom)
			}
			if !reflect.DeepEqual(src.gate, tc.want) {
				t.Errorf("gate = %+v, want %+v", src.gate, tc.want)
			}
			if (src.unread != "") != tc.wantUnread {
				t.Errorf("unread = %q, want unread: %v", src.unread, tc.wantUnread)
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
	src := func(from string, n int, bad string) gateSource {
		return gateSource{from: from, gate: config.Gate{MergeMinChecks: n, MergeMinChecksBad: bad}}
	}
	cases := []struct {
		name string
		srcs []gateSource
		want floorSpec
	}{
		{"none", []gateSource{src("co", 0, ""), src("base", 0, "")}, floorSpec{}},
		{"the checkout lowers the base's floor: the base's", []gateSource{src("co", 0, ""), src("base", 3, "")}, floorSpec{n: 3}},
		{"the checkout raises it: its own", []gateSource{src("co", 5, ""), src("base", 3, "")}, floorSpec{n: 5}},
		{"a value that is not a count, in the base: refused, and says where", []gateSource{src("co", 2, ""), src("base", 0, "three")},
			floorSpec{bad: "three", badFrom: "base"}},
		{"a value that is not a count, in the checkout", []gateSource{src("co", 0, "x"), src("base", 3, "")},
			floorSpec{bad: "x", badFrom: "co"}},
		// #223 review: an unread base's floor is unknown, not none.
		{"an unread base: the floor is unknown", []gateSource{src("co", 0, ""), {from: "base", gate: config.Gate{MergeIsDeploy: true}, unread: "GitHub down"}},
			floorSpec{unread: "GitHub down"}},
	}
	for _, tc := range cases {
		if got := strictestFloor(tc.srcs); got != tc.want {
			t.Errorf("%s: strictestFloor = %+v, want %+v", tc.name, got, tc.want)
		}
	}
}

// TestStrictestHolds pins the hold gate's settings over both sources (#223
// review): hold_max_age 0 (never) wins, else the longest; every coord_issue
// counts; an unread source sets nothing.
func TestStrictestHolds(t *testing.T) {
	src := func(age time.Duration, issue int) gateSource {
		return gateSource{gate: config.Gate{HoldMaxAge: age, CoordIssue: issue}}
	}
	cases := []struct {
		name   string
		srcs   []gateSource
		age    time.Duration
		issues []int
	}{
		{"the checkout shortens hold_max_age: the base's", []gateSource{src(time.Minute, 0), src(24*time.Hour, 0)}, 24 * time.Hour, nil},
		{"never expire beats any age", []gateSource{src(time.Hour, 0), src(0, 0)}, 0, nil},
		{"the checkout drops coord_issue: the base's mirror still read", []gateSource{src(time.Hour, 0), src(time.Hour, 36)}, time.Hour, []int{36}},
		{"two mirrors: both read, once each", []gateSource{src(time.Hour, 7), src(time.Hour, 36), src(time.Hour, 7)}, time.Hour, []int{7, 36}},
		{"an unread base sets nothing", []gateSource{src(time.Minute, 0), {gate: config.Gate{MergeIsDeploy: true}, unread: "x"}}, time.Minute, nil},
		{"no source read: the default", []gateSource{{unread: "x"}}, config.DefaultHoldMaxAge, nil},
	}
	for _, tc := range cases {
		age, issues := strictestHolds(tc.srcs)
		if age != tc.age || !reflect.DeepEqual(issues, tc.issues) {
			t.Errorf("%s: strictestHolds = (%v, %v), want (%v, %v)", tc.name, age, issues, tc.age, tc.issues)
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
		// #223 review: `merge_is_deploy_paths = !` (a typo) excluded nothing and
		// included nothing, so it never gated.
		{name: "a bare ! is an exclusions-only list: fails closed",
			srcs: []gateSource{src("co", false), src("base", true, "!")}, files: []string{"infra/x.yaml"},
			needFiles: true, fires: true, by: "base"},
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

// A bare `!`, alone or repeated, is an exclusions-only list, and an empty
// exclusion matches nothing next to a real include (#223 review).
func TestAnyDeployPath_bareBang(t *testing.T) {
	for _, globs := range [][]string{{"!"}, {"! "}, {"!", "!"}} {
		if !anyDeployPath([]string{"infra/x.yaml"}, globs) {
			t.Errorf("anyDeployPath(%q) = false: the gate never fires", globs)
		}
	}
	if !anyDeployPath([]string{"infra/x.yaml"}, []string{"infra/**", "!"}) {
		t.Error("an empty exclusion beside an include excluded a deploy path")
	}
}

func TestGateSummaryAndDiffer(t *testing.T) {
	a := gateSource{from: "co", gate: config.Gate{MergeIsDeploy: true, MergeIsDeployPaths: []string{"infra/**"}, HoldMaxAge: time.Hour}}
	b := gateSource{from: "base", gate: config.Gate{MergeIsDeploy: true, MergeIsDeployPaths: []string{"infrastructure/**"}, HoldMaxAge: time.Hour, CoordIssue: 36}}
	off := gateSource{from: "off", gate: config.Gate{MergeIsDeployPaths: []string{"infra/**"}}}
	for _, tc := range []struct {
		s    gateSource
		want string
	}{
		{a, "co: merge_is_deploy on [merge_is_deploy_paths infra/**], merge_min_checks off, hold_max_age 1h0m0s"},
		{b, "base: merge_is_deploy on [merge_is_deploy_paths infrastructure/**], merge_min_checks off, hold_max_age 1h0m0s, coord_issue #36"},
		{off, "off: merge_is_deploy off [merge_is_deploy_paths infra/**], merge_min_checks off, hold_max_age never"},
		{gateSource{from: "u", unread: "GitHub down", gate: config.Gate{MergeIsDeploy: true}},
			"u (GitHub down): taken as merge_is_deploy on (whole repository), merge_min_checks unknown"},
	} {
		if got := gateSummary(tc.s); got != tc.want {
			t.Errorf("gateSummary = %q, want %q", got, tc.want)
		}
	}
	if gatesDiffer([]gateSource{a, a}) || !gatesDiffer([]gateSource{a, b}) || gatesDiffer(nil) || gatesDiffer([]gateSource{a}) {
		t.Error("gatesDiffer: equal sources differ, different ones don't, or an empty list panics")
	}
	if !gatesDiffer([]gateSource{a, {gate: a.gate, unread: "x"}}) {
		t.Error("an unread source must always read as different")
	}
}

// baseGateGh is gh for #223's end-to-end tests: open PR 99999 on its own host
// (so ghx's process-wide auth memo, #172, stays apart). Files steer it:
// "basename" (the PR's base, default main), "prrepo" (its repository, default
// o/r), "prview.fail" (gh can't resolve the PR), "base.fail" (the .wt.conf
// read fails), "baseconf.json" (ReadBaseConf's --jq answer), "rest",
// "nfiles", "diffnames" (the PR's files). One check passed, nothing is
// required. Every call goes to "calls", a merge's argv to "argv".
const baseGateGh = `#!/bin/sh
d=$(dirname "$0")
H=basegate.example.com
printf '%s\n' "$*" >> "$d/calls"
B=main; [ -e "$d/basename" ] && B=$(cat "$d/basename")
R=o/r; [ -e "$d/prrepo" ] && R=$(cat "$d/prrepo")
case "$*" in
"auth status --json hosts --active"*) printf '{"hosts":{"%s":[{"state":"success","active":true,"host":"%s","login":"octo"}]}}\n' $H $H; exit 0 ;;
"auth status"*) exit 0 ;;
*"--json number,url,baseRefName"*)
	[ -e "$d/prview.fail" ] && { echo "gh: Something went wrong (HTTP 502)" >&2; exit 1; }
	echo "$3 https://$H/$R/pull/$3 $B"; exit 0 ;;
*"expr=refs/heads/"*)
	[ -e "$d/base.fail" ] && { echo "gh: Something went wrong (HTTP 502)" >&2; exit 1; }
	cat "$d/baseconf.json"; exit 0 ;;
"api --hostname $H repos/"*"/pulls/99999/files"*) cat "$d/rest"; exit 0 ;;
"api --include /"*) exit 1 ;;
"api --hostname $H repos/"*"/rules/branches/"*) echo '[]'; exit 0 ;;
"api --hostname $H repos/"*"/branches/"*) echo '` + unprotected + `'; exit 0 ;;
*"--json number,url,changedFiles"*) echo "$3 https://$H/$R/pull/$3 $(cat "$d/nfiles")"; exit 0 ;;
*"--json number,url"*) echo "$3 https://$H/$R/pull/$3"; exit 0 ;;
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
	*" url "*) echo "https://$H/$R/pull/99999" ;;
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

// gateEnv is a scratch repo whose origin is o/r on baseGateGh's host, with the
// fake gh alone on PATH, an isolated HOME and no WT_* gate overrides.
type gateEnv struct {
	t     *testing.T
	repo  string
	ghDir string
	git   func(args ...string) string
}

func newGateEnv(t *testing.T) *gateEnv {
	t.Helper()
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
	git("remote", "add", "origin", "https://basegate.example.com/o/r.git") // never fetched: fetchBase is stubbed
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
	for _, k := range []string{"WT_MERGE_IS_DEPLOY", "WT_MERGE_IS_DEPLOY_PATHS", "WT_MERGE_MIN_CHECKS", "WT_HOLD_MAX_AGE",
		"WT_COORD_ISSUE", "WT_BASE", "GH_REPO", "WT_SESSION", "CLAUDE_CODE_SESSION_ID", "CODEX_SESSION_ID", "WT_WINDOW"} {
		t.Setenv(k, "")
		os.Unsetenv(k) // the _PATHS key counts when set at all, even empty
	}
	t.Chdir(repo)
	postMergeSleep = func(time.Duration) {}
	liveChecksReads.sleep = func(time.Duration) {}
	realTTY, realFetch := stdinIsTTY, fetchBase
	stdinIsTTY = func() bool { return false }
	fetchBase = func(string) error { return errors.New("offline (test)") }
	t.Cleanup(func() {
		postMergeSleep, liveChecksReads.sleep = time.Sleep, time.Sleep
		stdinIsTTY, fetchBase = realTTY, realFetch
	})
	return &gateEnv{t: t, repo: repo, ghDir: ghDir, git: git}
}

// commit makes a commit whose tree holds .wt.conf = conf ("" = none) and
// returns it.
func (e *gateEnv) commit(conf string) string {
	e.t.Helper()
	p := filepath.Join(e.repo, ".wt.conf")
	if conf == "" {
		_ = os.Remove(p)
		e.git("rm", "-q", "--cached", "--ignore-unmatch", ".wt.conf")
	} else {
		if err := os.WriteFile(p, []byte(conf), 0o644); err != nil {
			e.t.Fatal(err)
		}
		e.git("add", ".wt.conf")
	}
	e.git("-c", "user.name=t", "-c", "user.email=t@t", "commit", "-q", "--allow-empty", "-m", "c")
	return e.git("rev-parse", "HEAD")
}

// checkout sets this checkout's .wt.conf on disk ("" = none): what
// config.Load reads, as in the PR's own worktree.
func (e *gateEnv) checkout(conf string) {
	e.t.Helper()
	p := filepath.Join(e.repo, ".wt.conf")
	if conf == "" {
		_ = os.Remove(p)
		return
	}
	if err := os.WriteFile(p, []byte(conf), 0o644); err != nil {
		e.t.Fatal(err)
	}
}

// fake sets the fake gh's state: the PR changes file, and kv's files.
func (e *gateEnv) fake(file string, kv map[string]string) {
	e.t.Helper()
	for _, name := range []string{"argv", "calls", "merged", "base.fail", "prview.fail", "basename", "prrepo", "baseconf.json"} {
		_ = os.Remove(filepath.Join(e.ghDir, name))
	}
	rest, _ := json.Marshal(map[string]any{"f": file, "p": nil, "s": "modified"})
	files := map[string]string{"rest": string(rest) + "\n", "nfiles": "1", "diffnames": file + "\n", "baseconf.json": baseJSON("", false)}
	for k, v := range kv {
		files[k] = v
	}
	for name, text := range files {
		if err := os.WriteFile(filepath.Join(e.ghDir, name), []byte(text), 0o644); err != nil {
			e.t.Fatal(err)
		}
	}
}

func (e *gateEnv) merge(args ...string) (code int, out string, merged bool) {
	e.t.Helper()
	stdout, stderr := captureT(e.t, func() { code = cmdMergePR(args) })
	_, err := os.Stat(filepath.Join(e.ghDir, "argv"))
	return code, stdout + stderr, err == nil
}

func (e *gateEnv) calls() string {
	b, _ := os.ReadFile(filepath.Join(e.ghDir, "calls"))
	return string(b)
}

// baseJSON is ReadBaseConf's --jq answer for a base whose .wt.conf is text
// (found) or absent, at the given mode (0 = a regular file).
func baseJSON(text string, found bool, mode ...int) string {
	v := map[string]any{"oid": fakeHead, "mode": nil, "conf": nil}
	if found {
		m := 0o100644
		if len(mode) > 0 {
			m = mode[0]
		}
		v["mode"] = m
		v["conf"] = map[string]any{"__typename": "Blob", "text": text, "isBinary": false, "isTruncated": false}
	}
	b, _ := json.Marshal(v)
	return string(b)
}

const gateInfra = "merge_is_deploy = true\nmerge_is_deploy_paths = infra/**\n"

// TestCmdMergePR_gateSettingsComeFromTheBaseToo is #223 end to end: merge-pr
// runs in the PR's own worktree, whose .wt.conf is the branch being merged.
// It read its gates from that file alone, so a PR turning merge_is_deploy off,
// lowering merge_min_checks, or cut before the base added a deploy glob merged
// without the gate. The base branch's .wt.conf (GitHub's copy, else this
// clone's origin/<base>, else fail closed) now counts too, and the stricter
// applies.
func TestCmdMergePR_gateSettingsComeFromTheBaseToo(t *testing.T) {
	e := newGateEnv(t)
	noConf := e.commit("")
	gated := e.commit(gateInfra)
	cases := []struct {
		name      string
		checkout  string // this checkout's .wt.conf; "" = none
		kv        map[string]string
		origin    string // what origin/main points at: "", "gated", "noConf"
		file      string // the PR's one changed file
		args      []string
		code      int
		merged    bool
		want, not []string
	}{
		{name: "the checkout turns the deploy gate off, the base has it on: gates",
			checkout: "merge_is_deploy = false\nmerge_is_deploy_paths = infra/**\n", kv: map[string]string{"baseconf.json": baseJSON(gateInfra, true)},
			file: "infra/x.yaml", code: 1,
			want: []string{"the stricter of the two applies", "this checkout's settings alone would skip the prod gate", "prod deploy not confirmed",
				"AUTO-APPLIES to prod"}},
		{name: "a branch older than the base's newest deploy glob: gates",
			checkout: gateInfra, kv: map[string]string{"baseconf.json": baseJSON("merge_is_deploy = true\nmerge_is_deploy_paths = infra/**,newtree/**\n", true)},
			file: "newtree/a.yaml", code: 1,
			want: []string{"main's .wt.conf on GitHub (at 0123456): merge_is_deploy on [merge_is_deploy_paths infra/**,newtree/**]", "prod deploy not confirmed"}},
		{name: "the checkout lowers the checks floor: held to the base's",
			checkout: "merge_min_checks = off\n", kv: map[string]string{"baseconf.json": baseJSON("merge_min_checks = 3\n", true)},
			file: "docs/a.md", code: 1, want: []string{"fewer than merge_min_checks = 3"}},
		{name: "the checkout tightens the gate: held to its own",
			checkout: "merge_is_deploy = true\n", file: "docs/a.md", code: 1,
			want: []string{"prod deploy not confirmed"}, not: []string{"alone would skip"}},
		{name: "both scope the gate and the PR is outside it: merges",
			checkout: gateInfra, kv: map[string]string{"baseconf.json": baseJSON(gateInfra, true)}, file: "docs/a.md", code: 0, merged: true,
			want: []string{"changes no deploy-path files"}, not: []string{"differ"}},
		{name: "GitHub unread, no origin/main here: fails closed, says MAY, and refuses the unknown floor",
			kv: map[string]string{"base.fail": ""}, file: "docs/a.md", args: []string{"--confirm-deploy"}, code: 1,
			want: []string{"this clone's copy can't be read", "merge_min_checks is unknown", "--checks-ok"}},
		{name: "... and with --checks-ok it stops at the deploy gate, which says MAY",
			kv: map[string]string{"base.fail": ""}, file: "docs/a.md", args: []string{"--checks-ok"}, code: 1,
			want: []string{"MAY auto-apply to prod", "prod deploy not confirmed"}, not: []string{"AUTO-APPLIES"}},
		{name: "GitHub unread: this clone's origin/main copy decides (the fetch failed: may be stale)",
			checkout: "merge_is_deploy = false\n", kv: map[string]string{"base.fail": ""}, origin: "gated", file: "infra/x.yaml", code: 1,
			want: []string{"which may be stale", "origin/main's .wt.conf as last fetched", "prod deploy not confirmed"}},
		{name: "GitHub unread, origin/main has no .wt.conf: merges, with a warning",
			kv: map[string]string{"base.fail": ""}, origin: "noConf", file: "infra/x.yaml", code: 0, merged: true,
			want: []string{"which may be stale"}, not: []string{"prod deploy not confirmed"}},
		// #223 review F1: the fallback read origin/<default branch>, not the PR's base.
		{name: "a PR into release, GitHub unread: the fallback reads origin/release",
			checkout: "merge_is_deploy = false\n", kv: map[string]string{"base.fail": "", "basename": "release"}, origin: "noConf",
			file: "infra/x.yaml", code: 1, want: []string{"origin/release's .wt.conf as last fetched", "prod deploy not confirmed"}},
		// #223 review F2: the PR's own `base =` steered the fallback.
		{name: "the checkout's base key never picks the fallback's branch",
			checkout: "merge_is_deploy = false\nbase = feat-x\n", kv: map[string]string{"base.fail": ""}, origin: "gated",
			file: "infra/x.yaml", code: 1, want: []string{"origin/main's .wt.conf", "prod deploy not confirmed"}, not: []string{"origin/feat-x"}},
		// #223 review F3: a PR in another repository (URL, GH_REPO, a fork's
		// default) read THIS clone's origin.
		{name: "a PR in another repository, GitHub unread: fails closed",
			kv: map[string]string{"base.fail": "", "prrepo": "other/repo"}, origin: "noConf", file: "infra/x.yaml",
			args: []string{"--checks-ok"}, code: 1, want: []string{"other/repo is not this clone's origin", "MAY auto-apply"}},
		// #223 review F5: a symlinked .wt.conf read as its target's path.
		{name: "the base's .wt.conf is a symlink: never read as a config with no keys",
			kv: map[string]string{"baseconf.json": baseJSON("conf/wt.conf", true, 0o120000)}, file: "conf/wt.conf",
			args: []string{"--checks-ok"}, code: 1, want: []string{"is a symlink", "prod deploy not confirmed"}},
		// #223 review: gh can't name the PR at all: never guess the base.
		{name: "gh can't resolve the PR: fails closed",
			kv: map[string]string{"prview.fail": ""}, origin: "gated", file: "docs/a.md", args: []string{"--checks-ok"}, code: 1,
			want: []string{"the PR's base branch could not be resolved"}},
		// #223 review: a passthrough that merges nothing deploys nothing.
		{name: "--disable-auto in a repo that never deploys, GitHub unread: no deploy gate",
			kv: map[string]string{"base.fail": ""}, file: "docs/a.md", args: []string{"--", "--disable-auto"},
			not: []string{"deploy not confirmed", "auto-apply", "AUTO-APPLIES"}},
		{name: "--disable-auto in a repo that deploys: disarming auto-merge deploys nothing",
			checkout: "merge_is_deploy = true\n", kv: map[string]string{"baseconf.json": baseJSON("merge_is_deploy = true\n", true)},
			file: "infra/x.yaml", args: []string{"--", "--disable-auto"},
			not: []string{"deploy not confirmed", "AUTO-APPLIES", "gate settings"}},
		{name: "--dry-run names both sources",
			checkout: "merge_is_deploy = false\n", kv: map[string]string{"baseconf.json": baseJSON(gateInfra, true)}, file: "infra/x.yaml",
			args: []string{"--dry-run"}, code: 0,
			want: []string{"this checkout's .wt.conf and WT_* environment: merge_is_deploy off, merge_min_checks off, hold_max_age 24h0m0s",
				"main's .wt.conf on GitHub (at 0123456): merge_is_deploy on [merge_is_deploy_paths infra/**], merge_min_checks off",
				"a real merge stops at the deploy gate"}},
		{name: "--dry-run with nothing set still says where the settings came from",
			file: "infra/x.yaml", args: []string{"--dry-run"}, code: 0,
			want: []string{"merge-pr's gate settings:", "main on GitHub, which has no .wt.conf: merge_is_deploy off"}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			switch tc.origin {
			case "":
				_ = exec.Command("git", "-C", e.repo, "update-ref", "-d", "refs/remotes/origin/main").Run()
				_ = exec.Command("git", "-C", e.repo, "update-ref", "-d", "refs/remotes/origin/release").Run()
			case "gated":
				e.git("update-ref", "refs/remotes/origin/main", gated)
				e.git("update-ref", "refs/remotes/origin/feat-x", noConf) // the PR's own branch, if anything read it
			case "noConf":
				e.git("update-ref", "refs/remotes/origin/main", noConf)
				e.git("update-ref", "refs/remotes/origin/release", gated)
			}
			e.checkout(tc.checkout)
			e.fake(tc.file, tc.kv)
			code, out, merged := e.merge(append([]string{"99999", "--keep"}, tc.args...)...)
			if code != tc.code {
				t.Errorf("cmdMergePR = %d, want %d\n%s", code, tc.code, out)
			}
			for _, w := range tc.want {
				if !strings.Contains(out, w) {
					t.Errorf("output lacks %q\n%s", w, out)
				}
			}
			for _, w := range tc.not {
				if strings.Contains(out, w) {
					t.Errorf("output has %q\n%s", w, out)
				}
			}
			if strings.Contains(strings.Join(tc.args, " "), "--disable-auto") {
				// A passthrough that merges nothing reads no gate settings at all.
				if strings.Contains(e.calls(), "baseRefName") {
					t.Errorf("--disable-auto read the base's settings:\n%s", e.calls())
				}
				return // gh "merges" in the fake either way; only the gates matter here
			}
			if merged != tc.merged {
				t.Errorf("gh pr merge ran = %v, want %v\n%s", merged, tc.merged, out)
			}
		})
	}
}

// #223 review F6: with a forwarded -R, the deploy gate's file list and draft
// read are of THAT repository's PR, not this clone's PR of the same number.
func TestCmdMergePR_forwardedRepoReadsThatPRsFiles(t *testing.T) {
	e := newGateEnv(t)
	e.fake("docs/a.md", map[string]string{"prrepo": "other/repo", "baseconf.json": baseJSON(gateInfra, true)})
	_, out, _ := e.merge("99999", "--keep", "--dry-run", "--", "-R", "other/repo")
	var listed bool
	for _, ln := range strings.Split(e.calls(), "\n") {
		if strings.Contains(ln, "number,url,changedFiles") {
			listed = true
			if !strings.Contains(ln, "-R other/repo") {
				t.Errorf("the deploy gate's file list read this clone's PR, not other/repo's: %q\n%s", ln, out)
			}
		}
	}
	if !listed {
		t.Errorf("the deploy gate never listed the PR's files\n%s", out)
	}
}

// #223 review: hold_max_age and coord_issue are gate settings too. A PR's own
// `hold_max_age = 1m` made another session's 2h-old hold stale, so it no
// longer blocked; the base's 24h now holds.
func TestCmdMergePR_holdSettingsComeFromTheBaseToo(t *testing.T) {
	e := newGateEnv(t)
	c, err := config.Load()
	if err != nil {
		t.Fatal(err)
	}
	logPath, self := coordCtx(c)
	hold := coord.Record{ID: "h1", Kind: coord.KindAnnounce, Window: self.Window, Session: "another-session",
		Hold: []string{"merge-main"}, Message: "rolling the cluster", TS: time.Now().Add(-2 * time.Hour).UTC().Format(time.RFC3339)}
	if err := coord.Append(logPath, hold); err != nil {
		t.Fatal(err)
	}
	t.Setenv("WT_SESSION", "this-session")
	for _, tc := range []struct {
		name     string
		checkout string
		base     string
		code     int
	}{
		{"the checkout shortens hold_max_age, the base keeps the default: the hold blocks", "hold_max_age = 1m\n", "", 1},
		{"both short: the hold is stale and warns", "hold_max_age = 1m\n", "hold_max_age = 1m\n", 0},
	} {
		t.Run(tc.name, func(t *testing.T) {
			e.checkout(tc.checkout)
			e.fake("docs/a.md", map[string]string{"baseconf.json": baseJSON(tc.base, tc.base != "")})
			code, out, _ := e.merge("99999", "--keep")
			if code != tc.code {
				t.Errorf("cmdMergePR = %d, want %d\n%s", code, tc.code, out)
			}
			if tc.code == 1 && !strings.Contains(out, "merge blocked") {
				t.Errorf("refused, but not by the hold:\n%s", out)
			}
		})
	}
}
