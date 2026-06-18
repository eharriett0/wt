// Package merge implements the guarded squash-merge — the documented merge
// ritual ported from awesome-o's scripts/merge-pr.sh (#1344). It refuses to
// merge a PR whose diff vs base is EMPTY, or whose commits are ALL
// "WIP: claim #" placeholders (the claim-work placeholder bug class that
// merged vacuous PRs with zero real content).
package merge

import (
	"fmt"
	"os"
	"strconv"
	"strings"

	"github.com/eharriett0/wt/internal/ghx"
)

// Verdict is the outcome of the pure merge guard.
type Verdict string

const (
	VerdictOK              Verdict = "ok"
	VerdictEmptyDiff       Verdict = "block:empty_diff"
	VerdictPlaceholderOnly Verdict = "block:placeholder_only"
)

// GuardVerdict is the pure decision function (testable without gh).
//
//	fileCount — number of files changed in the PR diff vs base, as a string
//	            (matches the bash $1; unparseable or "0" → empty_diff).
//	subjects  — commit subject lines (one messageHeadline per commit).
//
// The empty-diff check is the load-bearing one; the placeholder-only check is
// belt-and-braces for the case where a non-empty diff somehow pairs with
// commits that are all claim-work placeholders.
func GuardVerdict(fileCount string, subjects []string) Verdict {
	n, err := strconv.Atoi(strings.TrimSpace(fileCount))
	if err != nil || n <= 0 {
		return VerdictEmptyDiff
	}

	hasAny, hasReal := false, false
	for _, line := range subjects {
		line = strings.TrimSpace(line)
		if line == "" {
			continue
		}
		hasAny = true
		if !strings.HasPrefix(line, "WIP: claim #") {
			hasReal = true
		}
	}
	if hasAny && !hasReal {
		return VerdictPlaceholderOnly
	}
	return VerdictOK
}

// Run executes the guarded merge for PR number pr. dryRun prints the verdict
// without merging; bypass proceeds past a block verdict (loud warning).
// extraArgs pass through to `gh pr merge`.
func Run(pr string, dryRun, bypass bool, extraArgs []string) error {
	fileCount := ghx.PRChangedFileCount(pr)
	subjects := ghx.PRCommitSubjects(pr)
	v := GuardVerdict(fileCount, subjects)

	switch v {
	case VerdictEmptyDiff:
		fmt.Fprintf(os.Stderr, "REFUSING to merge PR #%s — diff vs base is EMPTY (no real content).\n", pr)
		fmt.Fprintln(os.Stderr, "This is the claim-work placeholder bug class: a placeholder PR merging vacuously.")
		fmt.Fprintf(os.Stderr, "Bypass (rare — e.g. a deliberately empty change): wt merge-pr %s --bypass\n", pr)
		if !bypass {
			return fmt.Errorf("empty diff")
		}
		fmt.Fprintln(os.Stderr, "--bypass set — proceeding despite empty diff.")
	case VerdictPlaceholderOnly:
		fmt.Fprintf(os.Stderr, "REFUSING to merge PR #%s — every commit is a 'WIP: claim #' placeholder.\n", pr)
		fmt.Fprintln(os.Stderr, "The real work never landed on the branch.")
		fmt.Fprintf(os.Stderr, "Bypass: wt merge-pr %s --bypass\n", pr)
		if !bypass {
			return fmt.Errorf("placeholder only")
		}
		fmt.Fprintln(os.Stderr, "--bypass set — proceeding.")
	case VerdictOK:
		// fallthrough to merge
	}

	if dryRun {
		fmt.Printf("merge-pr: PR #%s verdict=%s file_count=%s (dry-run, not merging)\n", pr, v, fileCount)
		return nil
	}

	fmt.Printf("merge-pr: PR #%s has %s changed file(s) — merging (squash).\n", pr, fileCount)
	return ghx.MergePRSquash(pr, extraArgs)
}
