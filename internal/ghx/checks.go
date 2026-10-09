package ghx

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/url"
	"os/exec"
	"sort"
	"strings"
)

// prChecksQuery reads the checks on a PR's head commit for merge-pr's checks
// gate (#179): every check run and commit status in the commit's
// statusCheckRollup, 100 per page. Each page also carries the PR's headRefOid
// and the commit it read, so the parser can tell that commit is the head and
// that a push between two pages did not mix two commits' checks.
//
// ⚠ The rollup is GitHub's raw list, not what the merge box shows: a re-run,
// or a workflow that each label event triggers again, keeps every run
// (cli/cli#14044: 21 runs, 9 checks). latestChecks keeps the newest of each.
const prChecksQuery = `query($url:URI!,$endCursor:String){resource(url:$url){... on PullRequest{` +
	`headRefOid baseRefName commits(last:1){nodes{commit{oid statusCheckRollup{` +
	`contexts(first:100,after:$endCursor){pageInfo{hasNextPage endCursor} nodes{__typename ` +
	`... on CheckRun{databaseId name status conclusion checkSuite{workflowRun{event workflow{name}}}} ` +
	`... on StatusContext{context state createdAt}}}}}}}}}}`

// Check is one check on a PR's head commit (#179): a check run (GitHub Actions
// or another app) or a commit status.
type Check struct {
	Name string // the run's name, or the status's context: what a required check names
	// State is where it stands, read the way gh pr checks reads it: a status's
	// state, a finished run's conclusion, an unfinished run's status. "" when
	// GitHub gave none (a finished run without a conclusion): never green.
	State    string
	Workflow string // the Actions workflow that ran it; "" for a status or another app's run
	Event    string // the event that ran that workflow
	Status   bool   // a commit status, not a check run
}

// PRChecksRead is a PR's head commit and the checks reported on it (#179).
type PRChecksRead struct {
	Head   string  // headRefOid: the commit these checks are for
	Base   string  // baseRefName: the branch whose protection and rulesets require checks
	Checks []Check // the newest of each check, sorted; none = no check reported
}

// PRChecks reads the checks on PR pr's head commit (#179). An error when gh
// fails or its answer is not a PR with a head commit, never a parsed
// placeholder (#168): a PR that cannot be read is not a PR with no checks.
func PRChecks(pr string) (PRChecksRead, error) {
	if !Present() || !Authed() {
		return PRChecksRead{}, errors.New("gh is not available or not authenticated")
	}
	u, err := run("pr", "view", pr, "--json", "url", "--jq", ".url")
	if err != nil {
		return PRChecksRead{}, fmt.Errorf("cannot resolve PR #%s's URL: %w", pr, ghErr(err))
	}
	if strings.TrimSpace(u) == "" {
		return PRChecksRead{}, fmt.Errorf("cannot resolve PR #%s's URL: gh printed none", pr)
	}
	out, err := run("api", "graphql", "--paginate", "-f", "query="+prChecksQuery, "-f", "url="+strings.TrimSpace(u))
	if err != nil {
		return PRChecksRead{}, ghErr(err)
	}
	return parsePRChecks(out)
}

// checksPage is one page of prChecksQuery's answer. Pointers, so that a field
// GitHub left out is told apart from an empty one.
type checksPage struct {
	Data *struct {
		Resource *struct {
			HeadRefOid  *string `json:"headRefOid"`
			BaseRefName *string `json:"baseRefName"`
			Commits     *struct {
				Nodes []struct {
					Commit *struct {
						Oid    *string `json:"oid"`
						Rollup *struct {
							Contexts *rollupContexts `json:"contexts"`
						} `json:"statusCheckRollup"`
					} `json:"commit"`
				} `json:"nodes"`
			} `json:"commits"`
		} `json:"resource"`
	} `json:"data"`
}

// rollupNode is a CheckRun or a StatusContext; __typename says which.
type rollupNode struct {
	Typename   string  `json:"__typename"`
	DatabaseID *int64  `json:"databaseId"`
	Name       *string `json:"name"`
	Status     *string `json:"status"`
	Conclusion *string `json:"conclusion"`
	CheckSuite *struct {
		WorkflowRun *struct {
			Event    string `json:"event"`
			Workflow *struct {
				Name string `json:"name"`
			} `json:"workflow"`
		} `json:"workflowRun"`
	} `json:"checkSuite"`
	Context   *string `json:"context"`
	State     *string `json:"state"`
	CreatedAt string  `json:"createdAt"`
}

// rawCheck is a rollup entry before latestChecks keeps the newest of each.
type rawCheck struct {
	Check
	id int64  // a check run's databaseId: a later run has a larger one
	at string // a status's createdAt (RFC 3339, UTC: compares as text)
}

