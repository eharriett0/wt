// Claude Code PreToolUse collision hook (#95). `wt check` is human-invoked, and
// the failure mode that motivated the pre-push guard is that people — and now
// AGENTS — forget to run it. This wires wt's collision engine into the Claude
// Code edit loop: before an Edit/Write/MultiEdit, it runs the SAME grading as
// `wt check` on the target file and surfaces a HIGH cross-worktree overlap to
// the agent as advisory context (or a hard deny under WT_CLAUDE_HOOK_BLOCK=1).
//
// Advisory-first, and always fail-open (any error / uncertainty → allow) — a
// coordination nicety must never disrupt the editing session.
package cli

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"syscall"

	"github.com/eharriett0/wt/internal/collide"
	"github.com/eharriett0/wt/internal/config"
	"github.com/eharriett0/wt/internal/gitx"
	"github.com/eharriett0/wt/internal/ui"
)

// parseClaudeEdit extracts (cwd, file, relevant) from a Claude Code PreToolUse
// payload. Relevant only for the file-editing tools that carry a file_path
// (Edit / Write / MultiEdit). Pure — the testable core.
func parseClaudeEdit(b []byte) (cwd, file string, relevant bool) {
	var p struct {
		CWD       string `json:"cwd"`
		ToolName  string `json:"tool_name"`
		ToolInput struct {
			FilePath string `json:"file_path"`
		} `json:"tool_input"`
	}
	if err := json.Unmarshal(b, &p); err != nil {
		return "", "", false
	}
	switch p.ToolName {
	case "Edit", "Write", "MultiEdit":
	default:
		return p.CWD, "", false
	}
	// Not trimmed: a file name can begin or end with a space, and the path is
	// asked about exactly (#181), as git reports it (#200).
	f := p.ToolInput.FilePath
	return p.CWD, f, strings.TrimSpace(f) != ""
}

// claudeDecision shapes the hook's stdout JSON from the collisions kept for the
// edited file (regradePending's). No collision → ("", false). An entry not
// confirmed against the agent's PENDING edit (a Write, or the edit region
// couldn't be located) is an honest FILE-LEVEL heads-up that never claims hunk
// overlap, so it can't contradict `wt check` the way the old always-"overlapping
// hunks" message did (#108). Advisory (default) → additionalContext; block=true
// → permissionDecision "deny", for any kept entry, as before.
//
// Every window is named once, by identity (#201 review): deduplicated by its
// worktree, else its label (a label is not an identity, #193; namesakes
// collapsed into one name), each with its own liveness. All confirmed or all
// file-level keep their wording; a mix tags each window with its own grade
// instead of letting one file-level entry word them all. Pure.
func claudeDecision(file string, graded []pendingGrade, block bool) (string, bool) {
	if len(graded) == 0 {
		return "", false
	}
	seen := map[string]bool{}
	var kept []pendingGrade
	allConfirmed, anyConfirmed := true, false
	for _, g := range graded {
		id := g.entry.otherWorktree
		if id == "" {
			id = g.entry.Window
		}
		if seen[id] {
			continue
		}
		seen[id] = true
		kept = append(kept, g)
		allConfirmed = allConfirmed && g.confirmed
		anyConfirmed = anyConfirmed || g.confirmed
	}
	mixed := anyConfirmed && !allConfirmed
	var who []string
	for _, g := range kept {
		w := fmt.Sprintf("%s [%s]", g.entry.Window, g.entry.Liveness)
		if mixed {
			tag := "hunk overlap not computed"
			if g.confirmed {
				tag = "overlapping hunks"
			}
			w += " (" + tag + ")"
		}
		who = append(who, w)
	}
	var msg string
	switch {
	case allConfirmed:
		msg = fmt.Sprintf("wt collision: with this edit, your version of %s OVERLAPS a region that %s is also editing (`wt check %s` will grade it HIGH). "+
			"Coordinate before editing to avoid a merge conflict / duplicate PR. If you've already coordinated, set WT_SKIP_COLLISION=1.",
			file, strings.Join(who, ", "), file)
	case mixed:
		msg = fmt.Sprintf("wt: %s is also being edited by %s; run `wt check %s` for line-level detail. "+
			"Coordinate before editing to avoid a merge conflict / duplicate PR. If you've already coordinated, set WT_SKIP_COLLISION=1.",
			file, strings.Join(who, ", "), file)
	default:
		msg = fmt.Sprintf("wt: %s is also being edited by %s — file-level heads-up (hunk overlap not computed; run `wt check %s` for line-level detail). "+
			"Coordinate before editing to avoid a merge conflict / duplicate PR. If you've already coordinated, set WT_SKIP_COLLISION=1.",
			file, strings.Join(who, ", "), file)
	}

	var out struct {
		HookSpecificOutput struct {
			HookEventName            string `json:"hookEventName"`
			AdditionalContext        string `json:"additionalContext,omitempty"`
			PermissionDecision       string `json:"permissionDecision,omitempty"`
			PermissionDecisionReason string `json:"permissionDecisionReason,omitempty"`
		} `json:"hookSpecificOutput"`
	}
	out.HookSpecificOutput.HookEventName = "PreToolUse"
	if block {
		out.HookSpecificOutput.PermissionDecision = "deny"
		out.HookSpecificOutput.PermissionDecisionReason = msg
	} else {
		out.HookSpecificOutput.AdditionalContext = msg
	}
	j, err := json.Marshal(out)
	if err != nil {
		return "", false
	}
	return string(j), true
}

