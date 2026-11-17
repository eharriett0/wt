//go:build !unix

package lock

import (
	"os"
	"time"
)

// Exclusive is a no-op on non-unix platforms — wt targets darwin/linux, so this
// only keeps the tree buildable elsewhere. Without a real lock, concurrent
// read-modify-write degrades to racy rather than failing to build.
func Exclusive(_ *os.File) error { return nil }

// SharedWithin is a no-op on non-unix platforms.
func SharedWithin(_ *os.File, _ time.Duration) error { return nil }

// ExclusiveWithin is a no-op on non-unix platforms.
func ExclusiveWithin(_ *os.File, _, _ time.Duration, _ func()) error { return nil }

// Unsupported is false on non-unix platforms: no lock call fails there.
func Unsupported(_ error) bool { return false }

// Release is a no-op on non-unix platforms.
func Release(_ *os.File) {}