// parsePRChecks reads PRChecks' answer: one prChecksQuery page per JSON object,
// as `gh api graphql --paginate` prints them. Pure.
//
// Only a PR whose newest commit is its head counts. `{"data":{"resource":null}}`
// (no such PR) and `{"data":{"resource":{}}}` (an issue's URL) both come back
// with exit 0, and either would read as "no checks" if the rollup were all that
// was looked at (#168). A null statusCheckRollup on that commit is the one
// answer that means no check was reported.
func parsePRChecks(out string) (PRChecksRead, error) {
	dec := json.NewDecoder(strings.NewReader(out))
	var (
		read  PRChecksRead
		raw   []rawCheck
		pages int
		more  bool // the last page read said another follows
	)
	for {
		var p checksPage
		if err := dec.Decode(&p); err == io.EOF {
			break
		} else if err != nil {
			return PRChecksRead{}, fmt.Errorf("unreadable checks from gh: %w", err)
		}
		head, base, contexts, err := p.head()
		if err != nil {
			return PRChecksRead{}, err
		}
		pages++
		if pages == 1 {
			read.Head, read.Base = head, base
		} else {
			switch {
			case !more:
				return PRChecksRead{}, errors.New("gh returned a page of checks after the last one")
			case head != read.Head:
				return PRChecksRead{}, fmt.Errorf("the PR's head moved from %.12s to %.12s while wt read its checks", read.Head, head)
			case contexts == nil:
				return PRChecksRead{}, errors.New("gh's next page of checks has none")
			}
		}
		if contexts == nil { // no statusCheckRollup: no check reported
			more = false
			continue
		}
		if contexts.PageInfo == nil {
			return PRChecksRead{}, errors.New("gh's checks have no page info")
		}
		more = contexts.PageInfo.HasNextPage
		for _, n := range contexts.Nodes {
			c, err := n.check()
			if err != nil {
				return PRChecksRead{}, err
			}
			raw = append(raw, c)
		}
	}
	switch {
	case pages == 0:
		return PRChecksRead{}, errors.New("gh returned no checks answer")
	case more:
		return PRChecksRead{}, errors.New("gh stopped before the last page of checks")
	}
	read.Checks = latestChecks(raw)
	return read, nil
}

// rollupContexts is the contexts connection of a statusCheckRollup.
type rollupContexts struct {
	PageInfo *struct {
		HasNextPage bool `json:"hasNextPage"`
	} `json:"pageInfo"`
	Nodes []rollupNode `json:"nodes"`
}

// head validates a page and returns the PR's head commit, its base branch and
// the page's contexts (nil when the commit has no statusCheckRollup).
func (p checksPage) head() (head, base string, contexts *rollupContexts, err error) {
	if p.Data == nil || p.Data.Resource == nil {
		return "", "", nil, errors.New("gh found no such pull request")
	}
	r := p.Data.Resource
	if r.HeadRefOid == nil {
		return "", "", nil, errors.New("gh's answer is not a pull request with a head commit")
	}
	head, ok := parseHeadOid(*r.HeadRefOid)
	if !ok {
		return "", "", nil, fmt.Errorf("gh returned no head commit (got %q)", *r.HeadRefOid)
	}
	if r.BaseRefName == nil || strings.TrimSpace(*r.BaseRefName) == "" {
		return "", "", nil, errors.New("gh returned no base branch")
	}
	if r.Commits == nil || len(r.Commits.Nodes) != 1 || r.Commits.Nodes[0].Commit == nil || r.Commits.Nodes[0].Commit.Oid == nil {
		return "", "", nil, errors.New("gh returned no commit for the PR")
	}
	c := r.Commits.Nodes[0].Commit
	if oid, ok := parseHeadOid(*c.Oid); !ok || oid != head {
		return "", "", nil, fmt.Errorf("the PR's newest commit (%.12s) is not its head (%.12s), so wt cannot tell whose checks it read", *c.Oid, head)
	}
	if c.Rollup == nil {
		return head, *r.BaseRefName, nil, nil
	}
	if c.Rollup.Contexts == nil {
		return "", "", nil, errors.New("gh's statusCheckRollup has no contexts")
	}
	return head, *r.BaseRefName, c.Rollup.Contexts, nil
}

