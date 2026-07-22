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

// BranchIsForeign reports whether head is NOT one of the wt-managed worktree
// branches (worktreeBranches) — i.e. the PR would merge a branch with no local
// wt lane here. This is the "merged the wrong lane" class from wt#15. Fail-open:
// an empty head OR an empty/unknown worktree set returns false (don't block —
// the surfaced head-branch line is the safety net when we can't be sure).
func BranchIsForeign(head string, worktreeBranches []string) bool {
	head = strings.TrimSpace(head)
	if head == "" || len(worktreeBranches) == 0 {
		return false
	}
	for _, b := range worktreeBranches {
		if strings.TrimSpace(b) == head {
			return false
		}
	}
	return true
}

// Run executes the guarded merge for PR number pr. dryRun prints the verdict
// without merging; bypass proceeds past a block verdict (loud warning).
// mergeForeign permits merging a PR whose head branch has no wt worktree here
// (the foreign-branch guard, wt#15). worktreeBranches is the set of wt-managed
// worktree branches for this repo. extraArgs pass through to `gh pr merge`.
func Run(pr string, dryRun, bypass, mergeForeign bool, worktreeBranches []string, extraArgs []string) error {
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

	head, _ := ghx.PRHeadBranch(pr)
	head = strings.TrimSpace(head)
	foreign := BranchIsForeign(head, worktreeBranches)

	// Always surface the head branch — the single line that catches a
	// wrong-branch merge before it happens (wt#15).
	label := "PR #" + pr
	if head != "" {
		label = fmt.Sprintf("PR #%s from branch %q", pr, head)
	}

	// dry-run previews (never blocks) — but flags a foreign head so the operator
	// sees the guard would fire on the real merge.
	if dryRun {
		note := ""
		if foreign {
			note = " [FOREIGN: head has no wt worktree here — a real merge needs --merge-foreign]"
		}
		fmt.Printf("merge-pr: %s verdict=%s file_count=%s%s (dry-run, not merging)\n", label, v, fileCount, note)
		return nil
	}

	// Foreign-branch guard (wt#15): refuse to merge a PR whose head branch has
	// no wt worktree here — in a multi-window setup this is the "merged the wrong
	// lane" class. --merge-foreign (or --bypass) proceeds. Fail-open via
	// BranchIsForeign when the head / worktree set is unknown.
	if foreign && !mergeForeign && !bypass {
		fmt.Fprintf(os.Stderr, "REFUSING to merge PR #%s — head branch %q has no wt worktree here (foreign branch).\n", pr, head)
		fmt.Fprintln(os.Stderr, "In a multi-window setup this is the 'merged the wrong lane' class (wt#15).")
		fmt.Fprintf(os.Stderr, "If intended: wt merge-pr %s --merge-foreign\n", pr)
		return fmt.Errorf("foreign branch")
	}

	fmt.Printf("merge-pr: %s — %s changed file(s) — merging (squash).\n", label, fileCount)
	return ghx.MergePRSquash(pr, extraArgs)
}
