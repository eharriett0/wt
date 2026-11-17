//go:build !unix

package gitx

import "os/exec"

// stopGroupOnCancel is a no-op off unix: the context's default kill stands.
func stopGroupOnCancel(_ *exec.Cmd) {}
