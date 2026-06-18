// Package ui is a tiny, dependency-free terminal styling layer. Colors are
// emitted only when stdout is a TTY and NO_COLOR / WT_NO_COLOR are unset, so
// piped or redirected output stays clean.
package ui

import (
	"fmt"
	"os"
)

// ANSI escape codes.
const (
	reset     = "\033[0m"
	boldSeq   = "\033[1m"
	dimSeq    = "\033[2m"
	redSeq    = "\033[31m"
	greenSeq  = "\033[32m"
	yellowSeq = "\033[33m"
	blueSeq   = "\033[34m"
	magenta   = "\033[35m"
	cyanSeq   = "\033[36m"
)

var enabled = detect()

func detect() bool {
	if _, ok := os.LookupEnv("NO_COLOR"); ok {
		return false
	}
	if _, ok := os.LookupEnv("WT_NO_COLOR"); ok {
		return false
	}
	fi, err := os.Stdout.Stat()
	if err != nil {
		return false
	}
	return (fi.Mode() & os.ModeCharDevice) != 0
}

// SetEnabled forces color on/off (used by tests).
func SetEnabled(v bool) { enabled = v }

func wrap(seq, s string) string {
	if !enabled {
		return s
	}
	return seq + s + reset
}

// Color/style helpers.
func Bold(s string) string    { return wrap(boldSeq, s) }
func Dim(s string) string     { return wrap(dimSeq, s) }
func Red(s string) string     { return wrap(redSeq, s) }
func Green(s string) string   { return wrap(greenSeq, s) }
func Yellow(s string) string  { return wrap(yellowSeq, s) }
func Blue(s string) string    { return wrap(blueSeq, s) }
func Magenta(s string) string { return wrap(magenta, s) }
func Cyan(s string) string    { return wrap(cyanSeq, s) }

// Semantic line helpers (to stdout unless noted).

// OK prints a green check line.
func OK(format string, a ...any) { fmt.Println(Green("✓ ") + fmt.Sprintf(format, a...)) }

// Step prints a dim arrow progress line.
func Step(format string, a ...any) { fmt.Println(Cyan("→ ") + fmt.Sprintf(format, a...)) }

// Info prints a blue info line.
func Info(format string, a ...any) { fmt.Println(Blue("ℹ ") + fmt.Sprintf(format, a...)) }

// Warn prints a yellow warning to stderr.
func Warn(format string, a ...any) {
	fmt.Fprintln(os.Stderr, Yellow("⚠ ")+fmt.Sprintf(format, a...))
}

// Err prints a red error to stderr.
func Err(format string, a ...any) {
	fmt.Fprintln(os.Stderr, Red("✗ ")+fmt.Sprintf(format, a...))
}

// Collision prints a bold-red collision line to stderr.
func Collision(format string, a ...any) {
	fmt.Fprintln(os.Stderr, Red(Bold("💥 "))+fmt.Sprintf(format, a...))
}

// Banner prints a small colorful header.
func Banner(title string) {
	fmt.Println()
	fmt.Println(Bold(Magenta("┃ ")) + Bold(title))
	fmt.Println()
}
