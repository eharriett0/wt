package collide

import (
	"strings"
	"testing"
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
	two := UnreadNote([]Window{wb, wc})
	for _, want := range []string{"2 windows could not be read", "wb (git status: fatal: bad object HEAD)", "#7 (git status: fatal: index file open failed)", "not compared"} {
		if !strings.Contains(two, want) {
			t.Errorf("two windows: %q lacks %q", two, want)
		}
	}
}
