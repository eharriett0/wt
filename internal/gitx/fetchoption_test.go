package gitx

import (
	"os"
	"path/filepath"
	"runtime"
	"testing"
)

// #243 review: git accepts a remote named `--upload-pack=<cmd>`, and passed
// to `git fetch` as an argument that name is an option that runs <cmd>. A
// no-prompt fetch passes --end-of-options, so it is only ever a remote name.
func TestFetchNoPrompt_remoteIsNeverAnOption(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("touch")
	}
	repo := gitRepo(t)
	marker := filepath.Join(t.TempDir(), "ran")
	t.Chdir(repo)
	if err := FetchPRHeadNoPrompt("--upload-pack=touch "+marker, "5"); err == nil {
		t.Errorf("a fetch from a remote named like an option worked")
	}
	if _, err := os.Stat(marker); err == nil {
		t.Errorf("a remote named --upload-pack=<cmd> ran <cmd>")
	}
}
