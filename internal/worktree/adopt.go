package worktree

import (
	"errors"
	"fmt"
	"path/filepath"
	"strings"

	"github.com/eharriett0/wt/internal/gitx"
	"github.com/eharriett0/wt/internal/ui"
)

// AdoptWant says which commit `wt adopt` must land on (#167). Adopting by PR
// number sets PR and PRHead (the PR's headRefOid from gh). Adopting by branch
// name leaves both empty, and the target is origin/<branch> as just fetched.
type AdoptWant struct {
	PR     string
	PRHead string
}

// Refusals a caller, or a test, can tell apart with errors.Is (#167).
var (
	// ErrPRHeadNotOnOrigin: adopting by PR, origin/<branch> does not carry the
	// PR's head, so it is not the PR's branch (a PR from a fork) or is an old copy.
	ErrPRHeadNotOnOrigin = errors.New("the PR head is not on origin/<branch>")
	// ErrBranchInUse: a worktree has the branch checked out, or that could not be
	// ruled out, so wt does not move it.
	ErrBranchInUse = errors.New("the branch is checked out in a worktree")
	// ErrCaseTwin: a branch whose name differs only in letter case exists, and the
	// filesystem ignores case, so the two are one ref.
	ErrCaseTwin = errors.New("a branch differing only in case exists")
	// ErrLandedElsewhere: the new worktree was not on the branch at the commit wt
	// meant (a tag of the same name wins over origin/<branch>), so wt removed it.
	ErrLandedElsewhere = errors.New("the new worktree landed elsewhere")
	// ErrWorktreeOffBranch: wt's worktree path for the branch holds a worktree
	// that is on another branch or a detached HEAD, which new and claim do not
	// hand back (#198 review).
	ErrWorktreeOffBranch = errors.New("wt's worktree for the branch is not on it")
	// ErrGoneFromOrigin: the branch was pushed before and origin no longer has it
	// (deleted there, typically after its PR merged), so claim does not push it
	// back (#198 review).
	ErrGoneFromOrigin = errors.New("origin no longer has the branch")
	// ErrOriginHasBranch: a claim with no local branch would cut one from the base,
	// and origin already has a branch of that name (#198 review).
	ErrOriginHasBranch = errors.New("origin already has the branch")
)

// refusal is an error with a message of its own that still matches its kind
// under errors.Is (#167).
type refusal struct {
	kind error
	msg  string
}

func (r *refusal) Error() string { return r.msg }
func (r *refusal) Unwrap() error { return r.kind }

func refuse(kind error, format string, a ...any) error {
	return &refusal{kind: kind, msg: fmt.Sprintf(format, a...)}
}

// TipRelation is how a local tip relates to the commit `wt adopt` must land on
// (#167).
type TipRelation int

const (
	TipNoTarget TipRelation = iota // nothing to compare against: by branch name, with no origin/<branch>
	TipEqual                       // the local tip IS the target
	TipBehind                      // the target contains the local tip, so a fast-forward reaches it
	TipAhead                       // the local tip contains the target, plus commits the target lacks
	TipDiverged                    // each has commits the other lacks: the #167 stale branch
	TipUnknown                     // they differ and could not be compared (a commit is missing, or a shallow clone)
)

// ClassifyTips relates a local tip to the target (#167). localInTarget and
// targetInLocal are the two `merge-base --is-ancestor` answers, and known says
// both were computed. Equality needs no ancestry, so it is decided first. Pure.
func ClassifyTips(local, target string, localInTarget, targetInLocal, known bool) TipRelation {
	switch {
	case target == "":
		return TipNoTarget
	case local == target:
		return TipEqual
	case !known:
		return TipUnknown
	case localInTarget:
		return TipBehind
	case targetInLocal:
		return TipAhead
	default:
		return TipDiverged
	}
}

// HeadGate says whether origin/<branch>, as fetched, carries a PR's head (#167).
type HeadGate int

const (
	HeadOnOrigin    HeadGate = iota // origin/<branch> is the PR head or, just fetched, contains it (gh can lag a push)
	HeadNoRemote                    // this clone has no origin/<branch>
	HeadNotInClone                  // the PR head commit is not in this clone
	HeadNotOnOrigin                 // the PR head is in this clone, but origin/<branch> does not contain it, or was not re-fetched
)

// GatePRHead decides whether `wt adopt <pr>` may use origin/<branch> at all
// (#167). gh's headRefOid can sit on another repository (a PR from a fork),
// which `git fetch origin <branch>` never reaches: origin/<branch> is then a
// different branch that shares the name, or nothing, and when the fetch failed
// it is an old copy. Landing on it, creating the local branch from it, or
// fast-forwarding a local branch to the PR head (base `main`, for a fork PR
// from a contributor's main) would put commits that are not the PR's under the
// PR's name, so only HeadOnOrigin proceeds. headInRemote and known are
// `merge-base --is-ancestor <head> <remote>` and whether it could be computed;
// fetched says this run's fetch succeeded. Containment counts only after a
// successful fetch: it is what a push gh has not caught up with looks like,
// while an old copy that contains the head proves nothing about the PR now.
// Pure.
func GatePRHead(remote, head string, headInRemote, known, fetched bool) HeadGate {
	switch {
	case remote == "":
		return HeadNoRemote
	case remote == head:
		return HeadOnOrigin
	case !known:
		return HeadNotInClone
	case headInRemote && fetched:
		return HeadOnOrigin
	default:
		return HeadNotOnOrigin
	}
}

