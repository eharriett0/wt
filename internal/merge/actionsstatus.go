package merge

import (
	"encoding/json"
	"net"
	"strings"
	"time"
	"unicode"
	"unicode/utf8"
)

// GitHub's status page at merge-pr's deploy gate (eharriett0/wt#178). A
// deploy-path merge whose deploy runs on GitHub Actions can be lost to an
// Actions incident: a hosted job waited about 15 minutes for a runner and was
// cancelled with no step run, leaving the default branch ahead of what was
// deployed, while the status page had shown the incident for 84 minutes. The
// gate reads the page and warns; it never blocks (internal/cli does the read).

// ActionsHealth is what GitHub's status page says about GitHub Actions (#178).
type ActionsHealth string

const (
	ActionsOperational ActionsHealth = "operational" // the Actions component is operational and no open incident names Actions: say nothing
	ActionsDegraded    ActionsHealth = "degraded"    // the component is not operational, or an open incident names Actions: warn
	ActionsUnknown     ActionsHealth = "unknown"     // the page could not be read, or does not say: one line saying so
)

// ActionsComponent is a status-page component that names Actions, as the page
// gives it. Status is "operational", "degraded_performance", "partial_outage",
// "major_outage", "under_maintenance", or whatever the page adds later.
type ActionsComponent struct{ Name, Status string }

// ActionsIncident is an unresolved incident that names Actions.
type ActionsIncident struct {
	Name, Status, Impact string    // e.g. "Incident with Actions", "investigating", "major"
	Link                 string    // the page's shortlink to it; "" when it gave none
	Updated              time.Time // its last update; zero when the page gave no time wt can read
	UpdatedText          string    // that time as the page wrote it, for a time that does not parse
}

// ActionsStatus is DecideActionsStatus's verdict.
type ActionsStatus struct {
	Health     ActionsHealth
	Components []ActionsComponent // every component that names Actions (one, "Actions", on GitHub's page today)
	Incidents  []ActionsIncident  // every unresolved incident that names Actions, in the page's order
	Why        string             // ActionsUnknown: why wt cannot tell
}

// statusSummary is the part of the status page's summary
// (/api/v2/summary.json) the gate reads. Pointers, so that a list the answer
// left out is told apart from an empty one.
type statusSummary struct {
	Components *[]statusComponent `json:"components"`
	Incidents  *[]statusIncident  `json:"incidents"`
}

type statusComponent struct {
	Name   string `json:"name"`
	Status string `json:"status"`
}

type statusIncident struct {
	Name       string            `json:"name"`
	Status     string            `json:"status"`
	Impact     string            `json:"impact"`
	Shortlink  string            `json:"shortlink"`
	UpdatedAt  string            `json:"updated_at"`
	ResolvedAt *string           `json:"resolved_at"`
	Components []statusComponent `json:"components"`
	Updates    []struct {
		Affected  []statusComponent `json:"affected_components"`
		DisplayAt string            `json:"display_at"`
		CreatedAt string            `json:"created_at"`
	} `json:"incident_updates"`
}

// DecideActionsStatus reads GitHub's status page summary for the deploy gate
// (#178): degraded when a component that names Actions is not "operational",
// or an unresolved incident names Actions (in its name, its components or an
// update's affected components); unknown when the page could not be read
// (readErr), its answer is not the summary's JSON, or it does not say (no
// component list or incident list, no Actions component, one with no status);
// operational otherwise. A degraded signal outranks an unknown one: what the
// page does say is worth the warning. Pure.
//
// ⚠ Never operational on a page that does not say so. An answer listing no
// Actions component (a renamed component, some other JSON behind a proxy) is
// unknown, so the gate prints the line saying it could not check instead of
// nothing at all, which an operator would read as "Actions is fine".
func DecideActionsStatus(body []byte, readErr error) ActionsStatus {
	if readErr != nil {
		return ActionsStatus{Health: ActionsUnknown, Why: readErr.Error()}
	}
	var page statusSummary
	if err := json.Unmarshal(body, &page); err != nil {
		return ActionsStatus{Health: ActionsUnknown, Why: "its answer is not the status summary's JSON"}
	}
	var s ActionsStatus
	var unsaid []string
	degraded := false
	if page.Components == nil {
		unsaid = append(unsaid, "its answer lists no components")
	} else {
		for _, c := range *page.Components {
			if !namesActions(c.Name) {
				continue
			}
			c := ActionsComponent{Name: strings.TrimSpace(c.Name), Status: strings.TrimSpace(c.Status)}
			s.Components = append(s.Components, c)
			switch {
			case c.Status == "":
				unsaid = append(unsaid, "its "+c.Name+" component has no status")
			case !strings.EqualFold(c.Status, "operational"):
				degraded = true
			}
		}
		if len(s.Components) == 0 {
			unsaid = append(unsaid, "it lists no Actions component")
		}
	}
	if page.Incidents == nil {
		unsaid = append(unsaid, "its answer has no incident list")
	} else {
		for _, in := range *page.Incidents {
			if in.resolved() || !in.namesActions() {
				continue
			}
			updated, text := in.lastUpdate()
			s.Incidents = append(s.Incidents, ActionsIncident{
				Name: strings.TrimSpace(in.Name), Status: strings.TrimSpace(in.Status), Impact: strings.TrimSpace(in.Impact),
				Link: strings.TrimSpace(in.Shortlink), Updated: updated, UpdatedText: text,
			})
		}
	}
	switch {
	case degraded || len(s.Incidents) > 0:
		s.Health = ActionsDegraded
	case len(unsaid) > 0:
		s.Health, s.Why = ActionsUnknown, strings.Join(unsaid, "; ")
	default:
		s.Health = ActionsOperational
	}
	return s
}

