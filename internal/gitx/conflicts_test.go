package gitx

import (
	"errors"
	"fmt"
	"os/exec"
	"slices"
	"strings"
	"sync"
	"testing"
)

// #199: two windows' edits are graded by git's 3-way rule, not by line overlap.
// git merges two edits as ONE conflict region unless an unchanged line separates
// them, so windows editing lines 10 and 11 conflict; and an insertion meets only
// a change to a line on either side of its gap (or another insertion there), so
// the shape (LineRange.Gap) decides where a touch rule over spans would not.
func TestLineRangeConflicts(t *testing.T) {
	cases := []struct {
		name string
		a, b LineRange
		want bool
	}{
		{"same line", span(5, 5), span(5, 5), true},
		{"overlapping changes", span(5, 7), span(7, 9), true},
		{"touching changes: b right below a", span(5, 6), span(7, 7), true},
		{"touching changes: b right above a", span(5, 6), span(4, 4), true},
		{"one unchanged line between, below", span(5, 6), span(8, 8), false},
		{"one unchanged line between, above", span(5, 6), span(3, 3), false},
		{"insertion after the change's last line", gapAt(7), span(5, 7), true},
		{"insertion before the change's first line", gapAt(4), span(5, 7), true},
		{"insertion inside the change", gapAt(6), span(5, 7), true},
		{"insertion one line clear below", gapAt(8), span(5, 7), false},
		{"insertion one line clear above", gapAt(3), span(5, 7), false},
		{"insertions at one gap", gapAt(5), gapAt(5), true},
		{"insertions at neighbouring gaps", gapAt(5), gapAt(6), false},
		{"insertion at the top vs a change of line 1", gapAt(0), span(1, 1), true},
		{"insertion at the top vs a change of line 2", gapAt(0), span(2, 2), false},
	}
	for _, c := range cases {
		if got := c.a.Conflicts(c.b); got != c.want {
			t.Errorf("%s: %v.Conflicts(%v) = %v, want %v", c.name, c.a, c.b, got, c.want)
		}
		if got := c.b.Conflicts(c.a); got != c.want {
			t.Errorf("%s (swapped): %v.Conflicts(%v) = %v, want %v", c.name, c.b, c.a, got, c.want)
		}
	}
	// The shape is load-bearing: as spans, an insertion is both neighbours of its
	// gap, so line overlap misses touching changes and flags neighbouring
	// insertions, and a touch rule over spans would flag a change two lines away.
	if span(10, 10).Overlaps(span(11, 11)) {
		t.Error("precondition: touching changes don't overlap as spans")
	}
	if !gapAt(10).Overlaps(gapAt(11)) {
		t.Error("precondition: neighbouring insertions overlap as spans")
	}
}

// LineRange.Conflicts must BE git's rule end to end: each edit's ranges come from
// real `git diff -U0` output through parseHunkRangesOld (the up-to-date window's
// path in ChangedRanges), and the verdict must equal `git merge-file` on the two
// edited files for every single-edit pair: insertions, changes, deletions,
// growing and shrinking rewrites, at every position of a small file, against an
// edit of each kind in the middle and at both ends. The pre-#199 span overlap
// disagrees with git on a good share of the same pairs.
func TestLineRangeConflicts_MatchesGitMergeFile(t *testing.T) {
	hermeticGit(t)
	const n = 7
	type edit struct{ at, del, add int } // replace del lines after line `at` with add new ones
	base := make([]string, n)
	for i := range base {
		base[i] = fmt.Sprintf("b%d", i+1)
	}
	apply := func(e edit, tag string) string {
		out := append([]string(nil), base[:e.at]...)
		for k := 0; k < e.add; k++ {
			out = append(out, fmt.Sprintf("%s%d", tag, k))
		}
		return strings.Join(append(out, base[e.at+e.del:]...), "\n") + "\n"
	}
	var all []edit
	for _, k := range [][2]int{{0, 1}, {0, 2}, {1, 1}, {2, 2}, {1, 0}, {2, 0}, {1, 2}, {2, 1}} {
		for at := 0; at+k[0] <= n; at++ {
			all = append(all, edit{at, k[0], k[1]})
		}
	}
	var probes []edit // each kind in the middle, plus the file's two ends
	for _, k := range [][2]int{{0, 1}, {0, 2}, {1, 1}, {2, 2}, {1, 0}, {2, 0}, {1, 2}, {2, 1}} {
		probes = append(probes, edit{3, k[0], k[1]})
	}
	probes = append(probes, edit{0, 0, 1}, edit{0, 1, 1}, edit{0, 1, 0}, edit{n, 0, 1}, edit{n - 1, 1, 1}, edit{n - 1, 1, 0})

	dir := t.TempDir()
	writeFile(t, dir, "o", strings.Join(base, "\n")+"\n")
	git := func(args ...string) (string, bool) { // ok=false: exit 1 (differs / conflicts)
		cmd := exec.Command("git", args...)
		cmd.Dir = dir
		out, err := cmd.Output()
		var exit *exec.ExitError
		if err != nil && !errors.As(err, &exit) {
			t.Fatalf("git %v: %v", args, err)
		}
		return string(out), err == nil
	}
	ranges := map[string][]LineRange{}
	file := func(e edit, tag string) string {
		name := fmt.Sprintf("%s-%d-%d-%d", tag, e.at, e.del, e.add)
		if _, done := ranges[name]; !done {
			writeFile(t, dir, name, apply(e, tag))
			diff, _ := git("diff", "--no-index", "-U0", "o", name)
			ranges[name] = parseHunkRangesOld(diff)
		}
		return name
	}
	type pair struct{ a, b string }
	var pairs []pair
	for _, a := range probes {
		for _, b := range all {
			if a.add == 0 && b.add == 0 && a.at == b.at && a.del == b.del {
				continue // the same deletion twice: git takes identical changes cleanly
			}
			pairs = append(pairs, pair{file(a, "A"), file(b, "B")})
		}
	}
	verdicts := make([]bool, len(pairs)) // git merge-file conflicts
	var wg sync.WaitGroup
	sem := make(chan struct{}, 8)
	for i, p := range pairs {
		wg.Add(1)
		go func() {
			defer wg.Done()
			sem <- struct{}{}
			defer func() { <-sem }()
			_, clean := git("merge-file", "-p", p.a, "o", p.b)
			verdicts[i] = !clean
		}()
	}
	wg.Wait()

	conflict := func(a, b []LineRange, f func(x, y LineRange) bool) bool {
		for _, x := range a {
			for _, y := range b {
				if f(x, y) {
					return true
				}
			}
		}
		return false
	}
	spanRuleWrong := 0
	for i, p := range pairs {
		ra, rb := ranges[p.a], ranges[p.b]
		if got := conflict(ra, rb, LineRange.Conflicts); got != verdicts[i] {
			t.Errorf("%s %v vs %s %v: Conflicts = %v, git merge-file conflicts = %v", p.a, ra, p.b, rb, got, verdicts[i])
		}
		if conflict(ra, rb, LineRange.Overlaps) != verdicts[i] {
			spanRuleWrong++
		}
	}
	if spanRuleWrong == 0 {
		t.Errorf("the span overlap rule agrees with git on all %d pairs: the shapes don't exercise #199", len(pairs))
	}
	t.Logf("%d pairs agree with git merge-file; the pre-#199 span overlap got %d wrong", len(pairs), spanRuleWrong)
}