// AdoptAction is what `wt adopt` does with the local branch it is about to
// attach a new worktree to (#167). `wt new` and `wt claim` decide the same way
// (#198), against origin/<branch>.
type AdoptAction int

const (
	AdoptCreate      AdoptAction = iota // no local branch: worktree-add creates it (adopt: from origin/<branch>, as before #167; new and claim: from the base)
	AdoptAsIs                           // the local branch is the target: attach it unchanged
	AdoptAhead                          // the local branch is the target plus commits not on it (unpushed): attach it unchanged, and say so
	AdoptUnverified                     // nothing to compare against: attach the local branch as it is, and say so
	AdoptFastForward                    // the local branch is only behind the target: move it forward, then attach
	AdoptRefuseInUse                    // only behind, but a worktree has it checked out (or may): wt will not move it
	AdoptRefuse                         // diverged, or could not be compared: do not check it out
)

// DecideAdopt picks what `wt adopt` does with an existing local branch of the
// target's name before it worktree-adds it (#167). inUse says a worktree has the
// branch checked out, or that this could not be ruled out. Pure.
//
// `wt new` and `wt claim` re-attach a local branch of the name they want (#62),
// and decide with this too, against origin/<branch> (#198): they resumed a
// branch left by an earlier attempt that reused the name, or one behind or
// diverged from what was pushed, without looking. With no origin/<branch> the
// branch is attached unverified, which is the #62 case (a branch never pushed,
// with work on it).
//
// The #167 bug was this check missing. worktree-add takes refs/heads/<branch>
// whenever it exists, so a stale branch left by an earlier PR that reused the
// name was checked out instead of the PR head (ahead 6, behind 285), and a push
// from it would have been rejected or, forced, would have overwritten the PR.
// Diverged is that shape, and is refused. Only ahead is not: a branch that
// contains the whole target plus more is not left over from an earlier PR
// (that cannot contain the new PR's commits); it is unpushed work, attached as
// it is, as a re-run hands back a worktree that is only ahead (DecideExisting).
//
// Only a branch no worktree has checked out is fast-forwarded. Moving a
// checked-out branch underneath its worktree leaves that worktree's files at the
// old commit, so its next commit would revert what the move brought in. inUse
// comes from `git worktree list` (BranchInUse), which cannot see a worktree
// that is mid-rebase of the branch; gitx.FastForwardBranch refuses that case
// itself.
func DecideAdopt(localExists bool, rel TipRelation, inUse bool) AdoptAction {
	if !localExists {
		return AdoptCreate
	}
	switch rel {
	case TipNoTarget:
		return AdoptUnverified
	case TipEqual:
		return AdoptAsIs
	case TipAhead:
		return AdoptAhead
	case TipBehind:
		if inUse {
			return AdoptRefuseInUse
		}
		return AdoptFastForward
	default: // TipDiverged, TipUnknown
		return AdoptRefuse
	}
}

// DecideNew is DecideAdopt for `wt new` and `wt claim` (#198 review): a local
// branch that a worktree has checked out, or may have, is refused whatever its
// relation to origin, not only when it would have to move. git does not check
// one branch out in two worktrees, but it says so only at worktree-add, which a
// claim reaches after it assigned the issue. Pure.
func DecideNew(localExists bool, rel TipRelation, inUse bool) AdoptAction {
	if localExists && inUse {
		return AdoptRefuseInUse
	}
	return DecideAdopt(localExists, rel, inUse)
}

// ExistingAction is what `wt adopt` does when wt's worktree for the branch
// already exists, which is the re-run case (#167). `wt new` and `wt claim` check
// it the same way (#198).
type ExistingAction int

const (
	ExistingReuse        ExistingAction = iota // it matches, or there is nothing to compare: hand it back, as before #167
	ExistingReuseBehind                        // only behind the target: hand it back, and say how to update it there
	ExistingReuseAhead                         // only ahead (unpushed commits): hand it back, and say so
	ExistingRefuse                             // diverged, or could not be compared: it may be left over from an earlier PR
	ExistingRefuseBehind                       // only behind, for a command that commits on it and pushes (claim): refused, not moved
)

// DecideExisting picks what `wt adopt` does with wt's existing worktree for the
// branch, from how its HEAD relates to the target (#167). Before #167 it was
// handed back unchecked, so a worktree left over from an earlier PR that reused
// the name came back as "Adopted". Being only behind (the PR moved on) or only
// ahead (commits not pushed yet) is the normal state of a re-run, so those are
// handed back with a note. wt never moves the branch of an existing worktree.
// Pure.
func DecideExisting(rel TipRelation) ExistingAction {
	switch rel {
	case TipBehind:
		return ExistingReuseBehind
	case TipAhead:
		return ExistingReuseAhead
	case TipDiverged, TipUnknown:
		return ExistingRefuse
	default: // TipNoTarget, TipEqual
		return ExistingReuse
	}
}

