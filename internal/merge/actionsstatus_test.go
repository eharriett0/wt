package merge

import (
	"errors"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"
)

// TestDecideActionsStatus reads GitHub's status page summary the way the
// deploy gate does (#178). operational.json is a real capture of
// https://www.githubstatus.com/api/v2/summary.json; the other fixtures are
// that page with the Actions component's status changed and real incidents
// (from /api/v2/incidents.json) reopened, so every one has the page's full
// shape.
func TestDecideActionsStatus(t *testing.T) {
	at := func(s string) time.Time {
		tm, err := time.Parse(time.RFC3339Nano, s)
		if err != nil {
			t.Fatal(err)
		}
		return tm
	}
	actions := func(status string) []ActionsComponent { return []ActionsComponent{{"Actions", status}} }
	jobDelays := ActionsIncident{Name: "Actions Job Delays", Status: "investigating", Impact: "minor",
		Link: "https://stspg.io/kdqxfjn5qg6q", Updated: at("2026-10-01T14:54:13.101Z"), UpdatedText: "2026-10-01T14:54:13.101Z"}
	withActions := ActionsIncident{Name: "Incident with Actions", Status: "identified", Impact: "critical",
		Link: "https://stspg.io/c11dc9nb1zdq", Updated: at("2026-10-05T19:15:17.486Z"), UpdatedText: "2026-10-05T19:15:17.486Z"}

	cases := []struct {
		name      string
		fixture   string // a file under testdata/githubstatus, or
		body      string // the answer itself
		readErr   error
		health    ActionsHealth
		comps     []ActionsComponent
		incidents []ActionsIncident
		why       string // ActionsUnknown: in Why
	}{
		{name: "the real page, all operational", fixture: "operational.json", health: ActionsOperational, comps: actions("operational")},
		{name: "Actions degraded_performance, with its incident", fixture: "degraded-performance.json", health: ActionsDegraded,
			comps: actions("degraded_performance"), incidents: []ActionsIncident{jobDelays}},
		{name: "Actions partial_outage, before any incident is posted", fixture: "partial-outage.json", health: ActionsDegraded,
			comps: actions("partial_outage")},
		{name: "Actions major_outage; an incident about Copilot is left out", fixture: "major-outage.json", health: ActionsDegraded,
			comps: actions("major_outage"), incidents: []ActionsIncident{withActions}},
		{name: "an open incident names Actions while the component reads operational", fixture: "incident-component-operational.json",
			health: ActionsDegraded, comps: actions("operational"),
			incidents: []ActionsIncident{{Name: "Incident with Git Operations, Pull Requests and Actions", Status: "monitoring", Impact: "critical",
				Link: "https://stspg.io/96smrcth8bpg", Updated: at("2026-10-07T15:14:55.866Z"), UpdatedText: "2026-10-07T15:14:55.866Z"}}},
		{name: "an open incident names Actions only among its components", fixture: "incident-by-component.json",
			health: ActionsDegraded, comps: actions("operational"),
			incidents: []ActionsIncident{{Name: "Incident with several GitHub Services", Status: "investigating", Impact: "critical",
				Link: "https://stspg.io/8f3xch4y0v2k", Updated: at("2026-09-13T09:25:56.000Z"), UpdatedText: "2026-09-13T09:25:56.000Z"}}},
		{name: "an open incident names Actions only in an update's affected components", fixture: "incident-by-update.json",
			health: ActionsDegraded, comps: actions("operational"),
			incidents: []ActionsIncident{{Name: "Disruption with some GitHub services", Status: "investigating", Impact: "major",
				Link: "https://stspg.io/pjcfc4c3pm7g", Updated: at("2026-10-05T23:47:45.254Z"), UpdatedText: "2026-10-05T23:47:45.254Z"}}},
		{name: "a resolved incident with Actions: operational", fixture: "resolved-incident.json", health: ActionsOperational,
			comps: actions("operational")},
		{name: "an open incident about something else: operational", fixture: "unrelated-incident.json", health: ActionsOperational,
			comps: actions("operational")},
		{name: "no Actions component: unknown, never operational", fixture: "no-actions-component.json", health: ActionsUnknown,
			why: "it lists no Actions component"},
		{name: "no Actions component, but an open incident names it: degraded", fixture: "no-actions-component-incident.json",
			health: ActionsDegraded, incidents: []ActionsIncident{withActions}},
		{name: "malformed JSON", body: `{"components":[{"name":"Actions","status":"operational"}`, health: ActionsUnknown,
			why: "its answer is not the status summary's JSON"},
		{name: "an HTML page", body: "<!DOCTYPE html><title>Service Unavailable</title>", health: ActionsUnknown,
			why: "its answer is not the status summary's JSON"},
		{name: "a JSON list", body: `[]`, health: ActionsUnknown, why: "its answer is not the status summary's JSON"},
		{name: "null", body: `null`, health: ActionsUnknown, why: "its answer lists no components; its answer has no incident list"},
		{name: "an empty object", body: `{}`, health: ActionsUnknown, why: "its answer lists no components; its answer has no incident list"},
		{name: "no incident list: it cannot say no incident names Actions", body: `{"components":[{"name":"Actions","status":"operational"}]}`,
			health: ActionsUnknown, comps: actions("operational"), why: "its answer has no incident list"},
		{name: "no incident list, but the component is down: degraded", body: `{"components":[{"name":"Actions","status":"major_outage"}]}`,
			health: ActionsDegraded, comps: actions("major_outage")},
		{name: "an Actions component with no status", body: `{"components":[{"name":"Actions","status":""}],"incidents":[]}`,
			health: ActionsUnknown, comps: actions(""), why: "its Actions component has no status"},
		{name: "a status wt does not know is not operational", body: `{"components":[{"name":"Actions","status":"on_fire"}],"incidents":[]}`,
			health: ActionsDegraded, comps: actions("on_fire")},
		{name: "under_maintenance is not operational", body: `{"components":[{"name":"Actions","status":"under_maintenance"}],"incidents":[]}`,
			health: ActionsDegraded, comps: actions("under_maintenance")},
		{name: "the page could not be read", readErr: errors.New("no answer within 3s"), health: ActionsUnknown, why: "no answer within 3s"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			body := []byte(tc.body)
			if tc.fixture != "" {
				var err error
				if body, err = os.ReadFile(filepath.Join("testdata", "githubstatus", tc.fixture)); err != nil {
					t.Fatal(err)
				}
			}
			got := DecideActionsStatus(body, tc.readErr)
			if got.Health != tc.health {
				t.Errorf("Health = %q, want %q (why %q)", got.Health, tc.health, got.Why)
			}
			if !reflect.DeepEqual(got.Components, tc.comps) {
				t.Errorf("Components = %+v, want %+v", got.Components, tc.comps)
			}
			if !reflect.DeepEqual(got.Incidents, tc.incidents) {
				t.Errorf("Incidents =\n%+v\nwant\n%+v", got.Incidents, tc.incidents)
			}
			if tc.why != "" && got.Why != tc.why {
				t.Errorf("Why = %q, want %q", got.Why, tc.why)
			}
			if tc.health != ActionsUnknown && got.Why != "" {
				t.Errorf("Why = %q on a %s verdict, want none", got.Why, got.Health)
			}
		})
	}
}

