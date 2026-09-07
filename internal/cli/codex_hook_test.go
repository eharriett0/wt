package cli

import (
	"fmt"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/eharriett0/wt/internal/coord"
)

func TestCoordContextMessage(t *testing.T) {
	// empty inbox → nothing to say
	if _, has := coordContextMessage(nil, 0, time.Now()); has {
		t.Error("empty inbox → no message")
	}

	inbox := []coord.Record{
		{ID: "id-hold", Window: "feat/x", Message: "rolling main", Hold: []string{"merge-main", "rebase"}},
		{ID: "id-note", Window: "feat/y", Message: "fyi: renamed pkg"},
		{ID: "id-bare", Window: "feat/z"}, // no message
	}
	msg, has := coordContextMessage(inbox, 0, time.Now())
	if !has {
		t.Fatal("expected a message")
	}
	// hold rendered with its ops + ack id + comes first (before the plain note)
	if !strings.Contains(msg, "HOLD feat/x [merge-main,rebase]") {
		t.Errorf("hold ops missing: %q", msg)
	}
	if !strings.Contains(msg, "wt ack id-hold") {
		t.Errorf("hold ack id missing: %q", msg)
	}
	if strings.Index(msg, "id-hold") > strings.Index(msg, "id-note") {
		t.Errorf("holds must come before plain announcements: %q", msg)
	}
	// plain announcement + a message-less one
	if !strings.Contains(msg, "feat/y — fyi: renamed pkg (wt ack id-note)") {
		t.Errorf("announcement rendering off: %q", msg)
	}
	if !strings.Contains(msg, "feat/z (wt ack id-bare)") {
		t.Errorf("message-less announcement rendering off: %q", msg)
	}
	if !strings.Contains(msg, "wt inbox") {
		t.Errorf("missing the `wt inbox` reminder: %q", msg)
	}
}

// #150: the whole injection is bounded by a BYTE budget (per-entry truncation +
// coordMaxInjectBytes), filled newest-first so the OLDEST notes drop — never the
// newest. Short notes all fit; verbose ones are truncated and the tail is dropped.
func TestCoordContextMessage_ByteBudget(t *testing.T) {
	// many notes, oldest-first n00..n29, each with a long free-text (a unique MARK
	// at the front stays within the per-entry truncation window).
	var inbox []coord.Record
	for i := 0; i < 30; i++ {
		inbox = append(inbox, coord.Record{
			ID: fmt.Sprintf("n%02d", i), Window: fmt.Sprintf("w%02d", i),
			Message: fmt.Sprintf("MARK%02d ", i) + strings.Repeat("y", 500),
		})
	}
	msg, has := coordContextMessage(inbox, 0, time.Now())
	if !has {
		t.Fatal("expected a message")
	}
	if len(msg) > coordMaxInjectBytes+1024 { // + header/footer/summary overhead
		t.Errorf("injection %d bytes exceeds the budget %d:\n%s", len(msg), coordMaxInjectBytes, msg[:200])
	}
	if !strings.Contains(msg, "truncated — `wt inbox` for full") {
		t.Errorf("verbose entries must be per-entry truncated: %q", msg[:300])
	}
	if !strings.Contains(msg, "MARK29") {
		t.Errorf("the NEWEST note must always be kept: %q", msg[:300])
	}
	if strings.Contains(msg, "MARK00") {
		t.Errorf("the OLDEST notes must drop under the budget, not the newest:\n%s", msg)
	}
	if !strings.Contains(msg, "older not shown") {
		t.Errorf("summary line naming dropped count + size expected: %q", msg)
	}
}