// DecideExistingFor is DecideExisting for a command that may push the branch
// right after (#198). `wt claim` commits its placeholder on the existing
// worktree's HEAD and pushes it: on a HEAD only behind origin/<branch> that
// commit diverges from origin, which rejects the push, and the claim then rolls
// back. wt does not move the worktree's branch, so only-behind is refused there,
// with the fast-forward to run in that worktree. Pure.
func DecideExistingFor(rel TipRelation, pushes bool) ExistingAction {
	if pushes && rel == TipBehind {
		return ExistingRefuseBehind
	}
	return DecideExisting(rel)
}

// CheckedOutIn returns the path of the worktree that has branch checked out, or
// "" when none does (#167). With foldCase (core.ignorecase) a branch whose name
// differs only in letter case counts too: on such a filesystem the two names are
// one loose ref file, and git's own checked-out check compares names exactly. A
// worktree mid-rebase or mid-bisect of the branch lists as detached, so it is
// not found here. Pure.
func CheckedOutIn(refs []gitx.WorktreeRef, branch string, foldCase bool) string {
	if branch == "" {
		return ""
	}
	for _, r := range refs {
		if r.Branch == branch || (foldCase && r.Branch != "" && strings.EqualFold(r.Branch, branch)) {
			return r.Path
		}
	}
	return ""
}

// BranchInUse says whether wt must treat branch as checked out, and where (#167):
// a worktree has it (CheckedOutIn), or `git worktree list` failed, so that could
// not be ruled out (at is "" then). Pure.
func BranchInUse(refs []gitx.WorktreeRef, listErr error, branch string, foldCase bool) (at string, inUse bool) {
	if listErr != nil {
		return "", true
	}
	at = CheckedOutIn(refs, branch, foldCase)
	return at, at != ""
}

// CaseTwin returns the first of names that differs from branch only in letter
// case, or "" (#167). Pure.
func CaseTwin(names []string, branch string) string {
	for _, n := range names {
		if n != branch && strings.EqualFold(n, branch) {
			return n
		}
	}
	return ""
}

// AdoptedOnTarget reports whether a worktree `wt adopt` just added landed where
// it meant to (#167): on refs/heads/<branch> (gotRef is the worktree's full
// symbolic HEAD, "" when detached), at intended when that is set. worktree-add
// resolves <branch> itself, and a tag of the same name wins over origin/<branch>
// there, leaving a detached HEAD at the tag. Pure.
func AdoptedOnTarget(gotRef, gotHead, branch, intended string) bool {
	return gotRef == "refs/heads/"+branch && (intended == "" || gotHead == intended)
}

// listWorktrees is gitx.WorktreeList, as a variable so a test can make the
// listing fail (#167).
var listWorktrees = gitx.WorktreeList

// attachKind is the command an attach check runs for (#198).
type attachKind int

const (
	forAdopt attachKind = iota // wt adopt
	forNew                     // wt new
	forClaim                   // wt claim, which commits a placeholder on the branch and pushes it
)

// attachFor is the command an attach check runs for (#198). `wt new` and `wt
// claim` check a local branch, or wt's existing worktree, against
// origin/<branch> exactly as `wt adopt` does (#167); only their notes and the
// remediation a refusal prints differ. A missing branch is created from the
// base by new and claim, so their advice for taking what was pushed is `wt
// adopt`, not a re-run.
type attachFor struct {
	kind  attachKind
	rerun string // the command line that re-runs it: "wt adopt 1079", "wt new feat/x", "wt claim 42"
	dir   string // new and claim: the worktree directory wt would use
}

// adoptFor is the attachFor of `wt adopt` (#167).
func adoptFor(branch string, want AdoptWant) attachFor {
	if want.PR != "" {
		return attachFor{kind: forAdopt, rerun: "wt adopt " + want.PR}
	}
	return attachFor{kind: forAdopt, rerun: "wt adopt " + branch}
}

// pushes reports whether the command pushes the branch right after (claim).
func (a attachFor) pushes() bool { return a.kind == forClaim }

// tag is the issue a message cites: the guard is #167's, applied to new and
// claim by #198.
func (a attachFor) tag() string {
	if a.kind == forAdopt {
		return "#167"
	}
	return "#198"
}

// didNot ends a refusal: what wt did not do.
func (a attachFor) didNot() string {
	switch a.kind {
	case forNew:
		return "wt did not create the worktree"
	case forClaim:
		return "wt did not claim it"
	}
	return "wt did not adopt it"
}

// didNotReuse ends the refusal of wt's existing worktree for the branch, which
// `wt new` does not create but hands back.
func (a attachFor) didNotReuse() string {
	if a.kind == forNew {
		return "wt did not hand it back"
	}
	return a.didNot()
}

// adoptTarget is the commit `wt adopt` lands on, and how messages name it
// (#167). tip is "" when there is nothing to compare against: adopting by
// branch name, with no origin/<branch> in this clone.
type adoptTarget struct {
	tip, label string
}

