package ghx

import (
	"fmt"
	"net/url"
	"strconv"
	"strings"
)

// PRAt is a pull request resolved in a named repository (#228): merge-pr with
// a forwarded `-R owner/repo` merges THAT repository's PR, so every read it
// makes about the PR goes through its URL, which gh reads as that PR whatever
// the cwd's repository (a URL wins over -R, measured with gh 2.87).
type PRAt struct {
	Number      int
	URL         string
	Owner, Name string
}

// ResolvePRIn resolves pr in repo ("owner/name", as gh's -R takes it). An
// error when gh can't: the caller refuses, rather than read the PR of that
// number in the cwd's repository.
func ResolvePRIn(pr, repo string) (PRAt, error) {
	out, err := run("pr", "view", pr, "--json", "number,url", "--jq", `"\(.number) \(.url)"`, "-R", repo)
	if err != nil {
		return PRAt{}, fmt.Errorf("cannot resolve PR #%s in %s: %w", pr, repo, ghErrAt(RepoHost(), apiGraphQL, err))
	}
	return parsePRAt(out)
}

// parsePRAt reads ResolvePRIn's "<number> <url>": parsePRRef, then the owner
// and name from the URL's path (/<owner>/<name>/pull/<number>). Pure.
func parsePRAt(out string) (PRAt, error) {
	ref, err := parsePRRef(out)
	if err != nil {
		return PRAt{}, err
	}
	u, err := url.Parse(ref.url)
	if err != nil {
		return PRAt{}, err
	}
	parts := strings.Split(strings.Trim(u.Path, "/"), "/")
	if len(parts) < 4 || parts[len(parts)-2] != "pull" || parts[len(parts)-1] != strconv.Itoa(ref.number) {
		return PRAt{}, fmt.Errorf("gh printed no owner and name in PR #%d's URL (got %q)", ref.number, ref.url)
	}
	return PRAt{Number: ref.number, URL: ref.url, Owner: parts[len(parts)-4], Name: parts[len(parts)-3]}, nil
}

// IssueURL is issue n's URL in p's repository: gh reads it as that issue, as
// it reads p.URL as the PR. Pure.
func (p PRAt) IssueURL(n string) string {
	return strings.TrimSuffix(p.URL, "/pull/"+strconv.Itoa(p.Number)) + "/issues/" + n
}

// Repo is p's repository as gh's -R takes it. Pure.
func (p PRAt) Repo() string { return p.Owner + "/" + p.Name }
