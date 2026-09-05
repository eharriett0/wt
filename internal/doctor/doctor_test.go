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

// #138: severity for a base-tracking branch turns on push.default, NOT
// merge_is_deploy. Under simple (git's default) a bare push refuses on the name
// mismatch, so the old hard ✗ over-claimed — it must be info, not fail. Only
// upstream/tracking aim a bare push at the base (and the pre-push guard still
// blocks even that), so those warn.
func TestClassifyUpstream(t *testing.T) {
	for _, tc := range []struct {
		name         string
		mergeRef     string
		base         string
		pushDefault  string
		wantIssue    string
		wantSeverity string
	}{
		{"no upstream", "", "main", "", "no_upstream", "info"},
		{"no upstream, push.default=upstream", "", "main", "upstream", "no_upstream", "info"},
		{"tracks own branch — fine", "refs/heads/feat/x", "main", "", "", ""},
		{"tracks a different base name — fine", "refs/heads/develop", "main", "upstream", "", ""},

		// THE #138 FIX: default config (simple/unset) is INFO, never fail.
		{"tracks base, push.default unset (simple) — INFO not fail", "refs/heads/main", "main", "", "tracks_base", "info"},
		{"tracks base, push.default=simple — INFO", "refs/heads/main", "main", "simple", "tracks_base", "info"},
		{"tracks base, push.default=current — INFO", "refs/heads/main", "main", "current", "tracks_base", "info"},
		{"tracks base, push.default=nothing — INFO", "refs/heads/main", "main", "nothing", "tracks_base", "info"},

		// Only upstream/tracking aim a bare push at the base → warn (guard still blocks).
		{"tracks base, push.default=upstream — warn", "refs/heads/main", "main", "upstream", "tracks_base", "warn"},
		{"tracks base, push.default=tracking (alias) — warn", "refs/heads/main", "main", "tracking", "tracks_base", "warn"},
		{"tracks base by bare merge name, upstream — warn", "main", "main", "upstream", "tracks_base", "warn"},
		{"non-main base, upstream — warn", "refs/heads/develop", "develop", "upstream", "tracks_base", "warn"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			issue, sev := classifyUpstream(tc.mergeRef, tc.base, tc.pushDefault)
			if issue != tc.wantIssue || sev != tc.wantSeverity {
				t.Errorf("classifyUpstream(%q,%q,%q) = (%q,%q), want (%q,%q)",
					tc.mergeRef, tc.base, tc.pushDefault, issue, sev, tc.wantIssue, tc.wantSeverity)
			}
		})
	}
}

// #138: only upstream/tracking make a bare `git push` follow the branch's
// upstream ref (→ the base); every other value keys off the branch name.
func TestPushDefaultFollowsUpstream(t *testing.T) {
	for _, v := range []string{"upstream", "tracking", " upstream "} {
		if !pushDefaultFollowsUpstream(v) {
			t.Errorf("pushDefaultFollowsUpstream(%q) = false, want true", v)
		}
	}
	for _, v := range []string{"", "simple", "current", "matching", "nothing", "garbage"} {
		if pushDefaultFollowsUpstream(v) {
			t.Errorf("pushDefaultFollowsUpstream(%q) = true, want false (bare push keys off the branch name)", v)
		}
	}
}
