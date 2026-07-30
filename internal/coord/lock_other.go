//go:build !unix

package coord

import "os"

// flockExclusive is a no-op on non-unix platforms — wt targets darwin/linux, so
// this only exists to keep the package buildable elsewhere. Without a real lock,
// concurrent block-id allocation degrades to the pre-#23 (racy) behavior rather
// than failing to build.
func flockExclusive(_ *os.File) error { return nil }

// flockRelease is a no-op on non-unix platforms.
func flockRelease(_ *os.File) {}
