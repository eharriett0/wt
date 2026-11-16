package cli

import (
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"
)

// codexApplyCase is a patch over some files and what apply_patch leaves (nil
// want: it rejects or fails on the patch). Each want was measured with the
// apply_patch the Codex app ships (TestApplyPatchOps_MatchesCodex re-measures
// them where that binary is installed).
type codexApplyCase struct {
	name  string
	files map[string]string
	patch string
	want  map[string]string // the files it writes; nil: apply_patch rejects or fails
	gone  []string          // the files it deletes
}

func bounded(body string) string { return "*** Begin Patch\n" + body + "*** End Patch\n" }

var codexApplyCases = []codexApplyCase{
	{"replace a line", map[string]string{"f": "a\nb\nc\n"}, bounded("*** Update File: f\n@@\n a\n-b\n+B\n c\n"), map[string]string{"f": "a\nB\nc\n"}, nil},
	// #199 review: apply_patch ends every file it writes with a newline, so a
	// file without one also changes its last line, wherever the hunk is.
	{"no final newline: the last line gains one", map[string]string{"f": "a\nb\nc"}, bounded("*** Update File: f\n@@\n a\n-b\n+B\n c\n"), map[string]string{"f": "a\nB\nc\n"}, nil},
	{"no final newline, edit far from the end", map[string]string{"f": "a\nb\nc\nd\ne"}, bounded("*** Update File: f\n@@\n a\n-b\n+B\n c\n"), map[string]string{"f": "a\nB\nc\nd\ne\n"}, nil},
	{"no final newline, append after the last line", map[string]string{"f": "a\nb\nc"}, bounded("*** Update File: f\n@@\n c\n+d\n"), map[string]string{"f": "a\nb\nc\nd\n"}, nil},
	{"a hunk with no context appends", map[string]string{"f": "a\nb\nc\n"}, bounded("*** Update File: f\n@@\n+X\n"), map[string]string{"f": "a\nb\nc\nX\n"}, nil},
	{"appending above a final blank line takes its place", map[string]string{"f": "a\nb\n\n"}, bounded("*** Update File: f\n@@\n+X\n"), map[string]string{"f": "a\nb\nX\n"}, nil},
	{"first match from the top", map[string]string{"f": "x\ny\nx\ny\n"}, bounded("*** Update File: f\n@@\n x\n-y\n+Y\n"), map[string]string{"f": "x\nY\nx\ny\n"}, nil},
	{"End of File anchors at the end", map[string]string{"f": "x\ny\nx\ny\n"}, bounded("*** Update File: f\n@@\n x\n-y\n+Y\n*** End of File\n"), map[string]string{"f": "x\ny\nx\nY\n"}, nil},
	{"@@ context line locates the chunk", map[string]string{"f": "def a():\n  x\ndef b():\n  x\n"}, bounded("*** Update File: f\n@@ def b():\n-  x\n+  X\n"), map[string]string{"f": "def a():\n  x\ndef b():\n  X\n"}, nil},
	// The header drops trailing whitespace ("z  " is sought as "z"), and so does
	// the End of File anchor; the lines under it are taken as written.
	{"@@ header's trailing whitespace dropped", map[string]string{"f": "z\nq\nz  \nq"}, bounded("*** Update File: f\n@@ z  \n-q\n+Q\n"), map[string]string{"f": "z\nQ\nz  \nq\n"}, nil},
	{"@@ with only whitespace after it", map[string]string{"f": "a\nq\n"}, bounded("*** Update File: f\n@@\t\n-q\n+Q\n"), map[string]string{"f": "a\nQ\n"}, nil},
	{"End of File with trailing whitespace", map[string]string{"f": "x\ny\nx\ny\n"}, bounded("*** Update File: f\n@@\n x\n-y\n+Y\n*** End of File  \n"), map[string]string{"f": "x\ny\nx\nY\n"}, nil},
	{"a blank line after a chunk is skipped", map[string]string{"f": "w\nx\n"}, bounded("*** Update File: f\n@@\n-x\n+X\n*** End of File\n  \n"), map[string]string{"f": "w\nX\n"}, nil},
	{"a space-only first line is a context line", map[string]string{"f": "b\n  y\na\n\n"}, bounded("*** Update File: f\n  \n"), map[string]string{"f": "b\n  y\na\n \n"}, nil},
	{"a space-only first line, then an End of File chunk", map[string]string{"f": "\u201cs\u201d\n\n\tx\n  y\n\n"}, bounded("*** Update File: f\n \n-\n+q\n*** End of File\n@@\n+q\n"), map[string]string{"f": "\u201cs\u201d\n\n\tx\n  y\n\nq\n"}, nil},
	{"a tab-only first line is rejected", map[string]string{"f": "a\nq\n"}, bounded("*** Update File: f\n\t\n@@\n-q\n+Q\n"), nil, nil},
	{"'-' lines keep their trailing whitespace", map[string]string{"f": "z  \r\n\r\na\r\nz  \r\n}\r\nz  "}, bounded("*** Update File: f\n@@ z  \n-z  \n"), map[string]string{"f": "z  \r\n\r\na\r\nz  \r\n}\r\n"}, nil},
	// Context lines are written as the patch spells them: fuzzy matching drops a
	// trailing blank, CRLF or indentation the file had.
	{"trailing whitespace on a context line is rewritten", map[string]string{"f": "a  \nb\n"}, bounded("*** Update File: f\n@@\n a\n-b\n+B\n"), map[string]string{"f": "a\nB\n"}, nil},
	{"leading whitespace on a context line is rewritten", map[string]string{"f": "  a\nb\n"}, bounded("*** Update File: f\n@@\n a\n-b\n+B\n"), map[string]string{"f": "a\nB\n"}, nil},
	{"CRLF context lines lose their CR", map[string]string{"f": "a\r\nb\r\nc\r\n"}, bounded("*** Update File: f\n@@\n a\n-b\n+B\n c\n"), map[string]string{"f": "a\nB\nc\n"}, nil},
	{"typographic punctuation matches ASCII", map[string]string{"f": "a \u2013 b\nsay \u201chi\u201d\nc\n"}, bounded("*** Update File: f\n@@\n a - b\n say \"hi\"\n-c\n+C\n"), map[string]string{"f": "a - b\nsay \"hi\"\nC\n"}, nil},
	{"a trailing-whitespace match wins over an earlier surrounding one", map[string]string{"f": "  x\nx \nc\n"}, bounded("*** Update File: f\n@@\n-x\n+X\n"), map[string]string{"f": "  x\nX\nc\n"}, nil},
	{"an exact match wins over an earlier fuzzy one", map[string]string{"f": "  x\nx\nc\n"}, bounded("*** Update File: f\n@@\n x\n-c\n+C\n"), map[string]string{"f": "  x\nx\nC\n"}, nil},
	{"a trailing blank context line stands for the final newline", map[string]string{"f": "a\nb"}, bounded("*** Update File: f\n@@\n a\n-b\n+B\n\n"), map[string]string{"f": "a\nB\n"}, nil},
	{"a bare blank line is context", map[string]string{"f": "a\n\nb\nc\n"}, bounded("*** Update File: f\n@@\n a\n\n-b\n+B\n"), map[string]string{"f": "a\n\nB\nc\n"}, nil},
	{"chunks in order", map[string]string{"f": "a\nb\nc\nd\n"}, bounded("*** Update File: f\n@@\n-a\n+A\n@@\n-d\n+D\n"), map[string]string{"f": "A\nb\nc\nD\n"}, nil},
	{"the first chunk may omit @@", map[string]string{"f": "a\nb\n"}, bounded("*** Update File: f\n a\n-b\n+B\n"), map[string]string{"f": "a\nB\n"}, nil},
	{"two updates of one file", map[string]string{"f": "a\nb\n"}, bounded("*** Update File: f\n@@\n-a\n+A\n*** Update File: f\n@@\n-b\n+B\n"), map[string]string{"f": "A\nB\n"}, nil},
	{"heredoc-wrapped", map[string]string{"f": "a\nb\n"}, "<<'EOF'\n*** Begin Patch\n*** Update File: f\n@@\n-b\n+B\n*** End Patch\nEOF\n", map[string]string{"f": "a\nB\n"}, nil},
	{"padded markers", map[string]string{"f": "a\nb\n"}, "\n *** Begin Patch \n*** Update File: f\n@@\n-b\n+B\n *** End Patch \n", map[string]string{"f": "a\nB\n"}, nil},
	{"add over an existing file", map[string]string{"f": "a\n"}, bounded("*** Add File: f\n+new\n"), map[string]string{"f": "new\n"}, nil},
	{"delete", map[string]string{"f": "a\n", "g": "b\n"}, bounded("*** Delete File: f\n"), map[string]string{}, []string{"f"}},
	{"move", map[string]string{"f": "a\nb\n"}, bounded("*** Update File: f\n*** Move to: g\n@@\n-a\n+A\n"), map[string]string{"g": "A\nb\n"}, []string{"f"}},
	{"update an empty file", map[string]string{"f": ""}, bounded("*** Update File: f\n@@\n+x\n"), map[string]string{"f": "x\n"}, nil},
	// Rejected or failing: the hook can't say what the patch does.
	{"no Begin Patch", map[string]string{"f": "a\nb\n"}, "*** Update File: f\n@@\n-b\n+B\n*** End Patch\n", nil, nil},
	{"lines not found", map[string]string{"f": "a\n"}, bounded("*** Update File: f\n@@\n-zzz\n+Z\n"), nil, nil},
	{"a later chunk can't reach back", map[string]string{"f": "a\nb\nc\n"}, bounded("*** Update File: f\n@@\n-a\n-b\n+A\n@@\n-b\n+B\n"), nil, nil},
	{"tabs don't match spaces", map[string]string{"f": "a\tb\nc\n"}, bounded("*** Update File: f\n@@\n a b\n-c\n+C\n"), nil, nil},
	{"update of a missing file", map[string]string{}, bounded("*** Update File: f\n@@\n+x\n"), nil, nil},
	{"a stray line in an update", map[string]string{"f": "a\nb"}, bounded("*** Update File: f\n@@\n-b\n+B\n\\ No newline at end of file\n"), nil, nil},
}