// repoRelativePath converts file to a clean, slash-separated repo-relative path.
// Returns "" when it is outside root, or is root itself: nothing to
// collision-check.
//
// A relative file is read relative to the CURRENT directory, whose
// repo-relative prefix is prefix (`git rev-parse --show-prefix`, "" at the
// root), not relative to root. Collision matching is exact (#181), so reading
// `README.md` typed in pkg/svc/ as the root README.md would check the wrong
// file; the old suffix match used to paper over that by accident.
//
// Either way the file is read the way the filesystem reads it (#201): a
// relative one is joined onto the directory it is read from and resolved
// exactly like an absolute one (resolveExisting), with symlinks resolved on
// both sides, so a /var vs /private/var mismatch on macOS doesn't defeat the
// prefix strip, and `svclink/README.md` (svclink → pkg/svc) names
// pkg/svc/README.md as `$PWD/svclink/README.md` always did. Only the absolute
// spelling used to be resolved, so the relative one was checked at a path no
// window can touch (git never tracks a path beyond a symlink) and read clear.
// That includes a file that doesn't exist yet (a Write creating it, a path that
// exists only on another branch): its existing part is resolved and the rest
// re-appended. Then it is spelled the way git names it (placePath: on a
// case-insensitive filesystem, `readme.md` is the README.md git tracks).
func repoRelativePath(root, prefix, file string) string {
	return repoRelativeAs(root, prefix, file, true)
}

// repoRelativeEntry is repoRelativePath for a file that is about to be REMOVED
// rather than written (apply_patch's Delete, or a move's source): a symlink
// that is its last component is the link itself, which is what goes, not the
// file it points to (#201). A symlinked directory on the way is still followed.
func repoRelativeEntry(root, prefix, file string) string {
	return repoRelativeAs(root, prefix, file, false)
}

// repoRelativeAs is repoRelativePath, with followLast as resolvePath reads it.
func repoRelativeAs(root, prefix, file string, followLast bool) string {
	abs := file
	if !filepath.IsAbs(file) {
		abs = joinRaw(root, filepath.FromSlash(prefix), file)
	}
	pp, ok := placePath(resolvePath(abs, followLast), []string{resolveExisting(root)})
	if !ok || pp.rel == collide.RootPath {
		return ""
	}
	return pp.rel
}

// maxPredictBytes caps a file the edit hooks read to predict an edit: 8 MiB,
// the largest input wt already holds in memory (`wt mcp`'s line buffer). A
// larger file is left unpredicted (a file-level heads-up), never read.
const maxPredictBytes = 8 << 20

// readForPrediction reads path (following links) for a hook's prediction of
// the file an edit will produce (#201 re-review), and only a regular file of at
// most maxPredictBytes: os.ReadFile on a FIFO waited for a writer forever and
// on /dev/zero read without end, and a hook must always return promptly. It
// opens non-blocking and asks the OPENED file what it is, so a path swapped
// for a FIFO after a check can't block either. Nothing there is fileMissing;
// anything else not read is fileUnread (its content unknown). I/O.
func readForPrediction(path string) (string, fileRead) {
	f, err := os.OpenFile(path, os.O_RDONLY|syscall.O_NONBLOCK, 0)
	if err != nil {
		if errors.Is(err, fs.ErrNotExist) {
			return "", fileMissing
		}
		return "", fileUnread
	}
	defer f.Close()
	fi, err := f.Stat()
	if err != nil || !fi.Mode().IsRegular() || fi.Size() > maxPredictBytes {
		return "", fileUnread
	}
	b, err := io.ReadAll(io.LimitReader(f, maxPredictBytes+1))
	if err != nil || len(b) > maxPredictBytes {
		return "", fileUnread
	}
	return string(b), fileReadOK
}

