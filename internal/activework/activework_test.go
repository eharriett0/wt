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