// apply_patch, emulated: what each file holds after the patch (#199 review).
func TestApplyPatchOps(t *testing.T) {
	for _, c := range codexApplyCases {
		got, err := emulateCodex(c.files, c.patch)
		if c.want == nil {
			if err == nil {
				t.Errorf("%s: applied, want an error (apply_patch rejects or fails)", c.name)
			}
			continue
		}
		if err != nil {
			t.Errorf("%s: %v", c.name, err)
			continue
		}
		if want := expectedFiles(c); !reflect.DeepEqual(got, want) {
			t.Errorf("%s: files after = %q, want %q", c.name, got, want)
		}
	}
}

// The emulation against the apply_patch the Codex app ships, where installed
// (WT_CODEX_BIN, else the macOS app's bundled CLI): every case above.
func TestApplyPatchOps_MatchesCodex(t *testing.T) {
	bin := os.Getenv("WT_CODEX_BIN")
	if bin == "" {
		bin = "/Applications/ChatGPT.app/Contents/Resources/codex-cli/bin/codex"
	}
	if _, err := os.Stat(bin); err != nil {
		t.Skip("no Codex apply_patch here (set WT_CODEX_BIN)")
	}
	for _, c := range codexApplyCases {
		dir := t.TempDir()
		for name, content := range c.files {
			if err := os.WriteFile(filepath.Join(dir, name), []byte(content), 0o644); err != nil {
				t.Fatal(err)
			}
		}
		cmd := exec.Command(bin, "--codex-run-as-apply-patch", c.patch)
		cmd.Dir = dir
		cmd.Env = append(os.Environ(), "HOME="+t.TempDir(), "CODEX_HOME="+t.TempDir())
		runErr := cmd.Run()
		real := map[string]string{}
		entries, _ := os.ReadDir(dir)
		for _, e := range entries {
			b, _ := os.ReadFile(filepath.Join(dir, e.Name()))
			real[e.Name()] = string(b)
		}
		if c.want == nil {
			if runErr == nil && reflect.DeepEqual(real, c.files) {
				t.Errorf("%s: Codex applied nothing and exited 0; the case expects a failure", c.name)
			}
			if runErr == nil && !reflect.DeepEqual(real, c.files) {
				t.Errorf("%s: Codex applied it (%q); the emulation calls it an error", c.name, real)
			}
			continue
		}
		if runErr != nil {
			t.Errorf("%s: Codex failed (%v); the emulation applies it", c.name, runErr)
			continue
		}
		if want := expectedFiles(c); !reflect.DeepEqual(real, want) {
			t.Errorf("%s: Codex leaves %q, the case says %q", c.name, real, want)
		}
	}
}