// joinRaw joins path elements with the separator WITHOUT cleaning the result,
// so a ".." after a symlink is left for resolvePath to read as the OS does
// (from the link's target), not collapsed lexically as filepath.Join would.
// Pure.
func joinRaw(dir string, elems ...string) string {
	sep := string(filepath.Separator)
	s := dir
	for _, e := range elems {
		if e == "" {
			continue
		}
		if !strings.HasSuffix(s, sep) {
			s += sep
		}
		s += e
	}
	return s
}

// resolveExisting returns the absolute path p as the filesystem reads it, every
// symlink in it followed (resolvePath). filepath.EvalSymlinks fails outright for
// a path that doesn't exist, which left such a path on the logical /var/… side
// of a comparison with git's physical /private/var/… root, where it read as
// outside the repo and went unchecked; here the first missing component and the
// rest are re-appended as given.
func resolveExisting(p string) string { return resolvePath(p, true) }

// maxSymlinkHops bounds the symlinks resolvePath follows for one path, as the
// OS does (Linux gives up at 40), so a loop ends.
const maxSymlinkHops = 40

// resolvePath reads the absolute path p component by component, the way the
// OS opens it (#201): a symlink is replaced by its target where it occurs, and a
// ".." steps up from where the path has got to, which after a symlink is the
// TARGET's parent. Cleaning p first read `svclink/../x` as x while the OS opens
// pkg/x. followLast=false keeps a symlink that is the very last component as it
// is: the ENTRY p names, which is what git tracks at that path (a trailing "/"
// or "." still resolves it, as it does for the OS). The first component that
// doesn't exist (or a dangling link, or a loop) ends the walk: it and the rest
// are re-appended as given, cleaned. A relative p is only cleaned. I/O.
func resolvePath(p string, followLast bool) string {
	if !filepath.IsAbs(p) {
		return filepath.Clean(p)
	}
	sep := string(filepath.Separator)
	vol := filepath.VolumeName(p)
	top := vol + sep
	cur := top
	parts := strings.Split(p[len(vol):], sep)
	hops := 0
	for len(parts) > 0 {
		c := parts[0]
		parts = parts[1:]
		switch c {
		case "", ".":
			continue
		case "..":
			cur = filepath.Dir(cur)
			continue
		}
		next := filepath.Join(cur, c)
		fi, err := os.Lstat(next)
		if err != nil {
			return filepath.Join(append([]string{next}, parts...)...)
		}
		if fi.Mode()&os.ModeSymlink == 0 || (!followLast && len(parts) == 0) {
			cur = next
			continue
		}
		target, err := os.Readlink(next)
		if hops++; err != nil || hops > maxSymlinkHops {
			return filepath.Join(append([]string{next}, parts...)...)
		}
		if filepath.IsAbs(target) {
			cur = top
		}
		parts = append(strings.Split(target, sep), parts...)
	}
	return cur
}

// placedPath is a path inside one of the repository's worktrees: that
// worktree's top level, and the path relative to it, slash-separated
// (collide.RootPath for the top level itself).
type placedPath struct{ root, rel string }

// placePath places p (absolute, symlinks already resolved) in the worktree
// among roots that holds it (locateIn) and spells it the way git names it
// (#201). Where names are case-insensitive (the worktree's filesystem folds
// case, foldsCase, or core.ignorecase says so), the filesystem
// opens `readme.md` for README.md, but git and every other window call the file
// README.md, so each existing component is spelled as its directory lists it
// (spellAsListed), and then as git's index spells the path when the index has
// exactly one spelling for it (indexSpelling): a case-only rename on disk (mv,
// not git mv) leaves the index's, which is the one status and diff report.
// Either spelling replaces the typed one only when acceptFold says the two are
// one file (or neither exists on a filesystem that itself folds case), so a
// misread probe or a hand-set core.ignorecase never merges two files. A
// case-sensitive repository is left exactly as typed. ok=false when no
// worktree holds p. I/O.
func placePath(p string, roots []string) (placedPath, bool) {
	root, rel, ok := locateIn(p, roots, sameDir)
	if !ok {
		return placedPath{}, false
	}
	if rel == collide.RootPath {
		return placedPath{root, rel}, true
	}
	if fsFolds := foldsCase(root); fsFolds || ignoreCaseAt(root) {
		rel = spellAsListed(root, rel, fsFolds)
		if s := indexSpelling(rel, gitx.TrackedFolded(root, rel)); s != rel {
			if te, fe, same := foldFacts(root, rel, s); acceptFold(te, fe, same, fsFolds) {
				rel = s
			}
		}
	}
	return placedPath{root, rel}, true
}

