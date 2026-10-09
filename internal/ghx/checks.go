package ghx

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/url"
	"os/exec"
	"sort"
	"strconv"
	"strings"
)

// prChecksQuery reads the checks on a PR's head commit for merge-pr's checks
// gate (#179): every check run and commit status in the commit's
// statusCheckRollup, 100 per page. Each page also carries the PR's headRefOid
// and the commit it read, so the parser can tell that commit is the head and
// that a push between two pages did not mix two commits' checks, and the base
// repository, whose branch rules say what is required (not the current
// directory's repository: the PR can be another repo's, by URL or -R).
//
// Per run it reads what latestChecks groups runs by (the app, the workflow's
// name and file, the event, the workflow run), what a ruleset's required
// workflow is matched on (the workflow file and the repository it lives in),
// and GitHub's own isRequired for this PR, which honours a required check
// pinned to one app. isRequired costs nothing measurable: 1 rate-limit point a
// page with or without it, and the same wall time (pytorch/pytorch#200419, 339
// contexts in 4 pages: 4.1-4.9 s either way).
//
// ⚠ The rollup drops a re-run job's earlier attempt itself (cli/cli run
// 37941363988: attempt 1's FAILED job is not listed, attempt 2's is), but it
// keeps every workflow run a later event started on the same head: each label
// event runs a workflow again (cli/cli#14044: 21 runs of 9 checks), and a run
// that skips a job an earlier run FAILED is listed beside it. latestChecks
// decides which of those still count.
const prChecksQuery = `query($url:URI!,$number:Int!,$endCursor:String){resource(url:$url){... on PullRequest{` +
	`headRefOid baseRefName baseRepository{nameWithOwner databaseId} commits(last:1){nodes{commit{oid statusCheckRollup{` +
	`contexts(first:100,after:$endCursor){pageInfo{hasNextPage endCursor} nodes{__typename ` +
	`... on CheckRun{databaseId name status conclusion isRequired(pullRequestNumber:$number) ` +
	`checkSuite{databaseId app{databaseId} workflowRun{event file{path repositoryName} workflow{name}}}} ` +
	`... on StatusContext{context state createdAt isRequired(pullRequestNumber:$number)}}}}}}}}}}`

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
	App      int64  // the app that ran a check run; 0 for a status
	Required bool   // GitHub's isRequired for this PR: a branch rule requires this very check
	// WorkflowPath and WorkflowRepo are the file an Actions run's workflow came
	// from and the repository it is in (owner/name), "" when GitHub did not say:
	// what a ruleset's required workflow is matched on.
	WorkflowPath, WorkflowRepo string
}

// PRChecksRead is a PR's head commit and the checks reported on it (#179).
type PRChecksRead struct {
	Head   string  // headRefOid: the commit these checks are for
	Base   string  // baseRefName: the branch whose protection and rulesets require checks
	Repo   string  // baseRepository's owner/name: where that branch's rules are read
	RepoID int64   // its databaseId: a ruleset's required workflow names its repository by id
	Host   string  // the PR's host, from its URL: where every read goes
	Number int     // the PR's number
	Checks []Check // what still counts of each check, sorted; none = no check reported
}

// PRChecks reads the checks on PR pr's head commit (#179): pr in repo when repo
// is not "" (a forwarded -R/--repo), else as gh resolves it here. An error when
// gh fails or its answer is not a PR with a head commit, never a parsed
// placeholder (#168): a PR that cannot be read is not a PR with no checks.
func PRChecks(pr, repo string) (PRChecksRead, error) {
	if !Present() || !Authed() {
		return PRChecksRead{}, errors.New("gh is not available or not authenticated")
	}
	args := []string{"pr", "view", pr, "--json", "number,url", "--jq", `"\(.number) \(.url)"`}
	if repo != "" {
		args = append(args, "-R", repo)
	}
	out, err := run(args...)
	if err != nil {
		return PRChecksRead{}, fmt.Errorf("cannot resolve PR #%s's URL: %w", pr, ghErr(err))
	}
	ref, err := parsePRRef(out)
	if err != nil {
		return PRChecksRead{}, fmt.Errorf("cannot resolve PR #%s's URL: %w", pr, err)
	}
	out, err = run("api", "graphql", "--hostname", ref.host, "--paginate", "-f", "query="+prChecksQuery,
		"-f", "url="+ref.url, "-F", "number="+strconv.Itoa(ref.number))
	if err != nil {
		return PRChecksRead{}, ghErr(err)
	}
	read, err := parsePRChecks(out)
	if err != nil {
		return PRChecksRead{}, err
	}
	read.Host, read.Number = ref.host, ref.number
	return read, nil
}

