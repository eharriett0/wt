package doctor

import (
	"os"
	"testing"
)

// TestMain keeps doctor's tests off the real `claude agents` (#250): a test
// run inside a Claude Code session would reach it through
// CLAUDE_CODE_EXECPATH. A test that wants live sessions sets livePeers.
func TestMain(m *testing.M) {
	os.Setenv("WT_PEERS", "0")
	os.Exit(m.Run())
}
