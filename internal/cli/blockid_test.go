package cli

import (
	"os"
	"path/filepath"
	"testing"
)

// #152: fileHasBlock is the "is this block actually written" check --written uses
// so it can't clear a reservation for a block that isn't in the file.
func TestFileHasBlock(t *testing.T) {
	re, err := blockPatternRe("NEWEST-{n}")
	if err != nil {
		t.Fatal(err)
	}
	dir := t.TempDir()
	f := filepath.Join(dir, "resume.md")
	if err := os.WriteFile(f, []byte("## NEWEST-5 foo\nbody\n## NEWEST-7 bar\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	for n, want := range map[int]bool{5: true, 7: true, 6: false, 99: false} {
		got, err := fileHasBlock(f, re, n)
		if err != nil {
			t.Fatal(err)
		}
		if got != want {
			t.Errorf("fileHasBlock(%d) = %v, want %v", n, got, want)
		}
	}
	// missing file → false, no error (a fresh append-log)
	if got, err := fileHasBlock(filepath.Join(dir, "nope.md"), re, 5); err != nil || got {
		t.Errorf("missing file: got=%v err=%v, want false/nil", got, err)
	}
}

func TestBlockPatternRe(t *testing.T) {
	re, err := blockPatternRe("NEWEST-{n}")
	if err != nil {
		t.Fatal(err)
	}
	if m := re.FindStringSubmatch("## NEWEST-56 lane header"); m == nil || m[1] != "56" {
		t.Errorf("NEWEST-56 → %v, want capture 56", m)
	}

	// Literal special chars are escaped — the '.' must not act as any-char.
	re2, err := blockPatternRe("v{n}.log")
	if err != nil {
		t.Fatal(err)
	}
	if m := re2.FindStringSubmatch("v3.log"); m == nil || m[1] != "3" {
		t.Errorf("v3.log → %v, want 3", m)
	}
	if re2.MatchString("v3xlog") {
		t.Error("'.' should be a literal, but it matched v3xlog")
	}

	if _, err := blockPatternRe("NEWEST-"); err == nil {
		t.Error("pattern missing {n} should error")
	}
}

func TestScanFileMaxBlock(t *testing.T) {
	re, err := blockPatternRe("NEWEST-{n}")
	if err != nil {
		t.Fatal(err)
	}

	// Missing file → 0, no error (bootstrap from an empty/absent doc).
	if n, err := scanFileMaxBlock(filepath.Join(t.TempDir(), "nope.md"), re); err != nil || n != 0 {
		t.Errorf("missing file → (%d,%v), want (0,nil)", n, err)
	}

	f := filepath.Join(t.TempDir(), "resume.md")
	content := "## NEWEST-54 lane\nsome text\n## NEWEST-55 other lane\nNEWEST-12 inline\n"
	if err := os.WriteFile(f, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
	n, err := scanFileMaxBlock(f, re)
	if err != nil {
		t.Fatal(err)
	}
	if n != 55 {
		t.Errorf("max block = %d, want 55", n)
	}

	empty := filepath.Join(t.TempDir(), "empty.md")
	if err := os.WriteFile(empty, []byte("no ids here\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if n, _ := scanFileMaxBlock(empty, re); n != 0 {
		t.Errorf("no-match file → %d, want 0", n)
	}
}