// check converts one rollup node. A node missing what names it or says where it
// stands is an error, not a check with blanks; a kind of node wt does not know
// is one too, so the gate cannot read a check it does not understand as green.
func (n rollupNode) check() (rawCheck, error) {
	str := func(p *string) string {
		if p == nil {
			return ""
		}
		return strings.TrimSpace(*p)
	}
	switch n.Typename {
	case "CheckRun":
		name, status := str(n.Name), str(n.Status)
		if name == "" || status == "" {
			return rawCheck{}, errors.New("gh returned a check run with no name or status")
		}
		c := rawCheck{Check: Check{Name: name, State: RunState(status, str(n.Conclusion))}}
		if n.DatabaseID != nil {
			c.id = *n.DatabaseID
		}
		if n.CheckSuite != nil && n.CheckSuite.WorkflowRun != nil {
			c.Event = n.CheckSuite.WorkflowRun.Event
			if n.CheckSuite.WorkflowRun.Workflow != nil {
				c.Workflow = n.CheckSuite.WorkflowRun.Workflow.Name
			}
		}
		return c, nil
	case "StatusContext":
		name, state := str(n.Context), str(n.State)
		if name == "" || state == "" {
			return rawCheck{}, errors.New("gh returned a commit status with no context or state")
		}
		return rawCheck{Check: Check{Name: name, State: state, Status: true}, at: n.CreatedAt}, nil
	}
	return rawCheck{}, fmt.Errorf("gh returned a check of a kind wt does not know (%q)", n.Typename)
}

// RunState is where a check run stands: its conclusion once COMPLETED, else its
// status (QUEUED, IN_PROGRESS, …). A COMPLETED run with no conclusion is ""
// (unknown). The reading gh pr checks uses. Pure.
func RunState(status, conclusion string) string {
	if strings.EqualFold(status, "COMPLETED") {
		return conclusion
	}
	return status
}

// key groups the runs of one check: a status by its context (GitHub keeps the
// latest per context), a run by name, workflow and event, as gh pr checks does.
func (c rawCheck) key() string {
	if c.Status {
		return "status\x00" + c.Name
	}
	return "run\x00" + c.Name + "\x00" + c.Workflow + "\x00" + c.Event
}

// newer reports whether c is a later run of the same check than d.
func (c rawCheck) newer(d rawCheck) bool {
	if c.Status {
		return c.at > d.at
	}
	return c.id > d.id
}

// latestChecks keeps the newest run of each check, sorted by name, workflow
// and event. A run's databaseId orders it (a re-run's id is larger); gh pr
// checks orders by startedAt, which a queued re-run does not have yet. Pure.
func latestChecks(raw []rawCheck) []Check {
	at := map[string]int{}
	var kept []rawCheck
	for _, c := range raw {
		i, seen := at[c.key()]
		switch {
		case !seen:
			at[c.key()] = len(kept)
			kept = append(kept, c)
		case c.newer(kept[i]):
			kept[i] = c
		}
	}
	out := make([]Check, len(kept))
	for i, c := range kept {
		out[i] = c.Check
	}
	sort.SliceStable(out, func(i, j int) bool {
		a, b := out[i], out[j]
		if a.Name != b.Name {
			return a.Name < b.Name
		}
		if a.Workflow != b.Workflow {
			return a.Workflow < b.Workflow
		}
		return a.Event < b.Event
	})
	return out
}

// BranchProtectionChecks returns the status checks base's branch protection
// requires (#179): none when the branch is unprotected or its required checks
// are off. REST's branch endpoint answers any reader with the
// required_status_checks part of the protection (measured as a non-admin on
// cli/cli and kubernetes/kubernetes), where the protection endpoint itself
// needs admin. An error when gh fails or the answer does not say.
func BranchProtectionChecks(base string) ([]string, error) {
	if !Present() || !Authed() {
		return nil, errors.New("gh is not available or not authenticated")
	}
	out, err := run("api", "repos/{owner}/{repo}/branches/"+url.PathEscape(base))
	if err != nil {
		return nil, apiErr(out, err)
	}
	return parseBranchProtection(out)
}

// parseBranchProtection reads the branch endpoint's answer: protected says
// whether protection applies, and when it does, its required_status_checks
// must be there, with an enforcement level. Pure.
func parseBranchProtection(out string) ([]string, error) {
	var b struct {
		Protected  *bool `json:"protected"`
		Protection *struct {
			Required *struct {
				Level    *string  `json:"enforcement_level"`
				Contexts []string `json:"contexts"`
				Checks   []struct {
					Context string `json:"context"`
				} `json:"checks"`
			} `json:"required_status_checks"`
		} `json:"protection"`
	}
	if err := json.Unmarshal([]byte(strings.TrimSpace(out)), &b); err != nil {
		return nil, fmt.Errorf("unreadable branch from gh: %w", err)
	}
	switch {
	case b.Protected == nil:
		return nil, errors.New("gh's branch does not say whether it is protected")
	case !*b.Protected:
		return nil, nil
	case b.Protection == nil || b.Protection.Required == nil || b.Protection.Required.Level == nil:
		return nil, errors.New("the branch is protected, but gh's answer does not say which checks it requires")
	case *b.Protection.Required.Level == "off":
		return nil, nil
	}
	names := append([]string(nil), b.Protection.Required.Contexts...)
	for _, c := range b.Protection.Required.Checks {
		names = append(names, c.Context)
	}
	return uniqueNames(names), nil
}

