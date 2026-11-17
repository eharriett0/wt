package ghx

import (
	"encoding/json"
	"errors"
	"fmt"
	"net/url"
	"strings"

	"github.com/eharriett0/wt/internal/gitx"
)

// ConfFile is the repo-root config file wt reads.
const ConfFile = ".wt.conf"

// BaseConf is a PR's base branch and the .wt.conf on it, as GitHub has them
// now (#223).
type BaseConf struct {
	Base   string // the PR's base branch (baseRefName)
	Owner  string // the repository the PR merges into, from its URL
	Name   string
	Host   string // the PR URL's host
	Commit string // the base branch's tip when it was read
	Text   string // the file as the base branch has it; "" when !Found
	Found  bool   // the base branch has a .wt.conf
}

// Repo is the base repository as owner/name, "" when it was not resolved.
func (b BaseConf) Repo() string {
	if b.Owner == "" {
		return ""
	}
	return b.Owner + "/" + b.Name
}

// baseConfQuery reads a branch's tip, its root tree's entries (name and mode)
// and the .wt.conf at it. The mode tells a symlink apart: object(expression:)
// answers a symlink with a Blob whose text is the link's target (#223 review),
// which would parse as a config with no keys. Both name the branch as
// refs/heads/<base>: git resolves a bare name to a tag before a branch
// (gitrevisions), so a bare `<base>:.wt.conf` could read a tag of the same
// name (not measured on GitHub; the full ref rules it out). Measured: a
// missing file is a null object with no error (a missing branch nulls the ref
// too); a directory there is an object that is not a Blob (a Tree).
const baseConfQuery = `query($owner:String!,$name:String!,$ref:String!,$expr:String!){repository(owner:$owner,name:$name){` +
	`ref(qualifiedName:$ref){target{oid ... on Commit{tree{entries{name mode oid}}}}} ` +
	`object(expression:$expr){__typename ... on Blob{oid text isBinary isTruncated}}}}`

// ReadBaseConf reads PR pr's base branch and the .wt.conf on it (#223): pr in
// repo when repo is not "" (a forwarded -R/--repo), else as gh resolves it
// here (a URL, GH_REPO, gh's default repository). merge-pr's gates read it
// because the checkout merge-pr runs in is usually the PR's own worktree,
// whose .wt.conf is the branch being merged. It goes to the repository and
// host of the PR's URL (a PR URL is under the repository it merges into),
// never the cwd's {owner}/{repo}. An error when gh fails or its answer is not
// a branch tip with a regular file or a clear absence, never a parsed
// placeholder (#168): an unread base is not a base without one. ⚠ On an error
// the BaseConf still carries what was resolved before it (the base branch and
// repository, once gh named the PR), so the caller's fallback reads THAT
// branch of THAT repository, never a guess (#223 review).
func ReadBaseConf(pr, repo string) (BaseConf, error) {
	if !Present() || !SignedIn() {
		return BaseConf{}, notSignedIn(RepoHost())
	}
	args := []string{"pr", "view", pr, "--json", "number,url,baseRefName", "--jq", `"\(.number) \(.url) \(.baseRefName)"`}
	if repo != "" {
		args = append(args, "-R", repo)
	}
	out, err := run(args...)
	if err != nil {
		return BaseConf{}, fmt.Errorf("cannot resolve PR #%s's base branch: %w", pr, ghErrAt(RepoHost(), apiGraphQL, err))
	}
	ref, base, err := parsePRRefBase(out)
	if err != nil {
		return BaseConf{}, fmt.Errorf("cannot resolve PR #%s's base branch: %w", pr, err)
	}
	owner, name, err := prOwnerName(ref)
	if err != nil {
		return BaseConf{}, err
	}
	known := BaseConf{Base: base, Owner: owner, Name: name, Host: ref.host}
	out, err = run("api", "graphql", "--hostname", ref.host, "-f", "query="+baseConfQuery,
		"-f", "owner="+owner, "-f", "name="+name, "-f", "ref=refs/heads/"+base, "-f", "expr=refs/heads/"+base+":"+ConfFile,
		"--jq", `.data.repository | ([.ref.target.tree.entries[]? | select(.name == "`+ConfFile+`")] | first) as $e | {oid: .ref.target.oid, mode: $e.mode, eoid: $e.oid, conf: .object}`)
	if err != nil {
		return known, fmt.Errorf("cannot read %s's %s: %w", base, ConfFile, ghErrAt(ref.host, apiGraphQL, err))
	}
	read, err := parseBaseConf(out)
	if err != nil {
		return known, fmt.Errorf("cannot read %s's %s: %w", base, ConfFile, err)
	}
	read.Base, read.Owner, read.Name, read.Host = base, owner, name, ref.host
	return read, nil
}

// parsePRRefBase reads ReadBaseConf's `"<number> <url> <baseRefName>"` line:
// a PR reference as parsePRRef reads one, and a branch name. Pure.
func parsePRRefBase(out string) (prRef, string, error) {
	out = strings.TrimSpace(out)
	i := strings.LastIndexByte(out, ' ')
	if i < 0 {
		return prRef{}, "", fmt.Errorf("gh printed no PR number, URL and base branch (got %q)", out)
	}
	base := out[i+1:]
	if !validBranchName(base) {
		return prRef{}, "", fmt.Errorf("gh printed no base branch for the PR (got %q)", out)
	}
	ref, err := parsePRRef(out[:i])
	if err != nil {
		return prRef{}, "", err
	}
	return ref, base, nil
}

