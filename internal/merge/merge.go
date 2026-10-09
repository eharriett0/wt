// Package merge implements the guarded squash-merge — the documented merge
// ritual ported from awesome-o's scripts/merge-pr.sh (#1344). It refuses to
// merge a PR whose diff vs base is EMPTY, or whose commits are ALL
// "WIP: claim #" placeholders (the claim-work placeholder bug class that
// merged vacuous PRs with zero real content).
package merge

import (
	"errors"
	"fmt"
	"io"
	"os"
	"regexp"
	"sort"
	"strconv"
	"strings"

	"github.com/eharriett0/wt/internal/ghx"
)

// Verdict is the outcome of the pure merge guard.
type Verdict string

const (
	VerdictOK              Verdict = "ok"
	VerdictEmptyDiff       Verdict = "block:empty_diff"
	VerdictPlaceholderOnly Verdict = "block:placeholder_only"
)

// GuardVerdict is the pure decision function (testable without gh).
//
//	fileCount — number of files changed in the PR diff vs base, as a string
//	            (matches the bash $1; unparseable or "0" → empty_diff).
//	subjects  — commit subject lines (one messageHeadline per commit).
//
// The empty-diff check is the load-bearing one; the placeholder-only check is
// belt-and-braces for the case where a non-empty diff somehow pairs with
// commits that are all claim-work placeholders.
func GuardVerdict(fileCount string, subjects []string) Verdict {
	n, err := strconv.Atoi(strings.TrimSpace(fileCount))
	if err != nil || n <= 0 {
		return VerdictEmptyDiff
	}

	hasAny, hasReal := false, false
	for _, line := range subjects {
		line = strings.TrimSpace(line)
		if line == "" {
			continue
		}
		hasAny = true
		if !strings.HasPrefix(line, "WIP: claim #") {
			hasReal = true
		}
	}
	if hasAny && !hasReal {
		return VerdictPlaceholderOnly
	}
	return VerdictOK
}

// BranchIsForeign reports whether head is NOT one of the wt-managed worktree
// branches (worktreeBranches) — i.e. the PR would merge a branch with no local
// wt lane here. This is the "merged the wrong lane" class from wt#15. Fail-open:
// an empty head OR an empty/unknown worktree set returns false (don't block —
// the surfaced head-branch line is the safety net when we can't be sure).
func BranchIsForeign(head string, worktreeBranches []string) bool {
	head = strings.TrimSpace(head)
	if head == "" || len(worktreeBranches) == 0 {
		return false
	}
	for _, b := range worktreeBranches {
		if strings.TrimSpace(b) == head {
			return false
		}
	}
	return true
}

// WithAdmin returns extraArgs with "--admin" in front of them when admin is set,
// so the squash forwards --admin to `gh pr merge` — the maintainer bypass for a
// branch whose protection REQUIRES a PR review (own low-risk CI-green PRs on a
// required-review repo; wt#20). This bypasses GitHub branch protection, NOT
// wt's own safety checks: the CLI runs the merge_is_deploy deploy-gate + the
// empty-diff/placeholder + foreign-branch guards BEFORE the merge, so the value
// of wt merge-pr (the deploy gate the raw `gh` fallback loses) is preserved.
//
// ⚠ wt's own gh flags go IN FRONT of the operator's passthrough (#180). When
// they were appended, a passthrough ending in a value flag (`-- --subject`)
// took "--admin" as that flag's value: gh merged with the subject "--admin" and
// no admin. In front, a dangling flag fails inside gh instead. There is no
// dedupe: gh's parser takes a repeated bool fine, while the old token match
// read a VALUE (`-b --admin` is the body "--admin") as the flag and dropped it.
func WithAdmin(admin bool, extraArgs []string) []string {
	if !admin {
		return extraArgs
	}
	return append([]string{"--admin"}, extraArgs...)
}

