package cli

import (
	"encoding/json"
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

func TestParseCodexPatch(t *testing.T) {
	patch := `*** Begin Patch
*** Update File: internal/a.go
@@ func f()
 ctx line
-old line
+new line
 tail
@@ func g()
-only removed
*** Add File: internal/b.go
+brand new
+content
*** Delete File: internal/c.go
*** End Patch`
	files := parseCodexPatch(patch)
	if len(files) != 3 {
		t.Fatalf("got %d files, want 3: %+v", len(files), files)
	}
	// Update: two hunks; pre-image is context+removed, removed offsets tracked.
	up := files[0]
	if up.path != "internal/a.go" {
		t.Errorf("update path=%q", up.path)
	}
	if len(up.hunks) != 2 {
		t.Fatalf("update hunks=%d want 2: %+v", len(up.hunks), up.hunks)
	}
	if got := strings.Join(up.hunks[0].preImage, "\n"); got != "ctx line\nold line\ntail" {
		t.Errorf("hunk 0 pre-image=%q", got)
	}
	// pre-image is [ctx, old, tail]; only "old line" is removed → offset 1.
	if len(up.hunks[0].removed) != 1 || up.hunks[0].removed[0] != 1 {
		t.Errorf("hunk 0 removed offsets=%v want [1]", up.hunks[0].removed)
	}
	if strings.Join(up.hunks[1].preImage, "\n") != "only removed" || len(up.hunks[1].removed) != 1 {
		t.Errorf("hunk 1=%+v", up.hunks[1])
	}
	// Add: path only, no hunks (added lines aren't in the current file).
	if files[1].path != "internal/b.go" || len(files[1].hunks) != 0 {
		t.Errorf("add section=%+v", files[1])
	}
	// Delete: path only.
	if files[2].path != "internal/c.go" || len(files[2].hunks) != 0 {
		t.Errorf("delete section=%+v", files[2])
	}
}

func TestParseCodexPatch_Move(t *testing.T) {
	patch := "*** Update File: old/x.go\n*** Move to: new/x.go\n ctx\n-gone\n"
	files := parseCodexPatch(patch)
	if len(files) != 1 || files[0].path != "old/x.go" || files[0].newPath != "new/x.go" {
		t.Fatalf("move parse=%+v", files)
	}
	if strings.Join(files[0].hunks[0].preImage, "\n") != "ctx\ngone" {
		t.Errorf("move pre-image=%v", files[0].hunks[0].preImage)
	}
	if len(files[0].hunks[0].removed) != 1 || files[0].hunks[0].removed[0] != 1 {
		t.Errorf("move removed=%v want [1]", files[0].hunks[0].removed)
	}
}

func TestContiguousRuns(t *testing.T) {
	cases := []struct {
		in   []int
		want [][2]int
	}{
		{nil, nil},
		{[]int{2}, [][2]int{{2, 2}}},
		{[]int{1, 2, 3}, [][2]int{{1, 3}}},
		{[]int{1, 2, 5, 6, 9}, [][2]int{{1, 2}, {5, 6}, {9, 9}}},
	}
	for _, c := range cases {
		got := contiguousRuns(c.in)
		if len(got) != len(c.want) {
			t.Fatalf("runs(%v)=%v want %v", c.in, got, c.want)
		}
		for i := range got {
			if got[i] != c.want[i] {
				t.Errorf("runs(%v)[%d]=%v want %v", c.in, i, got[i], c.want[i])
			}
		}
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

func TestPatchRangesInFile(t *testing.T) {
	content := "package x\n\nfunc f() {\n\treturn 1\n}\n\nfunc g() {\n\treturn 2\n}\n"
	// Hunk: context "func f() {" (line 3), REMOVED "\treturn 1" (line 4),
	// context "}" (line 5). The block locates at line 3; only the removed line
	// is ranged → [4,4], NOT the context-inclusive [3,5].
	f := codexPatchFile{path: "x.go", hunks: []codexHunk{{
		preImage: []string{"func f() {", "\treturn 1", "}"},
		removed:  []int{1},
	}}}
	ranges, ok := patchRangesInFile(f, content)
	if !ok || len(ranges) != 1 {
		t.Fatalf("locate: ok=%v ranges=%v", ok, ranges)
	}
	if ranges[0].Start != 4 || ranges[0].End != 4 {
		t.Errorf("range=%+v want line 4 only (context excluded)", ranges[0])
	}

	// Pure-addition hunk (all context, nothing removed) modifies no existing line
	// → contributes no range → the only-hunk case falls back to file-level.
	fAdd := codexPatchFile{path: "x.go", hunks: []codexHunk{{
		preImage: []string{"func f() {"},
		removed:  nil,
	}}}
	if _, ok := patchRangesInFile(fAdd, content); ok {
		t.Error("pure-addition hunk should produce no range")
	}

	// Non-unique block → cannot localize → ok=false (file-level fallback).
	dup := "\treturn 1\n\treturn 1\n"
	fAmb := codexPatchFile{path: "x.go", hunks: []codexHunk{{
		preImage: []string{"\treturn 1"}, removed: []int{0},
	}}}
	if _, ok := patchRangesInFile(fAmb, dup); ok {
		t.Error("ambiguous block should not localize")
	}

	// A block that isn't present → ok=false.
	fMiss := codexPatchFile{path: "x.go", hunks: []codexHunk{{
		preImage: []string{"nonexistent line"}, removed: []int{0},
	}}}
	if _, ok := patchRangesInFile(fMiss, content); ok {
		t.Error("missing block should not localize")
	}

	// No hunks at all (an add/delete) → ok=false.
	if _, ok := patchRangesInFile(codexPatchFile{path: "x.go"}, content); ok {
		t.Error("no-hunk file should not localize")
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
	entries := []CheckEntry{
		{Path: "a.go", Window: "win-b", Liveness: "live"},
		{Path: "a.go", Window: "win-b", Liveness: "live"}, // dup path → collapse
		{Path: "d.go", Window: "win-c", Liveness: "live"},
	}

	// Empty → no output.
	if _, has := codexEditDecision(nil, false, false); has {
		t.Error("empty high should not emit")
	}

	// Advisory, confirmed HIGH wording, additionalContext (deny=false).
	out, has := codexEditDecision(entries, false, true)
	if !has {
		t.Fatal("expected output")
	}
	ctx, dec, _ := decodeDecision(t, out)
	if dec != "" {
		t.Errorf("advisory must not set permissionDecision, got %q", dec)
	}
	if !strings.Contains(ctx, "OVERLAPS hunks") {
		t.Errorf("confirmed wording missing:\n%s", ctx)
	}
	if !strings.Contains(ctx, "a.go") || !strings.Contains(ctx, "d.go") {
		t.Errorf("both files should appear:\n%s", ctx)
	}
	if strings.Count(ctx, "a.go") != 1 {
		t.Errorf("a.go should appear once (deduped):\n%s", ctx)
	}

	// File-level (unconfirmed) wording.
	out, _ = codexEditDecision(entries, false, false)
	ctx, _, _ = decodeDecision(t, out)
	if !strings.Contains(ctx, "hunk overlap not computed") {
		t.Errorf("file-level wording missing:\n%s", ctx)
	}

	// Deny mode → permissionDecision deny + reason (never additionalContext).
	out, _ = codexEditDecision(entries, true, true)
	ctx, dec, reason := decodeDecision(t, out)
	if dec != "deny" {
		t.Errorf("deny mode: decision=%q want deny", dec)
	}
	if reason == "" || ctx != "" {
		t.Errorf("deny should carry reason (not additionalContext): reason=%q ctx=%q", reason, ctx)
	}
}
