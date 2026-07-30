package section

import (
	"reflect"
	"regexp"
	"testing"

	"github.com/eharriett0/wt/internal/gitx"
)

func headings(secs []Section) []string {
	out := make([]string, len(secs))
	for i, s := range secs {
		out[i] = s.Heading
	}
	return out
}

func TestParse_MarkdownHeadings(t *testing.T) {
	re := mustCompile(t, `^#{2,4}\s`)
	content := "intro line\n## Alpha\na\nb\n### Beta\nc\n## Gamma\nd\n"
	secs := Parse(content, re)
	if got, want := headings(secs), []string{"", "## Alpha", "### Beta", "## Gamma"}; !reflect.DeepEqual(got, want) {
		t.Fatalf("headings = %v, want %v", got, want)
	}
	// spans: preamble line 1; Alpha 2-4; Beta 5-6; Gamma 7-8
	want := []Section{
		{"", 1, 1}, {"## Alpha", 2, 4}, {"### Beta", 5, 6}, {"## Gamma", 7, 8},
	}
	if !reflect.DeepEqual(secs, want) {
		t.Fatalf("sections = %+v, want %+v", secs, want)
	}
}

func TestParse_NoDelimiter_WholeFileIsPreamble(t *testing.T) {
	re := mustCompile(t, `^#{2,4}\s`)
	secs := Parse("just\nsome\ntext\n", re)
	if len(secs) != 1 || secs[0].Heading != "" || secs[0].Start != 1 || secs[0].End != 3 {
		t.Fatalf("no-delimiter → %+v, want one preamble [1,3]", secs)
	}
}

func TestParse_StartsWithDelimiter_NoPreamble(t *testing.T) {
	re := mustCompile(t, `^#{2,4}\s`)
	secs := Parse("## First\na\n## Second\nb\n", re)
	if got, want := headings(secs), []string{"## First", "## Second"}; !reflect.DeepEqual(got, want) {
		t.Fatalf("headings = %v, want %v (no preamble)", got, want)
	}
}

func TestParse_LaneBarDelimiter(t *testing.T) {
	// The resume memory partitions by "**═══" lane bars, not markdown headings.
	re := mustCompile(t, `^\*\*═══`)
	content := "**═══ LANE A\nx\n**═══ LANE B\ny\n"
	secs := Parse(content, re)
	if got, want := headings(secs), []string{"**═══ LANE A", "**═══ LANE B"}; !reflect.DeepEqual(got, want) {
		t.Fatalf("headings = %v, want %v", got, want)
	}
}

func TestParse_EmptyContent(t *testing.T) {
	re := mustCompile(t, `^#`)
	if secs := Parse("", re); secs != nil {
		t.Fatalf("empty content → %+v, want nil", secs)
	}
}

func TestEditedHeadings(t *testing.T) {
	secs := []Section{{"", 1, 1}, {"## Alpha", 2, 4}, {"### Beta", 5, 6}, {"## Gamma", 7, 8}}
	// a hunk at lines 3-3 → Alpha; a hunk at 7-8 → Gamma
	ranges := []gitx.LineRange{{Start: 3, End: 3}, {Start: 7, End: 8}}
	got := EditedHeadings(secs, ranges)
	if want := []string{"## Alpha", "## Gamma"}; !reflect.DeepEqual(got, want) {
		t.Fatalf("EditedHeadings = %v, want %v", got, want)
	}
	// a hunk spanning a boundary (4-5) touches BOTH Alpha and Beta
	got2 := EditedHeadings(secs, []gitx.LineRange{{Start: 4, End: 5}})
	if want := []string{"## Alpha", "### Beta"}; !reflect.DeepEqual(got2, want) {
		t.Fatalf("boundary hunk = %v, want %v", got2, want)
	}
	// preamble edit
	if got := EditedHeadings(secs, []gitx.LineRange{{Start: 1, End: 1}}); !reflect.DeepEqual(got, []string{""}) {
		t.Fatalf("preamble edit = %v, want [\"\"]", got)
	}
}

func TestIntersect(t *testing.T) {
	a := []string{"## Alpha", "### Beta", "## Gamma"}
	b := []string{"## Gamma", "## Delta", "## Alpha"}
	got := Intersect(a, b)
	// order follows b: Gamma then Alpha
	if want := []string{"## Gamma", "## Alpha"}; !reflect.DeepEqual(got, want) {
		t.Fatalf("Intersect = %v, want %v", got, want)
	}
	if got := Intersect([]string{"x"}, []string{"y"}); got != nil {
		t.Fatalf("disjoint → %v, want nil", got)
	}
}

func TestFind(t *testing.T) {
	secs := []Section{{"## Recent gotchas (last 10 days)", 1, 5}, {"## Architecture", 6, 9}}
	if s, ok := Find(secs, "## Architecture"); !ok || s.Start != 6 {
		t.Errorf("exact → (%+v,%v), want Architecture @6", s, ok)
	}
	// contains-match convenience
	if s, ok := Find(secs, "Recent gotchas"); !ok || s.Start != 1 {
		t.Errorf("contains → (%+v,%v), want the gotchas section", s, ok)
	}
	if _, ok := Find(secs, "Nonexistent"); ok {
		t.Error("nonexistent should not match")
	}
	if _, ok := Find(secs, ""); ok {
		t.Error("empty want should not match")
	}
}

func TestInsertUnderSection(t *testing.T) {
	delim := `^#{2,4}\s`
	content := "intro\n## Alpha\na1\na2\n## Beta\nb1\n"
	// insert at end of the MIDDLE section (Alpha) → before "## Beta"
	got, found, err := InsertUnderSection(content, delim, "## Alpha", "a3")
	if err != nil || !found {
		t.Fatalf("insert Alpha → (found=%v,err=%v)", found, err)
	}
	want := "intro\n## Alpha\na1\na2\na3\n## Beta\nb1\n"
	if got != want {
		t.Fatalf("insert Alpha:\n got %q\nwant %q", got, want)
	}

	// insert at end of the LAST section (Beta) → EOF
	got2, _, _ := InsertUnderSection(content, delim, "## Beta", "b2")
	if want := "intro\n## Alpha\na1\na2\n## Beta\nb1\nb2\n"; got2 != want {
		t.Fatalf("insert Beta:\n got %q\nwant %q", got2, want)
	}

	// contains-match + multi-line text
	got3, found3, _ := InsertUnderSection(content, delim, "Alpha", "x\ny")
	if !found3 || got3 != "intro\n## Alpha\na1\na2\nx\ny\n## Beta\nb1\n" {
		t.Fatalf("contains+multiline: found=%v got=%q", found3, got3)
	}

	// not-found → unchanged, found=false
	if out, found, _ := InsertUnderSection(content, delim, "Gamma", "z"); found || out != content {
		t.Fatalf("not-found should be a no-op: found=%v", found)
	}

	// bad regexp → error
	if _, _, err := InsertUnderSection(content, `(`, "Alpha", "z"); err == nil {
		t.Error("bad regexp should error")
	}
}

func TestHeadings(t *testing.T) {
	secs := []Section{{"", 1, 1}, {"## Alpha", 2, 3}, {"## Beta", 4, 5}}
	if got := Headings(secs); !reflect.DeepEqual(got, []string{"## Alpha", "## Beta"}) {
		t.Errorf("Headings = %v, want [Alpha Beta] (preamble excluded)", got)
	}
}

func mustCompile(t *testing.T, pat string) *regexp.Regexp {
	t.Helper()
	r, err := Compile(pat)
	if err != nil {
		t.Fatalf("compile %q: %v", pat, err)
	}
	return r
}
