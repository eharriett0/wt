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
	"sync"
	"time"

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

// authTTL bounds how long an AuthedFor answer is reused (#172). One command asks
// once per PR lookup (every worktree in `wt clean`, every colliding window in a
// hook), and the answer does not change mid-command. The bound is for `wt mcp`,
// which can outlive a `gh auth login`.
const authTTL = time.Minute

var (
	authMu   sync.Mutex
	authMemo = map[string]authAnswer{}
)

type authAnswer struct {
	ok bool
	at time.Time
}

// authFresh reports whether an answer taken at `at` may be reused at now: not in
// the future (a clock step) and younger than authTTL. A zero `at` (never asked)
// is centuries old, so it fails the second test. Pure.
func authFresh(at, now time.Time) bool {
	age := now.Sub(at)
	return age >= 0 && age < authTTL
}

// AuthedFor reports whether gh has an authenticated account FOR host. The answer
// is memoized per host for authTTL; the lock is held across the gh call so that
// concurrent first callers (ClassifyWindows runs 8) share one check (#172).
func AuthedFor(host string) bool {
	authMu.Lock()
	defer authMu.Unlock()
	now := time.Now()
	if a, ok := authMemo[host]; ok && authFresh(a.at, now) {
		return a.ok
	}
	ok := exec.Command("gh", authStatusArgs(host)...).Run() == nil
	authMemo[host] = authAnswer{ok: ok, at: now}
	return ok
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
//
// With no forge host (a local-path origin) the check stays bare, as #100 chose,
// and a bare check validates every configured host: about 6 s with two (#172).
// AuthedFor's memo is what keeps that to once per command.
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
		"--json", "number,state", "--jq", `.[0] // empty | "\(.number) \(.state)"`)
	if err != nil {
		return "", "", false
	}
	return parsePRForBranch(out)
}

// parsePRForBranch reads PRForBranch's `<number> <state>` line. Only a numeric
// number and a known state count as a PR. Pure.
//
// ⚠ Before #168 the query was `.[0] | …`, which prints "null null" for a branch
// with NO PR, and that parsed as ok=true with state "null". Every caller then
// matched no state, so "no PR" worked by accident, and the #168 tip fallback, the
// first caller to ask "was there a PR at all?", never ran. Both halves are fixed:
// the query prints nothing for no PR, and this refuses anything that is not a PR.
func parsePRForBranch(out string) (number, state string, ok bool) {
	f := strings.Fields(strings.TrimSpace(out))
	if len(f) != 2 {
		return "", "", false
	}
	if _, err := strconv.Atoi(f[0]); err != nil {
		return "", "", false
	}
	switch f[1] {
	case "OPEN", "MERGED", "CLOSED":
		return f[0], f[1], true
	}
	return "", "", false
}

// CommitPR is one pull request GitHub associates with a commit (#168).
type CommitPR struct {
	Number string
	Open   bool
	Merged bool
	// HasTip: the commit is in the PR's FINAL commit list. Read only for a merged
	// PR, where it is what makes "merged" mean "this work shipped".
	HasTip bool
}

// parseCommitPRs reads PRForTip's `<number> <state> <merged>` lines. A line that
// does not have that shape is skipped rather than guessed at. Pure.
func parseCommitPRs(out string) []CommitPR {
	var prs []CommitPR
	for _, ln := range strings.Split(out, "\n") {
		f := strings.Fields(ln)
		if len(f) != 3 {
			continue
		}
		if _, err := strconv.Atoi(f[0]); err != nil {
			continue
		}
		prs = append(prs, CommitPR{Number: f[0], Open: f[1] == "open", Merged: f[2] == "true"})
	}
	return prs
}

