// Package ghx wraps the GitHub CLI (gh) via os/exec. Same shell-out approach
// as the original bash; no go-github dependency.
package ghx

import (
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"strconv"
	"strings"

	"github.com/eharriett0/wt/internal/gitx"
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

// hostFromRemoteURL extracts the forge host from a git remote URL. Handles the
// three forms git emits: scp-style SSH (`git@host:owner/repo.git`), a real URL
// (`https://host/owner/repo`, `ssh://git@host:22/owner/repo`), and a bare local
// path (no host — returns ""). Pure, so the parsing is testable without a repo.
func hostFromRemoteURL(u string) string {
	u = strings.TrimSpace(u)
	if u == "" {
		return ""
	}
	// scp-style: [user@]host:path — no "//" and the colon precedes any slash.
	if !strings.Contains(u, "://") {
		colon := strings.Index(u, ":")
		slash := strings.Index(u, "/")
		if colon <= 0 || (slash >= 0 && slash < colon) {
			return "" // local path like /srv/repo.git or ../repo
		}
		hostPart := u[:colon]
		if at := strings.LastIndex(hostPart, "@"); at >= 0 {
			hostPart = hostPart[at+1:]
		}
		return hostPart
	}
	rest := u[strings.Index(u, "://")+3:]
	if slash := strings.Index(rest, "/"); slash >= 0 {
		rest = rest[:slash]
	}
	if at := strings.LastIndex(rest, "@"); at >= 0 {
		rest = rest[at+1:] // strip user[:password]@
	}
	if colon := strings.LastIndex(rest, ":"); colon >= 0 {
		rest = rest[:colon] // strip :port
	}
	return rest
}

// RepoHost returns the forge host this repo's origin points at, or "" when it
// can't be determined (no origin, or a local-path remote) — in which case the
// caller should fall back to gh's own default rather than guessing.
func RepoHost() string { return hostFromRemoteURL(gitx.RemoteURL("origin")) }

// authStatusArgs builds the `gh auth status` argv, scoped to host when known.
// Split out so the scoping is unit-testable without invoking gh. (#100)
func authStatusArgs(host string) []string {
	args := []string{"auth", "status"}
	if host != "" {
		args = append(args, "--hostname", host)
	}
	return args
}

// AuthedFor reports whether gh has an authenticated account FOR host.
func AuthedFor(host string) bool {
	return exec.Command("gh", authStatusArgs(host)...).Run() == nil
}

// Authed reports whether gh is authenticated for the host THIS repo uses.
//
// It is deliberately host-scoped (#100): bare `gh auth status` exits non-zero if
// ANY configured host fails, so one unreachable extra host — an enterprise
// instance behind a VPN, a stale entry — made doctor report "NOT authenticated"
// on a machine whose github.com login was perfectly fine. That is the #91 shape:
// a warning that can't distinguish "you are broken" from "the check is broken",
// and whose obvious remedy (`gh auth login`) re-authenticates the wrong host.
//
// Scoping also fixes the mirror-image false NEGATIVE: a repo hosted on an
// enterprise instance used to pass because github.com happened to be authed,
// while the host it actually needs was not.
func Authed() bool { return AuthedFor(RepoHost()) }

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

// IssueComment posts body as a comment on issue n (the cross-machine mirror for
// wt's coordination channel — announce/ack/all-clear become issue comments).
func IssueComment(n, body string) error {
	_, err := run("issue", "comment", n, "--body", body)
	return err
}

// IssueComments returns the bodies of every comment on issue n, oldest first —
// the read-back half of the coordination mirror (#36). Bodies can be multi-line,
// so it decodes the JSON payload rather than line-splitting. Empty (nil, nil) on
// no comments; error only when gh itself fails.
func IssueComments(n string) ([]string, error) {
	out, err := run("issue", "view", n, "--json", "comments")
	if err != nil {
		return nil, err
	}
	var payload struct {
		Comments []struct {
			Body string `json:"body"`
		} `json:"comments"`
	}
	if err := json.Unmarshal([]byte(out), &payload); err != nil {
		return nil, err
	}
	bodies := make([]string, 0, len(payload.Comments))
	for _, c := range payload.Comments {
		bodies = append(bodies, c.Body)
	}
	return bodies, nil
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

// PRChangedFiles returns the repo-relative paths changed in the PR diff vs base.
// An error is returned (not swallowed to empty) so callers can fail CLOSED — a
// deploy gate must not skip just because gh couldn't list the files.
func PRChangedFiles(pr string) ([]string, error) {
	out, err := run("pr", "diff", pr, "--name-only")
	if err != nil {
		return nil, err
	}
	var files []string
	for _, ln := range strings.Split(out, "\n") {
		if s := strings.TrimSpace(ln); s != "" {
			files = append(files, s)
		}
	}
	return files, nil
}

// PRCommitSubjects returns one messageHeadline per commit on the PR.
func PRCommitSubjects(pr string) []string {
	out, err := run("pr", "view", pr, "--json", "commits", "--jq", ".commits[].messageHeadline")
	if err != nil || out == "" {
		return nil
	}
	return strings.Split(out, "\n")
}

// OpenPRForBranch returns the number of an OPEN PR whose head is branch, and
// whether one exists. Used by collision liveness: a branch with an open PR is
// active contention; one without is a candidate for "stale" classification.
// Delegates to PRForBranch so EVERY PR-state read in wt (open / merged / closed,
// across clean / check / status / claim) resolves from the ONE `gh pr list
// --state all` query — they can never disagree about the same branch (#88). A
// head has at most one open PR and PRForBranch returns the most-recent, so a
// branch whose current PR is open is exactly OpenPRForBranch's true case.
// Best-effort: ("", false) on any gh error so callers degrade to git signals.
func OpenPRForBranch(branch string) (string, bool) {
	if n, state, ok := PRForBranch(branch); ok && state == "OPEN" {
		return n, true
	}
	return "", false
}

// PRForBranch resolves the branch's MOST-RECENT pull request in a single call —
// `gh pr list --head <branch> --state all` returns every PR head=branch, newest
// first, so `.[0]` is the one that decides liveness. Returns (number, state, ok)
// where state is "OPEN" | "MERGED" | "CLOSED"; ok=false when there is no PR or
// gh is unavailable. This unifies the open- + merged-lookups (#73) and adds the
// closed-unmerged case (#79) that a merge-only check misses — a closed-PR branch
// keeps its remote ref, so it can't be detected from git alone. Best-effort:
// ("", "", false) on any gh error so callers degrade to git signals.
func PRForBranch(branch string) (number, state string, ok bool) {
	if branch == "" || branch == "HEAD" || !Present() || !Authed() {
		return "", "", false
	}
	out, err := run("pr", "list", "--head", branch, "--state", "all",
		"--json", "number,state", "--jq", `.[0] | "\(.number) \(.state)"`)
	if err != nil {
		return "", "", false
	}
	fields := strings.Fields(strings.TrimSpace(out))
	if len(fields) != 2 {
		return "", "", false
	}
	return fields[0], fields[1], true
}

// PRHeadBranch returns the PR's head branch name (headRefName), for locating
// the worktree to clean up after a merge.
func PRHeadBranch(pr string) (string, error) {
	return run("pr", "view", pr, "--json", "headRefName", "--jq", ".headRefName")
}

// PRTitle returns the PR's title (for the #38 WIP-subject strip).
func PRTitle(pr string) (string, error) {
	return run("pr", "view", pr, "--json", "title", "--jq", ".title")
}

// PRBody returns the PR description body (for the #77 closing-keyword scan).
func PRBody(pr string) (string, error) {
	return run("pr", "view", pr, "--json", "body", "--jq", ".body")
}

// PRCommitText returns all commits' FULL messages (headline + body) joined into
// one blob. The squash body gh composes is built from these, so the closing-
// keyword scan must see them — a `Fixes #N` in a commit body fires on merge even
// when the PR body (and thus closingIssuesReferences) never mentions it (#77
// trap 2). Best-effort: "" on error / gh unavailable.
func PRCommitText(pr string) string {
	out, err := run("pr", "view", pr, "--json", "commits", "--jq",
		`[.commits[] | .messageHeadline + "\n" + .messageBody] | join("\n\n")`)
	if err != nil {
		return ""
	}
	return out
}

// PRClosingIssueNumbers returns the numbers in the PR's GraphQL
// closingIssuesReferences — what GitHub itself reports the PR will close (from
// the PR title/body ONLY; blind to the squash commit body). This is GraphQL-only
// (NOT a `gh pr view --json` field), queried via resource(url:) so no owner/repo
// split is needed. Best-effort: nil on error / gh unavailable. (#77)
func PRClosingIssueNumbers(pr string) []int {
	if !Present() || !Authed() {
		return nil
	}
	url, err := run("pr", "view", pr, "--json", "url", "--jq", ".url")
	if err != nil || strings.TrimSpace(url) == "" {
		return nil
	}
	out, err := run("api", "graphql",
		"-f", "query=query($url:URI!){resource(url:$url){... on PullRequest{closingIssuesReferences(first:50){nodes{number}}}}}",
		"-f", "url="+strings.TrimSpace(url),
		"--jq", ".data.resource.closingIssuesReferences.nodes[].number")
	if err != nil || strings.TrimSpace(out) == "" {
		return nil
	}
	var nums []int
	for _, ln := range strings.Split(out, "\n") {
		if n, e := strconv.Atoi(strings.TrimSpace(ln)); e == nil {
			nums = append(nums, n)
		}
	}
	return nums
}

// PRState returns the PR's state (OPEN / MERGED / CLOSED) for the merge-pr
// precheck (#39). Empty on error / gh unavailable, so the caller fails open.
func PRState(pr string) string {
	if !Present() || !Authed() {
		return ""
	}
	out, err := run("pr", "view", pr, "--json", "state", "--jq", ".state")
	if err != nil {
		return ""
	}
	return strings.TrimSpace(out)
}

// MergedPRForBranchNum returns the number of a MERGED PR whose head is branch,
// and whether one exists. `gh pr list --head` still resolves a merged PR after
// its branch is deleted (the PR record keeps headRefName), so this detects a
// squash-merged-then-deleted branch — the case git cherry can't (squash breaks
// patch-equivalence to base). Delegates to PRForBranch (#88) so the merged check
// shares the single PR-state query with open/closed. Note: this is true only
// when the branch's MOST-RECENT PR is merged — if the branch was reopened with a
// newer OPEN/CLOSED PR after an earlier merge, it is (correctly) treated as
// active/abandoned by that newer PR, not as shipped. Best-effort: ("", false).
func MergedPRForBranchNum(branch string) (string, bool) {
	if n, state, ok := PRForBranch(branch); ok && state == "MERGED" {
		return n, true
	}
	return "", false
}

// MergedPRForBranch reports whether branch has a MERGED PR (bool wrapper). Used
// by `wt clean` to treat a squash-merged wt branch as shipped even though
// `git cherry` never reads 0 for it.
func MergedPRForBranch(branch string) bool {
	_, ok := MergedPRForBranchNum(branch)
	return ok
}

// PRStateByURL returns a short live state ("OPEN", "MERGED", "DRAFT", "CLOSED")
// for a PR identified by URL — gh resolves the repo from the URL, so this works
// cross-repo. Empty string on error / gh unavailable.
func PRStateByURL(url string) string {
	if url == "" || !Present() || !Authed() {
		return ""
	}
	out, err := run("pr", "view", url, "--json", "state,isDraft", "--jq",
		`if .isDraft then "DRAFT" else .state end`)
	if err != nil {
		return ""
	}
	return strings.TrimSpace(out)
}

// PRIsDraft reports whether PR pr is a draft.
func PRIsDraft(pr string) (bool, error) {
	out, err := run("pr", "view", pr, "--json", "isDraft", "--jq", ".isDraft")
	if err != nil {
		return false, err
	}
	return strings.TrimSpace(out) == "true", nil
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