// locateIn finds the worktree that holds p (absolute, symlinks resolved): the
// deepest of roots that contains it, since a worktree can sit inside another's
// directory (a worktree_root inside the main checkout). All worktrees of one
// repository share its layout, so a path inside ANOTHER worktree is that
// worktree's repo-relative path (#201). A root holds p when its components are
// p's leading ones as spelled, or equal but for case where same confirms the two
// name one directory (a case-insensitive filesystem reached through a cwd typed
// as ~/Repo for ~/repo). The deepest holder wins whichever way it matched (#201
// review: stopping at the first as-spelled match let the main checkout claim
// `.wt/feat/x` from a nested worktree `.wt/Feat`, and a HIGH there read clear);
// as spelled wins only a tie. rel is slash-separated, collide.RootPath for the
// root itself. Pure given same.
func locateIn(p string, roots []string, same func(a, b string) bool) (root, rel string, ok bool) {
	p = filepath.Clean(p)
	pc := pathComponents(p)
	depth, asSpelled := -1, false
	for _, r := range roots {
		if r == "" {
			continue
		}
		r = filepath.Clean(r)
		rc := pathComponents(r)
		if len(rc) > len(pc) || len(rc) < depth {
			continue
		}
		exact, folded := true, true
		for i := range rc {
			if rc[i] != pc[i] {
				exact = false
				if folded = strings.EqualFold(rc[i], pc[i]); !folded {
					break
				}
			}
		}
		switch {
		case !folded:
			continue
		case len(rc) == depth && (asSpelled || !exact):
			continue // a tie: the as-spelled match, or the first, stands
		case !exact && !same(r, filepath.VolumeName(p)+string(filepath.Separator)+filepath.Join(pc[:len(rc)]...)):
			continue
		}
		depth, asSpelled = len(rc), exact
		root, rel, ok = r, collide.RootPath, true
		if len(pc) > len(rc) {
			rel = strings.Join(pc[len(rc):], "/")
		}
	}
	return root, rel, ok
}

// pathComponents splits a cleaned absolute path into its names. Pure.
func pathComponents(p string) []string {
	var out []string
	for _, c := range strings.Split(p[len(filepath.VolumeName(p)):], string(filepath.Separator)) {
		if c != "" {
			out = append(out, c)
		}
	}
	return out
}

// sameDir reports whether a and b name one existing file or directory. I/O.
func sameDir(a, b string) bool {
	fa, err := os.Stat(a)
	if err != nil {
		return false
	}
	fb, err := os.Stat(b)
	return err == nil && os.SameFile(fa, fb)
}

// ignoreCaseMemo is core.ignorecase per worktree, read once per process (git
// sets it when it creates the repository): the Codex hook resolves every path of
// a patch twice.
var ignoreCaseMemo sync.Map

// ignoreCaseAt is gitx.IgnoreCaseIn(root), memoized. I/O.
func ignoreCaseAt(root string) bool {
	if v, ok := ignoreCaseMemo.Load(root); ok {
		return v.(bool)
	}
	v := gitx.IgnoreCaseIn(root)
	ignoreCaseMemo.Store(root, v)
	return v
}

// foldsCase reports whether the filesystem holding the worktree at root treats
// two names that differ only in case as one (APFS and HFS+ by default). It
// probes the worktree's OWN filesystem (caseProbe): every worktree top holds a
// `.git` (a directory, or a file in a linked worktree), and where case folds,
// `.GIT` opens that same entry. ⚠ #201 review: walking up to the nearest
// ancestor whose name has a letter probed the PARENT filesystem for a
// case-sensitive volume mounted at a path like /2024/1, read "folds", and a new
// readme.md was checked as the tracked README.md, a different file there: an
// add/add conflict read clear. No `.git` at root (not a worktree top) reads as
// case-sensitive; core.ignorecase still decides. I/O.
func foldsCase(root string) bool {
	dot, swapped := caseProbe(root)
	a, err := os.Lstat(dot)
	if err != nil {
		return false
	}
	b, err := os.Lstat(swapped)
	return err == nil && os.SameFile(a, b)
}

// caseProbe is the pair of entries foldsCase compares: the worktree's own `.git`
// as spelled, and with its case swapped. Both inside root, never an ancestor.
// Pure.
func caseProbe(root string) (string, string) {
	return filepath.Join(root, ".git"), filepath.Join(root, ".GIT")
}

// acceptFold decides whether a case-folded spelling (as a directory lists it,
// or as the index tracks it) may stand for the typed one (#201 review): only
// when the filesystem says they are ONE file (both exist and os.SameFile), or
// when neither exists AND the worktree's filesystem itself folds case (fsFolds:
// foldsCase), so the two names can only ever be one file there. One existing
// without the other, or two existing different files, are two names: folding
// them merged two files, and a readme.md about to be created beside a tracked
// README.md on a case-sensitive volume (an add/add conflict with a window that
// created readme.md) read clear. ⚠ Neither existing is not enough on its own:
// with core.ignorecase set by hand on a case-sensitive volume and the tracked
// README.md deleted, a readme.md about to be created was folded into the
// index's README.md and the same add/add read clear. core.ignorecase alone
// never folds a spelling that is not on disk. So neither a wrong probe nor a
// hand-set core.ignorecase can merge two files. Pure.
func acceptFold(typedExists, foldedExists, same, fsFolds bool) bool {
	if typedExists && foldedExists {
		return same
	}
	return !typedExists && !foldedExists && fsFolds
}

