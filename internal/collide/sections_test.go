package collide

import (
	"os"
	"path/filepath"
	"reflect"
	"testing"

	"github.com/eharriett0/wt/internal/gitx"
	"github.com/eharriett0/wt/internal/section"
)

// SharedSectionsAcrossWith grades a worktree's copy of a structured doc as a
// pending edit will leave it (#226): the pending content and its NEW-frame
// ranges stand in for that worktree's file and ranges, and every other
// worktree is read from disk exactly as SharedSectionsAcross reads it.
func TestSharedSectionsAcrossWith(t *testing.T) {
	const doc = "# Plan\n## Alpha\na1\na2\na3\n## Beta\nb1\nb2\nb3\nb4\nb5\nb6\nb7\n"
	wtA, wtB := t.TempDir(), t.TempDir()
	for _, wt := range []string{wtA, wtB} {
		if err := os.WriteFile(filepath.Join(wt, "PLAN.md"), []byte(doc), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	// On disk A edits a1 (line 3), B edits b2 (line 8).
	disk := map[string][]gitx.LineRange{wtA: {{Start: 3, End: 3}}, wtB: {{Start: 8, End: 8}}}
	var asked []string
	ranges := func(wt, base, path string) []gitx.LineRange {
		asked = append(asked, wt)
		return disk[wt]
	}
	const delim = `^##\s`
	both := []string{wtA, wtB}

	shared, graded := SharedSectionsAcross("main", both, "PLAN.md", delim, NewOnly(ranges))
	if !graded || len(shared) != 0 {
		t.Fatalf("precondition: Alpha vs Beta on disk = %v, %v; want none, graded", shared, graded)
	}
	if got, ok := SharedSectionsAcrossWith("main", both, "PLAN.md", delim, NewOnly(ranges), nil); !reflect.DeepEqual(got, shared) || ok != graded {
		t.Errorf("nil pending = %v, %v; want SharedSectionsAcross's %v, %v", got, ok, shared, graded)
	}

	// A's pending copy inserts two lines in Alpha, which moves Beta down by
	// two: b6 is line 14 of the copy (line 12 on disk). Read in the copy's
	// frame, A now edits Beta too.
	moved := "# Plan\n## Alpha\na1\nNEW1\nNEW2\na2\na3\n## Beta\nb1\nb2\nb3\nb4\nb5\nXb6\nb7\n"
	pending := map[string]PendingDoc{wtA: {Content: moved, Edits: gitx.DocEdits{New: []gitx.LineRange{{Start: 4, End: 5}, {Start: 14, End: 14}}}}}
	asked = nil
	shared, graded = SharedSectionsAcrossWith("main", both, "PLAN.md", delim, NewOnly(ranges), pending)
	if !graded || !reflect.DeepEqual(shared, []string{"## Beta"}) {
		t.Errorf("A's pending copy edits b6: shared = %v, %v; want [## Beta], graded", shared, graded)
	}
	if !reflect.DeepEqual(asked, []string{wtB}) {
		t.Errorf("ranges asked for %v; want only B's (A's come with its pending copy)", asked)
	}
	// The insert alone stays in Alpha: still disjoint.
	pending[wtA] = PendingDoc{Content: moved, Edits: gitx.DocEdits{New: []gitx.LineRange{{Start: 4, End: 5}}}}
	if shared, graded = SharedSectionsAcrossWith("main", both, "PLAN.md", delim, NewOnly(ranges), pending); !graded || len(shared) != 0 {
		t.Errorf("A's pending copy stays in Alpha: shared = %v, %v; want none, graded", shared, graded)
	}

	// A worktree with no copy on disk still grades from its pending one.
	if err := os.Remove(filepath.Join(wtA, "PLAN.md")); err != nil {
		t.Fatal(err)
	}
	pending[wtA] = PendingDoc{Content: moved, Edits: gitx.DocEdits{New: []gitx.LineRange{{Start: 14, End: 14}}}}
	if shared, graded = SharedSectionsAcrossWith("main", both, "PLAN.md", delim, NewOnly(ranges), pending); !graded || !reflect.DeepEqual(shared, []string{"## Beta"}) {
		t.Errorf("A's copy only pending: shared = %v, %v; want [## Beta], graded", shared, graded)
	}
	// A bad delimiter still can't grade, pending or not.
	if _, graded = SharedSectionsAcrossWith("main", both, "PLAN.md", `(`, NewOnly(ranges), pending); graded {
		t.Error("bad delimiter: want graded=false")
	}
}

// #237: a window's sections include the ones holding the lines it DELETED,
// read in the copy it diffed from; a window whose doc is gone counts its
// deletions; a failed measurement (All) edits every section, its own copy's
// and the old one's. A window without the doc that deleted nothing is still
// skipped, as before.
func TestSharedSectionsAcross_CountsDeletions(t *testing.T) {
	const old = "# Plan\n## Alpha\na1\na2\n## Beta\nb1\nb2\nb3\n"
	const delim = `^##\s`
	write := func(t *testing.T, dir, content string) {
		t.Helper()
		if err := os.WriteFile(filepath.Join(dir, "PLAN.md"), []byte(content), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	betaEdit := gitx.DocEdits{New: []gitx.LineRange{{Start: 7, End: 7}}} // b2 in the full copy
	for _, tc := range []struct {
		name     string
		cContent *string // nil: wc has no PLAN.md
		cEdits   gitx.DocEdits
		graded   bool
		shared   []string
	}{
		{"wc deleted Beta", sp("# Plan\n## Alpha\na1\na2\n"),
			gitx.DocEdits{New: []gitx.LineRange{{Start: 4, End: 5}}, Deleted: []gitx.LineRange{{Start: 5, End: 8}}, Old: old},
			true, []string{"## Beta"}},
		// a1 deleted: the gap sits between "## Alpha" and a2, all Alpha. (Deleting
		// Alpha's LAST lines puts the gap against "## Beta", which counts Beta
		// too: git conflicts with an edit of that heading line.)
		{"wc deleted a line of Alpha only", sp("# Plan\n## Alpha\na2\n## Beta\nb1\nb2\nb3\n"),
			gitx.DocEdits{New: []gitx.LineRange{{Start: 2, End: 3}}, Deleted: []gitx.LineRange{{Start: 3, End: 3}}, Old: old},
			true, nil},
		{"wc deleted the doc", nil,
			gitx.DocEdits{New: []gitx.LineRange{{Start: 0, End: 1}}, Deleted: []gitx.LineRange{{Start: 1, End: 8}}, Old: old},
			true, []string{"## Beta"}},
		{"wc's measurement failed", sp("# Plan\n## Alpha\na1\na2\n"),
			gitx.DocEdits{All: true},
			true, []string{"## Beta"}},
		{"wc lacks the doc and deleted nothing", nil, gitx.DocEdits{}, true, nil},
	} {
		t.Run(tc.name, func(t *testing.T) {
			wa, wc := t.TempDir(), t.TempDir()
			write(t, wa, old)
			if tc.cContent != nil {
				write(t, wc, *tc.cContent)
			}
			edits := func(wt, base, path string) gitx.DocEdits {
				if wt == wa {
					return betaEdit
				}
				return tc.cEdits
			}
			shared, graded := SharedSectionsAcross("main", []string{wa, wc}, "PLAN.md", delim, edits)
			if graded != tc.graded || !reflect.DeepEqual(shared, tc.shared) {
				t.Errorf("shared = %v, graded = %v; want %v, %v", shared, graded, tc.shared, tc.graded)
			}
			// The same edits as a pending copy grade the same (the hooks).
			if tc.cContent != nil {
				pending := map[string]PendingDoc{wc: {Content: *tc.cContent, Edits: tc.cEdits}}
				if got, ok := SharedSectionsAcrossWith("main", []string{wa, wc}, "PLAN.md", delim, edits, pending); ok != graded || !reflect.DeepEqual(got, shared) {
					t.Errorf("pending: %v, %v; want %v, %v", got, ok, shared, graded)
				}
			}
		})
	}
	// Two windows whose measurements failed share every section either has.
	wa, wc := t.TempDir(), t.TempDir()
	write(t, wa, old)
	write(t, wc, old)
	all := func(string, string, string) gitx.DocEdits { return gitx.DocEdits{All: true} }
	if shared, _ := SharedSectionsAcross("main", []string{wa, wc}, "PLAN.md", delim, all); !reflect.DeepEqual(shared, []string{"", "## Alpha", "## Beta"}) {
		t.Errorf("both All: %v, want every section", shared)
	}
	// One failed window alone shares nothing with a window that edited
	// nothing. A failed diff comes with the whole file as its new side; it
	// counts once, as every section, never twice.
	none := func(wt, _, _ string) gitx.DocEdits {
		if wt == wa {
			return gitx.DocEdits{}
		}
		return gitx.DocEdits{All: true, New: []gitx.LineRange{{Start: 1, End: 1 << 30}}}
	}
	if shared, _ := SharedSectionsAcross("main", []string{wa, wc}, "PLAN.md", delim, none); len(shared) != 0 {
		t.Errorf("All against no edits: %v, want none", shared)
	}
}

func sp(s string) *string { return &s }

// #247: a window BEHIND base also counts the sections of base's copy its
// edits land in. Its own copy keeps the headings it forked with, so where
// base renamed, split or merged a section since, the same line sits under
// different headings in the two windows' copies, and they read disjoint
// while git conflicts.
func TestSharedSectionsAcross_BehindCountsBasesSections(t *testing.T) {
	const forked = "# Plan\n## Alpha\na1\na2\n## Beta\nb1\nb2\nb3\n" // wa's merge base
	const delim = `^##\s`
	write := func(t *testing.T, dir, content string) {
		t.Helper()
		if err := os.WriteFile(filepath.Join(dir, "PLAN.md"), []byte(content), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	r := func(a int) []gitx.LineRange { return []gitx.LineRange{{Start: a, End: a}} }
	for _, tc := range []struct {
		name     string
		baseCopy string
		aLine    int // wa's edit, in its own copy (= the merge base's lines)
		aBase    int // the same edit in base's lines
		bLine    int // wb's edit, in base's lines (wb is current)
		shared   []string
	}{
		{"base renamed Beta; both edit b2", "# Plan\n## Alpha\na1\na2\n## Beta two\nb1\nb2\nb3\n", 7, 7, 7, []string{"## Beta two"}},
		{"base split Beta before b2; both edit b3", "# Plan\n## Alpha\na1\na2\n## Beta\nb1\n## Split\nb2\nb3\n", 8, 9, 9, []string{"## Split"}},
		{"base merged Beta into Alpha; both edit b3", "# Plan\n## Alpha\na1\na2\nb1\nb2\nb3\n", 8, 7, 7, []string{"## Alpha"}},
		{"base renamed Beta; wa edits a2, wb b2", "# Plan\n## Alpha\na1\na2\n## Beta two\nb1\nb2\nb3\n", 4, 4, 7, nil},
	} {
		t.Run(tc.name, func(t *testing.T) {
			wa, wb := t.TempDir(), t.TempDir()
			write(t, wa, forked)
			write(t, wb, tc.baseCopy)
			behind := gitx.DocEdits{New: r(tc.aLine), Base: r(tc.aBase), BaseCopy: tc.baseCopy}
			edits := func(wt, base, path string) gitx.DocEdits {
				if wt == wa {
					return behind
				}
				return gitx.DocEdits{New: r(tc.bLine)}
			}
			shared, graded := SharedSectionsAcross("main", []string{wa, wb}, "PLAN.md", delim, edits)
			if !graded || !reflect.DeepEqual(shared, tc.shared) {
				t.Errorf("shared = %v, %v; want %v, graded", shared, graded, tc.shared)
			}
			if tc.shared != nil {
				behind.Base, behind.BaseCopy = nil, ""
				if shared, _ := SharedSectionsAcross("main", []string{wa, wb}, "PLAN.md", delim, edits); len(shared) != 0 {
					t.Errorf("precondition: without base's frame %v, want none (the #247 miss)", shared)
				}
			}
		})
	}
}

// baseClaims: base-copy lines a behind window's base-frame edits claim
// (#247), by oldClaims' rules.
func TestBaseClaims(t *testing.T) {
	re, _ := section.Compile(`^##\s`)
	secs := section.Parse("# Plan\n## Alpha\na1\na2\n## Beta two\nb1\nb2\n", re) // the heading is line 5
	r := func(a, b int) gitx.LineRange { return gitx.LineRange{Start: a, End: b} }
	gap := func(k int) gitx.LineRange { return gitx.LineRange{Start: k, End: k + 1, Gap: true} }
	for _, tc := range []struct {
		name string
		base []gitx.LineRange
		want []gitx.LineRange
	}{
		{"a body line changed", []gitx.LineRange{r(6, 7)}, []gitx.LineRange{r(6, 7)}},
		{"a change starting at a heading: the line above too", []gitx.LineRange{r(5, 6)}, []gitx.LineRange{r(5, 6), r(4, 4)}},
		{"the first line: nothing above", []gitx.LineRange{r(1, 1)}, []gitx.LineRange{r(1, 1)}},
		{"an insertion: the line it lands after, never both neighbours", []gitx.LineRange{gap(4)}, []gitx.LineRange{r(4, 4)}},
		{"an insertion at the top: the first line", []gitx.LineRange{gap(0)}, []gitx.LineRange{r(1, 1)}},
	} {
		if got := baseClaims(secs, gitx.DocEdits{Base: tc.base}); !reflect.DeepEqual(got, tc.want) {
			t.Errorf("%s: baseClaims = %v, want %v", tc.name, got, tc.want)
		}
	}
}

// oldClaims: the old-copy lines a window's edit claims (#237 review).
func TestOldClaims(t *testing.T) {
	re, _ := section.Compile(`^##\s`)
	secs := section.Parse("# Plan\n## Alpha\na1\na2\n## Beta\nb1\nb2\n", re) // ## Beta is line 5
	r := func(a, b int) gitx.LineRange { return gitx.LineRange{Start: a, End: b} }
	gap := func(k int) gitx.LineRange { return gitx.LineRange{Start: k, End: k + 1, Gap: true} }
	for _, tc := range []struct {
		name string
		e    gitx.DocEdits
		want []gitx.LineRange
	}{
		{"a body line replaced", gitx.DocEdits{Deleted: []gitx.LineRange{r(6, 6)}}, []gitx.LineRange{r(6, 6)}},
		{"a heading replaced: the line above too", gitx.DocEdits{Deleted: []gitx.LineRange{r(5, 5)}}, []gitx.LineRange{r(5, 5), r(4, 4)}},
		{"the first line, a heading-less preamble: nothing above", gitx.DocEdits{Deleted: []gitx.LineRange{r(1, 1)}}, []gitx.LineRange{r(1, 1)}},
		{"an insertion: the line it lands after", gitx.DocEdits{Gaps: []gitx.LineRange{gap(4)}}, []gitx.LineRange{r(4, 4)}},
		{"an insertion at the top: the first line", gitx.DocEdits{Gaps: []gitx.LineRange{gap(0)}}, []gitx.LineRange{r(1, 1)}},
	} {
		if got := oldClaims(secs, tc.e); !reflect.DeepEqual(got, tc.want) {
			t.Errorf("%s: oldClaims = %v, want %v", tc.name, got, tc.want)
		}
	}
}
