package gitx

import (
	"net"
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
	if err == nil || !strings.Contains(err.Error(), "gave no answer") {
		t.Errorf("FetchPRHeadNoPrompt = %v, want the no-answer error", err)
	}
}
