package gitx

import (
	"errors"
	"net"
	"os/exec"
	"strings"
	"testing"
	"time"
)

// A no-prompt fetch gives up on an origin that takes the connection and never
// answers (#230 re-review: merge-pr's post-merge cleanup hung on one).
func TestFetchNoPrompt_GivesUpOnASilentOrigin(t *testing.T) {
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Skip("no local listener:", err)
	}
	defer ln.Close()
	var held []net.Conn
	go func() {
		for {
			c, err := ln.Accept()
			if err != nil {
				return
			}
			held = append(held, c) // accepted, never answered
		}
	}()
	t.Cleanup(func() {
		for _, c := range held {
			c.Close()
		}
	})
	// A runner's proxy would answer instead of the silent listener.
	for _, k := range []string{"http_proxy", "HTTP_PROXY", "https_proxy", "HTTPS_PROXY", "all_proxy", "ALL_PROXY"} {
		t.Setenv(k, "")
	}
	t.Setenv("NO_PROXY", "*")
	repo := gitRepo(t)
	runGit(t, repo, "remote", "add", "origin", "http://"+ln.Addr().String()+"/x.git")
	orig := fetchTimeout
	fetchTimeout = 2 * time.Second
	t.Cleanup(func() { fetchTimeout = orig })
	t.Chdir(repo)

	start := time.Now()
	err = FetchPRHeadNoPrompt("origin", "5")
	if took := time.Since(start); took > 10*time.Second {
		t.Fatalf("the fetch took %s against a silent origin", took)
	}
	if err == nil || !strings.Contains(err.Error(), "did not finish in") {
		t.Errorf("FetchPRHeadNoPrompt = %v, want the deadline error", err)
	}
	var late *FetchTimeoutError
	if !errors.As(err, &late) {
		t.Errorf("FetchPRHeadNoPrompt = %T, want a *FetchTimeoutError", err)
	}
	// Nothing of that fetch is left running while the origin still holds the
	// connection (#230 re-review: killing only the started git left the real
	// fetch and git-remote-http behind). The whole group got the signal.
	if pgrep, perr := exec.LookPath("pgrep"); perr == nil {
		time.Sleep(500 * time.Millisecond)
		if out, _ := exec.Command(pgrep, "-fl", ln.Addr().String()).Output(); len(strings.TrimSpace(string(out))) > 0 {
			t.Errorf("processes of the cancelled fetch are still running:\n%s", out)
		}
	}
}
