package cli

import (
	"os"
	"path/filepath"
	"testing"
)

// TestReadForPrediction_ReadsEveryRegularFile pins what the edit hooks read to
// predict an edit (#201 review): a regular file whatever its size, so a large
// tracked file keeps its hunk grade (an 8 MiB cap, tried in review, cost an
// 11.7 MB file a Codex deny and gave a disjoint Claude edit a false deny);
// anything that is not a regular file is unread, never read; nothing there is
// missing.
func TestReadForPrediction_ReadsEveryRegularFile(t *testing.T) {
	dir := t.TempDir()
	big := filepath.Join(dir, "big.txt")
	f, err := os.Create(big)
	if err != nil {
		t.Fatal(err)
	}
	const size = 9 << 20 // past the cap that was tried, sparse so it costs no disk
	if err := f.Truncate(size); err != nil {
		t.Fatal(err)
	}
	f.Close()
	if got, how := readForPrediction(big); how != fileReadOK || len(got) != size {
		t.Errorf("a %d-byte regular file: read %d bytes, %v; want all of it, fileReadOK", size, len(got), how)
	}
	link := filepath.Join(dir, "link.txt")
	if err := os.Symlink(big, link); err != nil {
		t.Fatal(err)
	}
	if got, how := readForPrediction(link); how != fileReadOK || len(got) != size {
		t.Errorf("a link to it: read %d bytes, %v; want the target, fileReadOK", len(got), how)
	}
	sub := filepath.Join(dir, "sub")
	if err := os.Mkdir(sub, 0o755); err != nil {
		t.Fatal(err)
	}
	if got, how := readForPrediction(sub); how != fileUnread || got != "" {
		t.Errorf("a directory: %q, %v; want unread", got, how)
	}
	if got, how := readForPrediction(filepath.Join(dir, "nope.txt")); how != fileMissing || got != "" {
		t.Errorf("nothing there: %q, %v; want missing", got, how)
	}
}
