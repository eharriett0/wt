// Command wt is a multi-window git coordination CLI: per-window worktrees,
// claim/release a unit of work, file-level collision detection across windows,
// and merge/push guards. Repo-agnostic — works in any git repo on any machine.
package main

import (
	"os"

	"github.com/eharriett0/wt/internal/cli"
)

func main() {
	os.Exit(cli.Main(os.Args[1:]))
}
