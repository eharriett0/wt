// merge-pr's look at GitHub's status page at the deploy gate
// (eharriett0/wt#178). A deploy-path PR was merged twice in one evening while
// the page showed an open Actions incident; each deploy job waited about 15
// minutes for a hosted runner, was cancelled having run no step, and the
// default branch stayed ahead of what was deployed. The gate now reads the
// page right before the deploy confirm and warns. It never blocks: the confirm
// is still the gate, and a status page that is down must not stop a merge.
// merge.DecideActionsStatus decides; this file reads the page and words it.
package cli

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"os"
	"strings"
	"time"
	"unicode"
	"unicode/utf8"

	"github.com/eharriett0/wt/internal/ghx"
	"github.com/eharriett0/wt/internal/merge"
	"github.com/eharriett0/wt/internal/ui"
)

// githubStatusURLEnv points the read at another page: for tests and diagnosis
// only (a fixture server standing in for an incident). Unset, the gate reads
// githubStatusDefaultURL.
const githubStatusURLEnv = "WT_GITHUB_STATUS_URL"

// githubStatusDefaultURL is GitHub's status page summary: every component's
// status and the unresolved incidents.
const githubStatusDefaultURL = "https://www.githubstatus.com/api/v2/summary.json"

// githubStatusTimeout bounds the whole read (connecting, redirects, the body),
// so a slow or unreachable page delays a merge by at most this much. A variable
// so a test's slow case does not take that long.
var githubStatusTimeout = 3 * time.Second

// githubStatusMaxBytes caps what is read of the answer (the summary is ~4 KB).
const githubStatusMaxBytes = 1 << 20

// githubStatusURL is the page the deploy gate reads.
func githubStatusURL() string {
	if u := strings.TrimSpace(os.Getenv(githubStatusURLEnv)); u != "" {
		return u
	}
	return githubStatusDefaultURL
}

// deployStatusHost is the host whose status the deploy gate looks up: the PR's,
// as the checks gate read it from the PR's URL (gh resolves an ssh alias and a
// forwarded -R there), else, when the checks gate read no PR (skipped, or
// unreadable), origin's.
func deployStatusHost(prHost string) string {
	if prHost != "" {
		return prHost
	}
	return ghx.RepoHost()
}

// warnActionsStatus is the deploy gate's look at GitHub's status page (#178),
// for a PR on host, printed after the prod banner and right before the
// confirm: a warning when Actions is not operational or an open incident names
// it, one line when the page could not be read, and nothing when Actions is
// fine. Off github.com, which is all the page covers, it reads nothing and says
// nothing. It never blocks.
func warnActionsStatus(host string) {
	if !merge.GitHubStatusCovers(host) {
		return
	}
	u := githubStatusURL()
	body, err := readGitHubStatus(u, githubStatusTimeout)
	warn, note := actionsStatusLines(merge.DecideActionsStatus(body, err), statusSource(u), time.Now())
	for i, l := range warn {
		if i == 0 {
			l = ui.Bold(l)
		}
		ui.Warn("%s", l)
	}
	if note != "" {
		ui.Info("%s", note)
	}
}

