package claim

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// #238: a claim made under a relative worktree_root, before #238, recorded a
// relative Worktree path. `release --clean` reads it from the main checkout,
// as the root now is, so it finds the lane from a subdirectory too; read from
// the cwd, it was "already gone" and the lane stayed.
func TestRelease_CleanReadsARelativeRecordFromTheMainCheckout(t *testing.T) {
	f := newClaimFixture(t)
	if err := f.claim(true); err != nil {
		t.Fatalf("claim: %v", err)
	}
	rel, err := filepath.Rel(f.repo, f.dir())
	if err != nil || filepath.IsAbs(rel) {
		t.Fatalf("precondition: %q relative to %q: %q, %v", f.dir(), f.repo, rel, err)
	}
	b, err := os.ReadFile(f.c.ActiveWork)
	if err != nil {
		t.Fatal(err)
	}
	recorded := strings.Replace(string(b), "`"+f.dir()+"`", "`"+rel+"`", 1)
	if recorded == string(b) {
		t.Fatalf("precondition: active-work doesn't record %s:\n%s", f.dir(), b)
	}
	if err := os.WriteFile(f.c.ActiveWork, []byte(recorded), 0o644); err != nil {
		t.Fatal(err)
	}
	f.c.MainRoot = f.repo
	sub := filepath.Join(f.repo, "sub")
	if err := os.MkdirAll(sub, 0o755); err != nil {
		t.Fatal(err)
	}
	t.Chdir(sub)

	out, err := capturedRelease(t, f)
	if err != nil {
		t.Fatalf("Release: %v\n%s", err, out)
	}
	if _, serr := os.Stat(f.dir()); serr == nil {
		t.Errorf("the lane was kept (read from the cwd?)\n%s", out)
	}
	if strings.Contains(out, "already gone") {
		t.Errorf("release read the relative record from the cwd:\n%s", out)
	}
}
