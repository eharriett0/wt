package ghx

import (
	"strings"
	"testing"
	"time"
)

// TestPRCreateArgs pins the `wt claim` draft-PR regression: PR creation must
// pass --head AND --base explicitly so it does not depend on the invoking
// cwd's current branch. `wt claim` runs from the main checkout (on the base
// branch); without --head, gh infers head=base → "no commits between base and
// base" → the draft PR silently never gets created (the bug this guards).
func TestPRCreateArgs(t *testing.T) {
	draft := strings.Join(PRCreateArgs(true, "feat-42-slug", "main", "WIP: #42", "body"), " ")
	for _, want := range []string{
		"pr create", "--head feat-42-slug", "--base main", "--draft", "--title WIP: #42", "--body body",
	} {
		if !strings.Contains(draft, want) {
			t.Errorf("PRCreateArgs(draft) missing %q\n got: %s", want, draft)
		}
	}

	// Non-draft must omit --draft (but still carry --head/--base).
	nd := strings.Join(PRCreateArgs(false, "b", "trunk", "t", "y"), " ")
	if strings.Contains(nd, "--draft") {
		t.Errorf("non-draft must omit --draft: %s", nd)
	}
	if !strings.Contains(nd, "--head b") || !strings.Contains(nd, "--base trunk") {
		t.Errorf("non-draft must still carry --head/--base: %s", nd)
	}
}

// TestHostFromRemoteURL pins the parsing behind the host-scoped auth check
// (#100). The scp-style form is the one worth care: it has no "://" and is
// distinguished from a local path only by a colon appearing before any slash.
func TestHostFromRemoteURL(t *testing.T) {
	cases := []struct{ url, want string }{
		{"git@github.com:owner/repo.git", "github.com"},
		{"https://github.com/owner/repo.git", "github.com"},
		{"http://github.com/owner/repo", "github.com"},
		{"ssh://git@ghe.example.com:2222/owner/repo.git", "ghe.example.com"},
		{"ssh://ghe.example.com/owner/repo.git", "ghe.example.com"},
		{"https://user:token@ghe.example.com/o/r", "ghe.example.com"},
		{"git@ghe.example.com:o/r", "ghe.example.com"},
		{"https://github.com", "github.com"},
		{"  git@github.com:o/r.git  ", "github.com"},

		// No host to scope to — the caller must fall back to gh's own default
		// rather than guess github.com, or it would ask about the wrong forge.
		{"/srv/git/repo.git", ""},
		{"../sibling-repo", ""},
		{"./repo", ""},
		{"", ""},
	}
	for _, c := range cases {
		if got := hostFromRemoteURL(c.url); got != c.want {
			t.Errorf("hostFromRemoteURL(%q) = %q, want %q", c.url, got, c.want)
		}
	}
}

// TestAuthStatusArgs is the actual #100 regression. Bare `gh auth status` exits
// non-zero when ANY configured host fails, so an unrelated unreachable host made
// `wt doctor` report "gh — found but NOT authenticated" on a machine whose
// github.com login was fine. The fix is that a known host MUST be passed through
// as --hostname; without it the check asks a broader question than it needs
// answered and cannot distinguish "you are broken" from "the check is broken".
func TestAuthStatusArgs(t *testing.T) {
	scoped := strings.Join(authStatusArgs("github.com"), " ")
	if scoped != "auth status --hostname github.com" {
		t.Errorf("host must be scoped: got %q", scoped)
	}

	// Unknown host: fall back to gh's default rather than inventing one. This is
	// the pre-#100 behaviour, deliberately retained ONLY for the can't-tell case.
	unscoped := strings.Join(authStatusArgs(""), " ")
	if unscoped != "auth status" {
		t.Errorf("unknown host must not invent a --hostname: got %q", unscoped)
	}

	// An enterprise host scopes to itself — the mirror-image false NEGATIVE the
	// old code had, where such a repo passed because github.com happened to be
	// authed while the host it actually needs was not.
	ghe := strings.Join(authStatusArgs("ghe.example.com"), " ")
	if !strings.Contains(ghe, "--hostname ghe.example.com") {
		t.Errorf("enterprise host must scope to itself: got %q", ghe)
	}
}

