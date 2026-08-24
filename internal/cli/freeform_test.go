package cli

import (
	"os"
	"path/filepath"
	"testing"
)

func TestReadFreeform(t *testing.T) {
	// --file unset → positional args joined + trimmed (the legacy path).
	if got, err := readFreeform("", []string{"  hello", "world  "}); err != nil || got != "hello world" {
		t.Fatalf("positional: got %q err %v", got, err)
	}
	if got, _ := readFreeform("", nil); got != "" {
		t.Fatalf("empty positional: got %q", got)
	}

	// --file set → content read opaquely (backticks/$/! survive verbatim), trimmed.
	dir := t.TempDir()
	p := filepath.Join(dir, "msg.txt")
	body := "append gates to `project_live_gates.md` (not $HOME) — done!\n"
	if err := os.WriteFile(p, []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}
	got, err := readFreeform(p, nil)
	if err != nil {
		t.Fatalf("file read err: %v", err)
	}
	want := "append gates to `project_live_gates.md` (not $HOME) — done!"
	if got != want {
		t.Fatalf("file: got %q want %q", got, want)
	}
	// --file takes precedence over any positional args.
	if got2, _ := readFreeform(p, []string{"ignored"}); got2 != want {
		t.Fatalf("file should win over positional: got %q", got2)
	}

	// A missing file surfaces an error (the caller reports it, not a silent empty).
	if _, err := readFreeform(filepath.Join(dir, "nope.txt"), nil); err == nil {
		t.Fatalf("expected error reading a missing --file")
	}
}

func TestHasUnbalancedBacktick(t *testing.T) {
	for _, c := range []struct {
		s    string
		want bool
	}{
		{"no backticks here", false},
		{"one `pair` balanced", false},
		{"a `dangling backtick", true}, // odd → the substitution signature
		{"`a` `b` two pairs", false},   // even
		{"`x` and one more `", true},   // 3 backticks → odd
	} {
		if got := hasUnbalancedBacktick(c.s); got != c.want {
			t.Errorf("hasUnbalancedBacktick(%q) = %v, want %v", c.s, got, c.want)
		}
	}
}