// short notes all fit under the byte budget — no truncation, no drop.
func TestCoordContextMessage_ShortNotesAllShown(t *testing.T) {
	var inbox []coord.Record
	inbox = append(inbox, coord.Record{ID: "keep", Window: "w0", Hold: []string{"merge-main"}})
	for i := 0; i < 20; i++ {
		inbox = append(inbox, coord.Record{ID: fmt.Sprintf("n%d", i), Window: fmt.Sprintf("w%d", i+1), Message: "note"})
	}
	msg, has := coordContextMessage(inbox, 0, time.Now())
	if !has {
		t.Fatal("expected a message")
	}
	if strings.Contains(msg, "older not shown") {
		t.Errorf("21 short entries fit under the byte budget — nothing should be dropped:\n%s", msg)
	}
	if !strings.Contains(msg, "HOLD w0") {
		t.Errorf("the hold must be present (holds first): %q", msg)
	}
}

func TestTruncateMessage(t *testing.T) {
	if got := truncateMessage("  short note  "); got != "short note" {
		t.Errorf("short message should be trimmed and unchanged, got %q", got)
	}
	long := strings.Repeat("z", coordMaxEntryChars+50)
	got := truncateMessage(long)
	if !strings.HasSuffix(got, "… (truncated — `wt inbox` for full)") {
		t.Errorf("long message should carry the truncation marker: %q", got)
	}
	if len([]rune(got)) >= len([]rune(long)) {
		t.Errorf("truncated message should be shorter than the original")
	}
	if got := truncateMessage(""); got != "" {
		t.Errorf("empty stays empty, got %q", got)
	}
}

// #147: the whole bug is ordering. Delivery must be NEWEST-first (so a fresh
// announcement is never buried under a deep backlog) and stale plain notes must
// age out of delivery, while holds are always delivered regardless of age.
func TestCoordContextMessage_NewestFirstAndAgeOut(t *testing.T) {
	now := time.Date(2026, 9, 7, 12, 0, 0, 0, time.UTC)
	note := func(id string, agoHours int) coord.Record {
		return coord.Record{
			ID: id, Window: "w-" + id, Message: "note " + id,
			TS: now.Add(-time.Duration(agoHours) * time.Hour).UTC().Format(time.RFC3339),
		}
	}
	// oldest-first inbox (log order): old, mid, new
	inbox := []coord.Record{note("old", 72), note("mid", 5), note("new", 1)}

	// no age-out (maxAge 0): all shown, NEWEST first
	msg, has := coordContextMessage(inbox, 0, now)
	if !has {
		t.Fatal("expected a message")
	}
	iNew, iMid, iOld := strings.Index(msg, "ack new"), strings.Index(msg, "ack mid"), strings.Index(msg, "ack old")
	if !(iNew >= 0 && iNew < iMid && iMid < iOld) {
		t.Errorf("expected newest-first (new < mid < old), got new=%d mid=%d old=%d:\n%s", iNew, iMid, iOld, msg)
	}

	// age-out at 24h: the 72h-old note drops from delivery; fresher ones remain
	msg2, has2 := coordContextMessage(inbox, 24*time.Hour, now)
	if !has2 {
		t.Fatal("expected a message")
	}
	if strings.Contains(msg2, "ack old") {
		t.Errorf("stale (72h) note should be aged out of delivery:\n%s", msg2)
	}
	if !strings.Contains(msg2, "ack new") || !strings.Contains(msg2, "ack mid") {
		t.Errorf("fresh notes should remain:\n%s", msg2)
	}

	// age-out that removes every note → silent (nothing to inject)
	if _, has := coordContextMessage(inbox, 30*time.Minute, now); has {
		t.Error("all notes older than 30m → nothing to deliver")
	}

	// a hold is NEVER aged out — an un-cleared hold is a standing safety request
	oldHold := coord.Record{
		ID: "h", Window: "wh", Hold: []string{"merge-main"},
		TS: now.Add(-100 * time.Hour).UTC().Format(time.RFC3339),
	}
	if hmsg, has := coordContextMessage([]coord.Record{oldHold}, time.Hour, now); !has || !strings.Contains(hmsg, "HOLD wh") {
		t.Errorf("an un-cleared hold must always be delivered regardless of age: has=%v msg=%q", has, hmsg)
	}
}

