package gitx

import (
	"os"
	"strings"
)

// ChangedRangesNewWith is ChangedRangesNew for a file as a pending edit will
// leave it (#226): content goes in as a blob through the file's clean filters,
// into a throwaway object directory (the repository's objects are only read),
// exactly as ChangedRangesWith does, and the ranges come back in NEW-frame
// line numbers, the frame a structured doc's sections are read in (#123): its
// own content's lines. It must BE ChangedRangesNew's measurement once the
// content is written, fallbacks included (#226 review: each one it lacked
// read as "unmeasurable" and dropped an entry `wt check` then graded HIGH):
//   - measured from the merge base when the branch is behind base, else from
//     base, as `git diff <rev> -- file` reads it: a side that doesn't have the
//     file is empty, and an untracked file is not the working tree's (a file
//     base has reads as deleted, one it lacks as no edits);
//   - no base: from HEAD (#247), and with no HEAD either the uncommitted
//     ranges (index to the copy, plus what is staged);
//   - a diff that fails: the whole file.
//
// ok=false only when the copy can't be put in as a blob at all; the caller
// then keeps a heads-up, never "no edits".
func ChangedRangesNewWith(dir, base, file string, content []byte) ([]LineRange, bool) {
	e, ok := ChangedDocEditsWith(dir, base, file, content)
	return e.New, ok
}

// ChangedDocEditsWith is ChangedDocEdits for the copy a pending edit will
// leave (#237): ChangedRangesNewWith's measurement, both sides of its diff,
// so the edit hooks see the lines the edit deletes as `wt check` will, and a
// behind branch's edits in base's frame (#247).
func ChangedDocEditsWith(dir, base, file string, content []byte) (DocEdits, bool) {
	objects, err := run(dir, "rev-parse", "--path-format=absolute", "--git-path", "objects")
	if err != nil || objects == "" || strings.ContainsAny(objects, ":\"") {
		return DocEdits{}, false
	}
	tmp, err := os.MkdirTemp("", "wt-pending-")
	if err != nil {
		return DocEdits{}, false
	}
	defer os.RemoveAll(tmp)
	env := []string{"GIT_OBJECT_DIRECTORY=" + tmp, "GIT_ALTERNATE_OBJECT_DIRECTORIES=" + objects}
	hash := func(b []byte, args ...string) string {
		out, err := gitOutput(dir, env, b, append([]string{"hash-object", "-w", "--stdin"}, args...)...)
		if err != nil {
			return ""
		}
		return strings.TrimSpace(string(out))
	}
	// The copy goes through the file's clean filters, as `git add` would put
	// it; the empty side is git's /dev/null, which no filter ever sees (#226
	// re-review: a filter that adds a header, or one that fails on empty
	// input, moved or lost every range).
	blob, empty := hash(content, "--path", file), hash(nil, "--no-filters")
	if blob == "" || empty == "" {
		return DocEdits{}, false
	}
	failed := DocEdits{New: wholeFile, All: true}
	diff := func(old, new string) (DocEdits, string, error) {
		out, err := gitOutput(dir, env, nil, gradeDiff([]string{old, new}, "")...)
		if err != nil {
			return DocEdits{}, "", err
		}
		return docEditsFrom(dir, old, string(out), old == empty), string(out), nil
	}
	// The working tree's side of `git diff <rev> -- file`: the copy when the
	// index tracks the file, else nothing.
	copySide := empty
	if tracked, terr := runRaw(dir, "ls-files", "-z", "--", literalPath(file)); terr != nil || tracked != "" {
		copySide = blob // can't tell: measure the copy, the louder reading
	}
	revSide := func(rev string) string {
		if _, err := RunDir(dir, "cat-file", "-e", rev+":"+file); err != nil {
			return empty
		}
		return rev + ":" + file
	}

	ref, sha, hasBase := resolveBaseRef(dir, base)
	if !hasBase {
		head, herr := headCommit(dir)
		if herr != nil {
			return failed, true
		}
		if head {
			e, _, err := diff(revSide("HEAD"), copySide)
			if err != nil {
				return failed, true
			}
			return e, true
		}
		// No HEAD: uncommittedRangesNew, index to working tree plus HEAD to
		// index; the new side only, as ChangedDocEdits reads it there.
		var ranges []LineRange
		if copySide == blob {
			e, _, err := diff(revSide(":0"), blob)
			if err != nil {
				return failed, true
			}
			ranges = e.New
		}
		staged, err := runRaw(dir, gradeDiff([]string{"--cached"}, file)...)
		if err != nil {
			return failed, true
		}
		return DocEdits{New: append(ranges, parseHunkRanges(staged)...)}, true
	}
	if mb := behindMergeBase(dir, ref, sha, file); mb != "" {
		if e, out, err := diff(revSide(mb), copySide); err == nil {
			return withBaseClaims(dir, ref, mb, file, out, e), true
		}
	}
	e, _, err := diff(revSide(ref), copySide)
	if err != nil {
		return failed, true
	}
	return e, true
}
