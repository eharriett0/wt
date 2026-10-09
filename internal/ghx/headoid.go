package ghx

import (
	"fmt"
	"strings"
)

// PRHeadOid returns the commit PR pr's head pointed at (headRefOid); for a
// merged PR, the head that merged. merge-pr's auto-clean deletes the local
// branch only when its tip is that commit or an ancestor of it (#187). An
// error when gh fails or does not return a full object id.
func PRHeadOid(pr string) (string, error) {
	out, err := run("pr", "view", pr, "--json", "headRefOid", "--jq", ".headRefOid // empty")
	if err != nil {
		return "", err
	}
	oid, ok := parseHeadOid(out)
	if !ok {
		return "", fmt.Errorf("gh returned no head commit for PR #%s (got %q)", pr, out)
	}
	return oid, nil
}

// parseHeadOid reads PRHeadOid's output: only a full hex object id (40 digits
// for SHA-1, 64 for SHA-256) counts, never a parsed placeholder such as "null"
// (the #168 rule). Pure.
func parseHeadOid(out string) (string, bool) {
	s := strings.ToLower(strings.TrimSpace(out))
	if len(s) != 40 && len(s) != 64 {
		return "", false
	}
	for _, r := range s {
		if !strings.ContainsRune("0123456789abcdef", r) {
			return "", false
		}
	}
	return s, true
}

// PRHead returns PR pr's head branch (headRefName) and head commit (headRefOid),
// in one gh call, so the two cannot come from different pushes. `wt adopt <pr#>`
// lands on that branch, and only once origin/<branch> carries that commit
// (#167). An error when gh fails or does not return both.
func PRHead(pr string) (branch, oid string, err error) {
	out, err := run("pr", "view", pr, "--json", "headRefName,headRefOid",
		"--jq", `"\(.headRefName // empty) \(.headRefOid // empty)"`)
	if err != nil {
		return "", "", err
	}
	branch, oid, ok := parsePRHead(out)
	if !ok {
		return "", "", fmt.Errorf("gh returned no head branch and commit for PR #%s (got %q)", pr, out)
	}
	return branch, oid, nil
}

// parsePRHead reads PRHead's `<headRefName> <headRefOid>` line: a branch name,
// then a full object id as parseHeadOid reads one. Anything else is ok=false,
// never a parsed placeholder (the #168 rule); the `// empty` query prints
// nothing when either field is missing. Pure.
func parsePRHead(out string) (branch, oid string, ok bool) {
	f := strings.Fields(out)
	if len(f) != 2 {
		return "", "", false
	}
	if oid, ok = parseHeadOid(f[1]); !ok {
		return "", "", false
	}
	return f[0], oid, true
}
