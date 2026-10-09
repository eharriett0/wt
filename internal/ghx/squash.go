package ghx

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"strings"
)

// PRCommit is one commit on a PR: its whole message and how many parents it
// has (#196).
type PRCommit struct {
	Message string
	Parents int
}

// prCommitsQuery pages through a PR's commits for their WHOLE message and their
// parent count. `gh pr view --json commits` has neither: its messageHeadline
// cuts a first line longer than 72 characters at 69 with "…" (the rest moves to
// messageBody, which splits "Fixes" from "#N"), and a squash's default message
// leaves merge commits out, which needs the parents.
const prCommitsQuery = `query($url:URI!,$endCursor:String){resource(url:$url){... on PullRequest{` +
	`commits(first:100,after:$endCursor){pageInfo{hasNextPage endCursor} nodes{commit{message parents{totalCount}}}}}}}`

// PRCommits returns every commit on PR pr, for the close check's model of the
// squash commit (#196). An error when gh fails or names no commit: a PR always
// has one, so none read means unread, never "no commits".
func PRCommits(pr string) ([]PRCommit, error) {
	if !Present() || !Authed() {
		return nil, errors.New("gh is not available or not authenticated")
	}
	url, err := run("pr", "view", pr, "--json", "url", "--jq", ".url")
	if err != nil || strings.TrimSpace(url) == "" {
		return nil, fmt.Errorf("cannot resolve PR #%s's URL: %v", pr, err)
	}
	out, err := run("api", "graphql", "--paginate",
		"-f", "query="+prCommitsQuery,
		"-f", "url="+strings.TrimSpace(url),
		"--jq", `.data.resource.commits.nodes[] | {message: .commit.message, parents: .commit.parents.totalCount}`)
	if err != nil {
		return nil, err
	}
	return parsePRCommits(out)
}

// parsePRCommits reads PRCommits' output: one JSON object per commit, carrying
// both "message" and "parents". Anything else is an error, never a parsed
// placeholder (the #168 rule): a `null` would decode as an empty commit. Pure.
func parsePRCommits(out string) ([]PRCommit, error) {
	dec := json.NewDecoder(strings.NewReader(out))
	var commits []PRCommit
	for {
		var c struct {
			Message *string `json:"message"`
			Parents *int    `json:"parents"`
		}
		err := dec.Decode(&c)
		if err == io.EOF {
			break
		}
		if err != nil {
			return nil, fmt.Errorf("unreadable commit list from gh: %w", err)
		}
		if c.Message == nil || c.Parents == nil {
			return nil, errors.New("a commit from gh has no message or parent count")
		}
		commits = append(commits, PRCommit{Message: *c.Message, Parents: *c.Parents})
	}
	if len(commits) == 0 {
		return nil, errors.New("gh listed no commits")
	}
	return commits, nil
}

// RepoSquashSettings returns this repository's default squash commit title and
// message settings (squash_merge_commit_title / _message), for the close check's
// model of the squash commit (#196); "" for one that cannot be read.
//
// ⚠ GraphQL, not `gh api repos/{owner}/{repo}`: REST returns both as null to a
// viewer who is not an admin (measured on a public repo as a READ viewer), while
// GraphQL's squashMergeCommitTitle / squashMergeCommitMessage answer any reader.
// {owner}/{repo} resolve the way the gh pr reads do.
func RepoSquashSettings() (title, message string) {
	if !Present() || !Authed() {
		return "", ""
	}
	out, err := run("api", "graphql",
		"-F", "owner={owner}", "-F", "name={repo}",
		"-f", "query=query($owner:String!,$name:String!){repository(owner:$owner,name:$name){squashMergeCommitTitle squashMergeCommitMessage}}",
		"--jq", `.data.repository | {title: .squashMergeCommitTitle, message: .squashMergeCommitMessage}`)
	if err != nil {
		return "", ""
	}
	return parseSquashSettings(out)
}

// parseSquashSettings reads RepoSquashSettings' `{"title":…,"message":…}`. A
// null, a missing field or output that is not that object reads as "": the
// caller treats it as unknown. Whether a value is one GitHub documents is
// merge.ShippedSquash's call, so it is passed through. Pure.
func parseSquashSettings(out string) (title, message string) {
	var s struct {
		Title   *string `json:"title"`
		Message *string `json:"message"`
	}
	if err := json.Unmarshal([]byte(strings.TrimSpace(out)), &s); err != nil {
		return "", ""
	}
	if s.Title != nil {
		title = *s.Title
	}
	if s.Message != nil {
		message = *s.Message
	}
	return title, message
}
