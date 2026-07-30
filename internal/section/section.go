// Package section partitions a structured shared doc (CLAUDE.md, MEMORY.md, the
// resume memory) into sections/lanes and attributes a window's diff to the
// sections it touches (eharriett0/wt#22). This turns "two windows touched the
// same shared doc → advisory" into "two windows touched the SAME SECTION → HIGH"
// — real coordination on the natural unit for these files.
//
// The logic is pure (content + line-ranges in, sections/headings out) so it is
// fully unit-tested; the caller (collide/cli) owns the git IO.
package section

import (
	"regexp"
	"strings"

	"github.com/eharriett0/wt/internal/gitx"
)

// Section is one lane of a structured doc: the delimiter line (heading) plus the
// 1-based inclusive line span it owns — from its heading line to the line before
// the next delimiter (or EOF). Heading is the STABLE cross-worktree identity:
// two windows' copies of the doc have different line numbers, but a section with
// the same heading text is "the same section".
type Section struct {
	Heading string // the delimiter line, trimmed ("" = the preamble before the first delimiter)
	Start   int    // 1-based line of the heading
	End     int    // 1-based last line of the section (inclusive)
}

// Compile compiles a section-delimiter pattern (a plain regexp).
func Compile(pattern string) (*regexp.Regexp, error) {
	return regexp.Compile(pattern)
}

// Parse splits content into consecutive sections at every line matching re.
// Content before the first delimiter is a preamble section (Heading ""), a real
// lane windows can still contend on (the doc's header/intro). A doc with no
// delimiter match is a single preamble section spanning the whole file. Line
// numbers are 1-based inclusive, matching git's diff line numbering.
func Parse(content string, re *regexp.Regexp) []Section {
	lines := strings.Split(content, "\n")
	// strings.Split leaves a trailing "" when content ends in "\n"; drop it so
	// the line count matches git's (git doesn't number the phantom final line).
	if n := len(lines); n > 0 && lines[n-1] == "" {
		lines = lines[:n-1]
	}
	n := len(lines)
	if n == 0 {
		return nil
	}

	var heads []int // 1-based line numbers of delimiter lines
	for i, ln := range lines {
		if re.MatchString(ln) {
			heads = append(heads, i+1)
		}
	}

	var secs []Section
	firstHead := n + 1
	if len(heads) > 0 {
		firstHead = heads[0]
	}
	if firstHead > 1 { // preamble (or the whole file when there are no delimiters)
		secs = append(secs, Section{Heading: "", Start: 1, End: firstHead - 1})
	}
	for i, h := range heads {
		end := n
		if i+1 < len(heads) {
			end = heads[i+1] - 1
		}
		secs = append(secs, Section{Heading: strings.TrimSpace(lines[h-1]), Start: h, End: end})
	}
	return secs
}

// EditedHeadings returns the heading of every section that any of ranges
// overlaps — the set of sections a window's diff touches. Order preserved,
// deduped. The preamble's "" heading is included as-is (it's a real lane).
func EditedHeadings(secs []Section, ranges []gitx.LineRange) []string {
	var out []string
	seen := map[string]bool{}
	for _, s := range secs {
		sr := gitx.LineRange{Start: s.Start, End: s.End}
		for _, r := range ranges {
			if sr.Overlaps(r) {
				if !seen[s.Heading] {
					seen[s.Heading] = true
					out = append(out, s.Heading)
				}
				break
			}
		}
	}
	return out
}

// Intersect returns the headings present in BOTH sets — the sections two windows
// are BOTH editing (a same-section, HIGH-risk collision). Order follows b.
func Intersect(a, b []string) []string {
	set := make(map[string]bool, len(a))
	for _, h := range a {
		set[h] = true
	}
	var out []string
	seen := map[string]bool{}
	for _, h := range b {
		if set[h] && !seen[h] {
			seen[h] = true
			out = append(out, h)
		}
	}
	return out
}

// Find returns the section whose heading, trimmed, equals want (case-sensitive,
// exact after trim), or matches by suffix/contains as a convenience for the CLI
// (`--section "Recent gotchas"` matching "## Recent gotchas (last 10 days)").
// Exact trimmed match is preferred; then a heading that CONTAINS want. Returns
// (Section{}, false) when nothing matches.
func Find(secs []Section, want string) (Section, bool) {
	want = strings.TrimSpace(want)
	if want == "" {
		return Section{}, false
	}
	for _, s := range secs { // exact (after trim) first
		if s.Heading == want {
			return s, true
		}
	}
	for _, s := range secs { // then contains
		if s.Heading != "" && strings.Contains(s.Heading, want) {
			return s, true
		}
	}
	return Section{}, false
}

// Headings returns the non-empty section headings, in order — used to list the
// available sections when an append target isn't found.
func Headings(secs []Section) []string {
	var out []string
	for _, s := range secs {
		if s.Heading != "" {
			out = append(out, s.Heading)
		}
	}
	return out
}

// InsertUnderSection inserts text at the END of the section whose heading
// matches want (via Find), returning the new content (#22, the coordinated
// section-scoped append). found=false with content unchanged when no section
// matches. The text is inserted after the section's last line — for a middle
// section that's just before the next delimiter, for the last section it's EOF.
// A trailing newline on the original content is preserved. Pure; the caller owns
// the flock'd read/write.
func InsertUnderSection(content, delimiter, want, text string) (out string, found bool, err error) {
	re, cerr := Compile(delimiter)
	if cerr != nil {
		return content, false, cerr
	}
	target, ok := Find(Parse(content, re), want)
	if !ok {
		return content, false, nil
	}
	lines := strings.Split(content, "\n")
	trailingNL := len(lines) > 0 && lines[len(lines)-1] == ""
	if trailingNL {
		lines = lines[:len(lines)-1] // operate on real lines; restore the final newline at the end
	}
	at := target.End // 1-based last line of section == 0-based insertion index (after it)
	if at > len(lines) {
		at = len(lines)
	}
	res := make([]string, 0, len(lines)+1)
	res = append(res, lines[:at]...)
	res = append(res, strings.Split(text, "\n")...)
	res = append(res, lines[at:]...)
	joined := strings.Join(res, "\n")
	if trailingNL {
		joined += "\n"
	}
	return joined, true, nil
}