// prRef is a PR's number, URL and host, as PRChecks resolves them.
type prRef struct {
	number    int
	url, host string
}

// parsePRRef reads PRChecks' `gh pr view --json number,url` line, "<number>
// <url>": a positive number and the http(s) URL of that pull request. Pure.
func parsePRRef(out string) (prRef, error) {
	num, raw, ok := strings.Cut(strings.TrimSpace(out), " ")
	n, err := strconv.Atoi(num)
	if !ok || err != nil || n <= 0 {
		return prRef{}, fmt.Errorf("gh printed no PR number and URL (got %q)", out)
	}
	raw = strings.TrimSpace(raw)
	u, err := url.Parse(raw)
	if err != nil || (u.Scheme != "https" && u.Scheme != "http") || u.Host == "" || !strings.HasSuffix(u.Path, "/pull/"+num) {
		return prRef{}, fmt.Errorf("gh printed no URL for PR %s (got %q)", num, raw)
	}
	return prRef{number: n, url: raw, host: u.Host}, nil
}

// checksPage is one page of prChecksQuery's answer. Pointers, so that a field
// GitHub left out is told apart from an empty one.
type checksPage struct {
	Data *struct {
		Resource *struct {
			HeadRefOid     *string `json:"headRefOid"`
			BaseRefName    *string `json:"baseRefName"`
			BaseRepository *struct {
				NameWithOwner *string `json:"nameWithOwner"`
				DatabaseID    *int64  `json:"databaseId"`
			} `json:"baseRepository"`
			Commits *struct {
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
	IsRequired *bool   `json:"isRequired"`
	CheckSuite *struct {
		DatabaseID *int64 `json:"databaseId"`
		App        *struct {
			DatabaseID *int64 `json:"databaseId"`
		} `json:"app"`
		WorkflowRun *struct {
			Event string `json:"event"`
			File  *struct {
				Path           string `json:"path"`
				RepositoryName string `json:"repositoryName"`
			} `json:"file"`
			Workflow *struct {
				Name string `json:"name"`
			} `json:"workflow"`
		} `json:"workflowRun"`
	} `json:"checkSuite"`
	Context   *string `json:"context"`
	State     *string `json:"state"`
	CreatedAt string  `json:"createdAt"`
}

// rawCheck is a rollup entry before latestChecks decides what still counts.
type rawCheck struct {
	Check
	id   int64  // a check run's databaseId: a later run has a larger one
	unit int64  // the workflow run (check suite) an Actions run is in; else the run's own id
	at   string // a status's createdAt (RFC 3339, UTC: compares as text)
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
		pr, contexts, err := p.head()
		if err != nil {
			return PRChecksRead{}, err
		}
		pages++
		if pages == 1 {
			read = pr
		} else {
			switch {
			case !more:
				return PRChecksRead{}, errors.New("gh returned a page of checks after the last one")
			case pr.Head != read.Head:
				return PRChecksRead{}, fmt.Errorf("the PR's head moved from %.12s to %.12s while wt read its checks", read.Head, pr.Head)
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
// repository, and the page's contexts (nil when the commit has no
// statusCheckRollup).
func (p checksPage) head() (PRChecksRead, *rollupContexts, error) {
	if p.Data == nil || p.Data.Resource == nil {
		return PRChecksRead{}, nil, errors.New("gh found no such pull request")
	}
	r := p.Data.Resource
	if r.HeadRefOid == nil {
		return PRChecksRead{}, nil, errors.New("gh's answer is not a pull request with a head commit")
	}
	head, ok := parseHeadOid(*r.HeadRefOid)
	if !ok {
		return PRChecksRead{}, nil, fmt.Errorf("gh returned no head commit (got %q)", *r.HeadRefOid)
	}
	if r.BaseRefName == nil || strings.TrimSpace(*r.BaseRefName) == "" {
		return PRChecksRead{}, nil, errors.New("gh returned no base branch")
	}
	repo := r.BaseRepository
	if repo == nil || repo.NameWithOwner == nil || repo.DatabaseID == nil || !validRepo(*repo.NameWithOwner) {
		return PRChecksRead{}, nil, errors.New("gh returned no base repository")
	}
	if r.Commits == nil || len(r.Commits.Nodes) != 1 || r.Commits.Nodes[0].Commit == nil || r.Commits.Nodes[0].Commit.Oid == nil {
		return PRChecksRead{}, nil, errors.New("gh returned no commit for the PR")
	}
	c := r.Commits.Nodes[0].Commit
	if oid, ok := parseHeadOid(*c.Oid); !ok || oid != head {
		return PRChecksRead{}, nil, fmt.Errorf("the PR's newest commit (%.12s) is not its head (%.12s), so wt cannot tell whose checks it read", *c.Oid, head)
	}
	read := PRChecksRead{Head: head, Base: *r.BaseRefName, Repo: *repo.NameWithOwner, RepoID: *repo.DatabaseID}
	if c.Rollup == nil {
		return read, nil, nil
	}
	if c.Rollup.Contexts == nil {
		return PRChecksRead{}, nil, errors.New("gh's statusCheckRollup has no contexts")
	}
	return read, c.Rollup.Contexts, nil
}

// validRepo reports whether s is an owner/name. Pure.
func validRepo(s string) bool {
	owner, name, ok := strings.Cut(s, "/")
	return ok && owner != "" && name != "" && !strings.ContainsAny(name, "/ ") && !strings.Contains(owner, " ")
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
	required := n.IsRequired != nil && *n.IsRequired
	switch n.Typename {
	case "CheckRun":
		name, status := str(n.Name), str(n.Status)
		if name == "" || status == "" || n.DatabaseID == nil {
			return rawCheck{}, errors.New("gh returned a check run with no name, status or id")
		}
		c := rawCheck{Check: Check{Name: name, State: RunState(status, str(n.Conclusion)), Required: required},
			id: *n.DatabaseID, unit: *n.DatabaseID}
		s := n.CheckSuite
		if s == nil {
			return c, nil
		}
		switch {
		case s.App != nil && s.App.DatabaseID != nil:
			c.App = *s.App.DatabaseID
		case s.DatabaseID != nil:
			c.App = -*s.DatabaseID // an app GitHub does not name: its own, never another's
		}
		if w := s.WorkflowRun; w != nil {
			if s.DatabaseID == nil {
				return rawCheck{}, errors.New("gh returned a workflow's check run with no check suite")
			}
			c.unit, c.Event = *s.DatabaseID, w.Event
			if w.Workflow != nil {
				c.Workflow = w.Workflow.Name
			}
			if w.File != nil {
				c.WorkflowPath, c.WorkflowRepo = w.File.Path, w.File.RepositoryName
			}
		}
		return c, nil
	case "StatusContext":
		name, state := str(n.Context), str(n.State)
		if name == "" || state == "" {
			return rawCheck{}, errors.New("gh returned a commit status with no context or state")
		}
		return rawCheck{Check: Check{Name: name, State: state, Status: true, Required: required}, at: n.CreatedAt}, nil
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

// Kind is how a check state stands (#179): the one reading of GitHub's states
// that latestChecks and merge-pr's gate (merge.BucketOf) share.
type Kind int

const (
	KindUnknown Kind = iota // a state wt does not know, or none: never green
	KindPassed              // SUCCESS
	KindQuiet               // SKIPPED, NEUTRAL: the run did not run it, or would not say
	KindPending             // has not finished
	KindFailed              // finished failing: stands until a later run of the check passes
	KindVoid                // CANCELLED, STALE: not a pass, but any later run replaces it
)

// StateKind reads a check's state (Check.State). Pure.
//
//   - Commit status states (StatusState): SUCCESS, PENDING, EXPECTED, FAILURE,
//     ERROR.
//   - A check run's status until it completes (CheckStatusState): QUEUED,
//     IN_PROGRESS, WAITING, PENDING, REQUESTED.
//   - Its conclusion after (CheckConclusionState): SUCCESS, NEUTRAL, SKIPPED,
//     FAILURE, CANCELLED, TIMED_OUT, ACTION_REQUIRED, STARTUP_FAILURE, STALE.
//
// STALE (GitHub gave up on a run left incomplete) is void, not pending: it will
// never finish. Anything else, including a state GitHub adds later, is unknown.
func StateKind(state string) Kind {
	switch strings.ToUpper(strings.TrimSpace(state)) {
	case "SUCCESS":
		return KindPassed
	case "SKIPPED", "NEUTRAL":
		return KindQuiet
	case "PENDING", "EXPECTED", "QUEUED", "IN_PROGRESS", "WAITING", "REQUESTED":
		return KindPending
	case "FAILURE", "ERROR", "TIMED_OUT", "ACTION_REQUIRED", "STARTUP_FAILURE":
		return KindFailed
	case "CANCELLED", "STALE":
		return KindVoid
	}
	return KindUnknown
}

// key groups the runs of one check: a status by its context (GitHub keeps the
// latest per context); a run by its app, its workflow (name and file), the
// event, and its name. The app keeps two apps' runs of one name apart: a
// dotnet/runtime PR carries "Helix Queue Insights (preview)" from two apps, and
// one app's FAILURE must not be replaced by the other's pass.
func (c rawCheck) key() string {
	if c.Status {
		return "status\x00" + c.Name
	}
	return fmt.Sprintf("run\x00%d\x00%s\x00%s\x00%s\x00%s", c.App, c.Workflow, c.WorkflowPath, c.Event, c.Name)
}

// latestChecks keeps, of the runs of each check, the ones that still count
// (#179), sorted by name, workflow and event. Pure.
//
// A commit status counts at its newest (GitHub keeps one per context). A run
// counts with the others of its check (key) like this:
//
//   - Within ONE workflow run every run counts: the rollup already dropped a
//     re-run job's earlier attempt, so two runs of one name in one workflow run
//     are two jobs (pytorch's reusable workflows repeat names), and each is
//     judged.
//   - Across the workflow runs of one check (a label event ran the workflow
//     again on the same head), the newest decides, except that a run that only
//     skipped the check (SKIPPED, NEUTRAL) does not replace an earlier one that
//     passed, is running or is in a state wt does not know (cli/cli#14044's
//     check-requirements passed, then two label events skipped it: it passed);
//     CANCELLED and STALE are replaced by any later run.
//   - A failure (FAILURE, ERROR, TIMED_OUT, STARTUP_FAILURE, ACTION_REQUIRED)
//     stands until a later workflow run passes the check: a later run that
//     skipped it, is neutral or was cancelled does not clear it, and one still
//     running is listed beside it. Taking the newest run alone read a label
//     event's run that skipped a job an earlier run FAILED as green.
//
// A run of an app outside Actions is its own unit, so a re-requested run
// replaces the one before it the same way.
func latestChecks(raw []rawCheck) []Check {
	groups := map[string][]rawCheck{}
	var order []string
	for _, c := range raw {
		k := c.key()
		if _, seen := groups[k]; !seen {
			order = append(order, k)
		}
		groups[k] = append(groups[k], c)
	}
	out := []Check{}
	for _, k := range order {
		g := groups[k]
		if g[0].Status {
			out = append(out, newestStatus(g).Check)
			continue
		}
		for _, c := range counting(g) {
			out = append(out, c.Check)
		}
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

// newestStatus is the newest of one context's statuses: the first of the
// newest when they tie. Pure.
func newestStatus(g []rawCheck) rawCheck {
	n := g[0]
	for _, c := range g[1:] {
		if c.at > n.at {
			n = c
		}
	}
	return n
}

// counting is the runs of one check that still count (latestChecks). Pure.
func counting(runs []rawCheck) []rawCheck {
	type unit struct {
		newest int64 // its newest run's id: units are ordered by it
		runs   []rawCheck
	}
	byID := map[int64]*unit{}
	var units []*unit
	for _, r := range runs {
		u := byID[r.unit]
		if u == nil {
			u = &unit{newest: r.id}
			byID[r.unit] = u
			units = append(units, u)
		}
		u.runs = append(u.runs, r)
		if r.id > u.newest {
			u.newest = r.id
		}
	}
	sort.SliceStable(units, func(i, j int) bool { return units[i].newest < units[j].newest })
	every := func(u *unit, k Kind) bool {
		for _, r := range u.runs {
			if StateKind(r.State) != k {
				return false
			}
		}
		return true
	}
	informative := func(u *unit) bool { // passed, running, failed or unknown
		for _, r := range u.runs {
			if k := StateKind(r.State); k != KindQuiet && k != KindVoid {
				return true
			}
		}
		return false
	}
	// A failure in a workflow run before the newest one that passed every run
	// of the check is cleared; any later one stands.
	cleared := -1
	for i, u := range units {
		if every(u, KindPassed) {
			cleared = i
		}
	}
	// The workflow run that decides: the newest, unless it only skipped the
	// check; then the newest before it that says more, if one does.
	decide := len(units) - 1
	if every(units[decide], KindQuiet) {
		for i := decide - 1; i >= 0; i-- {
			if informative(units[i]) {
				decide = i
				break
			}
		}
	}
	out := append([]rawCheck(nil), units[decide].runs...)
	for i, u := range units {
		if i == decide || i <= cleared {
			continue
		}
		for _, r := range u.runs {
			if StateKind(r.State) == KindFailed {
				out = append(out, r)
			}
		}
	}
	return out
}

// RequiredCheck is a status check a branch rule requires (#179): its name, and
// the app that must report it (0: any app).
type RequiredCheck struct {
	Name string
	App  int64
}

// RequiredWorkflow is a workflow a ruleset requires to run on a PR (#179): the
// file Path in the repository RepoID. Repo is that repository's owner/name,
// which merge-pr resolves before the gate matches runs to it ("" until then).
type RequiredWorkflow struct {
	Path   string
	RepoID int64
	Repo   string
}

// Required is what a branch's rules require before a merge (#179).
type Required struct {
	Checks    []RequiredCheck
	Workflows []RequiredWorkflow
}

// repoPath is the REST path of repository repo (owner/name). Pure.
func repoPath(repo string) (string, error) {
	if !validRepo(repo) {
		return "", fmt.Errorf("%q is not a repository (owner/name)", repo)
	}
	owner, name, _ := strings.Cut(repo, "/")
	return "repos/" + url.PathEscape(owner) + "/" + url.PathEscape(name), nil
}

// BranchProtectionChecks returns the status checks the branch protection of
// branch base in repo (owner/name, on host) requires (#179): none when the
// branch is unprotected or its required checks are off. REST's branch endpoint
// answers any reader with the required_status_checks part of the protection
// (measured as a non-admin on cli/cli and kubernetes/kubernetes), where the
// protection endpoint itself needs admin. An error when gh fails or the answer
// does not say.
func BranchProtectionChecks(host, repo, base string) (Required, error) {
	if !Present() || !Authed() {
		return Required{}, errors.New("gh is not available or not authenticated")
	}
	path, err := repoPath(repo)
	if err != nil {
		return Required{}, err
	}
	out, err := run("api", "--hostname", host, path+"/branches/"+url.PathEscape(base))
	if err != nil {
		return Required{}, apiErr(out, err)
	}
	checks, err := parseBranchProtection(out)
	return Required{Checks: checks}, err
}

// parseBranchProtection reads the branch endpoint's answer: protected says
// whether protection applies, and when it does, its required_status_checks
// must be there, with an enforcement level. A check's app_id pins it to that
// app; a context listed only in the older contexts list is any app's. Pure.
func parseBranchProtection(out string) ([]RequiredCheck, error) {
	var b struct {
		Protected  *bool `json:"protected"`
		Protection *struct {
			Required *struct {
				Level    *string  `json:"enforcement_level"`
				Contexts []string `json:"contexts"`
				Checks   []struct {
					Context string `json:"context"`
					AppID   *int64 `json:"app_id"`
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
	var checks []RequiredCheck
	listed := map[string]bool{}
	for _, c := range b.Protection.Required.Checks {
		checks = append(checks, RequiredCheck{Name: c.Context, App: pinnedApp(c.AppID)})
		listed[strings.TrimSpace(c.Context)] = true
	}
	for _, n := range b.Protection.Required.Contexts {
		if !listed[strings.TrimSpace(n)] {
			checks = append(checks, RequiredCheck{Name: n})
		}
	}
	return uniqueChecks(checks), nil
}

// pinnedApp is a required check's app: an app id, or 0 for "any" (null, or -1).
// Pure.
func pinnedApp(id *int64) int64 {
	if id == nil || *id <= 0 {
		return 0
	}
	return *id
}

// ErrRulesetsUnavailable is RulesetChecks' answer when the server has no
// rulesets for this repo, so none can require a check: a 404 (a GitHub
// Enterprise Server without the endpoint) or the 403 that a private repo on a
// plan without rulesets gets ("Upgrade to GitHub Pro or make this repository
// public to enable this feature.", measured).
var ErrRulesetsUnavailable = errors.New("rulesets are not available for this repository")

// RulesetChecks returns what the active rulesets on branch base in repo
// (owner/name, on host) require (#179): status checks, and workflows that must
// run. Any reader may ask (the endpoint returns only active rules). An error
// when gh fails, ErrRulesetsUnavailable when the server has none for this repo.
func RulesetChecks(host, repo, base string) (Required, error) {
	if !Present() || !Authed() {
		return Required{}, errors.New("gh is not available or not authenticated")
	}
	path, err := repoPath(repo)
	if err != nil {
		return Required{}, err
	}
	out, err := run("api", "--hostname", host, path+"/rules/branches/"+url.PathEscape(base), "--paginate")
	if err != nil {
		if rulesetsUnavailable(out) {
			return Required{}, ErrRulesetsUnavailable
		}
		return Required{}, apiErr(out, err)
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

// parseRulesets reads the rules endpoint's answer, one JSON array per page:
// the checks its required_status_checks rules name (an integration_id pins one
// to that app), and the workflows its workflows rules require to run. Every
// rule must say its type, and those two kinds must list what they require.
// Pure.
func parseRulesets(out string) (Required, error) {
	dec := json.NewDecoder(strings.NewReader(out))
	var req Required
	pages := 0
	for {
		var rules []struct {
			Type       *string `json:"type"`
			Parameters *struct {
				Checks *[]struct {
					Context       string `json:"context"`
					IntegrationID *int64 `json:"integration_id"`
				} `json:"required_status_checks"`
				Workflows *[]struct {
					Path         string `json:"path"`
					RepositoryID *int64 `json:"repository_id"`
				} `json:"workflows"`
			} `json:"parameters"`
		}
		if err := dec.Decode(&rules); err == io.EOF {
			break
		} else if err != nil {
			return Required{}, fmt.Errorf("unreadable rules from gh: %w", err)
		}
		if rules == nil { // `null`, not a list
			return Required{}, errors.New("gh's rules answer is not a list")
		}
		pages++
		for _, r := range rules {
			if r.Type == nil {
				return Required{}, errors.New("gh returned a rule with no type")
			}
			switch *r.Type {
			case "required_status_checks":
				if r.Parameters == nil || r.Parameters.Checks == nil {
					return Required{}, errors.New("gh returned a required_status_checks rule that lists no checks")
				}
				for _, c := range *r.Parameters.Checks {
					req.Checks = append(req.Checks, RequiredCheck{Name: c.Context, App: pinnedApp(c.IntegrationID)})
				}
			case "workflows":
				if r.Parameters == nil || r.Parameters.Workflows == nil {
					return Required{}, errors.New("gh returned a workflows rule that lists no workflows")
				}
				for _, w := range *r.Parameters.Workflows {
					if strings.TrimSpace(w.Path) == "" || w.RepositoryID == nil || *w.RepositoryID <= 0 {
						return Required{}, errors.New("gh returned a required workflow with no file or repository")
					}
					req.Workflows = append(req.Workflows, RequiredWorkflow{Path: strings.TrimSpace(w.Path), RepoID: *w.RepositoryID})
				}
			}
		}
	}
	if pages == 0 {
		return Required{}, errors.New("gh returned no rules answer")
	}
	req.Checks = uniqueChecks(req.Checks)
	req.Workflows = uniqueWorkflows(req.Workflows)
	return req, nil
}

// RepoName is the owner/name of the repository with databaseId id, on host
// (#179): where a ruleset's required workflow lives. An error when gh fails or
// prints no name.
func RepoName(host string, id int64) (string, error) {
	if !Present() || !Authed() {
		return "", errors.New("gh is not available or not authenticated")
	}
	out, err := run("api", "--hostname", host, "repositories/"+strconv.FormatInt(id, 10), "--jq", ".full_name")
	if err != nil {
		return "", apiErr(out, err)
	}
	if name := strings.TrimSpace(out); validRepo(name) {
		return name, nil
	}
	return "", fmt.Errorf("gh printed no name for repository %d (got %q)", id, out)
}

// uniqueChecks is checks with names trimmed, without blanks or repeats, sorted
// by name and app. Pure.
func uniqueChecks(checks []RequiredCheck) []RequiredCheck {
	seen := map[RequiredCheck]bool{}
	var out []RequiredCheck
	for _, c := range checks {
		c.Name = strings.TrimSpace(c.Name)
		if c.Name != "" && !seen[c] {
			seen[c] = true
			out = append(out, c)
		}
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i].Name != out[j].Name {
			return out[i].Name < out[j].Name
		}
		return out[i].App < out[j].App
	})
	return out
}

// uniqueWorkflows is workflows without repeats, sorted by repository and file.
// Pure.
func uniqueWorkflows(ws []RequiredWorkflow) []RequiredWorkflow {
	seen := map[RequiredWorkflow]bool{}
	var out []RequiredWorkflow
	for _, w := range ws {
		if !seen[w] {
			seen[w] = true
			out = append(out, w)
		}
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i].RepoID != out[j].RepoID {
			return out[i].RepoID < out[j].RepoID
		}
		return out[i].Path < out[j].Path
	})
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