// #172: an AuthedFor answer is reused for authTTL, so one command runs gh once
// per host instead of once per PR lookup (a bare check took about 6 s). The bound
// is what lets a long-lived `wt mcp` notice a later `gh auth login`.
func TestAuthFresh(t *testing.T) {
	now := time.Date(2026, 9, 30, 12, 0, 0, 0, time.UTC)
	cases := []struct {
		name string
		at   time.Time
		want bool
	}{
		{"never asked", time.Time{}, false},
		{"just asked", now, true},
		{"within the bound", now.Add(-authTTL + time.Second), true},
		{"at the bound", now.Add(-authTTL), false},
		{"past the bound", now.Add(-2 * authTTL), false},
		{"in the future (a clock step)", now.Add(time.Second), false},
	}
	for _, c := range cases {
		if got := authFresh(c.at, now); got != c.want {
			t.Errorf("%s: authFresh = %v, want %v", c.name, got, c.want)
		}
	}
}

// #134: parseCrossRefPRs turns the timeline cross-ref jq's TSV into refs. The
// jq already filters to OPEN PullRequest sources; this layer must dedup by PR
// number (one PR can raise several cross-reference events on the same issue),
// parse the draft flag, drop blank/short lines, and skip non-numeric ids.
func TestParseCrossRefPRs(t *testing.T) {
	out := strings.Join([]string{
		"42\tfeat-42-thing\tfalse\thttps://x/pull/42",
		"42\tfeat-42-thing\tfalse\thttps://x/pull/42", // dup event, same PR → collapse
		"7\tfix-7\ttrue\thttps://x/pull/7",            // draft
		"",                                            // blank
		"notanum\tbad\tfalse\thttp://x",               // non-numeric id → skip
		"9\tshort",                                    // too few fields → skip
	}, "\n")

	got := parseCrossRefPRs(out)
	if len(got) != 2 {
		t.Fatalf("parseCrossRefPRs returned %d refs, want 2 (deduped): %+v", len(got), got)
	}
	if got[0].Number != 42 || got[0].HeadRefName != "feat-42-thing" || got[0].IsDraft {
		t.Errorf("ref[0] = %+v, want {42 feat-42-thing draft=false}", got[0])
	}
	if got[1].Number != 7 || !got[1].IsDraft {
		t.Errorf("ref[1] = %+v, want {7 draft=true}", got[1])
	}
	if got[0].URL != "https://x/pull/42" {
		t.Errorf("ref[0].URL = %q, want the PR url", got[0].URL)
	}
	if r := parseCrossRefPRs(""); r != nil {
		t.Errorf("empty input → %v, want nil (no PR references)", r)
	}
}

// TestParsePRForBranch pins the fix the #168 e2e smoke found: for a branch with NO
// PR the old query printed "null null", which parsed as a PR with state "null".
// Callers matched no state, so it read as "no PR" by accident, and the tip
// fallback, which asks exactly "was there a PR?", never ran.
func TestParsePRForBranch(t *testing.T) {
	cases := []struct {
		out, n, st string
		ok         bool
	}{
		{"1128 MERGED", "1128", "MERGED", true},
		{"12 OPEN\n", "12", "OPEN", true},
		{"7 CLOSED", "7", "CLOSED", true},
		{"null null", "", "", false}, // the pre-#168 no-PR output
		{"", "", "", false},          // the no-PR output now
		{"x OPEN", "", "", false},
		{"12 DRAFT", "", "", false},
		{"12", "", "", false},
	}
	for _, c := range cases {
		n, st, ok := parsePRForBranch(c.out)
		if n != c.n || st != c.st || ok != c.ok {
			t.Errorf("parsePRForBranch(%q) = (%q, %q, %v), want (%q, %q, %v)", c.out, n, st, ok, c.n, c.st, c.ok)
		}
	}
}

// TestParseCommitPRs pins the parse of PRForTip's --jq output (#168): one
// `<number> <state> <merged>` line per PR, where state is the REST API's lower-case
// "open"/"closed" and merged is whether merged_at is set.
// #167: `wt adopt <pr#>` compares a local branch to the PR's headRefOid, so the
// head read must refuse anything that is not a branch plus a full object id.
// "null null" is the shape a pre-#168 style `.headRefName` query would print for
// a missing field; it must never parse as a PR head.
func TestParsePRHead(t *testing.T) {
	sha1 := "9f8e7d6c5b4a39281706f5e4d3c2b1a098765432"
	sha256 := "9f8e7d6c5b4a39281706f5e4d3c2b1a0987654329f8e7d6c5b4a39281706f5e4"
	cases := []struct {
		out, branch, oid string
		ok               bool
	}{
		{"bot/image " + sha1, "bot/image", sha1, true},
		{"bot/image " + sha1 + "\n", "bot/image", sha1, true},
		{"feat-42-x " + strings.ToUpper(sha1), "feat-42-x", sha1, true}, // normalised to lower case
		{"bot/image " + sha256, "bot/image", sha256, true},              // SHA-256 repo
		{"", "", "", false},                             // the `// empty` output when a field is missing
		{"null null", "", "", false},                    // the placeholder shape: never a PR head
		{"bot/image null", "", "", false},               // branch without a commit
		{"bot/image", "", "", false},                    // one field
		{"bot/image 9f8e7d6c", "", "", false},           // abbreviated id: not comparable to a local tip
		{"bot/image " + sha1[:39] + "g", "", "", false}, // not hex
		{"a b " + sha1, "", "", false},                  // three fields
	}
	for _, c := range cases {
		b, o, ok := parsePRHead(c.out)
		if b != c.branch || o != c.oid || ok != c.ok {
			t.Errorf("parsePRHead(%q) = (%q, %q, %v), want (%q, %q, %v)", c.out, b, o, ok, c.branch, c.oid, c.ok)
		}
	}
}

