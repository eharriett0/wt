package ghx

import (
	"fmt"
	"net/url"
	"os"
	"strconv"
	"strings"
)

// PRAt is a pull request resolved in a named repository (#228): merge-pr with
// a forwarded `-R owner/repo`, or given a PR's URL, merges THAT repository's
// PR, so every read it makes about the PR goes through its URL, which gh reads
// as that PR whatever the cwd's repository (a URL wins over -R, measured with
// gh 2.87).
type PRAt struct {
	Number      int
	URL, Host   string
	Owner, Name string
	// HeadOwner and HeadName are the repository the PR's branch is in (a fork
	// for a contributor's PR, or this clone's origin when origin is the fork
	// and -R names upstream); both "" when GitHub no longer knows it (a
	// deleted fork).
	HeadOwner, HeadName string
}

// prAtJQ prints "<number> <url>" and, on the next line, the head repository
// as "<owner>/<name>" ("/" when GitHub has none).
const prAtJQ = `"\(.number) \(.url)\n\(.headRepositoryOwner.login // "")/\(.headRepository.name // "")"`

// ResolvePRIn resolves pr in repo ("[HOST/]owner/name", as gh's -R takes it),
// or, with repo "", pr as gh reads it on its own (a URL names its repository).
// An error when gh can't: the caller refuses, rather than read the PR of that
// number in the cwd's repository.
func ResolvePRIn(pr, repo string) (PRAt, error) {
	args := []string{"pr", "view", pr, "--json", "number,url,headRepository,headRepositoryOwner", "--jq", prAtJQ}
	where := pr
	if repo != "" {
		args = append(args, "-R", repo)
		where = "#" + strings.TrimPrefix(pr, "#") + " in " + repo
	}
	out, err := run(args...)
	if err != nil {
		return PRAt{}, fmt.Errorf("cannot resolve PR %s: %w", where, ghErrAt(selectorHost(pr, repo), apiGraphQL, err))
	}
	return parsePRAt(out)
}

// selectorHost is the host gh sends a PR selector to: a URL's own host, else
// the HOST of a "HOST/owner/name" repo, else gh's default (GH_HOST, else
// github.com), which is what an "owner/name" -R means to gh. Only names the
// host in an error (and its rate-limit probe). Pure but for GH_HOST.
func selectorHost(pr, repo string) string {
	if IsPRURL(pr) {
		u, _ := url.Parse(pr)
		return u.Host
	}
	if parts := strings.Split(repo, "/"); len(parts) == 3 {
		return parts[0]
	}
	if repo == "" {
		return RepoHost()
	}
	if h := os.Getenv("GH_HOST"); h != "" {
		return h
	}
	return "github.com"
}

// IsPRURL reports whether s is a pull request's URL (http(s), a host, and a
// path ending /pull/<number>, a tail like /files allowed), which gh reads as
// that PR whatever the cwd's repository. Pure.
func IsPRURL(s string) bool {
	u, err := url.Parse(strings.TrimSpace(s))
	if err != nil || (u.Scheme != "https" && u.Scheme != "http") || u.Host == "" {
		return false
	}
	parts := strings.Split(strings.Trim(u.Path, "/"), "/")
	for i := 2; i+1 < len(parts); i++ {
		if parts[i] == "pull" {
			n, err := strconv.Atoi(parts[i+1])
			return err == nil && n > 0
		}
	}
	return false
}

// parsePRAt reads ResolvePRIn's output: "<number> <url>" (parsePRRef, then
// the owner and name from the URL's path, /<owner>/<name>/pull/<number>),
// then the head repository's "<owner>/<name>" on the next line, "/" or no
// line for none. Pure.
func parsePRAt(out string) (PRAt, error) {
	first, head, _ := strings.Cut(strings.TrimSpace(out), "\n")
	ref, err := parsePRRef(first)
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
	at := PRAt{Number: ref.number, URL: ref.url, Host: ref.host, Owner: parts[len(parts)-4], Name: parts[len(parts)-3]}
	if o, n, ok := strings.Cut(strings.TrimSpace(head), "/"); ok && o != "" && n != "" && !strings.Contains(n, "/") {
		at.HeadOwner, at.HeadName = o, n
	}
	return at, nil
}

// IssueURL is issue n's URL in p's repository: gh reads it as that issue, as
// it reads p.URL as the PR. Pure.
func (p PRAt) IssueURL(n string) string {
	return strings.TrimSuffix(p.URL, "/pull/"+strconv.Itoa(p.Number)) + "/issues/" + n
}

// Repo is p's repository as gh's -R takes it. Pure.
func (p PRAt) Repo() string { return p.Owner + "/" + p.Name }

// withURLHost appends `--hostname <host>` for rawURL's host to a `gh api`
// call: bare, gh asks its default host, which answers a GitHub Enterprise
// PR's URL with a null resource when gh is logged in to both (#228 review;
// PRChecks and ReadBaseConf already pass it). A URL with no host adds
// nothing. Pure.
func withURLHost(args []string, rawURL string) []string {
	if u, err := url.Parse(strings.TrimSpace(rawURL)); err == nil && u.Host != "" {
		return append(args, "--hostname", u.Host)
	}
	return args
}

// HeadRepo is the repository p's branch is in, "" when unknown. Pure.
func (p PRAt) HeadRepo() string {
	if p.HeadOwner == "" {
		return ""
	}
	return p.HeadOwner + "/" + p.HeadName
}
