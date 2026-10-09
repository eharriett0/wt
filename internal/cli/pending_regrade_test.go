package cli

import (
	"encoding/json"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"github.com/eharriett0/wt/internal/collide"
	"github.com/eharriett0/wt/internal/config"
	"github.com/eharriett0/wt/internal/gitx"
)

// regradePending is the pre-edit hooks' block predicate: an entry fires iff `wt
// check` will grade it HIGH once the pending edit is made (#108/#184). Pure.
func TestRegradePending(t *testing.T) {
	other := spans(20, 20)
	blocking := CheckEntry{Path: "f", Window: "blk", Category: CatBlocking, Severity: "HIGH", OtherRanges: other}
	fyi := CheckEntry{Path: "f", Window: "fyi", Category: CatFYI, Severity: "low", OtherRanges: other}
	notHunkGraded := []CheckEntry{
		{Path: "f", Window: "stale", Category: CatStale, Severity: "low", OtherRanges: other},
		{Path: "f", Window: "doc", Category: CatAdvisory, Severity: "low"},
		{Path: "f", Window: "merged", Category: CatFYI, Severity: "low", AlreadyMerged: true},
		{Path: "f", Window: "untracked", Category: CatFYI, Severity: "low", Untracked: true},
		{Path: "f", Window: "subsumed", Category: CatFYI, Severity: "low", Subsumed: true, OtherRanges: other},
		{Path: "f", Window: "appendonly", Category: CatFYI, Severity: "low"},
	}
	indeterminate := CheckEntry{Path: "f", Window: "indet", Category: CatBlocking, Severity: "HIGH"}
	section := CheckEntry{Path: "f", Window: "sect", Category: CatBlocking, Severity: "HIGH", SharedSections: []string{"## A"}}

	type kept map[string]bool // window → confirmed
	never := func(CheckEntry) bool { return false }
	cases := []struct {
		name      string
		entries   []CheckEntry
		cur       []gitx.LineRange
		curOK     bool
		pending   []gitx.LineRange
		pendingOK bool
		subsumed  func(CheckEntry) bool
		want      kept
	}{
		{"pending overlaps: HIGH entry fires, confirmed", []CheckEntry{blocking}, nil, true, spans(20, 20), true, never, kept{"blk": true}},
		{"pending disjoint: HIGH entry dropped", []CheckEntry{blocking}, nil, true, spans(30, 30), true, never, kept{}},
		{"FYI entry re-graded: pending overlaps → fires", []CheckEntry{fyi}, spans(35, 35), true, spans(20, 20), true, never, kept{"fyi": true}},
		{"FYI entry, pending disjoint: stays silent", []CheckEntry{fyi}, spans(35, 35), true, spans(30, 30), true, never, kept{}},
		{"own earlier edit already overlaps: still HIGH after", []CheckEntry{blocking}, spans(19, 21), true, spans(30, 30), true, never, kept{"blk": true}},
		{"newly HIGH but #122 subsumed: dropped", []CheckEntry{fyi}, nil, true, spans(20, 20), true, func(CheckEntry) bool { return true }, kept{}},
		{"own ranges unknown: conservative heads-up", []CheckEntry{blocking, fyi}, nil, false, spans(30, 30), true, never, kept{"blk": false, "fyi": false}},
		{"pending unlocatable: conservative heads-up", []CheckEntry{blocking, fyi}, nil, true, nil, false, never, kept{"blk": false, "fyi": false}},
		{"no line ranges to grade: heads-up for a HIGH", []CheckEntry{indeterminate, section}, nil, true, spans(30, 30), true, never, kept{"indet": false, "sect": false}},
		{"entries no edit can make HIGH stay out", notHunkGraded, nil, false, nil, false, never, kept{}},
	}
	for _, c := range cases {
		got := kept{}
		for _, g := range regradePending(c.entries, c.cur, c.curOK, c.pending, c.pendingOK, c.subsumed) {
			got[g.entry.Window] = g.confirmed
		}
		if !reflect.DeepEqual(got, c.want) {
			t.Errorf("%s: kept %v, want %v", c.name, got, c.want)
		}
	}

	// #122's check is wt check's, applied only where wt check applies it: to an
	// entry that becomes HIGH. A disjoint entry, or one already HIGH before the
	// edit (checked when graded), never pays for it.
	calls := 0
	count := func(CheckEntry) bool { calls++; return false }
	regradePending([]CheckEntry{blocking, fyi}, nil, true, spans(30, 30), true, count)
	regradePending([]CheckEntry{blocking}, nil, true, spans(20, 20), true, count)
	if calls != 0 {
		t.Errorf("subsumed called %d times for entries that don't newly become HIGH, want 0", calls)
	}
}