// WithMatchHead returns args with `--match-head-commit head` in front of them
// (#179), so gh merges only the commit whose checks the checks gate read: a
// push after the read makes GitHub refuse the merge ("Head branch was
// modified"), and merge-pr exits 1 instead of shipping a head nothing checked.
// In front for the same reason as WithAdmin (#180); an operator's own
// forwarded --match-head-commit comes later and wins (gh keeps the last). ""
// (the checks were not read) leaves args alone. Pure; args is not mutated.
func WithMatchHead(head string, args []string) []string {
	if head == "" {
		return args
	}
	return append([]string{"--match-head-commit", head}, args...)
}

// WithSubject returns args with `--subject subject` in front of them, for the
// WIP strip (#38). In front for the same reason as WithAdmin (#180), and so an
// operator's own forwarded --subject still wins: gh's parser keeps the last
// one. Pure; args is not mutated.
func WithSubject(subject string, args []string) []string {
	return append([]string{"--subject", subject}, args...)
}

// BodySource is where `gh pr merge` takes the squash commit BODY from (#180).
type BodySource int

const (
	BodyDefault BodySource = iota // no body flag: GitHub composes it from the commits
	BodyText                      // -b / --body <text>
	BodyFile                      // -F / --body-file <path>
	BodyStdin                     // -F - / --body-file -
)

// ForwardedBody is the squash-body override in a merge-pr passthrough (#180).
type ForwardedBody struct {
	Source BodySource
	Value  string // BodyText: the text. BodyFile: the path. BodyStdin: "-".
	Flag   string // the flag as written ("-F", "--body-file", "-b", …), for messages

	// fileAt locates the value of the --body-file occurrence gh will use, so
	// RedirectToStdin can re-point it.
	fileAt argPos
}

// argPos is where a flag's value sits in argv: args[index], after prefix.
// prefix is "" when the value is its own token (`-F path`), else the part of the
// token before it (`--body-file=` / `-F=` / `-F` / `-dF` for `-dFpath`).
type argPos struct {
	index  int
	prefix string
}

// Describe renders the source for a message: `--body-file body.txt`, `-F -
// (stdin)`, `--body`.
func (b ForwardedBody) Describe() string {
	switch b.Source {
	case BodyFile:
		return b.Flag + " " + b.Value
	case BodyStdin:
		return b.Flag + " - (stdin)"
	case BodyText:
		return b.Flag
	}
	return "the commit messages"
}

// The `gh pr merge` flags that take a value (gh v2.68.1, including the
// inherited -R/--repo). Every other flag it accepts is a bool. Only needed to
// know which tokens are values, so `--subject -b` is not misread as a body flag.
var (
	ghMergeValueLong = map[string]bool{
		"author-email": true, "body": true, "body-file": true,
		"match-head-commit": true, "repo": true, "subject": true,
	}
	ghMergeValueShort = map[byte]string{
		'A': "author-email", 'b': "body", 'F': "body-file", 'R': "repo", 't': "subject",
	}
	// ghMergeBoolShort are its bool shorthands, by long name (#179: -h asks for
	// help, which merges nothing).
	ghMergeBoolShort = map[byte]string{
		'd': "delete-branch", 'h': "help", 'm': "merge", 'r': "rebase", 's': "squash",
	}
)

// flagOcc is the occurrence of a `gh pr merge` value flag that gh uses: the
// last one.
type flagOcc struct {
	value string
	flag  string // as written: "-F", "--body-file", "-t", …
	at    argPos // where the value sits, for RedirectToStdin
}