// resolved reports whether the page says the incident is over: its status is
// resolved or postmortem, or, giving no status, it carries a resolved_at. A
// status that says otherwise outranks a resolved_at: a contradiction reads as
// open, since a warning too many costs a glance and one too few a lost deploy.
func (in statusIncident) resolved() bool {
	switch strings.ToLower(strings.TrimSpace(in.Status)) {
	case "resolved", "postmortem":
		return true
	case "":
		return in.ResolvedAt != nil && strings.TrimSpace(*in.ResolvedAt) != ""
	}
	return false
}

// namesActions reports whether the incident names Actions: in its name, or as
// one of its components or an update's affected components.
func (in statusIncident) namesActions() bool {
	if namesActions(in.Name) {
		return true
	}
	for _, c := range in.Components {
		if namesActions(c.Name) {
			return true
		}
	}
	for _, u := range in.Updates {
		for _, c := range u.Affected {
			if namesActions(c.Name) {
				return true
			}
		}
	}
	return false
}

// lastUpdate is when the incident was last updated: the newest of its updates'
// times (display_at, else created_at), which is what the status page shows,
// else the incident's own updated_at. That one moves on any edit: a resolved
// incident's later edits put it days past its resolution. A time that does not
// parse is kept as text.
func (in statusIncident) lastUpdate() (time.Time, string) {
	var newest time.Time
	text := ""
	for _, u := range in.Updates {
		raw := strings.TrimSpace(u.DisplayAt)
		if raw == "" {
			raw = strings.TrimSpace(u.CreatedAt)
		}
		if t, err := time.Parse(time.RFC3339Nano, raw); err == nil {
			if t.After(newest) {
				newest, text = t, raw
			}
		} else if text == "" {
			text = raw
		}
	}
	if !newest.IsZero() {
		return newest, text
	}
	if raw := strings.TrimSpace(in.UpdatedAt); raw != "" {
		if t, err := time.Parse(time.RFC3339Nano, raw); err == nil {
			return t, raw
		}
		if text == "" {
			text = raw
		}
	}
	return time.Time{}, text
}

// namesActions reports whether s holds the word "Actions" (any case, as a
// whole word): "Incident with Actions", "Actions Job Delays", "GitHub
// Actions-hosted runners", but not "Transactions". Pure.
func namesActions(s string) bool {
	const word = "actions"
	lower := strings.ToLower(s)
	for from := 0; from < len(lower); {
		i := strings.Index(lower[from:], word)
		if i < 0 {
			return false
		}
		start, end := from+i, from+i+len(word)
		before, _ := utf8.DecodeLastRuneInString(lower[:start])
		after, _ := utf8.DecodeRuneInString(lower[end:])
		if (start == 0 || !isWordRune(before)) && (end == len(lower) || !isWordRune(after)) {
			return true
		}
		from = end
	}
	return false
}

func isWordRune(r rune) bool { return r == '_' || unicode.IsLetter(r) || unicode.IsDigit(r) }

// GitHubStatusCovers reports whether GitHub's status page speaks for host, a
// PR's or a remote's host: github.com only. GitHub Enterprise Server and
// GHE.com (data residency) are not on it, so the deploy gate does not read it
// for them. Pure.
func GitHubStatusCovers(host string) bool {
	h := strings.ToLower(strings.TrimSpace(host))
	if hp, _, err := net.SplitHostPort(h); err == nil {
		h = hp
	}
	switch strings.TrimSuffix(h, ".") {
	case "github.com", "www.github.com", "ssh.github.com":
		return true
	}
	return false
}