// The pre-edit hooks end to end, driven by their stdin payloads in a scratch
// repo where this window (wc) is BEHIND base: base inserted 3 lines above the
// region after wc and wa forked, so wc's on-disk line numbers are 3 below base's.
// wa (also behind) edited "l20" (base line 23). Each hook must say exactly what
// `wt check` says once the edit is made: on-disk numbers taken as base numbers
// flag the "l23" edit (on-disk line 23) and miss nothing else; re-grading only
// HIGH entries misses an overlap once wc has a disjoint edit of its own.
func TestPreEditHooks_BehindWindow(t *testing.T) {
	wc := behindHookRepo(t)
	for _, ownEdit := range []bool{false, true} {
		if ownEdit {
			writeT(t, wc, "data.txt", strings.Replace(readT(t, wc, "data.txt"), "l35\n", "C35\n", 1))
			gitT(t, wc, "commit", "-qam", "wc edits l35 (disjoint from wa)")
		}
		for _, tc := range []struct {
			target string
			high   bool
		}{{"l20", true}, {"l23", false}, {"l22", false}} {
			name := fmt.Sprintf("ownEdit=%v/%s", ownEdit, tc.target)
			post := postEditSeverity(t, wc, tc.target)
			if post != tc.high {
				t.Fatalf("%s: precondition: wt check after the edit HIGH=%v, want %v", name, post, tc.high)
			}
			for _, h := range []struct {
				kind    string
				hook    func(io.Reader) int
				payload string
				blockEV string
			}{
				{"claude", hookClaudeEdit, claudeEditPayload(wc, tc.target), "WT_CLAUDE_HOOK_BLOCK"},
				{"codex", hookCodexEdit, codexEditPayload(wc, tc.target), "WT_CODEX_HOOK_BLOCK"},
			} {
				for _, block := range []bool{false, true} {
					t.Setenv(h.blockEV, map[bool]string{false: "0", true: "1"}[block])
					got := hookVerdict(t, runHookCapture(t, h.hook, h.payload))
					want := "silent"
					if tc.high {
						want = map[bool]string{false: "overlap", true: "deny"}[block]
					}
					if got != want {
						t.Errorf("%s %s block=%v: hook says %s, want %s (wt check after the edit: HIGH=%v)", name, h.kind, block, got, want, post)
					}
				}
			}
		}
	}
}

// A git failure measuring this window's own ranges must never read as "no
// edits" (#184 review): graded against the pending edit alone, a disjoint-looking
// edit would go silent while `wt check` (which reads the failure as
// indeterminate) says HIGH. The hooks keep a file-level heads-up instead.
func TestPreEditHooks_UnmeasurableOwnRangesStayHeadsUp(t *testing.T) {
	wc := behindHookRepo(t)
	writeT(t, wc, "data.txt", strings.Replace(readT(t, wc, "data.txt"), "l35\n", "C35\n", 1))
	gitT(t, wc, "commit", "-qam", "wc edits l35 (disjoint from wa)")
	t.Setenv("WT_CLAUDE_HOOK_BLOCK", "0")
	t.Setenv("WT_CODEX_HOOK_BLOCK", "0")
	if got := hookVerdict(t, runHookCapture(t, hookClaudeEdit, claudeEditPayload(wc, "l30"))); got != "silent" {
		t.Fatalf("precondition: a disjoint edit with measurable ranges: claude says %s, want silent", got)
	}

	orig := ownRanges
	t.Cleanup(func() { ownRanges = orig })
	ownRanges = func(dir, base, file string) ([]gitx.LineRange, bool) { return nil, false }
	if got := hookVerdict(t, runHookCapture(t, hookClaudeEdit, claudeEditPayload(wc, "l30"))); got != "file-level" {
		t.Errorf("own ranges unmeasurable: claude says %s, want a file-level heads-up", got)
	}
	if got := hookVerdict(t, runHookCapture(t, hookCodexEdit, codexEditPayload(wc, "l30"))); got != "file-level" {
		t.Errorf("own ranges unmeasurable: codex says %s, want a file-level heads-up", got)
	}
}