// resolveAdoptTarget finds what `wt adopt` must land on: origin/<branch> as
// fetched (#167). Adopting by PR, origin/<branch> must first carry the PR's
// head (GatePRHead), or Adopt refuses before it looks at an existing worktree
// and before it checks out, creates or moves anything. root is the worktree
// root, for the advice.
func resolveAdoptTarget(branch string, want AdoptWant, fetched bool, root string) (adoptTarget, error) {
	remote := gitx.RemoteTrackingTip(branch)
	if want.PRHead == "" { // by branch name
		if remote == "" {
			return adoptTarget{}, nil
		}
		t := adoptTarget{tip: remote, label: "origin/" + branch}
		if !fetched {
			t.label += " (as last fetched)"
		}
		return t, nil
	}
	in, known := false, true
	if remote != "" && remote != want.PRHead {
		var err error
		in, err = gitx.IsAncestor(want.PRHead, remote)
		known = err == nil
	}
	gate := GatePRHead(remote, want.PRHead, in, known, fetched)
	if gate == HeadOnOrigin {
		if remote == want.PRHead {
			return adoptTarget{tip: remote, label: "PR #" + want.PR + "'s head"}, nil
		}
		return adoptTarget{tip: remote, label: fmt.Sprintf("origin/%s (it contains PR #%s's head %s)", branch, want.PR, short(want.PRHead))}, nil
	}
	return adoptTarget{}, refuseOffOrigin(branch, want, remote, gate, fetched, root)
}

// refuseOffOrigin explains a PR head that origin/<branch> does not carry
// (#167). Nothing has been checked out, created or moved.
func refuseOffOrigin(branch string, want AdoptWant, remote string, gate HeadGate, fetched bool, root string) error {
	state := "this clone has no origin/" + branch
	if remote != "" {
		state = fmt.Sprintf("origin/%s is %s", branch, short(remote))
		switch {
		case !fetched:
			state += " as last fetched"
		case gate == HeadNotInClone:
			state += ", and the PR head is not in this clone"
		case gate == HeadNotOnOrigin:
			state += ", which does not contain it"
		}
	}
	ui.Warn("PR #%s's head (%s) is not on origin/%s (%s), so wt will not check out, create or move anything for it (#167)", want.PR, short(want.PRHead), branch, state)
	if !fetched {
		ui.Info("git fetch origin %s failed, so origin/%s is whatever this clone fetched last. Re-run once fetching works:", branch, branch)
		fmt.Printf("  wt adopt %s\n", want.PR)
	} else {
		dir := filepath.Join(root, "pr-"+want.PR)
		ui.Info("the PR's branch most likely lives on another repository (a PR from a fork), which `git fetch origin` does not reach; an origin/%s, if any, is a different branch with the same name. Check the PR out with gh, in a worktree of its own:", branch)
		fmt.Printf("  git worktree add --detach %s\n", dir)
		fmt.Printf("  cd %s && gh pr checkout %s --branch pr-%s\n", dir, want.PR, want.PR)
		ui.Info("(if the PR was force-pushed a moment ago, re-run: wt adopt %s)", want.PR)
	}
	return refuse(ErrPRHeadNotOnOrigin, "PR #%s's head (%s) is not on origin/%s (%s); wt did not adopt it", want.PR, short(want.PRHead), branch, state)
}

// relate classifies local against target from git's ancestry answers (#167).
// IsAncestor errors on an empty or missing commit, which classifies as unknown.
func relate(local, target string) TipRelation {
	if target == "" || local == target {
		return ClassifyTips(local, target, false, false, true)
	}
	inTarget, err1 := gitx.IsAncestor(local, target)
	inLocal, err2 := gitx.IsAncestor(target, local)
	return ClassifyTips(local, target, inTarget, inLocal, err1 == nil && err2 == nil)
}

// refuseCaseTwin refuses a branch that has a case-only twin on a filesystem that
// ignores case (#167): there the two share one loose ref file, so moving or
// attaching one moves or attaches the other, and git's checked-out check, which
// compares names exactly, does not see a worktree that has the twin. Creating
// the branch is refused too (#198): its new loose ref would be the twin's file.
func refuseCaseTwin(branch string, who attachFor) error {
	names, err := gitx.LocalBranches()
	if err != nil {
		return refuse(ErrCaseTwin, "could not list the local branches to rule out one that differs from %q only in case (core.ignorecase is set): %v; %s", branch, err, who.didNot())
	}
	twin := CaseTwin(names, branch)
	if twin == "" {
		return nil
	}
	ui.Warn("local branch %s differs from %s only in letter case, and this filesystem ignores case (core.ignorecase), so the two are one ref: wt will not check out, create or move either (%s)", twin, branch, who.tag())
	ui.Info("rename the local one, then re-run:")
	fmt.Printf("  git branch -m %s %s-local\n", twin, twin)
	return refuse(ErrCaseTwin, "local branch %q differs from %q only in letter case, and this filesystem ignores case, so they are one ref; %s", twin, branch, who.didNot())
}

