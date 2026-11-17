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
//   - no base: the uncommitted ranges (index to the copy, plus what is staged);
//   - a diff that fails: the whole file.
//
// ok=false only when the copy can't be put in as a blob at all; the caller
// then keeps a heads-up, never "no edits".
func ChangedRangesNewWith(dir, base, file string, content []byte) ([]LineRange, bool) {
	objects, err := run(dir, "rev-parse", "--path-format=absolute", "--git-path", "objects")
	if err != nil || objects == "" || strings.ContainsAny(objects, ":\"") {
		return nil, false
	}
	tmp, err := os.MkdirTemp("", "wt-pending-")
	if err != nil {
		return nil, false
	}
	defer os.RemoveAll(tmp)
	env := []string{"GIT_OBJECT_DIRECTORY=" + tmp, "GIT_ALTERNATE_OBJECT_DIRECTORIES=" + objects}
	hash := func(b []byte) string {
		out, err := gitOutput(dir, env, b, "hash-object", "-w", "--stdin", "--path", file)
		if err != nil {
			return ""
		}
		return strings.TrimSpace(string(out))
	}
	blob, empty := hash(content), hash(nil)
	if blob == "" || empty == "" {
		return nil, false
	}
	diff := func(old, new string) ([]LineRange, error) {
		out, err := gitOutput(dir, env, nil, gradeDiff([]string{old, new}, "")...)
		if err != nil {
			return nil, err
		}
		return parseHunkRanges(string(out)), nil
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
		// uncommittedRangesNew: index to working tree, plus HEAD to index.
		var ranges []LineRange
		if copySide == blob {
			r, err := diff(revSide(":0"), blob)
			if err != nil {
				return wholeFile, true
			}
			ranges = r
		}
		staged, err := runRaw(dir, gradeDiff([]string{"--cached"}, file)...)
		if err != nil {
			return wholeFile, true
		}
		return append(ranges, parseHunkRanges(staged)...), true
	}
	if mb := behindMergeBase(dir, ref, sha, file); mb != "" {
		if r, err := diff(revSide(mb), copySide); err == nil {
			return r, true
		}
	}
	r, err := diff(revSide(ref), copySide)
	if err != nil {
		return wholeFile, true
	}
	return r, true
}