// emulateCodex runs parseApplyPatch + applyPatchOps over files and returns
// every file afterwards (the ones it didn't touch included).
func emulateCodex(files map[string]string, patch string) (map[string]string, error) {
	ops, err := parseApplyPatch(patch)
	if err != nil {
		return nil, err
	}
	state, err := applyPatchOps(ops, func(p string) (string, bool) { c, ok := files[p]; return c, ok })
	if err != nil {
		return nil, err
	}
	out := map[string]string{}
	for p, c := range files {
		out[p] = c
	}
	for p, c := range state {
		if c == nil {
			delete(out, p)
		} else {
			out[p] = *c
		}
	}
	return out, nil
}

// expectedFiles is every file after case c: the untouched ones as they were,
// the deleted ones gone, the rest as c.want says.
func expectedFiles(c codexApplyCase) map[string]string {
	out := map[string]string{}
	for p, v := range c.files {
		out[p] = v
	}
	for _, p := range c.gone {
		delete(out, p)
	}
	for p, v := range c.want {
		out[p] = v
	}
	return out
}

// seekSequence's tiers, in order: exact, then trailing whitespace, then
// surrounding, then typographic punctuation; from start, or the end for an
// End of File chunk. Pure.
func TestSeekSequence(t *testing.T) {
	lines := []string{"a", "b  ", "  c", "\u2018q\u2019", "b"}
	cases := []struct {
		pattern []string
		start   int
		eof     bool
		want    int
		ok      bool
	}{
		{[]string{"b"}, 0, false, 4, true},  // exact beats the earlier fuzzy "b  "
		{[]string{"b"}, 0, true, 4, true},   // the end
		{[]string{"b "}, 0, false, 1, true}, // trailing whitespace
		{[]string{"c"}, 0, false, 2, true},  // surrounding whitespace
		{[]string{"'q'"}, 0, false, 3, true},
		{[]string{"a"}, 1, false, 0, false}, // not before start
		{[]string{"x"}, 0, false, 0, false},
		{nil, 3, false, 3, true},
		{[]string{"a", "b", "c", "d", "e", "f"}, 0, false, 0, false},
	}
	for _, c := range cases {
		got, ok := seekSequence(lines, c.pattern, c.start, c.eof)
		if ok != c.ok || (ok && got != c.want) {
			t.Errorf("seekSequence(%q, start %d, eof %v) = %d, %v; want %d, %v", c.pattern, c.start, c.eof, got, ok, c.want, c.ok)
		}
	}
	if got, ok := seekSequence([]string{"  x", "x "}, []string{"x"}, 0, false); !ok || got != 1 {
		t.Errorf("seekSequence: the trailing-whitespace tier comes before the surrounding one: %d, %v; want 1", got, ok)
	}
	if got := asciiPunct(" \u2014x\u00a0\u201cy\u201d "); got != `-x "y"` {
		t.Errorf("asciiPunct = %q", got)
	}
	if strings.TrimSpace(asciiPunct("\u3000")) != "" {
		t.Error("asciiPunct: an ideographic space is a space")
	}
}