// foldFacts is what the filesystem says about a typed path and a folded
// spelling of it, both relative to dir: whether each exists, and whether they
// are one file. With fsFolds (foldsCase of the worktree), the facts acceptFold
// decides on. I/O.
func foldFacts(dir, typed, folded string) (typedExists, foldedExists, same bool) {
	a, errA := os.Lstat(filepath.Join(dir, filepath.FromSlash(typed)))
	b, errB := os.Lstat(filepath.Join(dir, filepath.FromSlash(folded)))
	return errA == nil, errB == nil, errA == nil && errB == nil && os.SameFile(a, b)
}

// spellAsListed spells each component of rel (relative to root) that exists
// the way its directory lists it (listedName), when acceptFold allows it
// (fsFolds: foldsCase of the worktree); the first one that doesn't exist, and
// the rest, are kept as given. I/O.
func spellAsListed(root, rel string, fsFolds bool) string {
	segs := strings.Split(rel, "/")
	dir := root
	for i, s := range segs {
		if _, err := os.Lstat(filepath.Join(dir, s)); err != nil {
			break
		}
		if n := listedName(dir, s); n != s {
			if te, fe, same := foldFacts(dir, s, n); acceptFold(te, fe, same, fsFolds) {
				segs[i] = n
			}
		}
		dir = filepath.Join(dir, segs[i])
	}
	return strings.Join(segs, "/")
}

// listedName is name as dir's listing spells it (pickListed); name when dir
// can't be read. I/O.
func listedName(dir, name string) string {
	f, err := os.Open(dir)
	if err != nil {
		return name
	}
	defer f.Close()
	names, _ := f.Readdirnames(-1)
	return pickListed(name, names)
}

// pickListed is name as a directory listing spells it: name itself when it is
// listed, else the ONE listed name equal to it but for case. The filesystem
// opened name, so a listing without it means the filesystem folded case to find
// it, and the listed spelling is the file's name. name when no listed name, or
// more than one, folds to it. Pure.
func pickListed(name string, listed []string) string {
	match := ""
	for _, l := range listed {
		if l == name {
			return name
		}
		if strings.EqualFold(l, name) {
			if match != "" && match != l {
				return name
			}
			match = l
		}
	}
	if match == "" {
		return name
	}
	return match
}

// indexSpelling is rel as git's index spells it, given what
// gitx.TrackedFolded listed for it: rel itself when the index has it under that
// spelling (a file, or a directory with a file under it), else the ONE spelling,
// of the file or of the directory above the listed files, that differs from rel
// only in case. rel when the index has nothing for it, or case twins (two
// spellings: a case-insensitive checkout of a tree made elsewhere). Pure.
func indexSpelling(rel string, listed []string) string {
	want := strings.Split(rel, "/")
	spellings := map[string]bool{}
	for _, l := range listed {
		segs := strings.Split(l, "/")
		if len(segs) < len(want) {
			continue
		}
		s := strings.Join(segs[:len(want)], "/")
		if s == rel {
			return rel
		}
		match := true
		for i := range want {
			if !strings.EqualFold(segs[i], want[i]) {
				match = false
				break
			}
		}
		if match {
			spellings[s] = true
		}
	}
	if len(spellings) != 1 {
		return rel
	}
	for s := range spellings {
		return s
	}
	return rel
}

