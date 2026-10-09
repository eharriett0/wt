package ghx

import (
	"reflect"
	"testing"
)

func TestParsePRCommits(t *testing.T) {
	ok := []struct {
		name string
		out  string
		want []PRCommit
	}{
		{"one commit", `{"message":"feat: one","parents":1}`, []PRCommit{{"feat: one", 1}}},
		{"a multi-line message, gh's HTML escapes, a merge", `{"message":"Fix #6 flake\n\nbody <pr>","parents":1}
{"message":"Merge remote-tracking branch 'origin/main'","parents":2}`,
			[]PRCommit{{"Fix #6 flake\n\nbody <pr>", 1}, {"Merge remote-tracking branch 'origin/main'", 2}}},
		{"keys in either order, pages run together", `{"parents":1,"message":"a"}{"message":"b","parents":1}`,
			[]PRCommit{{"a", 1}, {"b", 1}}},
		{"an empty message is a message", `{"message":"","parents":1}`, []PRCommit{{"", 1}}},
	}
	for _, tc := range ok {
		got, err := parsePRCommits(tc.out)
		if err != nil || !reflect.DeepEqual(got, tc.want) {
			t.Errorf("%s: parsePRCommits = %+v (err %v), want %+v", tc.name, got, err, tc.want)
		}
	}
	// Never a parsed placeholder (#168): a PR always has a commit, so nothing
	// readable is "unread", and a null or partial object is not a commit.
	for name, out := range map[string]string{
		"empty output":          "",
		"null":                  "null",
		"no parents":            `{"message":"a"}`,
		"no message":            `{"parents":1}`,
		"a null message":        `{"message":null,"parents":1}`,
		"not JSON":              "feat: one",
		"a jq error message":    `jq: error (at <stdin>:0): Cannot iterate over null`,
		"a good one, then junk": `{"message":"a","parents":1}` + "\nnull",
	} {
		if got, err := parsePRCommits(out); err == nil {
			t.Errorf("%s: parsePRCommits(%q) = %+v, want an error", name, out, got)
		}
	}
}

func TestParseSquashSettings(t *testing.T) {
	cases := []struct {
		out            string
		title, message string
	}{
		{`{"message":"COMMIT_MESSAGES","title":"COMMIT_OR_PR_TITLE"}`, "COMMIT_OR_PR_TITLE", "COMMIT_MESSAGES"},
		{`{"title":"PR_TITLE","message":"BLANK"}`, "PR_TITLE", "BLANK"},
		{`{"title":null,"message":null}`, "", ""}, // REST's answer to a non-admin; GraphQL's when the field is hidden
		{`{"title":"PR_TITLE"}`, "PR_TITLE", ""},
		{`{"message":"PR_BODY"}`, "", "PR_BODY"},
		{"", "", ""},
		{"null", "", ""},
		{"PR_TITLE BLANK", "", ""},
		// passed through: whether it is a value GitHub documents is the model's call
		{`{"title":"SOMETHING_NEW","message":"PR_BODY"}`, "SOMETHING_NEW", "PR_BODY"},
	}
	for _, c := range cases {
		title, message := parseSquashSettings(c.out)
		if title != c.title || message != c.message {
			t.Errorf("parseSquashSettings(%q) = (%q, %q), want (%q, %q)", c.out, title, message, c.title, c.message)
		}
	}
}
