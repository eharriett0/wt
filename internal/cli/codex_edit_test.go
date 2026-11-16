package cli

import (
	"encoding/json"
	"reflect"
	"strings"
	"testing"
)

func TestParseCodexEdit(t *testing.T) {
	cases := []struct {
		name     string
		payload  string
		wantCWD  string
		wantPat  string
		relevant bool
	}{
		{
			name:     "apply_patch is relevant",
			payload:  `{"cwd":"/repo","tool_name":"apply_patch","tool_input":{"command":"*** Begin Patch\n*** Update File: a.go\n"}}`,
			wantCWD:  "/repo",
			wantPat:  "*** Begin Patch\n*** Update File: a.go\n",
			relevant: true,
		},
		{
			name:     "bash tool is not relevant (git guards cover it)",
			payload:  `{"cwd":"/repo","tool_name":"Bash","tool_input":{"command":"git push"}}`,
			relevant: false,
		},
		{
			name:     "empty patch command is not relevant",
			payload:  `{"cwd":"/repo","tool_name":"apply_patch","tool_input":{"command":""}}`,
			relevant: false,
		},
		{
			name:     "garbage json is not relevant",
			payload:  `not json`,
			relevant: false,
		},
		{
			name:     "cwd is trimmed",
			payload:  `{"cwd":"  /repo  ","tool_name":"apply_patch","tool_input":{"command":"x"}}`,
			wantCWD:  "/repo",
			wantPat:  "x",
			relevant: true,
		},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			cwd, pat, rel := parseCodexEdit([]byte(c.payload))
			if rel != c.relevant {
				t.Fatalf("relevant=%v want %v", rel, c.relevant)
			}
			if !rel {
				return
			}
			if cwd != c.wantCWD || pat != c.wantPat {
				t.Errorf("cwd=%q pat=%q want %q / %q", cwd, pat, c.wantCWD, c.wantPat)
			}
		})
	}
}

// parseCodexPatch lists every file a patch names, leniently: the paths are
// checked even when apply_patch would reject the patch.
func TestParseCodexPatch(t *testing.T) {
	patch := `*** Begin Patch
*** Update File: internal/a.go
@@ func f()
-old line
+new line
*** Add File: internal/b.go
+brand new
*** Delete File: internal/c.go
*** Update File: old/x.go
*** Move to: new/x.go
@@
-gone
*** End Patch`
	got := parseCodexPatch(patch)
	// a Delete and a move's source REMOVE their path (#201: a symlink there is the link)
	want := []codexPatchFile{{path: "internal/a.go"}, {path: "internal/b.go"}, {path: "internal/c.go", removed: true}, {path: "old/x.go", newPath: "new/x.go", removed: true}}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("parseCodexPatch = %+v, want %+v", got, want)
	}
	if got := parseCodexPatch("*** Update File: a.go\r\n-x\r\n"); len(got) != 1 || got[0].path != "a.go" {
		t.Errorf("unbounded, CRLF: %+v", got)
	}
}

func TestPatchPaths(t *testing.T) {
	files := []codexPatchFile{
		{path: "a.go"},
		{path: "b.go", newPath: "c.go"},
		{path: "a.go"}, // dup
		{path: ""},     // skip
	}
	got := patchPaths(files)
	want := []string{"a.go", "b.go", "c.go"}
	if strings.Join(got, ",") != strings.Join(want, ",") {
		t.Errorf("patchPaths=%v want %v", got, want)
	}
}

// decodeDecision unwraps the codexEditDecision JSON for assertions.
func decodeDecision(t *testing.T, s string) (ctx, decision, reason string) {
	t.Helper()
	var out struct {
		HookSpecificOutput struct {
			HookEventName            string `json:"hookEventName"`
			AdditionalContext        string `json:"additionalContext"`
			PermissionDecision       string `json:"permissionDecision"`
			PermissionDecisionReason string `json:"permissionDecisionReason"`
		} `json:"hookSpecificOutput"`
	}
	if err := json.Unmarshal([]byte(s), &out); err != nil {
		t.Fatalf("bad decision json: %v\n%s", err, s)
	}
	if out.HookSpecificOutput.HookEventName != "PreToolUse" {
		t.Errorf("hookEventName=%q want PreToolUse", out.HookSpecificOutput.HookEventName)
	}
	return out.HookSpecificOutput.AdditionalContext, out.HookSpecificOutput.PermissionDecision, out.HookSpecificOutput.PermissionDecisionReason
}

