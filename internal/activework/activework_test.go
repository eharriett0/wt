package activework

import (
	"strings"
	"testing"
	"time"
)

func fixedTime() time.Time {
	return time.Date(2026, 6, 18, 15, 4, 5, 0, time.UTC)
}

func TestAppendSection_CreatesHeaderWhenEmpty(t *testing.T) {
	got := AppendSection("", Entry{Issue: "42", Title: "Do X", Branch: "feat-42-do-x", Worktree: "/wt/feat-42-do-x", PRURL: "https://pr/1", Window: "win-a", When: fixedTime()})
	if !strings.Contains(got, "# Active work") {
		t.Fatal("header not created on empty content")
	}
	if !strings.Contains(got, "## #42 — claimed 2026-06-18T15:04:05Z") {
		t.Errorf("section header missing:\n%s", got)
	}
	for _, want := range []string{"- Title: Do X", "- Branch: `feat-42-do-x`", "- Draft PR: https://pr/1", "- Window: `win-a`"} {
		if !strings.Contains(got, want) {
			t.Errorf("missing %q", want)
		}
	}
}

func TestAppendSection_OmitsPRWhenEmpty(t *testing.T) {
	got := AppendSection("# Active work\n", Entry{Issue: "7", Title: "T", Branch: "b", Worktree: "w", Window: "win", When: fixedTime()})
	if strings.Contains(got, "Draft PR:") {
		t.Errorf("PR line should be omitted when PRURL empty:\n%s", got)
	}
}

func TestAppendSection_AppendsSecondWithoutClobber(t *testing.T) {
	one := AppendSection("", Entry{Issue: "1", Title: "one", Branch: "feat-1-one", Worktree: "w1", Window: "wa", When: fixedTime()})
	two := AppendSection(one, Entry{Issue: "2", Title: "two", Branch: "feat-2-two", Worktree: "w2", Window: "wb", When: fixedTime()})
	if !strings.Contains(two, "## #1 — claimed") || !strings.Contains(two, "## #2 — claimed") {
		t.Errorf("both sections should survive:\n%s", two)
	}
}

func TestRemoveSection(t *testing.T) {
	content := AppendSection(AppendSection("", Entry{Issue: "1", Title: "one", Branch: "b1", Worktree: "w1", Window: "wa", When: fixedTime()}),
		Entry{Issue: "2", Title: "two", Branch: "b2", Worktree: "w2", Window: "wb", When: fixedTime()})

	got, changed := RemoveSection(content, "1")
	if !changed {
		t.Fatal("expected changed=true removing #1")
	}
	if strings.Contains(got, "## #1 — claimed") {
		t.Errorf("#1 section should be gone:\n%s", got)
	}
	if !strings.Contains(got, "## #2 — claimed") {
		t.Errorf("#2 section should remain:\n%s", got)
	}
	if !strings.Contains(got, "# Active work") {
		t.Errorf("header should remain:\n%s", got)
	}
}

func TestRemoveSection_Noop(t *testing.T) {
	content := AppendSection("", Entry{Issue: "1", Title: "one", Branch: "b1", Worktree: "w1", Window: "wa", When: fixedTime()})
	_, changed := RemoveSection(content, "999")
	if changed {
		t.Error("removing a non-existent issue should be a no-op")
	}
}

func TestRemoveSection_PrefixSimilarNotRemoved(t *testing.T) {
	// #12 must not be removed when releasing #1 (token equality, not prefix).
	content := AppendSection(AppendSection("", Entry{Issue: "1", Title: "one", Branch: "b1", Worktree: "w1", Window: "wa", When: fixedTime()}),
		Entry{Issue: "12", Title: "twelve", Branch: "b12", Worktree: "w12", Window: "wb", When: fixedTime()})
	got, changed := RemoveSection(content, "1")
	if !changed {
		t.Fatal("expected #1 removed")
	}
	if !strings.Contains(got, "## #12 — claimed") {
		t.Errorf("#12 must survive releasing #1 (prefix-similar):\n%s", got)
	}
}

func TestParse(t *testing.T) {
	content := AppendSection(
		AppendSection("", Entry{Issue: "1", Title: "one", Branch: "feat-1-one", Worktree: "/wt/feat-1-one", PRURL: "https://pr/1", Window: "wa", When: fixedTime()}),
		Entry{Issue: "2", Title: "two", Branch: "feat-2-two", Worktree: "/wt/feat-2-two", Window: "wb", When: fixedTime()})

	got := Parse(content)
	if len(got) != 2 {
		t.Fatalf("expected 2 entries, got %d: %#v", len(got), got)
	}
	if got[0].Issue != "1" || got[0].Title != "one" || got[0].Branch != "feat-1-one" || got[0].Worktree != "/wt/feat-1-one" || got[0].PRURL != "https://pr/1" || got[0].Window != "wa" {
		t.Errorf("entry 0 mismatch: %#v", got[0])
	}
	if got[1].Issue != "2" || got[1].Branch != "feat-2-two" || got[1].PRURL != "" {
		t.Errorf("entry 1 mismatch: %#v", got[1])
	}
}