// The grade must be git's MERGE, end to end (#199 review): two windows' ranges
// as wt measures them (ChangedRangesWith, the same diffs as ChangedRanges),
// graded by LineRange.Conflicts, against `git merge-tree` (merge-ort) on the
// two edits as commits. The file repeats its lines ("}", blank, indented "}"),
// so an inserted or deleted line can sit in more than one gap and a diff has to
// choose: TestLineRangeConflicts_MatchesGitMergeFile's unique lines can't tell a
// plain `git diff` (myers, indent heuristic) from merge-ort's alignment
// (histogram, none). Pairs whose files come out identical are left out: git
// takes identical edits cleanly, and wt, which grades positions, not content,
// flags them on purpose.
func TestChangedRanges_MatchMergeTree(t *testing.T) {
	dir := gitRepo(t)
	base := []string{"a", "", "}", "\t}", "", "}"}
	text := func(ls []string) string { return strings.Join(ls, "\n") + "\n" }
	writeFile(t, dir, "f.txt", text(base))
	runGit(t, dir, "add", "f.txt")
	runGit(t, dir, "commit", "-qm", "base")
	git := func(stdin string, args ...string) (string, int) {
		cmd := exec.Command("git", args...)
		cmd.Dir = dir
		cmd.Stdin = strings.NewReader(stdin)
		out, err := cmd.Output()
		var exit *exec.ExitError
		if err != nil && !errors.As(err, &exit) {
			t.Fatalf("git %v: %v", args, err)
		}
		code := 0
		if exit != nil {
			code = exit.ExitCode()
		}
		return strings.TrimSpace(string(out)), code
	}
	commitOf := func(content string) string {
		blob, _ := git(content, "hash-object", "-w", "--stdin")
		tree, _ := git("100644 blob "+blob+"\tf.txt\n", "mktree")
		c, _ := git("", "commit-tree", tree, "-p", "main", "-m", "edit")
		return c
	}
	type variant struct{ content, commit string }
	var vs []variant
	add := func(ls []string) {
		c := text(ls)
		vs = append(vs, variant{c, commitOf(c)})
	}
	for g := 0; g <= len(base); g++ { // insert one line at each gap
		for _, l := range []string{"}", "", "\t}", "x"} {
			add(slices.Insert(slices.Clone(base), g, l))
		}
	}
	for i := range base { // delete one line, rewrite one line
		add(slices.Delete(slices.Clone(base), i, i+1))
		add(slices.Replace(slices.Clone(base), i, i+1, "y"))
	}
	ranges := make([][]LineRange, len(vs))
	for i, v := range vs {
		r, ok := ChangedRangesWith(dir, "main", "f.txt", []byte(v.content))
		if !ok {
			t.Fatalf("ChangedRangesWith(%q) not ok", v.content)
		}
		ranges[i] = r
	}
	conflicts := func(a, b []LineRange) bool {
		for _, x := range a {
			for _, y := range b {
				if x.Conflicts(y) {
					return true
				}
			}
		}
		return false
	}
	pairs := 0
	for i := range vs {
		for j := i + 1; j < len(vs); j++ {
			if vs[i].content == vs[j].content {
				continue
			}
			pairs++
			_, code := git("", "merge-tree", "--write-tree", vs[i].commit, vs[j].commit)
			if code > 1 {
				t.Fatalf("git merge-tree exited %d", code)
			}
			if got := conflicts(ranges[i], ranges[j]); got != (code == 1) {
				t.Errorf("%q %v vs %q %v: wt conflict=%v, git merge-tree conflict=%v", vs[i].content, ranges[i], vs[j].content, ranges[j], got, code == 1)
			}
		}
	}
	t.Logf("%d pairs agree with git merge-tree", pairs)
}