// hookClaudeEdit implements `wt _hook claude-edit`. Always exits 0 (the JSON
// permissionDecision drives any block); every failure path allows.
func hookClaudeEdit(r io.Reader) int {
	if os.Getenv("WT_SKIP_COLLISION") == "1" || os.Getenv("HOOK_DISABLE_MULTIWINDOW_CHECK") == "1" {
		return 0
	}
	b, err := io.ReadAll(r)
	if err != nil {
		return 0
	}
	cwd, file, relevant := parseClaudeEdit(b)
	if !relevant {
		return 0
	}
	// Resolve everything from the agent's cwd (which may be a native subagent
	// worktree), so the collision scan + config load target the right repo.
	if cwd != "" {
		if err := os.Chdir(cwd); err != nil {
			return 0
		}
	}
	// Cheap on the common case: a repo with ≤1 worktree can't collide.
	if paths, err := gitx.WorktreePaths(); err != nil || len(paths) <= 1 {
		return 0
	}
	c, err := config.Load()
	if err != nil {
		return 0
	}
	root, err := gitx.RepoRoot()
	if err != nil {
		return 0
	}
	prefix := ""
	if !filepath.IsAbs(file) { // Claude sends absolute paths; only a relative one needs the cwd
		prefix, _ = gitx.ShowPrefix()
	}
	rel := repoRelativePath(root, prefix, file)
	if rel == "" {
		return 0
	}
	ws, err := collide.Scan(c)
	if err != nil {
		return 0
	}
	// #181: the edit target is a real repo-relative path, so it matches EXACTLY,
	// as the pre-push guard does. A fuzzy match flagged an edit to the root
	// README.md because another window edited pkg/svc/README.md.
	entries := buildCheckReport(c, ws, root, collide.ExactQueries([]string{rel}), false)
	if len(entries) == 0 {
		return 0
	}

	// #108/#184/#199: the hook fires at PRE-edit time, so buildCheckReport's
	// "current" side (this worktree's own edits) doesn't yet include the edit the
	// agent is ABOUT to make. Re-grade every entry the way `wt check` will once
	// it's made: this window's ranges as they will read with the file the edit
	// produces (claudePendingContent), measured by the same diff and base mapping
	// as `wt check` (gitx.ChangedRangesWith), against the other window's ranges.
	// When that can't be computed (an old_string the tool would reject, a line
	// ending it doesn't model, a new file, a git error) the entry stays as a
	// file-level heads-up instead of being dropped.
	var after []gitx.LineRange
	afterOK := false
	// read with readForPrediction: a FIFO or a huge file there is left
	// unpredicted (a file-level heads-up), never read (#201 re-review)
	data, read := readForPrediction(filepath.Join(root, rel))
	if content, ok := claudePendingContent(b, data, read == fileReadOK); ok && read != fileUnread {
		after, afterOK = pendingRanges(root, c.Base, rel, []byte(content))
	}
	graded := regradePending(entries, after, afterOK, func(e CheckEntry) bool {
		return subsumedByBase(e.otherWorktree, c.Base, rel) // that window's, not a namesake's (#193)
	})

	if out, has := claudeDecision(rel, graded, os.Getenv("WT_CLAUDE_HOOK_BLOCK") == "1"); has {
		fmt.Println(out)
	}
	return 0
}

// pendingRanges is gitx.ChangedRangesWith: this window's ranges as they will
// read once the pending edit is made, ok=false when git couldn't measure them.
// A var only so the fail-safe test can make that measurement fail and pin that
// the hooks then keep a file-level heads-up rather than read the failure as "no
// edits" (#184 review).
var pendingRanges = gitx.ChangedRangesWith

// pendingGrade is one collision entry regradePending keeps for a pending edit.
// confirmed: graded from the edit itself (the file collides once it's made);
// false: a file-level heads-up, kept because that grade couldn't be computed,
// or because the edit leaves this window no edits to overlap with (`wt check`'s
// indeterminate HIGH).
type pendingGrade struct {
	entry     CheckEntry
	confirmed bool
}

// regradePending keeps exactly the entries `wt check` will grade HIGH once an
// agent's pending edit is made, the pre-edit hooks' block predicate (it MUST
// equal `wt check`'s, #92/#108). after is this window's ranges as `wt check`
// will read them then: the file the edit produces, measured by the same diff and
// base mapping (gitx.ChangedRangesWith, #199 review), so the grade is `wt
// check`'s by construction, not a guess at which lines the edit changes. A
// hunk-graded entry, HIGH or FYI alike, is HIGH afterwards iff after conflicts
// with the other window's ranges by git's rule (collide.ConflictSeverity: they
// overlap, or touch with no unchanged line between, #199). An FYI entry is
// re-graded too: a window whose earlier edits were disjoint can still be about
// to overlap (#184 review). A newly-HIGH entry is then put through the #122
// subsumed check `wt check` applies to every HIGH (subsumed is only called for
// those). An edit that leaves this window's copy as base's (after empty) reads
// HIGH, as `wt check` reads an empty side (indeterminate), but no hunk overlap
// is computed, so it is a heads-up, not a confirmed overlap.
//
// When that grade can't be computed (afterOK false, or an entry graded without
// line ranges: an indeterminate side, a structured-doc section) the entry is
// kept as a file-level heads-up if it could become HIGH: never dropped on an
// unknown. Left out: entries no edit of this window can make HIGH (stale,
// already merged, untracked, subsumed, append-only), and shared-doc advisories,
// which the hooks have never graded per edit (a structured doc's section grade
// is `wt check`'s and pre-push's). Pure.
func regradePending(entries []CheckEntry, after []gitx.LineRange, afterOK bool, subsumed func(CheckEntry) bool) []pendingGrade {
	var out []pendingGrade
	for _, e := range entries {
		hunkGraded := len(e.OtherRanges) > 0 && !e.Subsumed && (e.Category == CatBlocking || e.Category == CatFYI)
		if e.Category != CatBlocking && !hunkGraded {
			continue
		}
		if !hunkGraded || !afterOK {
			out = append(out, pendingGrade{entry: e})
			continue
		}
		if collide.ConflictSeverity(after, e.OtherRanges, false) != collide.SevHigh {
			continue
		}
		if e.Category != CatBlocking && subsumed(e) {
			continue
		}
		out = append(out, pendingGrade{entry: e, confirmed: len(after) > 0})
	}
	return out
}

