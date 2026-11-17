package ghx

import (
	"encoding/json"
	"errors"
	"fmt"
	"net/url"
	"strings"
)

// ConfFile is the repo-root config file wt reads.
const ConfFile = ".wt.conf"

// BaseConf is a PR's base branch and the .wt.conf on it, as GitHub has them
// now (#223).
type BaseConf struct {
	Base   string // the PR's base branch (baseRefName)
	Repo   string // owner/name of the repository the PR merges into, from its URL
	Commit string // the base branch's tip when it was read
	Text   string // the file as the base branch has it; "" when !Found
	Found  bool   // the base branch has a .wt.conf
}

// baseConfQuery reads a branch's tip and the .wt.conf at it. Both name the
// branch as refs/heads/<base>: in a bare `<base>:.wt.conf` a tag of the same
// name would win, as it does for git rev-parse. A missing file is a null
// object with no error (a missing branch nulls the ref too); a directory or a
// submodule there is an object that is not a Blob.
const baseConfQuery = `query($owner:String!,$name:String!,$ref:String!,$expr:String!){repository(owner:$owner,name:$name){` +
	`ref(qualifiedName:$ref){target{oid}} object(expression:$expr){__typename ... on Blob{text isBinary isTruncated}}}}`

// ReadBaseConf reads PR pr's base branch and the .wt.conf on it (#223): pr in
// repo when repo is not "" (a forwarded -R/--repo), else as gh resolves it
// here. merge-pr's gates read it because the checkout merge-pr runs in is
// usually the PR's own worktree, whose .wt.conf is the branch being merged.
// It goes to the repository and host of the PR's URL (a PR URL is under the
// repository it merges into), never the cwd's {owner}/{repo}. An error when gh
// fails or its answer is not a branch tip with a file or a clear absence,
// never a parsed placeholder (#168): an unread base is not a base without one.
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
	out, err = run("api", "graphql", "--hostname", ref.host, "-f", "query="+baseConfQuery,
		"-f", "owner="+owner, "-f", "name="+name, "-f", "ref=refs/heads/"+base, "-f", "expr=refs/heads/"+base+":"+ConfFile,
		"--jq", `.data.repository | {oid: .ref.target.oid, conf: .object}`)
	if err != nil {
		return BaseConf{}, fmt.Errorf("cannot read %s's %s: %w", base, ConfFile, ghErrAt(ref.host, apiGraphQL, err))
	}
	read, err := parseBaseConf(out)
	if err != nil {
		return BaseConf{}, fmt.Errorf("cannot read %s's %s: %w", base, ConfFile, err)
	}
	read.Base, read.Repo = base, owner+"/"+name
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

// parseBaseConf reads ReadBaseConf's `{"oid":…,"conf":…}`: the branch tip, and
// the object at <base>:.wt.conf. No tip is an error (the branch, or the
// repository, could not be read); a tip with a null object is a base without
// the file; anything but a whole text Blob is an error. Pure.
func parseBaseConf(out string) (BaseConf, error) {
	var v struct {
		Oid  *string `json:"oid"`
		Conf *struct {
			Typename    string  `json:"__typename"`
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
	case c == nil:
		return read, nil
	case c.Typename != "Blob":
		return BaseConf{}, fmt.Errorf("%s on the base branch is a %s, not a file", ConfFile, strings.ToLower(c.Typename))
	case c.IsBinary || c.IsTruncated || c.Text == nil:
		return BaseConf{}, fmt.Errorf("GitHub returned %s without its whole text (binary or truncated)", ConfFile)
	}
	read.Text, read.Found = *v.Conf.Text, true
	return read, nil
}