// validBranchName reports whether b can be a branch name for the query: not
// empty, not gh's placeholder for a missing field, and nothing git forbids in
// a ref name that would change what the query's expression means. Pure.
func validBranchName(b string) bool {
	if b == "" || b == "null" || strings.HasPrefix(b, "-") || strings.HasPrefix(b, "/") || strings.HasSuffix(b, "/") {
		return false
	}
	return !strings.ContainsAny(b, ": ~^?*[\\\t\n") && !strings.Contains(b, "..") && !strings.Contains(b, "@{")
}

// prOwnerName is the owner and name of the repository a PR URL belongs to
// (`https://<host>/<owner>/<name>/pull/<n>`). Pure.
func prOwnerName(ref prRef) (owner, name string, err error) {
	u, err := url.Parse(ref.url)
	if err != nil {
		return "", "", fmt.Errorf("unreadable PR URL %q: %w", ref.url, err)
	}
	seg := strings.Split(strings.Trim(u.Path, "/"), "/")
	if len(seg) != 4 || seg[2] != "pull" || !validRepo(seg[0]+"/"+seg[1]) {
		return "", "", fmt.Errorf("PR URL %q is not <owner>/<name>/pull/<n>", ref.url)
	}
	return seg[0], seg[1], nil
}

// Git tree entry modes, as GitHub's GraphQL reports them (decimal).
const (
	modeFile       = 0o100644
	modeExecutable = 0o100755
	modeSymlink    = 0o120000
)

// parseBaseConf reads ReadBaseConf's `{"oid":…,"mode":…,"conf":…}`: the branch
// tip, the mode of the root tree's .wt.conf entry, and the object at
// <base>:.wt.conf. No tip is an error (the branch, or the repository, could
// not be read); a tip with no entry and a null object is a base without the
// file; a symlink, a directory or a submodule there, an entry and an object
// that disagree, and anything but a whole text Blob are errors. Pure.
func parseBaseConf(out string) (BaseConf, error) {
	var v struct {
		Oid  *string `json:"oid"`
		Mode *int    `json:"mode"`
		EOid string  `json:"eoid"` // the tree entry's object id
		Conf *struct {
			Typename    string  `json:"__typename"`
			Oid         string  `json:"oid"`
			Text        *string `json:"text"`
			IsBinary    bool    `json:"isBinary"`
			IsTruncated bool    `json:"isTruncated"`
		} `json:"conf"`
	}
	if err := json.Unmarshal([]byte(strings.TrimSpace(out)), &v); err != nil {
		return BaseConf{}, fmt.Errorf("unreadable answer from gh: %w", err)
	}
	if v.Oid == nil || *v.Oid == "" {
		return BaseConf{}, errors.New("GitHub returned no tip for the base branch (the branch or the repository could not be read)")
	}
	read := BaseConf{Commit: *v.Oid}
	switch c := v.Conf; {
	case c == nil && v.Mode == nil:
		return read, nil
	case c == nil || v.Mode == nil:
		return BaseConf{}, fmt.Errorf("GitHub's tree and its %s object disagree on whether the file exists", ConfFile)
	case *v.Mode == modeSymlink:
		return BaseConf{}, fmt.Errorf("%s on the base branch is a symlink, which wt does not follow", ConfFile)
	case c.Typename != "Blob":
		return BaseConf{}, fmt.Errorf("%s on the base branch is a %s, not a file", ConfFile, strings.ToLower(c.Typename))
	case *v.Mode != modeFile && *v.Mode != modeExecutable:
		return BaseConf{}, fmt.Errorf("%s on the base branch has mode %o, not a regular file", ConfFile, *v.Mode)
	case v.EOid != "" && c.Oid != "" && v.EOid != c.Oid:
		// The tree entry and the object are resolved separately in one query: a
		// push in between could pair a regular file's mode with a symlink's blob
		// (#223 re-review).
		return BaseConf{}, fmt.Errorf("%s changed on the base branch while it was read", ConfFile)
	case c.IsBinary || c.IsTruncated || c.Text == nil:
		return BaseConf{}, fmt.Errorf("GitHub returned %s without its whole text (binary or truncated)", ConfFile)
	}
	read.Text, read.Found = *v.Conf.Text, true
	return read, nil
}

// OriginRepo is the host, owner and name origin's URL points at ("" each when
// there is no origin or it is a local path).
func OriginRepo() (host, owner, name string) { return parseRemoteRepo(gitx.RemoteURL("origin")) }

// parseRemoteRepo reads a git remote URL's host and <owner>/<name> path (a
// trailing .git and slash dropped): scp-style `git@host:owner/name.git`, and
// `https://host/owner/name` / `ssh://git@host:22/owner/name`. "" for a local
// path or a path that is not exactly owner/name. Pure.
func parseRemoteRepo(u string) (host, owner, name string) {
	host = hostFromRemoteURL(u)
	if host == "" {
		return "", "", ""
	}
	u = strings.TrimSpace(u)
	var path string
	if i := strings.Index(u, "://"); i >= 0 {
		rest := u[i+3:]
		slash := strings.Index(rest, "/")
		if slash < 0 {
			return "", "", ""
		}
		path = rest[slash+1:]
	} else {
		path = u[strings.Index(u, ":")+1:]
	}
	path = strings.TrimSuffix(strings.TrimSuffix(strings.Trim(path, "/"), ".git"), "/")
	seg := strings.Split(path, "/")
	if len(seg) != 2 || seg[0] == "" || seg[1] == "" {
		return "", "", ""
	}
	return host, seg[0], seg[1]
}
