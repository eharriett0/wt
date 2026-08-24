// Free-form message input for coordination commands (eharriett0/wt#75).
//
// announce / ack --state / append take prose that routinely contains backticks,
// $, ! and quotes. Passed as a shell argument, those are expanded by the shell
// BEFORE wt ever sees the string — a backticked span that substitutes cleanly is
// silently deleted from the message, and because these commands are usually piped
// to keep output short, the "✓ announced" line still prints while the warning
// went out with its filenames removed. `--file <path>` (or `--file -` for stdin)
// reads the content opaquely, sidestepping the shell entirely.
package cli

import (
	"io"
	"os"
	"strings"

	"github.com/eharriett0/wt/internal/ui"
)

// readFreeform resolves free-form text: from --file (a path, or "-" for stdin)
// when set — opaque to the shell — otherwise the positional args joined with
// spaces (the legacy path). The result is whitespace-trimmed.
func readFreeform(file string, pos []string) (string, error) {
	if file == "" {
		return strings.TrimSpace(strings.Join(pos, " ")), nil
	}
	var (
		b   []byte
		err error
	)
	if file == "-" {
		b, err = io.ReadAll(os.Stdin)
	} else {
		b, err = os.ReadFile(file)
	}
	if err != nil {
		return "", err
	}
	return strings.TrimSpace(string(b)), nil
}

// hasUnbalancedBacktick is the #75 heuristic: an odd number of backticks in a
// shell-sourced message means a `...` span was very likely eaten by command
// substitution before wt saw it.
func hasUnbalancedBacktick(s string) bool {
	return strings.Count(s, "`")%2 == 1
}

// warnSuspiciousFreeform advises switching to --file when a shell-sourced message
// shows the signature of substitution damage (#75). Advisory only — never blocks.
func warnSuspiciousFreeform(file, msg string) {
	if file == "" && hasUnbalancedBacktick(msg) {
		ui.Warn("message has an unbalanced backtick — your shell may have eaten a `...` span " +
			"before wt saw it; use --file <path> (or --file - for stdin) for messages with backticks / $ / !")
	}
}

// echoStored confirms what was actually recorded, so a shell-mangled message is
// caught at send time rather than days later in another window's inbox (#75).
func echoStored(msg string) {
	if msg == "" {
		return
	}
	preview := strings.ReplaceAll(msg, "\n", " ")
	if len(preview) > 80 {
		preview = preview[:80] + "…"
	}
	ui.Info("recorded %d bytes: %s", len(msg), ui.Dim(preview))
}