// attachPlan is what an attach check decided for the local branch a new
// worktree is about to take (#167, #198). planAttach decides it before anything
// is checked out, created or moved; apply carries it out right before
// worktree-add. `wt claim` asks gh, and may prompt, in between.
type attachPlan struct {
	branch string
	local  string // the local branch's tip when checked, "" when there is none
	target adoptTarget
	action AdoptAction
}

// prepareLocalBranch checks the local branch worktree-add is about to take
// against the target before anything is checked out (#167), and returns the
// commit the new worktree must then be on ("" when there is none to check: no
// local branch and no origin/<branch>). Equal and only-ahead (unpushed commits)
// are attached as they are, only-behind is fast-forwarded first, and anything
// else is refused. With no local branch, worktree-add creates it from
// origin/<branch>, as before.
func prepareLocalBranch(branch string, want AdoptWant, target adoptTarget) (string, error) {
	who := adoptFor(branch, want)
	fold := gitx.IgnoreCase()
	if fold {
		if err := refuseCaseTwin(branch, who); err != nil {
			return "", err
		}
	}
	p, err := planAttach(branch, gitx.BranchTip(branch), target, fold, who)
	if err != nil {
		return "", err
	}
	return p.apply(who)
}

// planAttach decides what happens to the local branch (tip local, "" when there
// is none) before anything is checked out, created or moved (#167, #198), and
// says so: a note for what is attached as it is, a refusal, with both tips and
// the remediation, for what must not be. A fast-forward is announced when apply
// makes it.
func planAttach(branch, local string, target adoptTarget, fold bool, who attachFor) (attachPlan, error) {
	rel := relate(local, target.tip)
	inUseAt, inUse := "", false
	if local != "" {
		refs, listErr := listWorktrees()
		inUseAt, inUse = BranchInUse(refs, listErr, branch, fold)
	}
	decide := DecideAdopt
	if who.kind != forAdopt {
		decide = DecideNew // new and claim: in use is refused whatever the relation (#198 review)
	}
	p := attachPlan{branch: branch, local: local, target: target, action: decide(local != "", rel, inUse)}

	switch p.action {
	case AdoptCreate, AdoptFastForward:
		return p, nil
	case AdoptAsIs:
		ui.OK("local branch %s matches %s (%s)", branch, target.label, short(local))
		return p, nil
	case AdoptAhead:
		ahead, _, _ := gitx.AheadBehind(local, target.tip)
		ui.Info("local branch %s is %s plus %d commit(s) not on it (not pushed yet?); attaching it as it is:", branch, target.label, ahead)
		printOnlyIn("local "+branch, local, target.tip, ahead)
		if who.pushes() {
			ui.Info("`%s` pushes them, with its placeholder commit on top", who.rerun)
		}
		return p, nil
	case AdoptUnverified:
		ui.Info("no origin/%s to compare the local branch with, so it is attached as it is (%s), unverified", branch, short(local))
		return p, nil
	case AdoptRefuseInUse:
		where := "in a worktree wt could not identify (git worktree list failed)"
		if inUseAt != "" {
			where = "at " + inUseAt
		}
		if rel == TipBehind {
			ui.Warn("local branch %s is behind %s (%s → %s), but it is checked out %s, and wt does not move a branch a worktree has checked out (%s)", branch, target.label, short(local), short(target.tip), where, who.tag())
			if inUseAt != "" {
				ui.Info("work in that worktree instead, after fast-forwarding it there:")
				fmt.Printf("  git -C %s merge --ff-only %s\n", inUseAt, target.tip)
			}
			return p, refuse(ErrBranchInUse, "local branch %q is behind %s but checked out %s; wt did not move it", branch, target.label, where)
		}
		// new and claim only (DecideNew): git would refuse the second checkout at
		// worktree-add, after a claim assigned the issue (#198 review).
		ui.Warn("local branch %s is checked out %s, and git does not check one branch out in two worktrees (%s)", branch, where, who.tag())
		if rel == TipDiverged || rel == TipUnknown {
			ui.Info("it does not match %s either:", target.label)
			printTips("local "+branch, local, target.label, target.tip, rel)
		}
		if inUseAt != "" {
			ui.Info("work in that worktree instead:")
			fmt.Printf("  cd %s\n", inUseAt)
		}
		return p, refuse(ErrBranchInUse, "local branch %q is checked out %s; %s", branch, where, who.didNot())
	}

	// AdoptRefuse: the local branch diverged from the target, or the two could
	// not be compared. Never check it out silently (#167, #198).
	ui.Warn("local branch %s does not match %s, so wt will not check it out (%s):", branch, target.label, who.tag())
	printTips("local "+branch, local, target.label, target.tip, rel)
	if inUseAt != "" {
		ui.Info("it is checked out at %s", inUseAt)
	}
	if who.kind == forAdopt {
		ui.Info("it is most likely left over from an earlier PR that reused the name. Inspect it, then set it aside (or delete it), and re-run:")
		fmt.Printf("  git log --oneline %s..%s\n", target.tip, local)
		fmt.Printf("  git branch -m %s %s-old-%s     # or: git branch -D %s\n", branch, branch, short(local), branch)
		fmt.Printf("  %s\n", who.rerun)
	} else {
		adviseDiverged(branch, local, target, who)
	}
	return p, fmt.Errorf("local branch %q (%s) does not match %s (%s): %s; wt did not check it out", branch, short(local), target.label, short(target.tip), relationPhrase(rel))
}