// TestDecideActionsStatus_incidentFields: which incidents count (unresolved,
// naming Actions) and when each was last updated.
func TestDecideActionsStatus_incidentFields(t *testing.T) {
	const comp = `"components":[{"name":"Actions","status":"operational"}]`
	cases := []struct {
		name, incidents string
		want            []string // Name|Status|Updated (RFC3339) or UpdatedText
	}{
		{"open statuses count, closed ones do not",
			`{"name":"Actions a","status":"investigating"},{"name":"Actions b","status":"identified"},{"name":"Actions c","status":"monitoring"},` +
				`{"name":"Actions d","status":"resolved"},{"name":"Actions e","status":"postmortem"},{"name":"Actions f","status":"Resolved"}`,
			[]string{"Actions a|investigating|", "Actions b|identified|", "Actions c|monitoring|"}},
		{"no status: resolved_at decides",
			`{"name":"Actions a","status":"","resolved_at":"2026-10-01T00:00:00Z"},{"name":"Actions b","resolved_at":null},{"name":"Actions c"}`,
			[]string{"Actions b||", "Actions c||"}},
		{"an open status outranks a resolved_at", `{"name":"Actions a","status":"investigating","resolved_at":"2026-10-01T00:00:00Z"}`,
			[]string{"Actions a|investigating|"}},
		{"the newest update's display time, not the incident's updated_at",
			`{"name":"Actions a","status":"investigating","updated_at":"2026-10-09T21:52:10.203Z","incident_updates":[` +
				`{"display_at":"2026-10-01T14:47:48.228Z","created_at":"2026-10-01T14:47:48.228Z"},` +
				`{"display_at":"2026-10-01T15:10:00.000Z","created_at":"2026-10-01T15:12:00.000Z"},` +
				`{"display_at":"","created_at":"2026-10-01T15:02:00.000Z"}]}`,
			[]string{"Actions a|investigating|2026-10-01T15:10:00Z"}},
		{"an update with no display time: its created_at",
			`{"name":"Actions a","status":"investigating","incident_updates":[{"created_at":"2026-10-01T15:02:00-06:00"}]}`,
			[]string{"Actions a|investigating|2026-10-01T21:02:00Z"}},
		{"no updates: the incident's updated_at", `{"name":"Actions a","status":"investigating","updated_at":"2026-10-09T21:52:10Z"}`,
			[]string{"Actions a|investigating|2026-10-09T21:52:10Z"}},
		{"a time that does not parse is kept as text", `{"name":"Actions a","status":"investigating","updated_at":"yesterday-ish"}`,
			[]string{"Actions a|investigating|yesterday-ish"}},
		{"named through a component or an update", `{"name":"Several services","status":"investigating","components":[{"name":"Actions"}]},` +
			`{"name":"Other services","status":"investigating","incident_updates":[{"affected_components":[{"name":"GitHub Actions"}]}]},` +
			`{"name":"Unrelated","status":"investigating","components":[{"name":"Pages"}],"incident_updates":[{"affected_components":[{"name":"Packages"}]}]}`,
			[]string{"Several services|investigating|", "Other services|investigating|"}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			s := DecideActionsStatus([]byte(`{`+comp+`,"incidents":[`+tc.incidents+`]}`), nil)
			var got []string
			for _, in := range s.Incidents {
				when := in.UpdatedText
				if !in.Updated.IsZero() {
					when = in.Updated.UTC().Format(time.RFC3339)
				}
				got = append(got, in.Name+"|"+in.Status+"|"+when)
			}
			if !reflect.DeepEqual(got, tc.want) {
				t.Errorf("incidents = %q, want %q", got, tc.want)
			}
			want := ActionsOperational
			if len(tc.want) > 0 {
				want = ActionsDegraded
			}
			if s.Health != want {
				t.Errorf("Health = %q, want %q", s.Health, want)
			}
		})
	}
}