// ParseForwardedBody reports which squash body the args forwarded after
// `wt merge-pr <pr> --` make `gh pr merge` use. Pure.
//
// It reads them the way gh's parser (pflag v1.0.6) does (scanMergeFlags),
// because the close check has to judge the body that will actually SHIP, not
// the one a simpler reading finds:
//
//   - `--body x`, `--body=x`, `-b x`, `-bx`, `-b=x`; the same for `--body-file`/`-F`.
//     Shorthands cluster (`-dF path`: -d is a bool), and a value flag ends the
//     cluster (`-Fd` is the file "d").
//   - A value flag takes the next token even when it starts with `-`, so the `-b`
//     in `--subject -b` is a subject, not a body flag.
//   - A repeated flag: the last one wins.
//   - `--body` counts as set whenever it appears, even empty (`--body=` ships an
//     empty body), but `--body-file` only when its final value is non-empty —
//     gh's own test is `bodyFile != ""`.
//   - A standalone `--` ends the flags.
//
// Errors where gh would not merge at all, so no body can be named: gh refuses
// `--body` together with `--body-file`; and a value flag (a body flag or any
// other) at the very end of the passthrough has no value. wt's own flags go in
// front of the passthrough (WithAdmin, WithSubject), so nothing follows it and
// gh fails with "flag needs an argument" — said here, before a body is read.
func ParseForwardedBody(args []string) (ForwardedBody, error) {
	last, _, dangling := scanMergeFlags(args)
	if dangling != "" {
		return ForwardedBody{}, danglingFlagError(dangling)
	}
	body, bodySet := last["body"]
	file := last["body-file"]
	fileSet := file.value != ""
	switch {
	case bodySet && fileSet:
		return ForwardedBody{}, fmt.Errorf("both %s and %s are forwarded, and gh pr merge refuses that (specify only one of --body or --body-file)", body.flag, file.flag)
	case fileSet && file.value == "-":
		return ForwardedBody{Source: BodyStdin, Value: file.value, Flag: file.flag, fileAt: file.at}, nil
	case fileSet:
		return ForwardedBody{Source: BodyFile, Value: file.value, Flag: file.flag, fileAt: file.at}, nil
	case bodySet:
		return ForwardedBody{Source: BodyText, Value: body.value, Flag: body.flag}, nil
	}
	return ForwardedBody{}, nil
}

// ForwardedSubject is the squash SUBJECT in a merge-pr passthrough (#196).
type ForwardedSubject struct {
	Given bool   // a --subject/-t is there; gh keeps the last one
	Value string // that one's value
	Flag  string // as written, for messages
}

// ParseForwardedSubject reports the --subject/-t the args forwarded after
// `wt merge-pr <pr> --` hand `gh pr merge`, read the way gh's parser does
// (scanMergeFlags, the same reading as ParseForwardedBody): the last one wins,
// and a value flag takes the next token whatever it looks like, so the `-t` in
// `-b -t` is a body. Value "" is still Given: gh then sends no subject at all
// (it sets the commit headline only when the value is non-empty), so GitHub
// writes the repo's default. Pure. Errors on a value flag left without a value,
// as ParseForwardedBody does.
func ParseForwardedSubject(args []string) (ForwardedSubject, error) {
	last, _, dangling := scanMergeFlags(args)
	if dangling != "" {
		return ForwardedSubject{}, danglingFlagError(dangling)
	}
	s, given := last["subject"]
	return ForwardedSubject{Given: given, Value: s.value, Flag: s.flag}, nil
}

func danglingFlagError(flag string) error {
	return fmt.Errorf("%s is the last forwarded gh arg and has no value, so gh pr merge would fail on it (flag needs an argument)", flag)
}