// claudePendingContent is the edited file's content once a Claude Code Edit,
// MultiEdit or Write lands, computed the way Claude Code computes it (#199
// review): a Write's content as given; an Edit's replacement of old_string in
// the file (applyClaudeEdit), each MultiEdit edit on the previous one's result.
// Claude Code matches a CRLF file as LF and writes it back as CRLF; an edit's
// own CRLFs are read as LF there too (if the tool didn't, it would find no match
// and make no edit, so nothing is graded wrongly). ok=false when it can't be
// told: an edit the tool would reject (old_string absent, or not unique without
// replace_all), a missing file, a file mixing line endings, a lone CR, an
// unknown tool. The hook then keeps its file-level heads-up. Pure.
func claudePendingContent(raw []byte, content string, exists bool) (string, bool) {
	type edit struct {
		OldString  string `json:"old_string"`
		NewString  string `json:"new_string"`
		ReplaceAll bool   `json:"replace_all"`
	}
	var p struct {
		ToolName  string `json:"tool_name"`
		ToolInput struct {
			edit
			Content *string `json:"content"`
			Edits   []edit  `json:"edits"`
		} `json:"tool_input"`
	}
	if err := json.Unmarshal(raw, &p); err != nil {
		return "", false
	}
	var edits []edit
	switch p.ToolName {
	case "Write":
		if p.ToolInput.Content == nil {
			return "", false
		}
		return *p.ToolInput.Content, true
	case "Edit":
		edits = []edit{p.ToolInput.edit}
	case "MultiEdit":
		edits = p.ToolInput.Edits
	}
	if !exists || len(edits) == 0 {
		return "", false
	}
	crlf := strings.Contains(content, "\r")
	if crlf {
		// Only a file whose every line ends CRLF has one reading as LF.
		if n := strings.Count(content, "\r\n"); n != strings.Count(content, "\n") || n != strings.Count(content, "\r") {
			return "", false
		}
		content = strings.ReplaceAll(content, "\r\n", "\n")
	}
	for _, e := range edits {
		if crlf {
			e.OldString = strings.ReplaceAll(e.OldString, "\r\n", "\n")
			e.NewString = strings.ReplaceAll(e.NewString, "\r\n", "\n")
		}
		if strings.Contains(e.OldString+e.NewString, "\r") {
			return "", false
		}
		next, ok := applyClaudeEdit(content, e.OldString, e.NewString, e.ReplaceAll)
		if !ok {
			return "", false
		}
		content = next
	}
	if crlf {
		content = strings.ReplaceAll(content, "\n", "\r\n")
	}
	return content, true
}

// applyClaudeEdit is Claude Code's replacement of old with new in content:
// refused (ok=false) when old is empty, unchanged by new, absent, or found more
// than once without replaceAll; the first match replaced, or every one under
// replaceAll. Deleting (new empty) an old that doesn't end its line takes the
// newline after it too, when one follows. Pure.
func applyClaudeEdit(content, old, new string, replaceAll bool) (string, bool) {
	n := strings.Count(content, old)
	if old == "" || old == new || n == 0 || (n > 1 && !replaceAll) {
		return "", false
	}
	if new == "" && !strings.HasSuffix(old, "\n") && strings.Contains(content, old+"\n") {
		old += "\n"
	}
	if replaceAll {
		return strings.ReplaceAll(content, old, new), true
	}
	return strings.Replace(content, old, new, 1), true
}

// claudeHookSnippet is the .claude/settings.json entry that wires all three hooks:
// PreToolUse (per-edit collision check) + PostToolUse/TodoWrite (mirror each
// window's TODO list so `wt todos` can show it) + UserPromptSubmit (per-turn
// multi-window awareness — overlaps + un-acked coordination holds/announcements).
const claudeHookSnippet = `{
  "hooks": {
    "PreToolUse": [
      {
        "matcher": "Edit|Write|MultiEdit",
        "hooks": [
          { "type": "command", "command": "wt _hook claude-edit" }
        ]
      }
    ],
    "PostToolUse": [
      {
        "matcher": "TodoWrite",
        "hooks": [
          { "type": "command", "command": "wt _hook todo-write" }
        ]
      }
    ],
    "UserPromptSubmit": [
      {
        "hooks": [
          { "type": "command", "command": "wt _hook claude-context" }
        ]
      }
    ]
  }
}`

