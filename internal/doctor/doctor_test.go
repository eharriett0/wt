package doctor

import "testing"

func TestClassifyUpstream(t *testing.T) {
	for _, tc := range []struct {
		name         string
		mergeRef     string
		base         string
		deploy       bool
		wantIssue    string
		wantSeverity string
	}{
		{"no upstream", "", "main", false, "no_upstream", "info"},
		{"no upstream, deploy repo", "", "main", true, "no_upstream", "info"},
		{"tracks own branch — fine", "refs/heads/feat/x", "main", false, "", ""},
		{"tracks base — warn (normal repo)", "refs/heads/main", "main", false, "tracks_base", "warn"},
		{"tracks base — FAIL (merge_is_deploy)", "refs/heads/main", "main", true, "tracks_base", "fail"},
		{"tracks base by bare name too", "main", "main", true, "tracks_base", "fail"},
		{"non-main base", "refs/heads/develop", "develop", true, "tracks_base", "fail"},
		{"tracks a different base name — fine", "refs/heads/develop", "main", true, "", ""},
	} {
		t.Run(tc.name, func(t *testing.T) {
			issue, sev := classifyUpstream(tc.mergeRef, tc.base, tc.deploy)
			if issue != tc.wantIssue || sev != tc.wantSeverity {
				t.Errorf("classifyUpstream(%q,%q,%v) = (%q,%q), want (%q,%q)",
					tc.mergeRef, tc.base, tc.deploy, issue, sev, tc.wantIssue, tc.wantSeverity)
			}
		})
	}
}