// readGitHubStatus GETs the status page within timeout: anything but a 200
// with a body of at most githubStatusMaxBytes is an error, worded in a few
// words for the line the gate prints when it could not check.
func readGitHubStatus(rawURL string, timeout time.Duration) ([]byte, error) {
	req, err := http.NewRequest(http.MethodGet, rawURL, nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("Accept", "application/json")
	req.Header.Set("User-Agent", "wt/"+version())
	resp, err := (&http.Client{Timeout: timeout}).Do(req)
	if err != nil {
		return nil, statusReadErr(err, timeout)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("HTTP %s", resp.Status)
	}
	body, err := io.ReadAll(io.LimitReader(resp.Body, githubStatusMaxBytes+1))
	if err != nil {
		return nil, statusReadErr(err, timeout)
	}
	if len(body) > githubStatusMaxBytes {
		return nil, fmt.Errorf("an answer over %d bytes", githubStatusMaxBytes)
	}
	return body, nil
}

// statusReadErr words a failed read: a timeout as such, and a transport error
// without the URL or address the line already names ("connect: connection
// refused", "lookup www.githubstatus.com: no such host").
func statusReadErr(err error, timeout time.Duration) error {
	var ne net.Error
	if errors.Is(err, context.DeadlineExceeded) || (errors.As(err, &ne) && ne.Timeout()) {
		return fmt.Errorf("no answer within %s", timeout)
	}
	var oe *net.OpError
	if errors.As(err, &oe) && oe.Err != nil {
		return oe.Err
	}
	var ue *url.Error
	if errors.As(err, &ue) {
		return ue.Err
	}
	return err
}

// statusSource names the page in the lines: its host.
func statusSource(rawURL string) string {
	if u, err := url.Parse(rawURL); err == nil && u.Host != "" {
		return u.Host
	}
	return rawURL
}

// actionsStatusLines is what the deploy gate prints for a status verdict
// (#178): warning lines when Actions is degraded (the components and the
// incidents that name Actions: name, status and last update), one note when
// wt could not tell, and nothing when Actions is operational. now dates the
// last updates. Every string the page supplied goes through termSafe. Pure.
//
//	GitHub's status page reports trouble with Actions (www.githubstatus.com):
//	  Actions: degraded performance
//	  incident: "Actions Job Delays" (investigating, impact minor), last update 2026-10-01 14:54 UTC (12 min ago) https://stspg.io/kdqxfjn5qg6q
//	If this deploy runs on GitHub Actions, …
func actionsStatusLines(s merge.ActionsStatus, source string, now time.Time) (warn []string, note string) {
	switch s.Health {
	case merge.ActionsOperational:
		return nil, ""
	case merge.ActionsDegraded:
	default:
		return nil, fmt.Sprintf("could not check GitHub's status for Actions (%s: %s); continuing without it.", source, termSafe(s.Why))
	}
	warn = append(warn, fmt.Sprintf("GitHub's status page reports trouble with Actions (%s):", source))
	if len(s.Components) == 0 {
		warn = append(warn, "  Actions: not listed as a component")
	}
	for _, c := range s.Components {
		warn = append(warn, fmt.Sprintf("  %s: %s", termSafe(c.Name), componentStatusText(termSafe(c.Status))))
	}
	for _, in := range s.Incidents {
		warn = append(warn, "  incident: "+incidentText(in, now))
	}
	return append(warn, "If this deploy runs on GitHub Actions, a job started now can wait for a runner and be cancelled "+
		"without running: merge once Actions recovers, or check afterwards that the deploy ran."), ""
}

// componentStatusText words a component status: "partial_outage" → "partial
// outage". Pure.
func componentStatusText(status string) string {
	if status == "" {
		return "no status given"
	}
	return strings.ReplaceAll(status, "_", " ")
}

// incidentText is one incident's line: its name, status, impact, last update
// and link. Pure.
func incidentText(in merge.ActionsIncident, now time.Time) string {
	status := termSafe(in.Status)
	if status == "" {
		status = "status not given"
	}
	if impact := termSafe(in.Impact); impact != "" {
		status += ", impact " + impact
	}
	s := fmt.Sprintf("%q (%s)", termSafe(in.Name), status)
	switch {
	case !in.Updated.IsZero():
		s += fmt.Sprintf(", last update %s (%s)", in.Updated.UTC().Format("2006-01-02 15:04 UTC"), agoText(now.Sub(in.Updated)))
	case in.UpdatedText != "":
		s += ", last update " + termSafe(in.UpdatedText)
	}
	if link := termSafe(in.Link); link != "" {
		s += " " + link
	}
	return s
}

// termSafeMax is where termSafe cuts a string from the status page.
const termSafeMax = 200

// termSafe makes a string the status page supplied safe to print: control
// characters (an escape sequence would drive the terminal, a newline would
// forge a line) are dropped, and a runaway string is cut at termSafeMax
// runes. Pure.
func termSafe(s string) string {
	s = strings.Map(func(r rune) rune {
		if unicode.IsControl(r) {
			return -1
		}
		return r
	}, s)
	if utf8.RuneCountInString(s) > termSafeMax {
		s = string([]rune(s)[:termSafeMax]) + "…"
	}
	return strings.TrimSpace(s)
}

// agoText words how long ago something was: "just now" under a minute (and for
// a time ahead of the clock), "N min ago", "NhMMm ago", then days. Pure.
func agoText(d time.Duration) string {
	switch {
	case d < time.Minute:
		return "just now"
	case d < time.Hour:
		return fmt.Sprintf("%d min ago", int(d/time.Minute))
	case d < 48*time.Hour:
		return fmt.Sprintf("%dh%02dm ago", int(d/time.Hour), int(d%time.Hour/time.Minute))
	}
	return fmt.Sprintf("%d days ago", int(d/(24*time.Hour)))
}
