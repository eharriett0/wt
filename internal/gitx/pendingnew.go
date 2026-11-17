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
// own content's lines. Measured from the merge base when the branch is behind
// base, as ChangedRangesNew is. ok=false when git couldn't measure them (no
// base, a git error): the caller keeps its pre-edit grade, never "no edits".
func ChangedRangesNewWith(dir, base, file string, content []byte) ([]LineRange, bool) {
	ref, sha, hasBase := resolveBaseRef(dir, base)
	if !hasBase {
		return nil, false
	}
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
	out, err := gitOutput(dir, env, content, "hash-object", "-w", "--stdin", "--path", file)
	blob := strings.TrimSpace(string(out))
	if err != nil || blob == "" {
		return nil, false
	}
	pending := &pendingSide{blob: blob, env: env}
	from := ref
	if mb := behindMergeBase(dir, ref, sha, file); mb != "" {
		from = mb
	}
	diff, err := pending.diffFrom(dir, from, file)
	if err != nil {
		return nil, false
	}
	return parseHunkRanges(diff), true
}
