package gitx

import (
	"crypto/sha1"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
)

// #210: three git mechanisms hide an edit to a TRACKED file from `git status`,
// and so from `git worktree remove`'s own check, which then deletes the edit
// with the worktree (measured, git 2.39):
//
//   - `git update-index --assume-unchanged <file>`;
//   - `git update-index --skip-worktree <file>`;
//   - `core.ignoreStat=true`, which sets the assume-unchanged flag on every file
//     git writes to the work tree (a new worktree's checkout included).
//
// `git ls-files -v` shows both flags (an assume-unchanged entry's tag in lower
// case, a skip-worktree one as S), so StatusEntries reads them there and
// compares each flagged file that exists with its index blob.

// hiddenEntry is one stage-0 index entry `git status` does not compare with its
// file: why names the flag.
type hiddenEntry struct {
	path, mode, sha, why string
}

// parseHiddenEntries reads `git ls-files -z -s -v` ("<tag> <mode> <sha>
// <stage>\t<path>" records) and returns the stage-0 entries flagged
// assume-unchanged (lower-case tag) or skip-worktree (S or s). A conflicted
// entry (stage 1-3) is skipped: status lists its path as unmerged anyway. Pure.
func parseHiddenEntries(out string) []hiddenEntry {
	var hidden []hiddenEntry
	for _, rec := range strings.Split(out, "\x00") {
		meta, path, ok := strings.Cut(rec, "\t")
		f := strings.Fields(meta)
		if !ok || path == "" || len(f) != 4 || len(f[0]) != 1 || f[3] != "0" {
			continue
		}
		tag := f[0][0]
		assumed := tag >= 'a' && tag <= 'z'
		skipped := tag == 'S' || tag == 's'
		var why string
		switch {
		case assumed && skipped:
			why = "skip-worktree and assume-unchanged"
		case skipped:
			why = "skip-worktree"
		case assumed:
			why = "assume-unchanged"
		default:
			continue
		}
		hidden = append(hidden, hiddenEntry{path: path, mode: f[1], sha: f[2], why: why})
	}
	return hidden
}

// hiddenEditLine is how StatusEntries lists a hidden entry: " M" when the file
// differs from the index, " ?" when that could not be told (and so it counts as
// a change). Pure.
func hiddenEditLine(e hiddenEntry, differs bool) string {
	if differs {
		return fmt.Sprintf(" M %s  (%s: git status does not show it)", e.path, e.why)
	}
	return fmt.Sprintf(" ? %s  (%s: could not be compared with the index, so it counts as changed)", e.path, e.why)
}

// blobID is the id git gives a blob holding content: sha1, or sha256 when the
// repository's ids are 64 hex digits long. ok is false for any other length.
// Pure.
func blobID(content []byte, idLen int) (string, bool) {
	header := fmt.Sprintf("blob %d\x00", len(content))
	switch idLen {
	case 40:
		h := sha1.New()
		h.Write([]byte(header))
		h.Write(content)
		return hex.EncodeToString(h.Sum(nil)), true
	case 64:
		h := sha256.New()
		h.Write([]byte(header))
		h.Write(content)
		return hex.EncodeToString(h.Sum(nil)), true
	}
	return "", false
}

// cQuote writes p the way `git hash-object --stdin-paths` unquotes a line that
// starts with a double quote: backslash and quote escaped, control bytes as
// three-digit octal, everything else as is. Quoting every path keeps a name
// holding a newline, a trailing CR or a leading quote one record. Pure.
func cQuote(p string) string {
	var b strings.Builder
	b.WriteByte('"')
	for i := 0; i < len(p); i++ {
		switch c := p[i]; {
		case c == '"' || c == '\\':
			b.WriteByte('\\')
			b.WriteByte(c)
		case c < 0x20 || c == 0x7f:
			fmt.Fprintf(&b, "\\%03o", c)
		default:
			b.WriteByte(c)
		}
	}
	b.WriteByte('"')
	return b.String()
}