// adviseDiverged prints the ways on for `wt new` / `wt claim` when the local
// branch diverged from origin/<branch> (#198). Setting the branch aside and
// re-running would create a new branch from the base, not take what was pushed,
// so that path is `wt adopt`. Keeping the local branch is the operator's call:
// new names the plain worktree-add, and claim, which pushes the branch, needs it
// reconciled with origin first.
//
// The commonest divergence is the operator's own rebase, not pushed yet (#198
// review). Rebasing that onto origin/<branch> replays the base's commits onto
// the old branch, so it gets its own way on: check that origin's side holds
// only commits it rewrote, then push over them, leased to the exact tip
// compared here so a push made since is never overwritten.
func adviseDiverged(branch, local string, target adoptTarget, who attachFor) {
	ui.Info("it may be left over from an earlier attempt that reused the name, or hold your own commits, rebased or not pushed yet. Inspect it:")
	fmt.Printf("  git log --oneline %s..%s\n", target.tip, local)
	ui.Info("to work on what was pushed, set the local branch aside and adopt origin/%s:", branch)
	fmt.Printf("  git branch -m %s %s-old-%s\n", branch, branch, short(local))
	fmt.Printf("  wt adopt %s\n", branch)
	ui.Info("if you rebased it yourself, check that each commit only origin has is one you rewrote, push over them (leased to the tip compared here), and re-run:")
	fmt.Printf("  git log --oneline %s..%s\n", local, target.tip)
	fmt.Printf("  git push --force-with-lease=%s:%s origin %s\n", branch, target.tip, branch)
	fmt.Printf("  %s\n", who.rerun)
	if who.kind == forClaim {
		ui.Info("to claim with the local branch otherwise, reconcile it with origin/%s first (claim pushes the branch, and origin rejects one that diverged), then re-run:", branch)
		fmt.Printf("  git worktree add %s %s\n", who.dir, branch)
		fmt.Printf("  git -C %s rebase origin/%s     # or merge it\n", who.dir, branch)
		fmt.Printf("  %s\n", who.rerun)
		return
	}
	ui.Info("to keep the local branch as it is, attach it yourself:")
	fmt.Printf("  git worktree add %s %s\n", who.dir, branch)
}

// apply carries out the plan right before worktree-add (#167, #198), and returns
// the commit the new worktree must then be on ("" when there is none to check).
// A branch that changed since planAttach looked is refused, not attached: the
// check was of that tip.
func (p attachPlan) apply(who attachFor) (string, error) {
	cur := gitx.BranchTip(p.branch)
	switch p.action {
	case AdoptCreate:
		if cur != "" {
			return "", fmt.Errorf("local branch %q appeared (at %s) after wt checked for it; %s. Re-run: %s", p.branch, short(cur), who.didNot(), who.rerun)
		}
		return p.target.tip, nil
	case AdoptFastForward:
		_, behind, _ := gitx.AheadBehind(p.local, p.target.tip)
		ui.Step("local branch %s is %d commit(s) behind %s: fast-forwarding it %s → %s", p.branch, behind, p.target.label, short(p.local), short(p.target.tip))
		if err := gitx.FastForwardBranch(p.branch, p.local, p.target.tip); err != nil {
			return "", fmt.Errorf("fast-forward local branch %q to %s: %w", p.branch, p.target.label, err)
		}
		return p.target.tip, nil
	}
	// AdoptAsIs, AdoptAhead, AdoptUnverified: attached as checked.
	if cur != p.local {
		return "", fmt.Errorf("local branch %q moved after wt checked it (%s → %s); %s. Re-run: %s", p.branch, short(p.local), short(cur), who.didNot(), who.rerun)
	}
	return p.local, nil
}

