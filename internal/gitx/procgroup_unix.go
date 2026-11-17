//go:build unix

package gitx

import (
	"os/exec"
	"syscall"
)

// stopGroupOnCancel puts cmd in a process group of its own and makes its
// cancel SIGTERM the whole group (#230 re-review): killing only the started
// git left the real fetch and its transport helper running (Apple's git runs
// the fetch as a child of a wrapper), and SIGKILL kept git from removing its
// .lock files. cmd.WaitDelay escalates to SIGKILL for the leader. The group
// no longer gets the terminal's Ctrl-C, so a git left behind stops on its own
// low-speed and connect timeouts.
func stopGroupOnCancel(cmd *exec.Cmd) {
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	cmd.Cancel = func() error {
		return syscall.Kill(-cmd.Process.Pid, syscall.SIGTERM)
	}
}