// predictCodexPatch is what the Codex hook grades: each file the patch touches,
// repo-relative, as apply_patch leaves it ("" for a file it deletes, which `wt
// check` then reads as every line deleted). A patch it can't vouch for (one
// apply_patch rejects or fails on, a move across the repo's edge) is ok=false:
// the hook keeps its file-level heads-up. An operation entirely outside the repo
// is left out and the rest still predicted (#201 review).
func TestPredictCodexPatch(t *testing.T) {
	root := t.TempDir()
	if err := os.MkdirAll(filepath.Join(root, "pkg"), 0o755); err != nil {
		t.Fatal(err)
	}
	for name, content := range map[string]string{"pkg/a.txt": "a\nb", "pkg/d.txt": "d\n", "pkg/m.txt": "m\n"} {
		if err := os.WriteFile(filepath.Join(root, name), []byte(content), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	patch := bounded("*** Update File: a.txt\n@@\n-b\n+B\n*** Delete File: d.txt\n*** Update File: m.txt\n*** Move to: ../moved.txt\n@@\n-m\n+M\n*** Add File: n.txt\n+n\n")
	got, ok := predictCodexPatch(patch, root, "pkg/") // Codex runs in pkg/
	want := map[string]string{"pkg/a.txt": "a\nB\n", "pkg/d.txt": "", "pkg/m.txt": "", "moved.txt": "M\n", "pkg/n.txt": "n\n"}
	if !ok || !reflect.DeepEqual(got, want) {
		t.Errorf("predictCodexPatch = %q, %v; want %q, true", got, ok, want)
	}
	outDir := t.TempDir()
	for name, content := range map[string]string{"note.txt": "n1\nn2\n", "ten.txt": "t1\nt2\n"} {
		if err := os.WriteFile(filepath.Join(outDir, name), []byte(content), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	note, ten, added := filepath.Join(outDir, "note.txt"), filepath.Join(outDir, "ten.txt"), filepath.Join(outDir, "new.txt")
	big := filepath.Join(outDir, "big.txt") // over maxPredictBytes: never read
	if f, err := os.Create(big); err != nil {
		t.Fatal(err)
	} else if err := f.Truncate(maxPredictBytes + 1); err != nil {
		t.Fatal(err)
	} else {
		f.Close()
	}
	for name, p := range map[string]string{
		"rejected":        "*** Update File: a.txt\n@@\n-b\n+B\n",
		"lines not found": bounded("*** Update File: a.txt\n@@\n-zz\n+Z\n"),
		"missing file":    bounded("*** Update File: nope.txt\n@@\n+x\n"),
		// a move across the edge whose chunks don't apply: apply_patch rejects it
		"out of the repo, chunks not found": bounded("*** Update File: a.txt\n*** Move to: ../../x.txt\n@@\n-zz\n+Z\n"),
		"into the repo, chunks not found":   bounded("*** Update File: " + note + "\n*** Move to: a2.txt\n@@\n-zz\n+Z\n"),
		// an outside file is replayed too: one that isn't there fails apply_patch
		"an outside file that isn't there":  bounded("*** Update File: ../../x.txt\n@@\n+x\n*** Update File: a.txt\n@@\n-b\n+B\n"),
		"a move in from a source not there": bounded("*** Update File: ../../nope.txt\n*** Move to: a.txt\n@@\n+x\n"),
	} {
		if _, ok := predictCodexPatch(p, root, "pkg/"); ok {
			t.Errorf("%s: ok=true, want the file-level fallback", name)
		}
	}
	// What the patch does to the repo, outside files replayed with it (#201
	// reviews): an outside operation counts only through what it later moves in;
	// a move out removes the source; a move in writes what the patch itself made
	// of the source; a source never read (over the cap, a FIFO) leaves what it
	// produces unpredicted (absent here: a file-level heads-up), never the
	// whole patch.
	for name, tc := range map[string]struct {
		patch string
		want  map[string]string
	}{
		"only outside":           {bounded("*** Update File: " + note + "\n@@\n-n2\n+N2\n"), map[string]string{}},
		"outside + in repo":      {bounded("*** Update File: " + note + "\n@@\n-n2\n+N2\n*** Update File: a.txt\n@@\n-b\n+B\n"), map[string]string{"pkg/a.txt": "a\nB\n"}},
		"absolute outside":       {bounded("*** Add File: " + filepath.Join(t.TempDir(), "y.txt") + "\n+y\n*** Delete File: d.txt\n"), map[string]string{"pkg/d.txt": ""}},
		"a move out of the repo": {bounded("*** Update File: a.txt\n*** Move to: ../../x.txt\n@@\n-b\n+B\n"), map[string]string{"pkg/a.txt": ""}},
		"a move into the repo":   {bounded("*** Update File: " + note + "\n*** Move to: a.txt\n@@\n-n2\n+N2\n"), map[string]string{"pkg/a.txt": "n1\nN2\n"}},
		// #201 re-review A1: an outside file added, moved in, moved again
		"added outside, moved in, moved on": {bounded("*** Add File: " + added + "\n+n1\n+n2\n*** Update File: " + added + "\n*** Move to: a.txt\n@@\n n2\n+n3\n*** Update File: a.txt\n*** Move to: b.txt\n@@\n n3\n+n4\n"),
			map[string]string{"pkg/a.txt": "", "pkg/b.txt": "n1\nn2\nn3\nn4\n"}},
		// #201 re-review A2: an outside file rewritten, then moved in
		"rewritten outside, then moved in": {bounded("*** Update File: " + ten + "\n@@\n-t1\n-t2\n+z1\n+z2\n*** Update File: " + ten + "\n*** Move to: b.txt\n@@\n z2\n+n4\n"),
			map[string]string{"pkg/b.txt": "z1\nz2\nn4\n"}},
		"a move in from an over-cap file": {bounded("*** Update File: " + big + "\n*** Move to: a.txt\n@@\n+x\n*** Delete File: d.txt\n"), map[string]string{"pkg/d.txt": ""}},
		"unread, moved in, updated":       {bounded("*** Update File: " + big + "\n*** Move to: a.txt\n@@\n+x\n*** Update File: a.txt\n@@\n-b\n+B\n"), map[string]string{}},
		// unknown is carried through a later move of the destination
		"unread, moved in, moved on": {bounded("*** Update File: " + big + "\n*** Move to: a.txt\n@@\n+x\n*** Update File: a.txt\n*** Move to: b.txt\n@@\n+y\n"), map[string]string{"pkg/a.txt": ""}},
	} {
		got, ok := predictCodexPatch(tc.patch, root, "pkg/")
		if !ok || !reflect.DeepEqual(got, tc.want) {
			t.Errorf("%s: predictCodexPatch = %q, %v; want %q, true", name, brief(got), ok, tc.want)
		}
	}
}

// brief is m with each value cut to 40 bytes, for a readable failure.
func brief(m map[string]string) map[string]string {
	out := make(map[string]string, len(m))
	for k, v := range m {
		if len(v) > 40 {
			v = fmt.Sprintf("%s... (%d bytes)", v[:40], len(v))
		}
		out[k] = v
	}
	return out
}

// #201 re-review B: a FIFO is never read (os.ReadFile on one waits for a
// writer forever), wherever it is: moved in from outside, or patched in the
// repo. What it would produce is left unpredicted, and the prediction returns.
func TestPredictCodexPatch_NeverReadsAFIFO(t *testing.T) {
	root, outDir := t.TempDir(), t.TempDir()
	if err := os.MkdirAll(filepath.Join(root, "pkg"), 0o755); err != nil {
		t.Fatal(err)
	}
	writeT(t, root, "pkg/d.txt", "d\n")
	for _, p := range []string{filepath.Join(outDir, "fifo"), filepath.Join(root, "pkg", "fifo")} {
		if out, err := exec.Command("mkfifo", p).CombinedOutput(); err != nil {
			t.Skipf("mkfifo unavailable: %v %s", err, out)
		}
	}
	for name, tc := range map[string]struct {
		patch string
		want  map[string]string
	}{
		"moved in from an outside FIFO": {bounded("*** Update File: " + filepath.Join(outDir, "fifo") + "\n*** Move to: a.txt\n@@\n+x\n*** Delete File: d.txt\n"), map[string]string{"pkg/d.txt": ""}},
		"a FIFO in the repo, patched":   {bounded("*** Update File: fifo\n@@\n+x\n*** Delete File: d.txt\n"), map[string]string{"pkg/d.txt": ""}},
	} {
		type result struct {
			got map[string]string
			ok  bool
		}
		done := make(chan result, 1)
		go func() {
			got, ok := predictCodexPatch(tc.patch, root, "pkg/")
			done <- result{got, ok}
		}()
		select {
		case r := <-done:
			if !r.ok || !reflect.DeepEqual(r.got, tc.want) {
				t.Errorf("%s: predictCodexPatch = %q, %v; want %q, true", name, r.got, r.ok, tc.want)
			}
		case <-time.After(10 * time.Second):
			t.Fatalf("%s: predictCodexPatch still blocked after 10s: it read the FIFO", name)
		}
	}
	// the Claude hook's read is the same one
	done := make(chan fileRead, 1)
	go func() { _, r := readForPrediction(filepath.Join(root, "pkg", "fifo")); done <- r }()
	select {
	case r := <-done:
		if r != fileUnread {
			t.Errorf("readForPrediction(FIFO) = %v, want fileUnread", r)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("readForPrediction blocked on a FIFO")
	}
}