// scanMergeFlags reads a `gh pr merge` passthrough the way gh's parser (pflag
// v1.0.6) does and returns the occurrence gh uses (the last) of each value flag,
// by long name; what each bool flag given ends up as, by long name (`--x=false`
// sets it false); and the first value flag left without a value ("" = none).
// Pure.
func scanMergeFlags(args []string) (last map[string]flagOcc, bools map[string]bool, dangling string) {
	last, bools = map[string]flagOcc{}, map[string]bool{}
	take := func(name, flag, value string, at argPos) {
		last[name] = flagOcc{value: value, flag: flag, at: at}
	}
scan:
	for i := 0; i < len(args); i++ {
		s := args[i]
		if len(s) < 2 || s[0] != '-' {
			continue // a positional ("-" alone is one too)
		}
		if s == "--" {
			break // pflag stops reading flags here
		}
		if s[1] == '-' { // --name, --name=value
			name, value, inline := strings.Cut(s[2:], "=")
			if !ghMergeValueLong[name] {
				bools[name] = boolValue(value, inline) // a bool flag, or one gh rejects
				continue
			}
			at := argPos{index: i, prefix: "--" + name + "="}
			if !inline {
				if i+1 >= len(args) {
					dangling = "--" + name
					break scan
				}
				i++
				value, at = args[i], argPos{index: i}
			}
			take(name, "--"+name, value, at)
			continue
		}
		cluster := s[1:] // -x, -xyz, -xVALUE, -x=VALUE
		if strings.HasPrefix(cluster, "test.") {
			continue // pflag skips go-test style flags entirely
		}
		for j := 0; j < len(cluster); j++ {
			eq := len(cluster)-j > 2 && cluster[j+1] == '='
			name, valued := ghMergeValueShort[cluster[j]]
			if !valued {
				if b, ok := ghMergeBoolShort[cluster[j]]; ok {
					value := ""
					if eq {
						value = cluster[j+2:]
					}
					bools[b] = boolValue(value, eq)
				}
				if eq {
					break // `-d=false`: the rest is this bool's value
				}
				continue // a bool (-d -h -m -r -s), or one gh rejects
			}
			flag := "-" + string(cluster[j])
			var value string
			var at argPos
			switch {
			case eq: // -F=path
				value, at = cluster[j+2:], argPos{index: i, prefix: "-" + cluster[:j+2]}
			case len(cluster)-j > 1: // -Fpath (and `-F=` alone is the file "=")
				value, at = cluster[j+1:], argPos{index: i, prefix: "-" + cluster[:j+1]}
			case i+1 < len(args): // -F path
				i++
				value, at = args[i], argPos{index: i}
			default:
				dangling = flag
				break scan
			}
			take(name, flag, value, at)
			break // a value flag ends the cluster
		}
	}
	return last, bools, dangling
}

// boolValue is what a bool flag is set to: true, or its `=value` read the way
// pflag reads it (strconv.ParseBool; anything else is gh's error, and gh then
// merges nothing, so it counts as set). Pure.
func boolValue(value string, given bool) bool {
	if !given {
		return true
	}
	b, err := strconv.ParseBool(value)
	return err != nil || b
}

// ParseForwardedRepo is the -R/--repo the args forwarded after `wt merge-pr
// <pr> --` hand `gh pr merge` (the last one, read as gh reads it), or "" (#179):
// gh then merges that repository's PR, so the checks gate reads that one. Pure.
func ParseForwardedRepo(args []string) string {
	last, _, _ := scanMergeFlags(args)
	return strings.TrimSpace(last["repo"].value)
}

// NonMerging is the forwarded flag that makes `gh pr merge` merge nothing
// (#179): --disable-auto (it only disarms auto-merge) or --help/-h, as gh
// reads the passthrough; "" when gh will merge, queue or arm the PR. The
// checks gate skips such a run: refusing to disarm auto-merge while checks are
// pending blocked the one thing an operator wants then. Pure.
func NonMerging(args []string) string {
	_, bools, _ := scanMergeFlags(args)
	switch {
	case bools["help"]:
		return "--help"
	case bools["disable-auto"]:
		return "--disable-auto"
	}
	return ""
}

// ForwardsAuto reports whether the args forwarded after `wt merge-pr <pr> --`
// ask gh to arm auto-merge (--auto), as gh reads them (#179). Pure.
func ForwardsAuto(args []string) bool {
	_, bools, _ := scanMergeFlags(args)
	return bools["auto"]
}

