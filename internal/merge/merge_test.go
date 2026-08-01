package merge

import (
	"reflect"
	"testing"
)

func TestGuardVerdict(t *testing.T) {
	cases := []struct {
		name      string
		fileCount string
		subjects  []string
		want      Verdict
	}{
		{"empty: zero files", "0", nil, VerdictEmptyDiff},
		{"empty: unparseable", "", nil, VerdictEmptyDiff},
		{"empty: non-numeric", "abc", nil, VerdictEmptyDiff},
		{"empty: negative", "-3", nil, VerdictEmptyDiff},
		{"empty: zero files even with real commits", "0", []string{"feat: do the thing"}, VerdictEmptyDiff},
		{"ok: one file one real commit", "1", []string{"feat: do the thing"}, VerdictOK},
		{"ok: real + placeholder mixed", "3", []string{"WIP: claim #42 — foo", "feat: real work"}, VerdictOK},
		{"placeholder-only: single placeholder, nonzero files", "2", []string{"WIP: claim #42 — foo"}, VerdictPlaceholderOnly},
		{"placeholder-only: multiple placeholders", "5", []string{"WIP: claim #1 — a", "WIP: claim #2 — b"}, VerdictPlaceholderOnly},
		{"ok: nonzero files, no commit subjects", "4", nil, VerdictOK},
		{"ok: blank lines ignored, real present", "2", []string{"", "feat: x", ""}, VerdictOK},
		{"placeholder-only: blank lines + placeholder", "2", []string{"", "WIP: claim #9 — z", ""}, VerdictPlaceholderOnly},
		{"ok: whitespace-padded count", "  7 ", []string{"fix: y"}, VerdictOK},
		{"ok: prefix-similar but not placeholder", "1", []string{"WIPfoo claim # not a placeholder"}, VerdictOK},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := GuardVerdict(tc.fileCount, tc.subjects); got != tc.want {
				t.Errorf("GuardVerdict(%q, %v) = %q, want %q", tc.fileCount, tc.subjects, got, tc.want)
			}
		})
	}
}

func TestBranchIsForeign(t *testing.T) {
	managed := []string{"feat-1-alpha", "feat-2-beta", "fix-3-gamma"}
	cases := []struct {
		name     string
		head     string
		branches []string
		want     bool
	}{
		{"managed branch is not foreign", "feat-2-beta", managed, false},
		{"unknown branch is foreign", "feat-99-other-window", managed, true},
		{"empty head fails open (not foreign)", "", managed, false},
		{"whitespace-only head fails open", "   ", managed, false},
		{"empty worktree set fails open (can't determine)", "feat-2-beta", nil, false},
		{"empty worktree set + unknown head still fails open", "whatever", []string{}, false},
		{"head matches after trimming", "  feat-1-alpha  ", managed, false},
		{"managed entry padded, head clean", "fix-3-gamma", []string{" fix-3-gamma "}, false},
		{"case-sensitive: different case IS foreign", "Feat-1-Alpha", managed, true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := BranchIsForeign(tc.head, tc.branches); got != tc.want {
				t.Errorf("BranchIsForeign(%q, %v) = %v, want %v", tc.head, tc.branches, got, tc.want)
			}
		})
	}
}

func TestWithAdmin(t *testing.T) {
	cases := []struct {
		name  string
		admin bool
		extra []string
		want  []string
	}{
		{"off: nil unchanged", false, nil, nil},
		{"off: extra passthrough unchanged", false, []string{"--delete-branch"}, []string{"--delete-branch"}},
		{"on: appends to nil", true, nil, []string{"--admin"}},
		{"on: appends after existing passthrough", true, []string{"--delete-branch"}, []string{"--delete-branch", "--admin"}},
		{"on: dedups when passthrough already has --admin", true, []string{"--admin"}, []string{"--admin"}},
		{"on: dedups whitespace-padded --admin", true, []string{" --admin "}, []string{" --admin "}},
		{"on: dedups --admin alongside other args", true, []string{"--delete-branch", "--admin"}, []string{"--delete-branch", "--admin"}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := WithAdmin(tc.admin, tc.extra); !reflect.DeepEqual(got, tc.want) {
				t.Errorf("WithAdmin(%v, %v) = %v, want %v", tc.admin, tc.extra, got, tc.want)
			}
		})
	}
}

// TestWithAdminDoesNotMutateCaller pins the copy-before-append: appending to a
// slice with spare capacity would clobber the caller's backing array. WithAdmin
// must leave the input slice untouched.
func TestWithAdminDoesNotMutateCaller(t *testing.T) {
	extra := make([]string, 1, 4) // len 1, cap 4 — spare capacity to clobber
	extra[0] = "--delete-branch"
	got := WithAdmin(true, extra)
	if len(extra) != 1 || extra[0] != "--delete-branch" {
		t.Errorf("caller slice mutated: %v", extra)
	}
	if want := []string{"--delete-branch", "--admin"}; !reflect.DeepEqual(got, want) {
		t.Errorf("WithAdmin returned %v, want %v", got, want)
	}
}

func TestDeWIPTitle(t *testing.T) {
	cases := []struct {
		in     string
		want   string
		wasWIP bool
	}{
		{"WIP: #1313 — Go runtime", "#1313 — Go runtime", true},
		{"WIP:no space", "no space", true},
		{"  WIP: trimmed  ", "trimmed", true},
		{"feat(x): real title", "feat(x): real title", false},
		{"not a wip", "not a wip", false},
	}
	for _, c := range cases {
		got, wip := DeWIPTitle(c.in)
		if got != c.want || wip != c.wasWIP {
			t.Errorf("DeWIPTitle(%q) = (%q,%v), want (%q,%v)", c.in, got, wip, c.want, c.wasWIP)
		}
	}
}