// verifyAdopted checks the worktree Adopt just added (#167), or the one new and
// claim attached to an existing local branch (#198). worktree-add resolves
// <branch> itself, and a tag of the same name, for one, wins over
// origin/<branch> and leaves a detached HEAD at the tag. On a mismatch it
// removes that worktree, and only it, and refuses.
func verifyAdopted(wtDir, branch, intended string, who attachFor) error {
	ref, head := gitx.SymbolicHead(wtDir), gitx.HeadCommit(wtDir)
	if AdoptedOnTarget(ref, head, branch, intended) {
		return nil
	}
	got := "a detached HEAD"
	if ref != "" {
		got = strings.TrimPrefix(ref, "refs/heads/")
	}
	want := branch
	if intended != "" {
		want += " at " + short(intended)
	}
	ui.Warn("the new worktree is on %s at %s, not on %s: git worktree add resolved %q to something else, such as a tag of that name, or the branch moved meanwhile (%s)", got, short(head), want, branch, who.tag())
	removed := "removed it again"
	if err := gitx.WorktreeRemove(wtDir, false); err != nil {
		removed = "could not remove it (" + err.Error() + "); remove it with: git worktree remove " + wtDir
		ui.Warn("%s", removed)
	} else {
		ui.Info("removed that worktree again")
	}
	if tags, err := gitx.Run("tag", "--list", branch); err == nil && strings.TrimSpace(tags) == branch {
		ui.Info("a tag named %s exists and git takes it over the branch; rename or delete the tag and re-run", branch)
		if gitx.RemoteTrackingTip(branch) != "" {
			ui.Info("or attach the branch yourself:")
			fmt.Printf("  git worktree add --track -b %s %s origin/%s\n", branch, wtDir, branch)
		}
	}
	if who.kind != forAdopt {
		return refuse(ErrLandedElsewhere, "the new worktree for %q landed on %s at %s, not on %s; wt %s", branch, got, short(head), want, removed)
	}
	return refuse(ErrLandedElsewhere, "the new worktree for %q landed on %s at %s, not on %s; wt %s and did not adopt it", branch, got, short(head), want, removed)
}

// checkExistingWorktree checks wt's existing worktree for the branch against the
// target before handing it back on a re-run (#167). It never moves the
// worktree's branch: a note when it is only behind or only ahead, a refusal when
// it diverged or could not be compared. `wt new` and `wt claim` check theirs the
// same way (#198), and for claim, which commits on that HEAD and pushes, only
// behind is refused too (DecideExistingFor).
//
// New and claim also refuse a worktree that is not on the branch, before any
// comparison (#198 review): new would hand back, and claim commit its
// placeholder on, whatever it has checked out (a detached HEAD mid-rebase, say),
// while claim's push of the branch went "up to date" without the placeholder.
// No `git -C <dir>` command is printed for a worktree off the branch: it would
// act on whatever that worktree has checked out.
func checkExistingWorktree(wtDir, branch string, target adoptTarget, who attachFor) error {
	ref := gitx.SymbolicHead(wtDir)
	detached := ref == ""
	other := ""
	if detached {
		other = fmt.Sprintf(" (its HEAD is detached, not on %s)", branch)
	} else if ref != "refs/heads/"+branch {
		other = fmt.Sprintf(" (it has %s checked out, not %s)", strings.TrimPrefix(ref, "refs/heads/"), branch)
	}
	if other != "" && who.kind != forAdopt {
		return refuseOffBranch(wtDir, branch, ref, who)
	}
	head := gitx.HeadCommit(wtDir)
	rel := relate(head, target.tip)
	action := DecideExistingFor(rel, who.pushes())
	if action == ExistingReuse {
		return nil
	}
	switch action {
	case ExistingReuseBehind:
		_, behind, _ := gitx.AheadBehind(head, target.tip)
		if other != "" {
			ui.Warn("the existing worktree%s is %d commit(s) behind %s (%s → %s). It is not on %s, so wt names no command to update it: put %s back in it first", other, behind, target.label, short(head), short(target.tip), branch, branch)
			return nil
		}
		ui.Warn("the existing worktree is %d commit(s) behind %s (%s → %s). wt does not move a checked-out branch, so update it there:", behind, target.label, short(head), short(target.tip))
		fmt.Printf("  git -C %s merge --ff-only %s\n", wtDir, target.tip)
		return nil
	case ExistingReuseAhead:
		ahead, _, _ := gitx.AheadBehind(head, target.tip)
		ui.Info("the existing worktree%s has %d commit(s) that %s does not (not pushed yet?)", other, ahead, target.label)
		if who.pushes() {
			printOnlyIn("the worktree", head, target.tip, ahead)
			ui.Info("`%s` pushes them, with its placeholder commit on top", who.rerun)
		}
		return nil
	case ExistingRefuseBehind:
		// claim only, so the worktree is on the branch (refuseOffBranch above):
		// the merge below fast-forwards the branch itself.
		_, behind, _ := gitx.AheadBehind(head, target.tip)
		ui.Warn("wt's worktree for %s already exists at %s, %d commit(s) behind %s (%s → %s). A claim commits on its HEAD and pushes, which origin would reject, and wt does not move a checked-out branch (%s). Update it there, then re-run:", branch, wtDir, behind, target.label, short(head), short(target.tip), who.tag())
		fmt.Printf("  git -C %s merge --ff-only %s\n", wtDir, target.tip)
		fmt.Printf("  %s\n", who.rerun)
		return refuse(ErrBranchInUse, "wt's worktree for %q (HEAD %s) is behind %s (%s), and wt does not move a checked-out branch; %s", branch, short(head), target.label, short(target.tip), who.didNotReuse())
	}

	ui.Warn("wt's worktree for %s already exists at %s%s, but its HEAD does not match %s (%s):", branch, wtDir, other, target.label, who.tag())
	printTips("the worktree", head, target.label, target.tip, rel)
	if who.kind != forAdopt {
		// new and claim: setting the branch aside and re-running would create a
		// new branch from the base, so taking what was pushed is `wt adopt`.
		if who.kind == forClaim {
			ui.Info("if it is your own work and origin moved on underneath it, reconcile it there (rebase or merge onto %s, then push), and re-run: %s", short(target.tip), who.rerun)
		} else {
			ui.Info("if it is your own work and origin moved on underneath it, reconcile it there (rebase or merge onto %s)", short(target.tip))
		}
		ui.Info("if it is left over from an earlier attempt that reused the name, keep what it holds, remove it, set the branch aside, and adopt what was pushed:")
	} else {
		ui.Info("if it is left over from an earlier PR that reused the name, keep what it holds, remove it, set the branch aside, and re-run:")
	}
	if detached {
		// git worktree remove keeps nothing that only a detached HEAD has. Named by
		// its sha, not `git -C <dir> branch`, which would act on that worktree.
		fmt.Printf("  git branch %s-wip-%s %s     # its detached HEAD: removing the worktree would orphan these commits\n", branch, short(head), head)
	}
	fmt.Printf("  git worktree remove %s     # refuses while it holds uncommitted or untracked changes: commit or stash them first\n", wtDir)
	if tip := gitx.BranchTip(branch); tip != "" {
		fmt.Printf("  git branch -m %s %s-old-%s\n", branch, branch, short(tip))
	}
	if who.kind != forAdopt {
		fmt.Printf("  wt adopt %s\n", branch)
	} else {
		fmt.Printf("  %s\n", who.rerun)
		ui.Info("if it is your own work and the target moved on underneath it, reconcile it there instead (rebase or merge onto %s)", short(target.tip))
	}
	return fmt.Errorf("wt's worktree for %q (HEAD %s) does not match %s (%s): %s; %s", branch, short(head), target.label, short(target.tip), relationPhrase(rel), who.didNotReuse())
}

