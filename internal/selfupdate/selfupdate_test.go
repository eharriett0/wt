package selfupdate

import (
	"os"
	"path/filepath"
	"testing"
	"time"
)

func TestElapsed(t *testing.T) {
	now := time.Unix(1_000_000, 0)
	// stamp 2h ago, interval 24h → not elapsed
	if Elapsed("993000", now, 24*time.Hour) {
		t.Error("2h-old stamp should NOT be elapsed at 24h interval")
	}
	// stamp 48h ago → elapsed
	if !Elapsed("827200", now, 24*time.Hour) {
		t.Error("48h-old stamp SHOULD be elapsed at 24h interval")
	}
	// empty/garbage → elapsed (first run always checks)
	if !Elapsed("", now, 24*time.Hour) || !Elapsed("garbage", now, 24*time.Hour) {
		t.Error("empty/garbage stamp should be elapsed")
	}
}

func TestBehind(t *testing.T) {
	if !Behind("a5607d7f29d0", "a1b2c3d4e5f6aaaa") {
		t.Error("different commits → behind")
	}
	// local pseudo-version 12-hex prefix of the full remote SHA → NOT behind
	if Behind("a5607d7f29d0", "a5607d7f29d0ffffffff") {
		t.Error("prefix match → NOT behind")
	}
	if Behind("", "abc") || Behind("abc", "") {
		t.Error("empty side → not behind (can't tell)")
	}
}

func TestCheck_ThrottleFastPath(t *testing.T) {
	// A fresh stamp (now) means the interval hasn't elapsed → Check returns ""
	// WITHOUT any network I/O. This is the hot path taken on ~every run.
	stamp := filepath.Join(t.TempDir(), "update-check")
	now := time.Unix(2_000_000, 0)
	if err := os.WriteFile(stamp, []byte("2000000"), 0o644); err != nil {
		t.Fatal(err)
	}
	if got := Check(stamp, now, 24*time.Hour); got != "" {
		t.Errorf("fresh stamp should short-circuit to \"\", got %q", got)
	}
}
