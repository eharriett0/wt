package ghx

import (
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"runtime"
	"strings"
	"testing"
)

// fakeGh puts a shell-script gh FIRST and ALONE on PATH (plus the system
// dirs for sh's own tools) that records its argv and stdin and talks to no
// network, and refuses to run unless "gh" resolves to it — so a test can never
// reach the real gh and merge a real PR.
func fakeGh(t *testing.T, script string) string {
	t.Helper()
	if runtime.GOOS == "windows" {
		t.Skip("the fake gh is a shell script")
	}
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "gh"), []byte("#!/bin/sh\nd=$(dirname \"$0\")\n"+script), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", dir+string(os.PathListSeparator)+"/usr/bin"+string(os.PathListSeparator)+"/bin")
	if got, err := exec.LookPath("gh"); err != nil || got != filepath.Join(dir, "gh") {
		t.Fatalf("gh resolves to %q (%v), not the fake", got, err)
	}
	return dir
}

// stdinFrom points os.Stdin at a file holding s for the rest of the test.
func stdinFrom(t *testing.T, s string) {
	t.Helper()
	f := filepath.Join(t.TempDir(), "stdin")
	if err := os.WriteFile(f, []byte(s), 0o644); err != nil {
		t.Fatal(err)
	}
	in, err := os.Open(f)
	if err != nil {
		t.Fatal(err)
	}
	old := os.Stdin
	os.Stdin = in
	t.Cleanup(func() { os.Stdin = old; in.Close() })
}

func readFile(t *testing.T, path string) string {
	t.Helper()
	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	return string(b)
}

// TestMergePRSquashStdin pins the #180 handoff: merge-pr reads a forwarded
// squash body ONCE and points gh at stdin, so gh must be fed exactly the bytes
// it is given. Fed wt's own stdin instead (already drained), gh would merge an
// EMPTY body. nil still means wt's own stdin.
func TestMergePRSquashStdin(t *testing.T) {
	dir := fakeGh(t, `printf '%s\n' "$@" > "$d/argv"; cat > "$d/stdin"`+"\n")
	stdinFrom(t, "wt's own stdin")

	if err := MergePRSquash("99999", []string{"--admin", "-F", "-"}, strings.NewReader("Forwarded body.\n")); err != nil {
		t.Fatal(err)
	}
	if got, want := strings.Fields(readFile(t, filepath.Join(dir, "argv"))), []string{"pr", "merge", "99999", "--squash", "--admin", "-F", "-"}; !reflect.DeepEqual(got, want) {
		t.Errorf("gh argv = %q, want %q", got, want)
	}
	if got := readFile(t, filepath.Join(dir, "stdin")); got != "Forwarded body.\n" {
		t.Errorf("gh stdin = %q, want the reader's bytes", got)
	}

	if err := MergePRSquash("99999", nil, nil); err != nil {
		t.Fatal(err)
	}
	if got := readFile(t, filepath.Join(dir, "stdin")); got != "wt's own stdin" {
		t.Errorf("gh stdin with a nil reader = %q, want wt's own stdin", got)
	}
}