// ErrRulesetsUnavailable is RulesetChecks' answer when the server has no
// rulesets for this repo, so none can require a check: a 404 (a GitHub
// Enterprise Server without the endpoint) or the 403 that a private repo on a
// plan without rulesets gets ("Upgrade to GitHub Pro or make this repository
// public to enable this feature.", measured).
var ErrRulesetsUnavailable = errors.New("rulesets are not available for this repository")

// RulesetChecks returns the status checks the active rulesets on base require
// (#179). Any reader may ask (the endpoint returns only active rules). An
// error when gh fails, ErrRulesetsUnavailable when the server has none for
// this repo.
func RulesetChecks(base string) ([]string, error) {
	if !Present() || !Authed() {
		return nil, errors.New("gh is not available or not authenticated")
	}
	out, err := run("api", "repos/{owner}/{repo}/rules/branches/"+url.PathEscape(base), "--paginate")
	if err != nil {
		if rulesetsUnavailable(out) {
			return nil, ErrRulesetsUnavailable
		}
		return nil, apiErr(out, err)
	}
	return parseRulesets(out)
}

// rulesetsUnavailable reports whether a failed rules read's body says the
// server has no rulesets for the repo (ErrRulesetsUnavailable), rather than
// that the read failed. A rate limit or an SSO refusal is a 403 too, and is a
// failed read. Pure.
func rulesetsUnavailable(out string) bool {
	var e struct {
		Message string `json:"message"`
		Status  string `json:"status"`
	}
	if json.Unmarshal([]byte(strings.TrimSpace(out)), &e) != nil {
		return false
	}
	switch e.Status {
	case "404":
		return true
	case "403":
		return strings.Contains(e.Message, "Upgrade to GitHub") || strings.Contains(e.Message, "make this repository public")
	}
	return false
}

// parseRulesets reads the rules endpoint's answer, one JSON array per page,
// and returns the contexts its required_status_checks rules name. Every rule
// must say its type, and a required_status_checks rule must list its checks.
// Pure.
func parseRulesets(out string) ([]string, error) {
	dec := json.NewDecoder(strings.NewReader(out))
	var names []string
	pages := 0
	for {
		var rules []struct {
			Type       *string `json:"type"`
			Parameters *struct {
				Checks *[]struct {
					Context string `json:"context"`
				} `json:"required_status_checks"`
			} `json:"parameters"`
		}
		if err := dec.Decode(&rules); err == io.EOF {
			break
		} else if err != nil {
			return nil, fmt.Errorf("unreadable rules from gh: %w", err)
		}
		if rules == nil { // `null`, not a list
			return nil, errors.New("gh's rules answer is not a list")
		}
		pages++
		for _, r := range rules {
			switch {
			case r.Type == nil:
				return nil, errors.New("gh returned a rule with no type")
			case *r.Type != "required_status_checks":
				continue
			case r.Parameters == nil || r.Parameters.Checks == nil:
				return nil, errors.New("gh returned a required_status_checks rule that lists no checks")
			}
			for _, c := range *r.Parameters.Checks {
				names = append(names, c.Context)
			}
		}
	}
	if pages == 0 {
		return nil, errors.New("gh returned no rules answer")
	}
	return uniqueNames(names), nil
}

// uniqueNames is names trimmed, without blanks or repeats, sorted. Pure.
func uniqueNames(names []string) []string {
	seen := map[string]bool{}
	var out []string
	for _, n := range names {
		if n = strings.TrimSpace(n); n != "" && !seen[n] {
			seen[n] = true
			out = append(out, n)
		}
	}
	sort.Strings(out)
	return out
}

// apiErr is a failed `gh api` read's error: the message and HTTP status of the
// body gh printed, else what gh said on stderr.
func apiErr(out string, err error) error {
	var e struct {
		Message string `json:"message"`
		Status  string `json:"status"`
	}
	if json.Unmarshal([]byte(strings.TrimSpace(out)), &e) == nil && e.Message != "" {
		if e.Status != "" {
			return fmt.Errorf("%s (HTTP %s)", e.Message, e.Status)
		}
		return errors.New(e.Message)
	}
	return ghErr(err)
}

// ghErr is a failed gh call's error with the first line gh printed to stderr,
// which says why ("exit status 1" does not). run's cmd.Output keeps stderr on
// the *exec.ExitError.
func ghErr(err error) error {
	var ee *exec.ExitError
	if errors.As(err, &ee) {
		msg := strings.TrimSpace(string(ee.Stderr))
		if i := strings.IndexByte(msg, '\n'); i >= 0 {
			msg = msg[:i]
		}
		if msg != "" {
			return errors.New(msg)
		}
	}
	return err
}
