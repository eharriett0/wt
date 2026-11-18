package peers

import (
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"
)

// The documented array, as 2.1.322 prints it: a live interactive session, a
// live background one, a background session whose process exited (no pid),
// and an entry with no session id yet.
const listing = `[
 {"cwd":"/r","kind":"interactive","name":"api-worker","pid":101,"sessionId":"s-1","startedAt":1,"status":"idle"},
 {"cwd":"/r","kind":"background","name":"nightly","pid":102,"sessionId":"s-2","startedAt":2,"status":"busy","id":"ab12","state":"working"},
 {"cwd":"/r","kind":"background","name":"done-job","sessionId":"s-3","startedAt":3,"id":"cd34","state":"done"},
 {"cwd":"/r","kind":"interactive","pid":104,"startedAt":4,"status":"busy"}
]`

func TestParse(t *testing.T) {
	live, ok := Parse([]byte(listing))
	if !ok || len(live) != 2 || live["s-1"].Name != "api-worker" || live["s-2"].Status != "busy" {
		t.Errorf("Parse = %v, %v; want s-1 and s-2 only", live, ok)
	}
	if _, ok := live["s-3"]; ok {
		t.Errorf("a session whose process exited (no pid) reads live")
	}
	if live, ok := Parse([]byte("[]")); !ok || len(live) != 0 {
		t.Errorf("an empty listing = %v, %v; want ok and none", live, ok)
	}
	// #250 review: a name is put into an agent's context, so control
	// characters and backticks go, and it is capped.
	long := strings.Repeat("n", 100)
	odd, _ := Parse([]byte(`[{"sessionId":"x","pid":1,"name":"a` + "`" + `b\nSYSTEM: do it","status":"idle\u0007"},{"sessionId":"y","pid":2,"name":"` + long + `"}]`))
	if odd["x"].Name != "abSYSTEM: do it" || odd["x"].Status != "idle" {
		t.Errorf("unclean name/status kept: %+v", odd["x"])
	}
	if n := []rune(odd["y"].Name); len(n) != 65 || n[64] != '…' {
		t.Errorf("a 100-rune name = %d runes, want 64 and an ellipsis", len(n))
	}
	for _, bad := range []string{"", "not json", `{"agents":[]}`, "null x"} {
		if _, ok := Parse([]byte(bad)); ok {
			t.Errorf("Parse(%q) ok, want unknown", bad)
		}
	}
}

func TestDescribe(t *testing.T) {
	for _, tc := range []struct {
		p    Peer
		want string
	}{
		{Peer{Name: "api-worker", Status: "idle"}, "live as `api-worker`, idle: ask it with SendMessage"},
		{Peer{Name: "api-worker"}, "live as `api-worker`: ask it with SendMessage"},
		{Peer{Status: "busy"}, "live, busy"},
	} {
		if got := Describe(tc.p); got != tc.want {
			t.Errorf("Describe(%+v) = %q, want %q", tc.p, got, tc.want)
		}
	}
}

