// Package ghx wraps the GitHub CLI (gh) via os/exec. Same shell-out approach
// as the original bash; no go-github dependency.
package ghx

import (
	"fmt"
	"os"
	"os/exec"
	"strings"
)

func run(args ...string) (string, error) {
	cmd := exec.Command("gh", args...)
	out, err := cmd.Output()
	return strings.TrimSpace(string(out)), err
}

// Present reports whether gh is on PATH.
func Present() bool {
	_, err := exec.LookPath("gh")
	return err == nil
}

// Authed reports whether gh has an authenticated account.
func Authed() bool {
	cmd := exec.Command("gh", "auth", "status")
	return cmd.Run() == nil
}

// CurrentUser returns the authenticated login.
func CurrentUser() (string, error) { return run("api", "user", "--jq", ".login") }

// --- Issues ---

// IssueExists reports whether issue n exists in the current repo.
func IssueExists(n string) bool {
	cmd := exec.Command("gh", "issue", "view", n, "--json", "number")
	return cmd.Run() == nil
}

// IssueState returns OPEN/CLOSED.
func IssueState(n string) (string, error) {
	return run("issue", "view", n, "--json", "state", "--jq", ".state")
}

// IssueTitle returns the issue title.
func IssueTitle(n string) (string, error) {
	return run("issue", "view", n, "--json", "title", "--jq", ".title")
}

// IssueAssignees returns the assignee logins.
func IssueAssignees(n string) []string {
	out, err := run("issue", "view", n, "--json", "assignees", "--jq", ".assignees[].login")
	if err != nil || out == "" {
		return nil
	}
	return strings.Split(out, "\n")
}

// IssueAddAssigneeMe assigns the issue to the current user.
func IssueAddAssigneeMe(n string) error {
	_, err := run("issue", "edit", n, "--add-assignee", "@me")
	return err
}

// IssueRemoveAssignee unassigns user from the issue (best-effort).
func IssueRemoveAssignee(n, user string) error {
	_, err := run("issue", "edit", n, "--remove-assignee", user)
	return err
}

// --- PRs ---

// PRCreateArgs builds the `gh pr create` argv. head and base are passed
// EXPLICITLY so PR creation does not depend on the invoking working directory's
// current branch: `wt claim` runs from the main checkout (sitting on the base
// branch), and without --head, gh infers head=base and fails "no commits
// between base and base" — which made the draft PR silently never appear.
// Pure (no I/O) so the --head/--base contract is unit-testable.
func PRCreateArgs(draft bool, head, base, title, body string) []string {
	args := []string{"pr", "create", "--head", head, "--base", base, "--title", title, "--body", body}
	if draft {
		args = append(args, "--draft")
	}
	return args
}

// PRCreate opens a PR for head against base and returns its URL.
func PRCreate(draft bool, head, base, title, body string) (string, error) {
	return run(PRCreateArgs(draft, head, base, title, body)...)
}

// PRChangedFileCount returns the number of files in the PR diff vs base, as a
// string (matches merge.GuardVerdict's input contract).
func PRChangedFileCount(pr string) string {
	out, err := run("pr", "diff", pr, "--name-only")
	if err != nil {
		return "0"
	}
	n := 0
	for _, ln := range strings.Split(out, "\n") {
		if strings.TrimSpace(ln) != "" {
			n++
		}
	}
	return fmt.Sprintf("%d", n)
}

// PRCommitSubjects returns one messageHeadline per commit on the PR.
func PRCommitSubjects(pr string) []string {
	out, err := run("pr", "view", pr, "--json", "commits", "--jq", ".commits[].messageHeadline")
	if err != nil || out == "" {
		return nil
	}
	return strings.Split(out, "\n")
}

// MergePRSquash runs `gh pr merge <pr> --squash <extra...>`, inheriting stdio.
func MergePRSquash(pr string, extra []string) error {
	args := append([]string{"pr", "merge", pr, "--squash"}, extra...)
	cmd := exec.Command("gh", args...)
	cmd.Stdout = os.Stdout
	cmd.Stderr = os.Stderr
	cmd.Stdin = os.Stdin
	return cmd.Run()
}