func TestParse_Empty(t *testing.T) {
	if got := Parse(""); len(got) != 0 {
		t.Errorf("Parse(\"\") = %v, want empty", got)
	}
}

func TestOtherClaims(t *testing.T) {
	content := AppendSection(AppendSection("", Entry{Issue: "1", Title: "one", Branch: "b1", Worktree: "w1", Window: "wa", When: fixedTime()}),
		Entry{Issue: "2", Title: "two", Branch: "b2", Worktree: "w2", Window: "wb", When: fixedTime()})

	others := OtherClaims(content, "1")
	if len(others) != 1 || others[0] != "#2" {
		t.Errorf("OtherClaims(.., 1) = %v, want [#2]", others)
	}
	all := OtherClaims(content, "")
	if len(all) != 2 {
		t.Errorf("OtherClaims(.., \"\") should return all 2, got %v", all)
	}
}

func TestUpsertSection_RefreshesLastSeenNoDuplicate(t *testing.T) {
	t0 := time.Date(2026, 6, 18, 15, 4, 5, 0, time.UTC)
	base := AppendSection("", Entry{Issue: "42", Title: "T", Branch: "feat-42", Worktree: "/w", Window: "win", When: t0})
	t1 := t0.Add(3 * time.Hour)
	got := UpsertSection(base, Entry{Issue: "42", Title: "T", Branch: "feat-42", Worktree: "/w", Window: "win", When: t1})
	if strings.Count(got, "## #42 ") != 1 {
		t.Fatalf("resume duplicated the section:\n%s", got)
	}
	if !strings.Contains(got, "- Last seen: "+t1.Format(time.RFC3339)) {
		t.Fatalf("Last seen not refreshed:\n%s", got)
	}
	// original claimed timestamp preserved (header line unchanged)
	if !strings.Contains(got, "## #42 — claimed "+t0.Format(time.RFC3339)) {
		t.Fatalf("claimed timestamp was reset:\n%s", got)
	}
}

func TestUpsertSection_AppendsWhenAbsent(t *testing.T) {
	t0 := time.Date(2026, 6, 18, 15, 4, 5, 0, time.UTC)
	got := UpsertSection("", Entry{Issue: "99", Title: "N", Branch: "feat-99", Worktree: "/w", Window: "win", When: t0})
	if strings.Count(got, "## #99 ") != 1 {
		t.Fatalf("expected one #99 section:\n%s", got)
	}
}

// IssueForBranch finds the claim recorded on a branch, the first one when two
// share it, as merge-pr's auto-clean and `wt discard` drop it (#40, #177).
func TestIssueForBranch(t *testing.T) {
	entries := []Entry{
		{Issue: "7", Branch: "canary"},
		{Issue: "8", Branch: "feat/other"},
		{Issue: "9", Branch: "canary"},
		{Issue: "10", Branch: ""},
	}
	for branch, want := range map[string]string{"canary": "7", "feat/other": "8", "nope": "", "": ""} {
		if got := IssueForBranch(entries, branch); got != want {
			t.Errorf("IssueForBranch(%q) = %q, want %q", branch, got, want)
		}
	}
}

// RemoveBranchSections drops exactly the claims recorded on the branch (#177):
// not another worktree's claim on the same issue, and nothing else in the file.
func TestRemoveBranchSections(t *testing.T) {
	t0 := fixedTime()
	content := AppendSection("", Entry{Issue: "7", Title: "canary", Branch: "canary", Worktree: "/w/canary", Window: "a", When: t0})
	content = AppendSection(content, Entry{Issue: "7", Title: "same issue, other worktree", Branch: "feat/7", Worktree: "/w/feat-7", Window: "b", When: t0})
	content = AppendSection(content, Entry{Issue: "9", Title: "canary again", Branch: "canary", Worktree: "/w/canary", Window: "a", When: t0})
	content = AppendSection(content, Entry{Issue: "8", Title: "other", Branch: "canary-2", Worktree: "/w/canary-2", Window: "c", When: t0})

	got, removed := RemoveBranchSections(content, "canary")
	if strings.Join(removed, ",") != "7,9" {
		t.Errorf("removed %v, want [7 9]", removed)
	}
	left := Parse(got)
	if len(left) != 2 || left[0].Branch != "feat/7" || left[0].Issue != "7" || left[1].Branch != "canary-2" {
		t.Fatalf("left %+v, want #7 on feat/7 and #8 on canary-2", left)
	}
	if !strings.HasPrefix(got, header) {
		t.Errorf("the file header went:\n%s", got)
	}
	// One section on the branch: the same bytes RemoveSection leaves.
	single := AppendSection(AppendSection("", Entry{Issue: "1", Branch: "a", When: t0}), Entry{Issue: "2", Branch: "b", When: t0})
	byBranch, _ := RemoveBranchSections(single, "a")
	byIssue, _ := RemoveSection(single, "1")
	if byBranch != byIssue {
		t.Errorf("RemoveBranchSections = %q, want RemoveSection's %q", byBranch, byIssue)
	}
	for _, b := range []string{"nope", ""} {
		if got, removed := RemoveBranchSections(content, b); got != content || removed != nil {
			t.Errorf("RemoveBranchSections(%q) changed the file or removed %v", b, removed)
		}
	}
}