// RedirectToStdin returns a copy of args (the slice that was parsed) with the
// --body-file value gh will use re-pointed at "-", for a caller that has already
// read the body and hands gh those same bytes on its stdin. A pipe or a process
// substitution (`-F <(…)`) can be read only once, so gh re-reading the path
// would merge an EMPTY body; and this way gh merges exactly the bytes that were
// checked. Unchanged for any other source. Pure; args is not mutated.
func (b ForwardedBody) RedirectToStdin(args []string) []string {
	if (b.Source != BodyFile && b.Source != BodyStdin) || b.fileAt.index >= len(args) {
		return args
	}
	out := append([]string(nil), args...)
	out[b.fileAt.index] = b.fileAt.prefix + "-"
	return out
}

// Run executes the guarded merge for PR number pr. dryRun prints the verdict
// without merging; bypass proceeds past a block verdict (loud warning).
// mergeForeign permits merging a PR whose head branch has no wt worktree here
// (the foreign-branch guard, wt#15). worktreeBranches is the set of wt-managed
// worktree branches for this repo. extraArgs pass through to `gh pr merge`;
// ghStdin is what gh reads as its stdin (nil = wt's own), which carries a
// forwarded body wt already read (#180). checks is the checks gate's verdict
// (#179), which the dry-run line carries beside the guard's, so a dry run the
// checks would refuse never reads as a bare verdict=ok ("" leaves it out). An
// error from gh itself wraps ErrMergeCommand (#196); a guard's refusal does
// not.
func Run(pr string, dryRun, bypass, mergeForeign bool, worktreeBranches []string, extraArgs []string, ghStdin io.Reader, checks string) error {
	fileCount := ghx.PRChangedFileCount(pr)
	subjects := ghx.PRCommitSubjects(pr)
	v := GuardVerdict(fileCount, subjects)

	switch v {
	case VerdictEmptyDiff:
		fmt.Fprintf(os.Stderr, "REFUSING to merge PR #%s — diff vs base is EMPTY (no real content).\n", pr)
		fmt.Fprintln(os.Stderr, "This is the claim-work placeholder bug class: a placeholder PR merging vacuously.")
		fmt.Fprintf(os.Stderr, "Bypass (rare — e.g. a deliberately empty change): wt merge-pr %s --bypass\n", pr)
		if !bypass {
			return fmt.Errorf("empty diff")
		}
		fmt.Fprintln(os.Stderr, "--bypass set — proceeding despite empty diff.")
	case VerdictPlaceholderOnly:
		fmt.Fprintf(os.Stderr, "REFUSING to merge PR #%s — every commit is a 'WIP: claim #' placeholder.\n", pr)
		fmt.Fprintln(os.Stderr, "The real work never landed on the branch.")
		fmt.Fprintf(os.Stderr, "Bypass: wt merge-pr %s --bypass\n", pr)
		if !bypass {
			return fmt.Errorf("placeholder only")
		}
		fmt.Fprintln(os.Stderr, "--bypass set — proceeding.")
	case VerdictOK:
		// fallthrough to merge
	}

	head, _ := ghx.PRHeadBranch(pr)
	head = strings.TrimSpace(head)
	foreign := BranchIsForeign(head, worktreeBranches)

	// Always surface the head branch — the single line that catches a
	// wrong-branch merge before it happens (wt#15).
	label := "PR #" + pr
	if head != "" {
		label = fmt.Sprintf("PR #%s from branch %q", pr, head)
	}

	// dry-run previews (never blocks) — but flags a foreign head so the operator
	// sees the guard would fire on the real merge.
	if dryRun {
		note := ""
		if foreign {
			note = " [FOREIGN: head has no wt worktree here — a real merge needs --merge-foreign]"
		}
		fmt.Printf("merge-pr: %s %s file_count=%s%s (dry-run, not merging)\n", label, VerdictField(v, checks), fileCount, note)
		return nil
	}

	// Foreign-branch guard (wt#15): refuse to merge a PR whose head branch has
	// no wt worktree here — in a multi-window setup this is the "merged the wrong
	// lane" class. --merge-foreign (or --bypass) proceeds. Fail-open via
	// BranchIsForeign when the head / worktree set is unknown.
	if foreign && !mergeForeign && !bypass {
		fmt.Fprintf(os.Stderr, "REFUSING to merge PR #%s — head branch %q has no wt worktree here (foreign branch).\n", pr, head)
		fmt.Fprintln(os.Stderr, "In a multi-window setup this is the 'merged the wrong lane' class (wt#15).")
		fmt.Fprintf(os.Stderr, "If intended: wt merge-pr %s --merge-foreign\n", pr)
		return fmt.Errorf("foreign branch")
	}

	// #38: strip a "WIP:" prefix (the wt-claim placeholder title) from the squash
	// SUBJECT so it doesn't land on base history. gh's --squash defaults the
	// subject to the PR title; --subject overrides it. Best-effort — a failed
	// title lookup just leaves the default behavior. It goes in FRONT of the
	// passthrough (WithSubject, #180), so a --subject the operator forwards wins.
	mergeArgs := extraArgs
	if title, err := ghx.PRTitle(pr); err == nil {
		if stripped, ok := WIPSubject(title); ok {
			mergeArgs = WithSubject(stripped, extraArgs)
			fmt.Fprintf(os.Stderr, "note: stripping 'WIP:' from the squash subject → %q (a --subject forwarded after -- still wins)\n", stripped)
		}
	}

	fmt.Printf("merge-pr: %s — %s changed file(s) — merging (squash).\n", label, fileCount)
	if err := ghx.MergePRSquash(pr, mergeArgs, ghStdin); err != nil {
		return fmt.Errorf("%w: %w", ErrMergeCommand, err)
	}
	return nil
}

