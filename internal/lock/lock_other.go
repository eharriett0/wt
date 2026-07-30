//go:build !unix

package lock

import "os"

// Exclusive is a no-op on non-unix platforms — wt targets darwin/linux, so this
// only keeps the tree buildable elsewhere. Without a real lock, concurrent
// read-modify-write degrades to racy rather than failing to build.
func Exclusive(_ *os.File) error { return nil }

// Release is a no-op on non-unix platforms.
func Release(_ *os.File) {}
