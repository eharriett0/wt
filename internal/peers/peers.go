// Package peers reads which Claude Code sessions are live on this machine, and
// what they are called, so wt can say who holds what in words an agent can act
// on: Claude Code's cross-session messaging (v2.1.224+) addresses a session by
// its name with SendMessage (#250).
//
// The read is `claude agents --json`, which Claude Code documents as "the
// supported way to read session state from outside Claude Code". It is
// advisory: a listing wt can't read is unknown, never "no sessions", and a
// session missing from it is never treated as gone (the listing's coverage
// varies by version and entrypoint).
package peers

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"sync"
	"time"
)

// Peer is one live Claude Code session as `claude agents --json` lists it.
type Peer struct {
	SessionID string `json:"sessionId"`
	Name      string `json:"name"`
	Status    string `json:"status"` // busy, waiting or idle
	Kind      string `json:"kind"`   // interactive or background
	Cwd       string `json:"cwd"`
	PID       int    `json:"pid"`
}

// Describe is how a message names a live session: its name and status, and
// how to reach it ("live as `api-worker`, idle: ask it with SendMessage").
// Pure.
func Describe(p Peer) string {
	s := "live"
	if p.Name != "" {
		s += " as `" + p.Name + "`"
	}
	if p.Status != "" {
		s += ", " + p.Status
	}
	if p.Name != "" {
		s += ": ask it with SendMessage"
	}
	return s
}

// Timeout bounds the listing (measured at about 140 ms on 2.1.322). A var so
// a test can shorten it.
var Timeout = 2 * time.Second

// memoTTL is how long one listing is reused: one command reads it once, and
// a long-lived `wt mcp` still sees sessions come and go.
const memoTTL = 10 * time.Second

var (
	mu       sync.Mutex
	memoAt   time.Time
	memoLive map[string]Peer
	memoOK   bool
)

// Live lists the live sessions by session id. ok is false when the listing
// could not be read (no claude binary, a failure, the Timeout, output that
// isn't the documented array, or WT_PEERS=0): unknown, which callers read as
// "say nothing more", never as "no sessions".
func Live() (live map[string]Peer, ok bool) {
	if os.Getenv("WT_PEERS") == "0" {
		return nil, false
	}
	mu.Lock()
	defer mu.Unlock()
	if !memoAt.IsZero() && time.Since(memoAt) < memoTTL {
		return memoLive, memoOK
	}
	memoLive, memoOK = read()
	memoAt = time.Now()
	return memoLive, memoOK
}

// Reset drops the memoized listing; for tests.
func Reset() {
	mu.Lock()
	defer mu.Unlock()
	memoAt, memoLive, memoOK = time.Time{}, nil, false
}

func read() (map[string]Peer, bool) {
	bin := binary()
	if bin == "" {
		return nil, false
	}
	ctx, cancel := context.WithTimeout(context.Background(), Timeout)
	defer cancel()
	cmd := exec.CommandContext(ctx, bin, "agents", "--json")
	cmd.WaitDelay = 250 * time.Millisecond
	out, err := cmd.Output()
	if err != nil {
		return nil, false
	}
	return Parse(out)
}

// binary is the claude to ask: the one on PATH, else the one running this
// session (CLAUDE_CODE_EXECPATH, set in its hooks and Bash commands; a session
// the VS Code extension started has no claude on PATH). "" when neither.
func binary() string {
	if p, err := exec.LookPath("claude"); err == nil {
		return p
	}
	if p := os.Getenv("CLAUDE_CODE_EXECPATH"); filepath.IsAbs(p) && filepath.Base(p) == "claude" {
		if fi, err := os.Stat(p); err == nil && fi.Mode().IsRegular() && fi.Mode()&0o111 != 0 {
			return p
		}
	}
	return ""
}

// Parse reads `claude agents --json`: a JSON array of sessions. Only an entry
// with a session id and a live process (`pid` is "present while the process is
// alive") is a peer; a background session whose process exited is listed
// without one. ok is false for anything but an array. Pure.
func Parse(out []byte) (map[string]Peer, bool) {
	var all []Peer
	if err := json.Unmarshal(out, &all); err != nil {
		return nil, false
	}
	live := make(map[string]Peer, len(all))
	for _, p := range all {
		if p.SessionID != "" && p.PID > 0 {
			live[p.SessionID] = p
		}
	}
	return live, true
}

// String is a Peer as a debug line.
func (p Peer) String() string {
	return fmt.Sprintf("%s (%s, pid %d, %s)", p.Name, p.Status, p.PID, p.SessionID)
}