// VerdictField is the dry-run line's verdict: `verdict=ok`, with the checks
// gate's `checks=blocked` (green, none, unread, skipped) beside it when it
// read them (#179). Pure.
func VerdictField(v Verdict, checks string) string {
	if checks == "" {
		return fmt.Sprintf("verdict=%s", v)
	}
	return fmt.Sprintf("verdict=%s checks=%s", v, checks)
}

// ErrMergeCommand marks a Run error that `gh pr merge` itself returned, as
// opposed to a guard refusing to run it (#196). gh can fail AFTER merging — `-d`
// merges, then cannot delete a local branch that a wt worktree has checked out —
// so this error alone does not mean "not merged": the caller reads the PR state.
var ErrMergeCommand = errors.New("gh pr merge failed")

// WIPSubject is the squash subject the #38 WIP strip forwards for a PR titled
// title: the title without its "WIP:", when it has one and something follows
// it. Run and the close check both decide through it (#196), so the check
// judges the subject Run sends. Pure.
func WIPSubject(title string) (string, bool) {
	stripped, wasWIP := DeWIPTitle(title)
	return stripped, wasWIP && stripped != ""
}

// PreVerdict is the merge-pr PR-state precheck outcome (#39).
type PreVerdict string

const (
	PreProceed       PreVerdict = "proceed"        // OPEN / unknown → merge as normal
	PreAlreadyMerged PreVerdict = "already_merged" // skip the merge, still run cleanup
	PreClosed        PreVerdict = "closed"         // closed-not-merged → nothing to merge
)

// PreMergeVerdict maps a gh PR state (OPEN / MERGED / CLOSED; "" = unknown) to a
// merge action. Unknown/empty → proceed (fail-open: the merge itself surfaces a
// real error), so a gh hiccup never blocks a legitimate merge. Pure. #39.
func PreMergeVerdict(state string) PreVerdict {
	switch strings.ToUpper(strings.TrimSpace(state)) {
	case "MERGED":
		return PreAlreadyMerged
	case "CLOSED":
		return PreClosed
	default:
		return PreProceed
	}
}