// ChooseTipPR picks the PR that speaks for a branch whose tip commit belongs to
// PRs under OTHER head names (#168). Pure.
//   - An OPEN PR wins: the work is live under another name. It is surfaced as
//     contention, never suppressed.
//   - Otherwise the highest-numbered MERGED PR whose final commits contain the
//     tip: that work shipped.
//   - A merged PR that no longer contains the tip (force-pushed away before the
//     merge) proves nothing, and a closed-unmerged one is not used either. Both
//     fall through to git signals, exactly as before #168.
func ChooseTipPR(prs []CommitPR) (number, state string, ok bool) {
	pick := func(want func(CommitPR) bool) string {
		best, bestN := -1, ""
		for _, p := range prs {
			if n, err := strconv.Atoi(p.Number); err == nil && want(p) && n > best {
				best, bestN = n, p.Number
			}
		}
		return bestN
	}
	if n := pick(func(p CommitPR) bool { return p.Open }); n != "" {
		return n, "OPEN", true
	}
	if n := pick(func(p CommitPR) bool { return p.Merged && p.HasTip }); n != "" {
		return n, "MERGED", true
	}
	return "", "", false
}

// TipLookupApplies decides whether to look a branch's PR up by its tip commit
// (#168). Only when no PR has the branch's own name as its head, the tip
// resolves, and the tip is known NOT to be on base. Pure.
//
// ⚠ The base condition is load-bearing. A branch with nothing ahead of base has
// a tip that IS a base commit, and on a repo that merges by merge commit or
// rebase every base commit belongs to some merged PR's final commits. Without
// this, a fresh `wt new` worktree would read as shipped, and `wt clean` would
// reap it: the #61 data-loss class. Unknown ancestry skips the lookup, which is
// the behaviour before #168.
func TipLookupApplies(prFound bool, tip string, onBase, ancestryKnown bool) bool {
	return !prFound && tip != "" && ancestryKnown && !onBase
}

// PRForTip resolves the PR that a commit belongs to (#168), for a branch whose
// commits were pushed under another name: `git push origin fix-x:bot/y` onto a
// PR an automation had opened. No PR has head `fix-x`, so PRForBranch finds
// nothing, and `git cherry` cannot see a squash. GitHub still lists the PR for
// the commit after the squash merge. Returns ("OPEN"|"MERGED") per ChooseTipPR.
// Best-effort: ("", "", false) on any gh error, including the 422 GitHub returns
// for a commit it has never seen, which is the normal case for unpushed work.
func PRForTip(sha string) (number, state string, ok bool) {
	if sha == "" || !Present() || !Authed() {
		return "", "", false
	}
	out, err := run("api", "repos/{owner}/{repo}/commits/"+sha+"/pulls",
		"--jq", `.[] | "\(.number) \(.state) \(.merged_at != null)"`)
	if err != nil {
		return "", "", false
	}
	prs := parseCommitPRs(out)
	for i := range prs {
		if prs[i].Merged {
			prs[i].HasTip = prContainsCommit(prs[i].Number, sha)
		}
	}
	return ChooseTipPR(prs)
}

// prContainsCommit reports whether sha is in PR pr's final commit list. false on
// any error, so an unreadable list never counts as proof of shipping.
func prContainsCommit(pr, sha string) bool {
	out, err := run("api", "--paginate", "repos/{owner}/{repo}/pulls/"+pr+"/commits",
		"--jq", ".[].sha")
	if err != nil {
		return false
	}
	for _, ln := range strings.Split(out, "\n") {
		if strings.TrimSpace(ln) == sha {
			return true
		}
	}
	return false
}