func TestParseCommitPRs(t *testing.T) {
	got := parseCommitPRs(strings.Join([]string{
		"1128 closed true", // squash-merged: REST state is "closed"
		"1125 closed false",
		"1130 open false",
		"",
		"x open false", // non-numeric number → skip
		"7 open",       // too few fields → skip
	}, "\n"))
	want := []CommitPR{
		{Number: "1128", Merged: true},
		{Number: "1125"},
		{Number: "1130", Open: true},
	}
	if len(got) != len(want) {
		t.Fatalf("parseCommitPRs = %+v, want %+v", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Errorf("pr[%d] = %+v, want %+v", i, got[i], want[i])
		}
	}
}

// TestChooseTipPR pins which PR speaks for a branch found only by its tip commit
// (#168). The two rows that must NOT count are the load-bearing ones: a merged PR
// that no longer holds the tip, and a closed-unmerged PR. Either would otherwise
// mark unshipped work as shipped, and `wt clean` reaps a shipped worktree.
func TestChooseTipPR(t *testing.T) {
	cases := []struct {
		name   string
		prs    []CommitPR
		n, st  string
		wantOK bool
	}{
		{"no PRs", nil, "", "", false},
		{"merged and holds the tip → shipped", []CommitPR{{Number: "1128", Merged: true, HasTip: true}}, "1128", "MERGED", true},
		{"merged but the tip was force-pushed away → nothing", []CommitPR{{Number: "1128", Merged: true}}, "", "", false},
		{"closed, never merged → nothing", []CommitPR{{Number: "1125"}}, "", "", false},
		{"open wins over merged: the work is live under another name",
			[]CommitPR{{Number: "1128", Merged: true, HasTip: true}, {Number: "1130", Open: true}}, "1130", "OPEN", true},
		{"highest merged number wins",
			[]CommitPR{{Number: "90", Merged: true, HasTip: true}, {Number: "1128", Merged: true, HasTip: true}}, "1128", "MERGED", true},
		{"a merged PR without the tip does not outrank one with it",
			[]CommitPR{{Number: "2000", Merged: true}, {Number: "1128", Merged: true, HasTip: true}}, "1128", "MERGED", true},
	}
	for _, c := range cases {
		n, st, ok := ChooseTipPR(c.prs)
		if n != c.n || st != c.st || ok != c.wantOK {
			t.Errorf("%s: ChooseTipPR = (%q, %q, %v), want (%q, %q, %v)", c.name, n, st, ok, c.n, c.st, c.wantOK)
		}
	}
}

// TestTipLookupApplies pins when the #168 tip lookup runs. The on-base row is the
// one that protects data: a tip already on base belongs to some merged PR on a
// merge-commit or rebase repo, so looking it up would call a fresh worktree shipped.
func TestTipLookupApplies(t *testing.T) {
	cases := []struct {
		name                  string
		prFound               bool
		tip                   string
		onBase, ancestryKnown bool
		want                  bool
	}{
		{"no PR by name, tip off base → look it up", false, "abc", false, true, true},
		{"a PR by name already decided", true, "abc", false, true, false},
		{"no tip (no such branch)", false, "", false, true, false},
		{"tip is ON base → never (fresh worktree, #61)", false, "abc", true, true, false},
		{"ancestry unknown → never (fail toward the old behaviour)", false, "abc", false, false, false},
	}
	for _, c := range cases {
		if got := TipLookupApplies(c.prFound, c.tip, c.onBase, c.ancestryKnown); got != c.want {
			t.Errorf("%s: TipLookupApplies = %v, want %v", c.name, got, c.want)
		}
	}
}