// DeWIPTitle strips a leading "WIP:" — the wt-claim placeholder title prefix
// ("WIP: #N — title") — so it isn't stamped onto shared base history as the
// squash commit subject (#38). Returns (stripped, wasWIP). Pure.
func DeWIPTitle(title string) (string, bool) {
	if rest, ok := strings.CutPrefix(strings.TrimSpace(title), "WIP:"); ok {
		return strings.TrimSpace(rest), true
	}
	return title, false
}

// ClosingRef is one issue that a body of text will AUTO-CLOSE on merge (#77).
type ClosingRef struct {
	Number int    // issue number
	Repo   string // "" = same-repo (#N or same-repo URL); "owner/repo" = cross-repo
	Raw    string // the matched span, for display

	// Suspect names why this close looks UNINTENDED, or "" for an ordinary one
	// (#164). GitHub matches `<keyword> #N` and reads none of the surrounding
	// words, so a sentence written to say a PR does NOT close an issue closes
	// it. That phrasing is never a deliberate close, which is what makes it
	// safe to gate on where gating on every close would not be (#104).
	Suspect string
	// Context is the sentence the match sits in, so the warning can show the
	// phrasing rather than a bare number — a number alone reads as the ordinary
	// case, which is how these get merged past.
	Context string
}

// Suspect reasons.
const (
	SuspectNegated   = "negated"   // "does NOT close #N"
	SuspectQualified = "qualified" // "Closes #N partially"
)

// negationRe matches, in the text BEFORE a close keyword and within the same
// sentence, a word that makes the close non-literal. Deliberately conservative:
// a false positive here blocks a legitimate merge and pushes people toward the
// override, which is the failure #104 exists to prevent. Hypothetical modals
// ("would have closed #N as a false alarm") are included because they describe
// something that did not happen and are never an intended close.
var negationRe = regexp.MustCompile(
	`(?i)\b(?:not|never|neither|nor|without)\b|n't\b|\bno longer\b|` +
		`\brather than\b|\binstead of\b|\b(?:would|could|might)(?:'ve| have)\b`)

// qualifierRe matches a hedge IMMEDIATELY after the reference. "Closes #N
// partially" closes #N completely — the qualifier is prose and the parser never
// sees it.
var qualifierRe = regexp.MustCompile(`(?i)^\W{0,3}\b(?:partially|partly|in part)\b`)

// sentenceAround returns the span of the sentence containing text[start:end],
// bounded by line breaks and by sentence terminators. Pure. Markdown headings
// carry no terminator, so the line break is the load-bearing bound.
func sentenceAround(text string, start, end int) (int, int) {
	lo := strings.LastIndexByte(text[:start], '\n') + 1
	hi := strings.IndexByte(text[end:], '\n')
	if hi < 0 {
		hi = len(text)
	} else {
		hi += end
	}
	if i := strings.LastIndexAny(text[lo:start], ".!?"); i >= 0 {
		lo += i + 1
	}
	if i := strings.IndexAny(text[end:hi], ".!?"); i >= 0 {
		hi = end + i + 1
	}
	return lo, hi
}

// classifySuspect decides whether a close keyword matched at text[start:end]
// reads as unintended, and returns the reason plus the sentence it sits in.
// Pure.
func classifySuspect(text string, start, end int) (reason, context string) {
	lo, hi := sentenceAround(text, start, end)
	switch {
	case negationRe.MatchString(text[lo:start]):
		reason = SuspectNegated
	case qualifierRe.MatchString(text[end:hi]):
		reason = SuspectQualified
	default:
		return "", ""
	}
	return reason, strings.TrimSpace(text[lo:hi])
}

