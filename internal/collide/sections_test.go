package collide

import (
	"os"
	"path/filepath"
	"reflect"
	"testing"

	"github.com/eharriett0/wt/internal/gitx"
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