// hiddenEdits returns a status line for every hidden entry (parseHiddenEntries)
// of the worktree at dir whose file exists and is not what the index holds
// (#210). A file the entry no longer has on disk loses nothing when the worktree
// goes, so it is skipped. Anything that can't be compared (an unreadable file, a
// type git status would call a change, a hash that fails) is listed as changed:
// fail closed. A regular file is hashed by git itself (`hash-object
// --stdin-paths`, run in dir), so the repository's clean and eol filters apply
// as they do to a `git add`. Read-only: neither command writes the index or an
// object.
func hiddenEdits(dir string) ([]string, error) {
	out, err := runRawReadOnly(dir, "ls-files", "-z", "-s", "-v")
	if err != nil {
		return nil, err
	}
	var lines []string
	var toHash []hiddenEntry
	for _, e := range parseHiddenEntries(out) {
		fi, err := os.Lstat(filepath.Join(dir, e.path))
		switch {
		case errors.Is(err, fs.ErrNotExist):
			continue // nothing on disk to lose
		case err != nil:
			lines = append(lines, hiddenEditLine(e, false))
		case e.mode == "120000":
			lines = append(lines, symlinkEditLines(dir, e, fi)...)
		case e.mode == "100644" || e.mode == "100755":
			if !fi.Mode().IsRegular() {
				lines = append(lines, hiddenEditLine(e, true)) // now a directory or a link
				continue
			}
			toHash = append(toHash, e)
		default: // a gitlink, or a mode wt does not know: nothing to compare with
			lines = append(lines, hiddenEditLine(e, false))
		}
	}
	return append(lines, hashCompare(dir, toHash)...), nil
}

// symlinkEditLines compares a hidden symlink entry with the link on disk: the
// blob is the link's target.
func symlinkEditLines(dir string, e hiddenEntry, fi fs.FileInfo) []string {
	if fi.Mode()&fs.ModeSymlink == 0 {
		return []string{hiddenEditLine(e, true)}
	}
	target, err := os.Readlink(filepath.Join(dir, e.path))
	if err != nil {
		return []string{hiddenEditLine(e, false)}
	}
	id, ok := blobID([]byte(target), len(e.sha))
	if !ok {
		return []string{hiddenEditLine(e, false)}
	}
	if id != e.sha {
		return []string{hiddenEditLine(e, true)}
	}
	return nil
}

// hashCompare hashes the files of entries with one `git hash-object
// --stdin-paths` in dir and lists those whose id is not the index's. When that
// fails (one unreadable file fails the whole run) each file is hashed on its
// own, and one that still can't be hashed counts as changed.
func hashCompare(dir string, entries []hiddenEntry) []string {
	if len(entries) == 0 {
		return nil
	}
	var in strings.Builder
	for _, e := range entries {
		in.WriteString(cQuote(e.path) + "\n")
	}
	ids, err := hashObjects(dir, in.String())
	var lines []string
	for i, e := range entries {
		id := ""
		if err == nil && len(ids) == len(entries) {
			id = ids[i]
		} else if one, oerr := hashObjects(dir, "", "--", e.path); oerr == nil && len(one) == 1 {
			id = one[0]
		}
		switch {
		case id == "":
			lines = append(lines, hiddenEditLine(e, false))
		case id != e.sha:
			lines = append(lines, hiddenEditLine(e, true))
		}
	}
	return lines
}

// hashObjects runs `git hash-object` in dir: with stdin, as --stdin-paths fed
// those paths; otherwise on the files args name. It returns the ids, in order.
// Nothing is written to the object store.
func hashObjects(dir, stdin string, args ...string) ([]string, error) {
	if stdin != "" {
		args = append([]string{"--stdin-paths"}, args...)
	}
	cmd := exec.Command("git", append([]string{"hash-object"}, args...)...)
	cmd.Dir = dir
	cmd.Env = readOnlyEnv(scopedEnv())
	cmd.Stdin = strings.NewReader(stdin)
	out, err := cmd.Output()
	if err != nil {
		return nil, err
	}
	return strings.Fields(string(out)), nil
}