// closingRefRe matches a GitHub closing keyword immediately followed by an issue
// reference. The keyword set is exactly GitHub's (close/closes/closed,
// fix/fixes/fixed, resolve/resolves/resolved). The reference must be #N (same
// repo), owner/repo#N (cross-repo — TWO path segments required), or a full issue
// URL. A single-segment `repo#N` has no `/` so it matches nothing here — encoding
// the real asymmetry that `Closes repo#N` does NOT close but `Closes owner/repo#N`
// does. A bare `#N` with no keyword is not matched (the reference-without-closing
// form). Negation ("does not close #N") is IGNORED by GitHub's parser, so `close
// #N` is (correctly) still matched — surfacing that trap rather than hiding it.
var closingRefRe = regexp.MustCompile(
	`(?i)\b(?:close[sd]?|fix(?:es|ed)?|resolve[sd]?):?\s+` +
		`(?:#(\d+)|([\w.-]+/[\w.-]+)#(\d+)|https?://github\.com/([\w.-]+/[\w.-]+)/issues/(\d+))`)

// ClosingRefs returns the issues a text will auto-close on merge — the same text
// GitHub's closing-reference resolver sees. Deduplicated by repo#number. Pure.
func ClosingRefs(text string) []ClosingRef {
	var out []ClosingRef
	seen := map[string]bool{}
	for _, idx := range closingRefRe.FindAllStringSubmatchIndex(text, -1) {
		group := func(i int) string {
			if idx[2*i] < 0 {
				return ""
			}
			return text[idx[2*i]:idx[2*i+1]]
		}
		var repo, num string
		switch {
		case group(1) != "": // #N (same repo)
			num = group(1)
		case group(2) != "" && group(3) != "": // owner/repo#N
			repo, num = group(2), group(3)
		case group(4) != "" && group(5) != "": // full issue URL
			repo, num = group(4), group(5)
		}
		n, err := strconv.Atoi(num)
		if err != nil {
			continue
		}
		key := repo + "#" + num
		if seen[key] {
			continue
		}
		seen[key] = true
		reason, context := classifySuspect(text, idx[0], idx[1])
		out = append(out, ClosingRef{
			Number:  n,
			Repo:    repo,
			Raw:     strings.TrimSpace(text[idx[0]:idx[1]]),
			Suspect: reason,
			Context: context,
		})
	}
	return out
}

// SuspectClosings returns the refs whose phrasing says the close is not meant —
// a negation before the keyword or a qualifier after the number (#164). Pure.
//
// ⚠ Deliberately NOT deduplicated against ClosingRefs' ordering: a text that
// says "Closes #5" once and "does not close #5" later keeps the FIRST match, so
// an ordinary close is not retroactively made suspect by later prose about it.
// That direction matters — the alternative flags the postmortem describing the
// trap, which is how the advice "don't quote it" kept failing.
func SuspectClosings(text string) []ClosingRef {
	var out []ClosingRef
	for _, r := range ClosingRefs(text) {
		if r.Suspect != "" {
			out = append(out, r)
		}
	}
	return out
}

// SameRepoClosings returns just the same-repo issue numbers a text will close
// (Repo == ""), sorted — the set whose state a same-repo `gh` query can verify.
func SameRepoClosings(text string) []int {
	var nums []int
	for _, r := range ClosingRefs(text) {
		if r.Repo == "" {
			nums = append(nums, r.Number)
		}
	}
	sort.Ints(nums)
	return nums
}

// ExtraClosings returns same-repo issue numbers the SQUASH text will close that
// are NOT in the PR's own closingIssuesReferences (graphNums) — the exact
// signature of trap 2 (#77): a close keyword in the commit body that the
// GraphQL closingIssuesReferences query, which only sees the PR title/body,
// never reports. Sorted. Pure.
func ExtraClosings(squashText string, graphNums []int) []int {
	inGraph := map[int]bool{}
	for _, n := range graphNums {
		inGraph[n] = true
	}
	var extra []int
	for _, n := range SameRepoClosings(squashText) {
		if !inGraph[n] {
			extra = append(extra, n)
		}
	}
	sort.Ints(extra)
	return extra
}