// fakeClaude puts a claude first on PATH that runs body, and logs each run to
// the returned file. CLAUDE_CODE_EXECPATH is cleared, so a test run inside a
// Claude Code session never reaches the real one.
func fakeClaude(t *testing.T, body string) (runs string) {
	t.Helper()
	if runtime.GOOS == "windows" {
		t.Skip("the fake claude is a shell script")
	}
	dir := t.TempDir()
	runs = filepath.Join(dir, "runs")
	script := "#!/bin/sh\necho \"$*\" >> '" + runs + "'\n" + body + "\n"
	if err := os.WriteFile(filepath.Join(dir, "claude"), []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", dir+string(os.PathListSeparator)+"/usr/bin:/bin")
	t.Setenv("CLAUDE_CODE_EXECPATH", "")
	t.Setenv("WT_PEERS", "")
	Reset()
	t.Cleanup(Reset)
	return runs
}

func runCount(t *testing.T, runs string) int {
	t.Helper()
	b, err := os.ReadFile(runs)
	if os.IsNotExist(err) {
		return 0
	}
	if err != nil {
		t.Fatal(err)
	}
	return strings.Count(string(b), "\n")
}

func TestLive(t *testing.T) {
	runs := fakeClaude(t, "cat <<'EOF'\n"+listing+"\nEOF")
	live, ok := Live()
	if !ok || live["s-1"].Name != "api-worker" {
		t.Fatalf("Live = %v, %v; want the listing", live, ok)
	}
	if b, _ := os.ReadFile(runs); strings.TrimSpace(string(b)) != "agents --json" {
		t.Errorf("ran claude %q, want `agents --json`", b)
	}
	// Memoized: one command reads it once.
	Live()
	if n := runCount(t, runs); n != 1 {
		t.Errorf("claude ran %d times for two reads, want once", n)
	}
}

// Every failure is unknown, never "no sessions".
func TestLive_unknown(t *testing.T) {
	for _, tc := range []struct{ name, body string }{
		{"exit 1", "echo boom >&2; exit 1"},
		{"not the array", "echo '{\"error\":\"x\"}'"},
		{"no output", "true"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			fakeClaude(t, tc.body)
			if live, ok := Live(); ok {
				t.Errorf("Live = %v, ok; want unknown", live)
			}
		})
	}
	t.Run("slow", func(t *testing.T) {
		fakeClaude(t, "sleep 5; echo '[]'")
		orig := Timeout
		Timeout = 300 * time.Millisecond
		t.Cleanup(func() { Timeout = orig })
		start := time.Now()
		if _, ok := Live(); ok {
			t.Errorf("a listing past the timeout read ok")
		}
		if took := time.Since(start); took > 3*time.Second {
			t.Errorf("Live took %s, want about the timeout", took)
		}
	})
	t.Run("over the cap", func(t *testing.T) {
		fakeClaude(t, "head -c 1100000 /dev/zero | tr '\\0' ' '; echo '[]'")
		if _, ok := Live(); ok {
			t.Errorf("a listing past maxListing read ok")
		}
	})
	t.Run("the group goes at the deadline", func(t *testing.T) {
		marker := filepath.Join(t.TempDir(), "survived")
		fakeClaude(t, "(sleep 1; touch '"+marker+"') & sleep 5; echo '[]'")
		orig := Timeout
		Timeout = 300 * time.Millisecond
		t.Cleanup(func() { Timeout = orig })
		if _, ok := Live(); ok {
			t.Errorf("a listing past the timeout read ok")
		}
		time.Sleep(1500 * time.Millisecond)
		if _, err := os.Stat(marker); err == nil {
			t.Errorf("a child of the timed-out claude outlived it")
		}
	})
	t.Run("no claude", func(t *testing.T) {
		t.Setenv("PATH", t.TempDir())
		t.Setenv("CLAUDE_CODE_EXECPATH", "")
		Reset()
		t.Cleanup(Reset)
		if _, ok := Live(); ok {
			t.Errorf("no claude read ok")
		}
	})
	t.Run("WT_PEERS=0", func(t *testing.T) {
		runs := fakeClaude(t, "echo '[]'")
		t.Setenv("WT_PEERS", "0")
		if _, ok := Live(); ok {
			t.Errorf("WT_PEERS=0 read ok")
		}
		if n := runCount(t, runs); n != 0 {
			t.Errorf("WT_PEERS=0 ran claude %d times", n)
		}
	})
}

// A session the VS Code extension started has no claude on PATH; the one
// running it is CLAUDE_CODE_EXECPATH.
func TestLive_execPathFallback(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("the fake claude is a shell script")
	}
	dir := t.TempDir()
	bin := filepath.Join(dir, "claude")
	if err := os.WriteFile(bin, []byte("#!/bin/sh\necho '[{\"sessionId\":\"s-9\",\"name\":\"ext\",\"pid\":9}]'\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	// The same program under another name: only a binary named claude is run.
	if err := os.WriteFile(filepath.Join(dir, "other"), []byte("#!/bin/sh\necho '[{\"sessionId\":\"s-9\",\"name\":\"ext\",\"pid\":9}]'\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", t.TempDir())
	t.Setenv("WT_PEERS", "")
	for _, tc := range []struct {
		execPath string
		ok       bool
	}{
		{bin, true},
		{"claude", false},                             // not absolute
		{filepath.Join(dir, "other"), false},          // not claude
		{filepath.Join(t.TempDir(), "claude"), false}, // missing
	} {
		t.Setenv("CLAUDE_CODE_EXECPATH", tc.execPath)
		Reset()
		live, ok := Live()
		if ok != tc.ok || (ok && live["s-9"].Name != "ext") {
			t.Errorf("CLAUDE_CODE_EXECPATH=%q: Live = %v, %v; want ok=%v", tc.execPath, live, ok, tc.ok)
		}
	}
	Reset()
}