func TestNamesActions(t *testing.T) {
	for _, c := range []struct {
		s    string
		want bool
	}{
		{"Actions", true},
		{"Incident with Actions", true},
		{"Actions Job Delays", true},
		{"[Retroactive] Actions workflow run failures after deployment", true},
		{"Incident with Git Operations, Pull Requests and Actions", true},
		{"GitHub Actions-hosted runners", true},
		{"ACTIONS", true},
		{"some actions fail", true},
		{"Transactions are delayed", false},
		{"Action required", false},
		{"Actionsfoo", false},
		{"Actions_beta", false},
		{"Disruption with some GitHub services", false},
		{"", false},
		{"résactions", false},
	} {
		if got := namesActions(c.s); got != c.want {
			t.Errorf("namesActions(%q) = %v, want %v", c.s, got, c.want)
		}
	}
}

func TestGitHubStatusCovers(t *testing.T) {
	for _, c := range []struct {
		host string
		want bool
	}{
		{"github.com", true},
		{"GitHub.com", true},
		{" github.com ", true},
		{"github.com.", true},
		{"github.com:443", true},
		{"www.github.com", true},
		{"ssh.github.com", true},
		{"ghe.example.com", false},
		{"octocorp.ghe.com", false},
		{"github.example.com", false},
		{"github.com.evil.example", false},
		{"notgithub.com", false},
		{"github-work", false}, // an ssh alias: the PR's URL, not the remote, is what names github.com
		{"", false},
	} {
		if got := GitHubStatusCovers(c.host); got != c.want {
			t.Errorf("GitHubStatusCovers(%q) = %v, want %v", c.host, got, c.want)
		}
	}
}

// The fixtures stay the page's shape: every one has the summary's lists.
func TestActionsStatusFixturesHaveThePagesShape(t *testing.T) {
	files, err := filepath.Glob(filepath.Join("testdata", "githubstatus", "*.json"))
	if err != nil || len(files) < 10 {
		t.Fatalf("fixtures: %v, %v", files, err)
	}
	for _, f := range files {
		b, err := os.ReadFile(f)
		if err != nil {
			t.Fatal(err)
		}
		for _, key := range []string{`"page"`, `"components"`, `"incidents"`, `"scheduled_maintenances"`, `"status"`} {
			if !strings.Contains(string(b), key) {
				t.Errorf("%s has no %s", f, key)
			}
		}
	}
}