// #147 review (HIGH): `wt ack --all` must NEVER bulk-ack a HOLD — acking a hold
// removes it from coord.Inbox and thus from the merge-main interlock, so a fresh
// hold buried past the cap would be silently waived. bulkAckTargets keeps holds
// out of the bulk-ack set and counts them as left-standing.
func TestBulkAckTargets_ExcludesHolds(t *testing.T) {
	box := []coord.Record{
		{ID: "n1", Message: "note one"},
		{ID: "h1", Hold: []string{"merge-main"}},
		{ID: "n2", Message: "note two"},
		{ID: "h2", Hold: []string{"rebase", "deploy"}},
	}
	notes, holdsLeft := bulkAckTargets(box)
	if holdsLeft != 2 {
		t.Errorf("holdsLeft = %d, want 2", holdsLeft)
	}
	if len(notes) != 2 || notes[0].ID != "n1" || notes[1].ID != "n2" {
		t.Fatalf("notes = %+v, want the two plain announcements [n1 n2]", notes)
	}
	for _, n := range notes {
		if len(n.Hold) > 0 {
			t.Errorf("bulk-ack set must never contain a hold: %+v", n)
		}
	}
	// all-holds inbox → nothing to bulk-ack, both held
	if notes, holds := bulkAckTargets([]coord.Record{{ID: "h", Hold: []string{"x"}}}); len(notes) != 0 || holds != 1 {
		t.Errorf("all-holds: notes=%v holds=%d, want [] and 1", notes, holds)
	}
	// empty inbox
	if notes, holds := bulkAckTargets(nil); len(notes) != 0 || holds != 0 {
		t.Errorf("empty: notes=%v holds=%d", notes, holds)
	}
}

func TestParseCodexCwd(t *testing.T) {
	if cwd, ok := parseCodexCwd([]byte(`{"cwd":"/repo","session_id":"x","prompt":"hi"}`)); !ok || cwd != "/repo" {
		t.Errorf("valid: (%q,%v)", cwd, ok)
	}
	// empty cwd is still ok (caller falls back to process cwd)
	if cwd, ok := parseCodexCwd([]byte(`{"session_id":"x"}`)); !ok || cwd != "" {
		t.Errorf("no cwd: (%q,%v)", cwd, ok)
	}
	// whitespace trimmed
	if cwd, _ := parseCodexCwd([]byte(`{"cwd":"  /r  "}`)); cwd != "/r" {
		t.Errorf("trim: %q", cwd)
	}
	// garbage → not ok
	if _, ok := parseCodexCwd([]byte(`not json`)); ok {
		t.Error("garbage should not parse")
	}
}

func TestWindowsExcluding(t *testing.T) {
	got := windowsExcluding([]string{"#1", "feat/x", "#1", "feat/y"}, "#1")
	if len(got) != 2 || got[0] != "feat/x" || got[1] != "feat/y" {
		t.Fatalf("windowsExcluding = %v, want [feat/x feat/y]", got)
	}
}