// PRForBranchOrTip is PRForBranch, falling back to the branch's tip commit when
// no PR carries the branch's own name (#168). baseRef is the ref the tip is
// tested against (gitx.ResolveRemoteBase). viaTip says the answer came from the
// fallback, so callers can say so. Used where liveness and shipped-ness are
// decided (collide.Classify, worktree.Clean); the fallback costs gh calls, and
// those paths already run only for the few worktrees that matter.
func PRForBranchOrTip(branch, baseRef string) (number, state string, viaTip, ok bool) {
	if n, s, found := PRForBranch(branch); found {
		return n, s, false, true
	}
	tip := gitx.BranchTip(branch)
	onBase, err := gitx.IsAncestor(tip, baseRef)
	if !TipLookupApplies(false, tip, onBase, err == nil) {
		return "", "", false, false
	}
	n, s, found := PRForTip(tip)
	return n, s, found, found
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

// IssuePRRef is an OPEN pull request that cross-references an issue, drawn from
// the issue's timeline. (#134)
type IssuePRRef struct {
	Number      int
	HeadRefName string
	IsDraft     bool
	URL         string
}

// OpenPRsReferencingIssue returns every OPEN pull request whose body
// cross-references issue #n — the SUPERSET of closingIssuesReferences. GitHub
// records a CROSS_REFERENCED_EVENT on the issue for ANY `#n` mention in a PR
// body regardless of keyword, so this catches a plain `Refs #n` (which is NEVER
// a linked/closing reference, so `gh pr view --json closingIssuesReferences`
// reports []) exactly as well as `Closes #n`. That gap is the #134 bug: claim
// had no existing-PR check at all, so a hand-opened `Refs #n` PR was invisible
// and a second `wt claim n` silently duplicated it. Best-effort: nil on error /
// gh unavailable, so the caller proceeds exactly as before. (#134)
func OpenPRsReferencingIssue(n string) []IssuePRRef {
	if !Present() || !Authed() {
		return nil
	}
	// resource(url:) sidesteps an owner/repo split (mirrors PRClosingIssueNumbers).
	url, err := run("issue", "view", n, "--json", "url", "--jq", ".url")
	if err != nil || strings.TrimSpace(url) == "" {
		return nil
	}
	// first:100 caps the timeline scan — for a presence check that's ample (an
	// open PR is rarely the 101st cross-reference on an issue). isCrossRepository
	// drops a same-numbered PR in ANOTHER repo that happens to mention owner/repo#n,
	// which would otherwise false-refuse the claim AND mislead `wt adopt n` onto an
	// unrelated branch. The `... on PullRequest{}` fragment yields {} for Issue
	// sources; `.state=="OPEN"` then drops them (and any CLOSED/MERGED PR).
	out, err := run("api", "graphql",
		"-f", "query=query($url:URI!){resource(url:$url){... on Issue{timelineItems(itemTypes:[CROSS_REFERENCED_EVENT],first:100){nodes{... on CrossReferencedEvent{isCrossRepository source{... on PullRequest{number headRefName isDraft url state}}}}}}}}",
		"-f", "url="+strings.TrimSpace(url),
		"--jq", `.data.resource.timelineItems.nodes[]? | select(.isCrossRepository == false) | .source // empty | select(.state=="OPEN") | [(.number|tostring),(.headRefName // ""),(.isDraft|tostring),(.url // "")] | @tsv`)
	if err != nil {
		return nil
	}
	return parseCrossRefPRs(out)
}

// parseCrossRefPRs parses OpenPRsReferencingIssue's tab-separated
// (number, headRefName, isDraft, url) lines, deduping by PR number (one PR can
// raise several cross-reference events on the same issue). Pure, for testing.
func parseCrossRefPRs(out string) []IssuePRRef {
	var refs []IssuePRRef
	seen := map[int]bool{}
	for _, ln := range strings.Split(out, "\n") {
		ln = strings.TrimRight(ln, "\r")
		if strings.TrimSpace(ln) == "" {
			continue
		}
		f := strings.Split(ln, "\t")
		if len(f) < 4 {
			continue
		}
		num, e := strconv.Atoi(strings.TrimSpace(f[0]))
		if e != nil || num <= 0 || seen[num] {
			continue
		}
		seen[num] = true
		refs = append(refs, IssuePRRef{Number: num, HeadRefName: f[1], IsDraft: f[2] == "true", URL: f[3]})
	}
	return refs
}

// PRURL returns the PR's html URL (best-effort, empty on error / gh
// unavailable). Used by `wt adopt` to record the adopted PR in active-work. (#134)
func PRURL(pr string) string {
	if !Present() || !Authed() {
		return ""
	}
	out, err := run("pr", "view", pr, "--json", "url", "--jq", ".url")
	if err != nil {
		return ""
	}
	return strings.TrimSpace(out)
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
