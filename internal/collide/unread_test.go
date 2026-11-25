package collide

import (
	"strings"
	"testing"

	"github.com/eharriett0/wt/internal/gitx"
)

// #253: windows git could not read are named; the caller's own is left out,
// and the order is the scan's.
func TestUnreadWindows(t *testing.T) {
	ws := []Window{
		{Worktree: "/r/main", Branch: "main"},
		{Worktree: "/r/wb", Branch: "wb", Unread: "git status: fatal: bad object HEAD"},
		{Worktree: "/r/wa", Branch: "wa", Unread: "git status: fatal: index file open failed"},
	}
	labels := func(ws []Window) string {
		var l []string
		for _, w := range ws {
			l = append(l, w.Label())
		}
		return strings.Join(l, ",")
	}
	for _, tc := range []struct{ self, want string }{
		{"", "wb,wa"},
		{"/r/main", "wb,wa"},
		{"/r/wa", "wb"},
		{"/r/wb", "wa"},
	} {
		if got := labels(UnreadWindows(ws, tc.self)); got != tc.want {
			t.Errorf("UnreadWindows(self %q) = %s, want %s", tc.self, got, tc.want)
		}
	}
	if got := UnreadWindows(ws[:1], ""); got != nil {
		t.Errorf("no unread window: %v, want nil", got)
	}
}

func TestUnreadNote(t *testing.T) {
	wb := Window{Worktree: "/r/wb", Branch: "wb", Unread: "git status: fatal: bad object HEAD"}
	wc := Window{Worktree: "/r/wc", Branch: "wc", Issue: "7", Unread: "git status: fatal: index file open failed"}
	if got := UnreadNote(nil); got != "" {
		t.Errorf("UnreadNote(nil) = %q, want empty", got)
	}
	one := UnreadNote([]Window{wb})
	for _, want := range []string{"window wb could not be read", "bad object HEAD", "not compared", "git status` in /r/wb"} {
		if !strings.Contains(one, want) {
			t.Errorf("one window: %q lacks %q", one, want)
		}
	}
	// A reason that is not git's own failure gets no `git status` advice: in
	// a --separate-git-dir repository's git directory it can't help.
	sep := UnreadNote([]Window{{Worktree: "/r/sep.git", Branch: "main", Unread: "it is the repository's git directory"}})
	if !strings.Contains(sep, "git directory") || strings.Contains(sep, "git status`") {
		t.Errorf("git-directory window: %q, want its reason and no `git status` advice", sep)
	}
	// git never ran there: its commits weren't read either, and `git status`
	// there can't help (#254 review).
	nodir := UnreadNote([]Window{{Worktree: "/r/wd", Branch: "wd", Unread: gitx.ErrNoDir.Error() + ": open /r/wd: permission denied", CommitsUnread: true}})
	if !strings.Contains(nodir, "none of its edits are compared") || strings.Contains(nodir, "git status`") {
		t.Errorf("unreadable directory: %q, want none of its edits compared and no `git status` advice", nodir)
	}
	two := UnreadNote([]Window{wb, wc})
	for _, want := range []string{"2 windows could not be read", "wb at /r/wb (git status: fatal: bad object HEAD)", "#7 at /r/wc (git status: fatal: index file open failed)", "not compared"} {
		if !strings.Contains(two, want) {
			t.Errorf("two windows: %q lacks %q", two, want)
		}
	}
	if strings.Contains(two, "git status` in each") {
		t.Errorf("two windows: %q gives `git status` advice that can't help every kind", two)
	}
}

// An unread window counts as dirty (IsClean fails), so it is never hidden, but
// its tag says its working tree was not read, not that it has edits (#254
// review).
func TestWindowLiveness_UnreadLabel(t *testing.T) {
	for _, c := range []struct {
		wl   WindowLiveness
		want string
	}{
		{WindowLiveness{Level: LiveDirty, Dirty: true, Unread: true}, "working tree unread"},
		{WindowLiveness{Level: LiveDirty, Dirty: true}, "uncommitted edits"},
		{WindowLiveness{Level: LiveOpenPR, PR: "7", Dirty: true, Unread: true}, "open PR #7 · working tree unread"},
		{WindowLiveness{Level: LiveStale, MergedPR: "9", Dirty: true, Unread: true}, "merged #9 · working tree unread"},
		{WindowLiveness{Level: LiveStale, MergedPR: "9", Dirty: true}, "merged #9 · leftover uncommitted edits"},
	} {
		if got := c.wl.Label(); got != c.want {
			t.Errorf("%+v: Label() = %q, want %q", c.wl, got, c.want)
		}
	}
}

// What an unread window did not compare comes from what was read, not from
// its reason's wording (#254 review).
func TestNotCompared(t *testing.T) {
	for _, c := range []struct {
		w           Window
		want, scope string
	}{
		{Window{}, "", "its uncommitted edits are not compared"},
		{Window{Unread: "git status: fatal: index file corrupt"}, "uncommitted", "its uncommitted edits are not compared"},
		{Window{Unread: "git status: fatal: not a git repository", CommitsUnread: true}, "all", "none of its edits are compared, committed or not"},
		{Window{Unread: "git status: d/k.txt: Permission denied", UnreadSome: true}, "some_uncommitted", "some of its uncommitted edits are not compared"},
		{Window{Unread: "git status: x", UnreadSome: true, CommitsUnread: true}, "some_uncommitted", "some of its uncommitted edits are not compared"},
	} {
		if got := NotCompared(c.w); got != c.want {
			t.Errorf("NotCompared(%+v) = %q, want %q", c.w, got, c.want)
		}
		if c.w.Unread != "" {
			if got := UnreadScope(c.w); got != c.scope {
				t.Errorf("UnreadScope(%+v) = %q, want %q", c.w, got, c.scope)
			}
		}
	}
}
