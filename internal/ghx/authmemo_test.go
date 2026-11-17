//go:build unix

package ghx

import (
	"os"
	"os/exec"
	"path/filepath"
	"testing"
)

// The gh auth answer is one gh binary's (#197 review): keyed by host alone,
// a check asked of one gh was reused for another gh in the same process
// within authTTL (a test's real gh said "not logged in", and a later test's
// fake gh never got asked, so its merge refused). Asked of a second binary,
// the check runs again.
func TestAuthMemo_perGhBinary(t *testing.T) {
	authMu.Lock()
	savedMemo, savedNoJSON := authMemo, authNoJSON
	authMemo, authNoJSON = map[string]authAnswer{}, map[string]bool{}
	authMu.Unlock()
	t.Cleanup(func() {
		authMu.Lock()
		authMemo, authNoJSON = savedMemo, savedNoJSON
		authMu.Unlock()
	})
	shim := func() (dir, calls string) {
		dir = t.TempDir()
		calls = filepath.Join(dir, "calls")
		if err := os.WriteFile(filepath.Join(dir, "gh"), []byte("#!/bin/sh\necho \"$*\" >> '"+calls+"'\nexit 1\n"), 0o755); err != nil {
			t.Fatal(err)
		}
		return dir, calls
	}
	use := func(dir string) {
		t.Setenv("PATH", dir+string(os.PathListSeparator)+"/usr/bin"+string(os.PathListSeparator)+"/bin")
		if p, err := exec.LookPath("gh"); err != nil || p != filepath.Join(dir, "gh") {
			t.Fatalf("gh resolves to %q (%v), not the shim", p, err)
		}
	}
	asked := func(calls string) bool {
		_, err := os.Stat(calls)
		return err == nil
	}
	dirA, callsA := shim()
	dirB, callsB := shim()
	use(dirA)
	AuthedFor("git.acme.test")
	if !asked(callsA) {
		t.Fatal("precondition: the first gh was never asked")
	}
	use(dirB)
	AuthedFor("git.acme.test")
	if !asked(callsB) {
		t.Error("the second gh binary was never asked: the first one's answer was reused")
	}
	// Same binary, same host, within authTTL: the answer is reused.
	if err := os.Remove(callsB); err != nil {
		t.Fatal(err)
	}
	AuthedFor("git.acme.test")
	if asked(callsB) {
		t.Error("the same gh was asked again within authTTL")
	}
}