// refuseOffBranch refuses, for new and claim, wt's worktree for branch when it is
// not on the branch (#198 review). ref is its symbolic HEAD, "" when detached.
// The advice names no `git -C <dir>` command: that would act on whatever the
// worktree has checked out.
func refuseOffBranch(wtDir, branch, ref string, who attachFor) error {
	has := "a detached HEAD (a rebase or bisect in progress there, or a detached checkout)"
	if ref != "" {
		has = strings.TrimPrefix(ref, "refs/heads/") + " checked out"
	}
	ui.Warn("wt's worktree for %s, %s, has %s, not %s (%s)", branch, wtDir, has, branch, who.tag())
	ui.Info("finish what is in progress there or put %s back in it, or move that worktree out of wt's way, then re-run:", branch)
	fmt.Printf("  git worktree move %s %s-moved     # to move it\n", wtDir, wtDir)
	fmt.Printf("  %s\n", who.rerun)
	return refuse(ErrWorktreeOffBranch, "wt's worktree for %q has %s, not %s; %s", branch, has, branch, who.didNotReuse())
}

// maxShownCommits caps the commit lists printed below.
const maxShownCommits = 10

// printTips prints both tips, how far apart they are, and the commits only the
// local side has: what an operator needs to judge a stale branch (#167).
func printTips(localLabel, local, targetLabel, target string, rel TipRelation) {
	w := max(len(localLabel), len(targetLabel))
	if local == "" {
		local = "(none)"
	}
	ahead, behind, err := gitx.AheadBehind(local, target)
	if rel == TipUnknown || err != nil {
		fmt.Printf("    %-*s  %s\n", w, localLabel, local)
		fmt.Printf("    %-*s  %s  (the two cannot be compared)\n", w, targetLabel, target)
		return
	}
	fmt.Printf("    %-*s  %s  has %d commit(s) that %s lacks\n", w, localLabel, local, ahead, targetLabel)
	fmt.Printf("    %-*s  %s  has %d commit(s) that %s lacks\n", w, targetLabel, target, behind, localLabel)
	printOnlyIn(localLabel, local, target, ahead)
}

// printOnlyIn lists, newest first, up to maxShownCommits of the n commits in
// local that are not in target.
func printOnlyIn(localLabel, local, target string, n int) {
	if n == 0 {
		return
	}
	lines, err := gitx.OnelineLog(target, local, maxShownCommits)
	if err != nil || len(lines) == 0 {
		return
	}
	fmt.Printf("    commits only in %s, newest first:\n", localLabel)
	for _, ln := range lines {
		fmt.Printf("      %s\n", ln)
	}
	if n > len(lines) {
		fmt.Printf("      … and %d more\n", n-len(lines))
	}
}

// relationPhrase names a refused relation for the one-line error (#167).
func relationPhrase(rel TipRelation) string {
	switch rel {
	case TipAhead:
		return "it has commits the target lacks"
	case TipDiverged:
		return "the two have diverged"
	case TipUnknown:
		return "the two cannot be compared"
	}
	return "they differ"
}

// short abbreviates a commit id for a message; anything shorter is returned as is.
func short(sha string) string {
	if len(sha) > 8 {
		return sha[:8]
	}
	if sha == "" {
		return "(none)"
	}
	return sha
}