func TestCodexEditDecision(t *testing.T) {
	confirmed := []codexGradedEntry{
		{entry: CheckEntry{Path: "a.go", Window: "win-b", Liveness: "live"}, confirmed: true},
		{entry: CheckEntry{Path: "a.go", Window: "win-b", Liveness: "live"}, confirmed: true}, // dup → collapse
		{entry: CheckEntry{Path: "d.go", Window: "win-c", Liveness: "live"}, confirmed: true},
	}

	// Empty → no output.
	if _, has := codexEditDecision(nil, false); has {
		t.Error("empty high should not emit")
	}

	// All-confirmed advisory: OVERLAPS header, additionalContext (deny=false).
	out, has := codexEditDecision(confirmed, false)
	if !has {
		t.Fatal("expected output")
	}
	ctx, dec, _ := decodeDecision(t, out)
	if dec != "" {
		t.Errorf("advisory must not set permissionDecision, got %q", dec)
	}
	if !strings.Contains(ctx, "OVERLAPS hunks") {
		t.Errorf("all-confirmed header missing:\n%s", ctx)
	}
	if !strings.Contains(ctx, "a.go") || !strings.Contains(ctx, "d.go") {
		t.Errorf("both files should appear:\n%s", ctx)
	}
	if strings.Count(ctx, "a.go") != 1 {
		t.Errorf("a.go should appear once (deduped):\n%s", ctx)
	}
	if !strings.Contains(ctx, "overlapping hunks") {
		t.Errorf("confirmed line tag missing:\n%s", ctx)
	}

	// All file-level: neutral header + per-line "hunk overlap not computed".
	fileLevel := []codexGradedEntry{{entry: CheckEntry{Path: "a.go", Window: "win-b", Liveness: "live"}}}
	out, _ = codexEditDecision(fileLevel, false)
	ctx, _, _ = decodeDecision(t, out)
	if strings.Contains(ctx, "OVERLAPS hunks") {
		t.Errorf("file-level must not use the OVERLAPS header:\n%s", ctx)
	}
	if !strings.Contains(ctx, "hunk overlap not computed") {
		t.Errorf("file-level tag missing:\n%s", ctx)
	}

	// Mixed confirmed + file-level: NEUTRAL header, each line tagged accurately —
	// the unverified file must NOT be labeled "overlapping hunks" (#117 review).
	mixed := []codexGradedEntry{
		{entry: CheckEntry{Path: "a.go", Window: "win-b", Liveness: "live"}, confirmed: true},
		{entry: CheckEntry{Path: "b.go", Window: "win-c", Liveness: "live"}, confirmed: false},
	}
	out, _ = codexEditDecision(mixed, true) // deny justified by the confirmed a.go
	ctx, dec, reason := decodeDecision(t, out)
	if dec != "deny" {
		t.Errorf("mixed w/ a confirmed HIGH under deny → decision=%q want deny", dec)
	}
	body := reason
	if strings.Contains(body, "OVERLAPS hunks") {
		t.Errorf("mixed batch must use the neutral header, not OVERLAPS:\n%s", body)
	}
	// a.go tagged confirmed, b.go tagged file-level.
	for _, w := range []string{"a.go", "overlapping hunks", "b.go", "hunk overlap not computed"} {
		if !strings.Contains(body, w) {
			t.Errorf("mixed body missing %q:\n%s", w, body)
		}
	}
	if ctx != "" {
		t.Errorf("deny must carry the reason, not additionalContext: ctx=%q", ctx)
	}

	// Deny mode with a confirmed batch → permissionDecision deny + reason.
	out, _ = codexEditDecision(confirmed, true)
	ctx, dec, reason = decodeDecision(t, out)
	if dec != "deny" || reason == "" || ctx != "" {
		t.Errorf("deny: decision=%q reason=%q ctx=%q", dec, reason, ctx)
	}
}

// #201: every window editing a path is named on that path's line, each with
// its own liveness and its own confirmed / not-computed tag. The line used to
// stop at the first entry per path. Deduplicated by path + window (the
// worktree, else the label, #193), in the order the entries came.
func TestCodexEditDecision_NamesEveryWindowPerPath(t *testing.T) {
	const (
		hunks = "overlapping hunks"
		file  = "hunk overlap not computed — run `wt check`"
	)
	ent := func(path, window, live, wt string, confirmed bool) codexGradedEntry {
		return codexGradedEntry{entry: CheckEntry{Path: path, Window: window, Liveness: live, otherWorktree: wt}, confirmed: confirmed}
	}
	cases := []struct {
		name  string
		high  []codexGradedEntry
		lines []string
	}{
		{"two windows on one path, each tagged",
			[]codexGradedEntry{ent("a.go", "win-b", "live", "/w/b", true), ent("a.go", "win-c", "dormant", "/w/c", false)},
			[]string{"  a.go — also being edited by win-b [live] (" + hunks + "), win-c [dormant] (" + file + ")"}},
		{"the same window twice collapses",
			[]codexGradedEntry{ent("a.go", "win-b", "live", "/w/b", true), ent("a.go", "win-b", "live", "/w/b", true)},
			[]string{"  a.go — also being edited by win-b [live] (" + hunks + ")"}},
		{"namesakes are two windows (#193)",
			[]codexGradedEntry{ent("a.go", "#5", "live", "/p1/x", true), ent("a.go", "#5", "live", "/p2/x", false)},
			[]string{"  a.go — also being edited by #5 [live] (" + hunks + "), #5 [live] (" + file + ")"}},
		{"no worktree: the label identifies",
			[]codexGradedEntry{ent("a.go", "win-b", "live", "", false), ent("a.go", "win-b", "live", "", false), ent("a.go", "win-c", "live", "", false)},
			[]string{"  a.go — also being edited by win-b [live] (" + file + "), win-c [live] (" + file + ")"}},
		{"paths and windows keep their order",
			[]codexGradedEntry{ent("b.go", "win-c", "live", "/w/c", true), ent("a.go", "win-b", "live", "/w/b", true), ent("b.go", "win-b", "live", "/w/b", true)},
			[]string{"  b.go — also being edited by win-c [live] (" + hunks + "), win-b [live] (" + hunks + ")",
				"  a.go — also being edited by win-b [live] (" + hunks + ")"}},
	}
	for _, tc := range cases {
		out, has := codexEditDecision(tc.high, false)
		if !has {
			t.Fatalf("%s: no output", tc.name)
		}
		ctx, _, _ := decodeDecision(t, out)
		body := strings.Split(ctx, "\n")
		if len(body) < 2+len(tc.lines) || !reflect.DeepEqual(body[1:1+len(tc.lines)], tc.lines) {
			t.Errorf("%s: lines =\n%s\nwant\n%s", tc.name, strings.Join(body, "\n"), strings.Join(tc.lines, "\n"))
		}
	}
}