func TestCodexContextMessage(t *testing.T) {
	// no overlaps → nothing
	if _, has := codexContextMessage(nil, "#1"); has {
		t.Error("no overlaps → no message")
	}
	// an overlap whose only window IS the current one → skipped (nothing to say)
	if _, has := codexContextMessage([]StatusOverlap{{File: "a", Windows: []string{"#1"}}}, "#1"); has {
		t.Error("overlap containing only the current window should be skipped")
	}

	ov := []StatusOverlap{
		{File: "foo.go", Windows: []string{"#1", "feat/x"}, Severity: "HIGH"},    // #1 participates → "also"
		{File: "bar.go", Windows: []string{"feat/y", "#1"}, Severity: "low"},     // #1 participates → "also"
		{File: "baz.go", Windows: []string{"feat/y", "feat/z"}, Severity: "low"}, // #1 NOT a participant → no "also"
	}
	msg, has := codexContextMessage(ov, "#1")
	if !has {
		t.Fatal("expected a message")
	}
	if strings.Contains(msg, "#1") {
		t.Errorf("current window #1 must be excluded from the message: %q", msg)
	}
	if !strings.Contains(msg, "feat/x") || !strings.Contains(msg, "feat/y") || !strings.Contains(msg, "feat/z") {
		t.Errorf("other windows missing: %q", msg)
	}
	if !strings.Contains(msg, "foo.go") || !strings.Contains(msg, "HIGH") {
		t.Errorf("HIGH overlap not surfaced: %q", msg)
	}
	if !strings.Contains(msg, "bar.go") || !strings.Contains(msg, "same file") {
		t.Errorf("non-HIGH overlap not surfaced / mislabeled: %q", msg)
	}
	if !strings.Contains(msg, "wt check") {
		t.Errorf("missing the `wt check` reminder: %q", msg)
	}
	// wording accuracy: "also" only where the current window participates
	if !strings.Contains(msg, "foo.go — also being edited by feat/x") {
		t.Errorf("participant overlap should say 'also being edited by': %q", msg)
	}
	if !strings.Contains(msg, "baz.go — being edited by feat/y, feat/z") {
		t.Errorf("non-participant overlap should NOT say 'also': %q", msg)
	}
}

func TestCodexContextMessage_Caps(t *testing.T) {
	var ov []StatusOverlap
	for i := 0; i < codexMaxOverlapLines+5; i++ {
		ov = append(ov, StatusOverlap{File: fmt.Sprintf("f%d.go", i), Windows: []string{"other"}, Severity: "HIGH"})
	}
	msg, has := codexContextMessage(ov, "#1")
	if !has {
		t.Fatal("expected a message")
	}
	if !strings.Contains(msg, "…and 5 more") {
		t.Errorf("expected the cap summary line: %q", msg)
	}
}

func TestMergeCodexHook_FreshAndIdempotent(t *testing.T) {
	dir := t.TempDir()
	path := dir + "/hooks.json"
	out, changed, err := mergeCodexHook(path)
	if err != nil || !changed {
		t.Fatalf("fresh: changed=%v err=%v", changed, err)
	}
	s := string(out)
	for _, want := range []string{codexHookCommand, "UserPromptSubmit", codexEditHookCommand, "PreToolUse", "apply_patch"} {
		if !strings.Contains(s, want) {
			t.Errorf("fresh output missing %q:\n%s", want, s)
		}
	}
	if err := os.WriteFile(path, out, 0o644); err != nil {
		t.Fatal(err)
	}
	if _, changed, err := mergeCodexHook(path); err != nil || changed {
		t.Errorf("idempotent second merge: changed=%v err=%v", changed, err)
	}
}

func TestMergeCodexHook_PreservesExisting(t *testing.T) {
	dir := t.TempDir()
	path := dir + "/hooks.json"
	existing := `{"hooks":{"PostToolUse":[{"hooks":[{"type":"command","command":"other-tool"}]}],` +
		`"UserPromptSubmit":[{"hooks":[{"type":"command","command":"my-existing"}]}]}}`
	if err := os.WriteFile(path, []byte(existing), 0o644); err != nil {
		t.Fatal(err)
	}
	out, changed, err := mergeCodexHook(path)
	if err != nil || !changed {
		t.Fatalf("changed=%v err=%v", changed, err)
	}
	s := string(out)
	if !strings.Contains(s, "other-tool") || !strings.Contains(s, "my-existing") {
		t.Errorf("existing hooks clobbered:\n%s", s)
	}
	if !strings.Contains(s, codexHookCommand) {
		t.Errorf("our command not added:\n%s", s)
	}
}

func TestMergeCodexHook_RefusesGarbage(t *testing.T) {
	dir := t.TempDir()
	path := dir + "/hooks.json"
	if err := os.WriteFile(path, []byte("not json"), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, _, err := mergeCodexHook(path); err == nil {
		t.Error("expected a refuse (error) on non-JSON, not a blind overwrite")
	}
}