const claudeHookCommand = "wt _hook claude-edit"

// claudeContextCommand is the UserPromptSubmit hook — the per-turn multi-window
// awareness snapshot (shares hookAgentContext with codex-context).
const claudeContextCommand = "wt _hook claude-context"

// todoWriteCommand is the PostToolUse/TodoWrite hook that mirrors each window's
// Claude Code TODO list into the wt todo store (the data source for `wt todos`).
// Nothing wired it before #144, so `wt todos` was permanently empty even though
// the recording hook (`wt _hook todo-write`) already existed.
const todoWriteCommand = "wt _hook todo-write"

// cmdInstallClaudeHook prints (or, with --write, merges) the PreToolUse hook
// entry into the project's .claude/settings.json (#95).
func cmdInstallClaudeHook(args []string) int {
	write := false
	for _, a := range args {
		if a == "--write" {
			write = true
		}
	}
	if !write {
		ui.Info("add this to .claude/settings.json (project) so Claude Code gets multi-window collision awareness:")
		ui.Info("  • PreToolUse — collision-check every agent edit (#95)")
		ui.Info("  • PostToolUse — mirror each window's TodoWrite list so `wt todos` shows what every window is on")
		ui.Info("  • UserPromptSubmit — per-turn snapshot of file overlaps + coordination holds/announcements")
		fmt.Println(claudeHookSnippet)
		ui.Info("or run `wt install-claude-hook --write` to merge all three automatically")
		ui.Info("advisory by default; WT_CLAUDE_HOOK_BLOCK=1 makes a HIGH collision a hard deny; WT_SKIP_COLLISION=1 bypasses")
		ui.Info("tip: set WT_MAX_AGE (e.g. 5d) to keep the per-turn hook from flagging stale/abandoned worktrees as HIGH")
		return 0
	}
	return withConfig(func(c *config.Config) int {
		path := filepath.Join(c.Root, ".claude", "settings.json")
		merged, changed, err := mergeClaudeHook(path)
		if err != nil {
			ui.Err("install-claude-hook: %v (add the snippet by hand: `wt install-claude-hook`)", err)
			return 1
		}
		if !changed {
			ui.OK("Claude Code hooks (PreToolUse + PostToolUse + UserPromptSubmit) already wired in %s", path)
			return 0
		}
		if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
			ui.Err("install-claude-hook: %v", err)
			return 1
		}
		if err := os.WriteFile(path, merged, 0o644); err != nil {
			ui.Err("install-claude-hook: %v", err)
			return 1
		}
		ui.OK("wired the Claude hooks (PreToolUse + PostToolUse + UserPromptSubmit) into %s", path)
		ui.Info("advisory by default; WT_CLAUDE_HOOK_BLOCK=1 makes a HIGH collision a hard deny")
		ui.Info("`wt todos` will populate once the agent uses the TodoWrite tool in a window")
		return 0
	})
}

// mergeClaudeHook reads .claude/settings.json (a fresh {} if absent), ensures the
// PreToolUse (Edit|Write|MultiEdit), PostToolUse (TodoWrite) and UserPromptSubmit
// entries running our commands are present WITHOUT clobbering any existing hooks,
// and returns the pretty-printed result + whether it changed. An unparseable file
// is a refuse (err) — never overwrite blind.
func mergeClaudeHook(path string) (out []byte, changed bool, err error) {
	root := map[string]any{}
	if b, rerr := os.ReadFile(path); rerr == nil {
		if jerr := json.Unmarshal(b, &root); jerr != nil {
			return nil, false, fmt.Errorf("%s is not valid JSON", path)
		}
	}
	hooks, _ := root["hooks"].(map[string]any)
	if hooks == nil {
		hooks = map[string]any{}
	}
	c1 := ensureHookEntry(hooks, "PreToolUse", "Edit|Write|MultiEdit", claudeHookCommand)
	c2 := ensureHookEntry(hooks, "UserPromptSubmit", "", claudeContextCommand)
	c3 := ensureHookEntry(hooks, "PostToolUse", "TodoWrite", todoWriteCommand)
	if !c1 && !c2 && !c3 {
		return nil, false, nil
	}
	root["hooks"] = hooks
	b, err := json.MarshalIndent(root, "", "  ")
	if err != nil {
		return nil, false, err
	}
	return append(b, '\n'), true, nil
}
