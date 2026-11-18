package merge

import (
	"errors"
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
	remotes := prRemotes(pr)
	var ancestor bool
	var ancestorErr error
	if tip != "" && head != "" && !strings.EqualFold(tip, head) {
		ancestor, ancestorErr = gitx.IsAncestor(tip, head)
		if ancestorErr != nil {
			// #230 review: the head can be a commit this clone never had (the
			// branch updated on GitHub, then deleted with the merge); no plain
			// fetch brings it, so the lane was kept on every run.
			fetchHead(remotes, PRNumber(pr), func() bool {
				ancestor, ancestorErr = gitx.IsAncestor(tip, head)
				return ancestorErr == nil
			})
		}
	}
	remote := remotes[0]
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

// fetchHead brings PR n's head into this clone (#243 review): it fetches
// refs/pull/<n>/head from each of remotes in turn, then origin, until here()
// says the head arrived. A fetch that works but brings another PR (a mirror's
// own PR n; origin's, in a fork clone) goes on to the next, and so does one
// that fails. origin last, because it can be the PR's own repository under a
// name prRemotes can't match: a renamed or transferred one, which GitHub's
// URL names by its new name. A fetch that ran out of time ends the walk (each
// costs fetchTimeout; the lane is only kept). Where the commits come from
// can't change the check's answer: it compares with the head gh named.
func fetchHead(remotes []string, n string, here func() bool) {
	tried := map[string]bool{}
	for _, r := range append(remotes, "origin") {
		if tried[r] {
			continue
		}
		tried[r] = true
		err := fetchPRHead(r, n)
		var late *gitx.FetchTimeoutError
		if errors.As(err, &late) {
			return
		}
		if err == nil && here() {
			return
		}
	}
}

// prRemotes are where pr's refs/pull/<n>/head can be fetched from, best
// first: origin, unless pr is the URL of a PR in another repository than
// origin's (origin a fork, -R naming upstream, #228 review), where that
// number on origin is another PR or none. Then the configured remotes whose
// URL names that repository (`upstream`), so the fetch and its hint use the
// user's own transport and credentials (#243 review: an ssh-only user has no
// https ones): those on the URL's host first, since `git remote` lists
// alphabetically and a mirror elsewhere can carry the same owner/name (round
// 2), then the others (an ssh alias names the host differently); then the
// repository's URL, which git fetches from directly. An origin wt can't read
// as owner/name keeps "origin".
var prRemotes = func(pr string) []string {
	if !ghx.IsPRURL(pr) {
		return []string{"origin"}
	}
	u, err := url.Parse(strings.TrimSpace(pr))
	if err != nil {
		return []string{"origin"}
	}
	parts := strings.Split(strings.TrimPrefix(u.Path, "/"), "/")
	names := func(owner, name string) bool {
		return strings.EqualFold(parts[0], owner) && strings.EqualFold(parts[1], name)
	}
	_, owner, name := originRepo()
	if owner == "" || names(owner, name) {
		return []string{"origin"}
	}
	var onHost, elsewhere []string
	for _, r := range remoteNames() {
		host, owner, name := remoteRepo(r)
		switch {
		case strings.HasPrefix(r, "-"), owner == "" || !names(owner, name):
			// A name git would read as an option is never a candidate (#243
			// review; fetchNoPrompt also passes --end-of-options).
		case strings.EqualFold(host, u.Hostname()):
			onHost = append(onHost, r)
		default:
			elsewhere = append(elsewhere, r)
		}
	}
	return append(append(onHost, elsewhere...), u.Scheme+"://"+u.Host+"/"+parts[0]+"/"+parts[1])
}

// prRemote is the first of prRemotes: where the kept lane's hint fetches from.
func prRemote(pr string) string { return prRemotes(pr)[0] }

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