// behindHookRepo: main = l01..l40; wa and wc fork; main then inserts 3 lines after
// l02; wa edits l20. Returns wc (behind, no edits yet).
func behindHookRepo(t *testing.T) (wc string) {
	t.Helper()
	bin := t.TempDir()
	if err := os.WriteFile(filepath.Join(bin, "gh"), []byte("#!/bin/sh\nexit 1\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", bin+string(os.PathListSeparator)+os.Getenv("PATH")) // no real GitHub calls
	t.Setenv("HOME", t.TempDir())
	t.Setenv("WT_SKIP_COLLISION", "")
	t.Setenv("HOOK_DISABLE_MULTIWINDOW_CHECK", "")
	t.Chdir(t.TempDir()) // the hooks chdir into the payload's cwd; restored after

	root := t.TempDir()
	gitT(t, root, "init", "-q", "-b", "main")
	gitT(t, root, "config", "user.email", "t@t.test")
	gitT(t, root, "config", "user.name", "t")
	var b strings.Builder
	for i := 1; i <= 40; i++ {
		fmt.Fprintf(&b, "l%02d\n", i)
	}
	writeT(t, root, "data.txt", b.String())
	gitT(t, root, "add", "data.txt")
	gitT(t, root, "commit", "-qm", "base")
	gitT(t, root, "branch", "wa")
	gitT(t, root, "branch", "wc")
	writeT(t, root, "data.txt", strings.Replace(b.String(), "l02\n", "l02\nN1\nN2\nN3\n", 1))
	gitT(t, root, "commit", "-qam", "main inserts 3 lines after wa and wc forked")

	wa := filepath.Join(t.TempDir(), "wa")
	wc = filepath.Join(t.TempDir(), "wc")
	gitT(t, root, "worktree", "add", "-q", wa, "wa")
	gitT(t, root, "worktree", "add", "-q", wc, "wc")
	writeT(t, wa, "data.txt", strings.Replace(b.String(), "l20\n", "A20\n", 1))
	gitT(t, wa, "commit", "-qam", "wa edits l20")
	return wc
}

func claudeEditPayload(wc, target string) string {
	p, _ := json.Marshal(map[string]any{"cwd": wc, "tool_name": "Edit", "tool_input": map[string]string{
		"file_path": filepath.Join(wc, "data.txt"), "old_string": target + "\n", "new_string": "X" + target + "\n"}})
	return string(p)
}

func codexEditPayload(wc, target string) string {
	p, _ := json.Marshal(map[string]any{"cwd": wc, "tool_name": "apply_patch", "tool_input": map[string]string{
		"command": "*** Begin Patch\n*** Update File: data.txt\n@@\n-" + target + "\n+X" + target + "\n*** End Patch\n"}})
	return string(p)
}

// postEditSeverity makes the edit in wc, asks the `wt check` grader whether wa's
// entry is HIGH, and undoes the edit.
func postEditSeverity(t *testing.T, wc, target string) bool {
	t.Helper()
	orig := readT(t, wc, "data.txt")
	writeT(t, wc, "data.txt", strings.Replace(orig, target+"\n", "X"+target+"\n", 1))
	defer writeT(t, wc, "data.txt", orig)
	t.Chdir(wc)
	c, err := config.Load()
	if err != nil {
		t.Fatal(err)
	}
	ws, err := collide.Scan(c)
	if err != nil {
		t.Fatal(err)
	}
	for _, e := range buildCheckReport(c, ws, wc, collide.ExactQueries([]string{"data.txt"}), false) {
		if e.Window == "wa" {
			return e.Category == CatBlocking
		}
	}
	t.Fatal("no wa entry in the check report")
	return false
}

// spans builds []gitx.LineRange from start, end pairs.
func spans(se ...int) []gitx.LineRange {
	var out []gitx.LineRange
	for i := 0; i+1 < len(se); i += 2 {
		out = append(out, gitx.LineRange{Start: se[i], End: se[i+1]})
	}
	return out
}

func runHookCapture(t *testing.T, hook func(io.Reader) int, payload string) string {
	t.Helper()
	r, w, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	stdout := os.Stdout
	os.Stdout = w
	code := hook(strings.NewReader(payload))
	os.Stdout = stdout
	w.Close()
	out, _ := io.ReadAll(r)
	if code != 0 {
		t.Errorf("hook exit %d, want 0 (fail-open)", code)
	}
	return strings.TrimSpace(string(out))
}

// hookVerdict: "silent", "overlap" (advisory, computed overlap), "file-level"
// (advisory heads-up), or "deny".
func hookVerdict(t *testing.T, out string) string {
	t.Helper()
	if out == "" {
		return "silent"
	}
	var o struct {
		HSO struct {
			AdditionalContext  string `json:"additionalContext"`
			PermissionDecision string `json:"permissionDecision"`
		} `json:"hookSpecificOutput"`
	}
	if err := json.Unmarshal([]byte(out), &o); err != nil {
		t.Fatalf("hook output is not JSON: %v\n%s", err, out)
	}
	switch {
	case o.HSO.PermissionDecision == "deny":
		return "deny"
	case strings.Contains(o.HSO.AdditionalContext, "OVERLAPS"):
		return "overlap"
	default:
		return "file-level"
	}
}

func gitT(t *testing.T, dir string, args ...string) {
	t.Helper()
	cmd := exec.Command("git", args...)
	cmd.Dir = dir
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("git %v: %v\n%s", args, err, out)
	}
}

func writeT(t *testing.T, dir, rel, content string) {
	t.Helper()
	if err := os.WriteFile(filepath.Join(dir, rel), []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
}

func readT(t *testing.T, dir, rel string) string {
	t.Helper()
	b, err := os.ReadFile(filepath.Join(dir, rel))
	if err != nil {
		t.Fatal(err)
	}
	return string(b)
}
