package doctor

import (
	"testing"

	"github.com/eharriett0/wt/internal/collide"
)

func TestClassifyStaleCheckout(t *testing.T) {
	for _, tc := range []struct {
		name    string
		behind  int
		wantSev string
	}{
		{"at threshold → warn", collide.StaleBaseBehindThreshold, "warn"},
		{"far past threshold → warn", 144, "warn"},
		{"just below threshold → nothing", collide.StaleBaseBehindThreshold - 1, ""},
		{"current → nothing", 0, ""},
		{"uncomputable (-1) → nothing (git error never manufactures a warning)", -1, ""},
	} {
		t.Run(tc.name, func(t *testing.T) {
			_, sev := classifyStaleCheckout(tc.behind)
			if sev != tc.wantSev {
				t.Errorf("classifyStaleCheckout(%d) severity = %q, want %q", tc.behind, sev, tc.wantSev)
			}
		})
	}
}

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
