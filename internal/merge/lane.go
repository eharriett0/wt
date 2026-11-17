package merge

import (
	"net/url"
	"regexp"
	"strconv"
	"strings"

	"github.com/eharriett0/wt/internal/ghx"
	"github.com/eharriett0/wt/internal/gitx"
)

// fetchPRHead is gitx.FetchPRHeadNoPrompt; a var so a test can count it.
var fetchPRHead = gitx.FetchPRHeadNoPrompt

// originRepo is ghx.OriginRepo, remoteNames gitx.Remotes and remoteRepo
// ghx.RemoteRepo; vars so a test can name the remotes.
var (
	originRepo  = ghx.OriginRepo
	remoteNames = gitx.Remotes
	remoteRepo  = ghx.RemoteRepo
)

var prURLNumber = regexp.MustCompile(`/pull/(\d+)(?:[/?#]|$)`)

// PRNumber is pr's number, as gh reads a selector: a number (`5`, `#5`), or
// the number in a PR URL, `/files` and `?…` tails included; "" for anything
// else (a branch name) (#230 re-review: a URL got no fetch, and a broken fetch
// command). Normalized: `05` is 5. Pure.
func PRNumber(pr string) string {
	s := strings.TrimPrefix(pr, "#")
	if m := prURLNumber.FindStringSubmatch(s); m != nil {
		s = m[1]
	}
	if n, err := strconv.Atoi(s); err == nil && n > 0 && !strings.HasPrefix(s, "+") {
		return strconv.Itoa(n)
	}
	return ""
}

// PRLabel is how a message names pr: "#5" for a number, the selector as given
// otherwise (a URL, a branch), never "#https://…" (#228 review: a forwarded -R
// hands merge-pr's reads the PR's URL). Pure.
func PRLabel(pr string) string {
	s := strings.TrimPrefix(pr, "#")
	if n, err := strconv.Atoi(s); err == nil && n > 0 && !strings.HasPrefix(s, "+") {
		return "#" + s
	}
	return pr
}

// PRArg is pr as a command hint should spell it: "#5" loses its "#", which a
// shell reads as the start of a comment (#228 review: the pasted `gh pr ready
// #5 -R o/r` ran a bare `gh pr ready`, marking the current branch's PR ready).
// Only a "#" before a number goes: gh reads "#hotfix" as that branch (#228
// round-3 review). Pure.
func PRArg(pr string) string {
	if s := strings.TrimPrefix(pr, "#"); s != pr && isPRNumber(s) {
		return s
	}
	return pr
}

// isPRNumber reports whether s is a PR number as gh reads one: digits, above
// zero. Pure.
func isPRNumber(s string) bool {
	n, err := strconv.Atoi(s)
	return err == nil && n > 0 && !strings.HasPrefix(s, "+") && !strings.HasPrefix(s, "-")
}

// UnshippedLane returns "" when tip, the local branch's tip as the caller read
// it, shipped in merged PR pr (LocalTipVerdict), and otherwise the warning
// that keeps the lane (#187). merge-pr's auto-clean and `wt clean` both ask it
// (#230 review: clean reaped a merged PR's lane without it, and deleted the
// commits made there after the merge), and both then delete the branch only
// at that same tip.
func UnshippedLane(pr, branch, tip string) string {
	head, _ := ghx.PRHeadOid(pr)
	remote := prRemote(pr)
	var ancestor bool
	var ancestorErr error
	if tip != "" && head != "" && !strings.EqualFold(tip, head) {
		ancestor, ancestorErr = gitx.IsAncestor(tip, head)
		if ancestorErr != nil && fetchHeadFrom(remote, PRNumber(pr)) {
			// #230 review: the head can be a commit this clone never had (the
			// branch updated on GitHub, then deleted with the merge); no plain
			// fetch brings it, so the lane was kept on every run. Once.
			ancestor, ancestorErr = gitx.IsAncestor(tip, head)
		}
	}
	v := LocalTipVerdict(tip, head, ancestor, ancestorErr)
	if v == TipShipped {
		return ""
	}
	n := -1
	if v == TipUnshipped {
		n = gitx.BehindCount(head, tip) // rev-list --count head..tip: the commits the PR lacks
	}
	return KeptLaneMessage(v, branch, pr, remote, n, tip, head)
}

// fetchHeadFrom fetches PR n's head from remote and, when that fails and
// remote is not origin, from origin (#243 review), reporting whether either
// fetch worked. origin can be the PR's own repository under a name prRemote
// can't match: a renamed or transferred repository, which GitHub's URL names
// by its new name. In a fork clone origin's PR n is another PR, whose
// commits change nothing: the check compares with the head gh named.
func fetchHeadFrom(remote, n string) bool {
	if fetchPRHead(remote, n) == nil {
		return true
	}
	return remote != "origin" && fetchPRHead("origin", n) == nil
}

// prRemote is where pr's refs/pull/<n>/head can be fetched from: origin,
// unless pr is the URL of a PR in another repository than origin's (origin a
// fork, -R naming upstream, #228 review), where that number on origin is
// another PR or none. Then a configured remote whose URL names that
// repository (`upstream`), so the fetch and its hint use the user's own
// transport and credentials (#243 review: an ssh-only user has no https
// ones), else the repository's URL, which git fetches from directly. The
// host is not compared (an ssh alias names it differently). An origin wt
// can't read as owner/name keeps "origin".
var prRemote = func(pr string) string {
	if !ghx.IsPRURL(pr) {
		return "origin"
	}
	u, err := url.Parse(strings.TrimSpace(pr))
	if err != nil {
		return "origin"
	}
	parts := strings.Split(strings.TrimPrefix(u.Path, "/"), "/")
	names := func(owner, name string) bool {
		return strings.EqualFold(parts[0], owner) && strings.EqualFold(parts[1], name)
	}
	_, owner, name := originRepo()
	if owner == "" || names(owner, name) {
		return "origin"
	}
	for _, r := range remoteNames() {
		if _, owner, name := remoteRepo(r); owner != "" && names(owner, name) {
			return r
		}
	}
	return u.Scheme + "://" + u.Host + "/" + parts[0] + "/" + parts[1]
}

// LaneSelector is how `wt clean` names PR n to UnshippedLane, given the URL gh
// gave for it (#243): the URL when it is another repository's PR than
// origin's, so the check fetches from there and its message says which; else
// the number, so the message says "#5" as every other clean line does.
func LaneSelector(n, url string) string {
	if url != "" && prRemote(url) != "origin" {
		return url
	}
	return n
}
