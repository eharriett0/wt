package cli

// Codex's apply_patch, emulated (#199 review). The Codex pre-edit hook grades a
// patch by the file it WILL produce, measured through the same diff and base
// mapping as `wt check` (gitx.ChangedRangesWith), so the hook says what `wt
// check` says once the patch lands. Locating each hunk's lines and guessing
// which of them git will call changed missed what apply_patch does beyond the
// '-' and '+' lines: it ends every file it writes with a newline (so a file
// without one also changes its last line), writes context lines as the patch
// spells them (fuzzy matching ignores trailing or surrounding whitespace and
// typographic punctuation), and appends a hunk with no context at the end.
//
// This follows codex-rs/apply-patch (parser.rs, lib.rs, seek_sequence.rs) and
// is checked against the apply_patch the Codex app ships. Anything it would
// reject, or this doesn't model, is an error: the hook then keeps its
// file-level heads-up.

import (
	"errors"
	"slices"
	"sort"
	"strings"
	"unicode"
)

// patchOp is one file operation of an apply_patch payload: Add (the new file's
// content), Delete, or Update (chunks, and an optional move).
type patchOp struct {
	kind    byte   // 'A' add, 'D' delete, 'U' update
	path    string // as the patch spells it: relative to Codex's cwd, or absolute
	moveTo  string // Update: the file's new path ("" = stays)
	content string // Add: the file
	chunks  []patchChunk
}

// patchChunk is one Update chunk: the lines it replaces (context and '-'
// lines), what replaces them (context and '+' lines), the optional `@@ <line>`
// it is located after, and whether it is anchored at the end of the file.
type patchChunk struct {
	context  *string
	old, new []string
	eof      bool
}

var errPatch = errors.New("apply_patch would reject or isn't modelled")

// parseApplyPatch parses patch as apply_patch does: trimmed, split into lines
// (a CR before each LF dropped), bounded by "*** Begin Patch" / "*** End
// Patch" (or wrapped in a <<EOF heredoc), then one operation after another.
// Pure.
func parseApplyPatch(patch string) ([]patchOp, error) {
	var lines []string
	if s := strings.TrimSpace(patch); s != "" {
		for _, l := range strings.Split(s, "\n") {
			lines = append(lines, strings.TrimSuffix(l, "\r"))
		}
	}
	bounded := func(ls []string) bool {
		return len(ls) >= 2 && strings.TrimSpace(ls[0]) == "*** Begin Patch" && strings.TrimSpace(ls[len(ls)-1]) == "*** End Patch"
	}
	if !bounded(lines) {
		n := len(lines)
		heredoc := n >= 4 && (lines[0] == "<<EOF" || lines[0] == "<<'EOF'" || lines[0] == `<<"EOF"`) && strings.HasSuffix(lines[n-1], "EOF")
		if !heredoc || !bounded(lines[1:n-1]) {
			return nil, errPatch
		}
		lines = lines[1 : n-1]
	}
	body := lines[1 : len(lines)-1]
	var ops []patchOp
	for len(body) > 0 {
		op, n, err := parsePatchOp(body)
		if err != nil {
			return nil, err
		}
		ops = append(ops, op)
		body = body[n:]
	}
	return ops, nil
}

// parsePatchOp parses the operation lines starts with and says how many lines
// it took. Pure.
func parsePatchOp(lines []string) (patchOp, int, error) {
	first := strings.TrimSpace(lines[0])
	if p, ok := strings.CutPrefix(first, "*** Add File: "); ok {
		var b strings.Builder
		n := 1
		for _, l := range lines[1:] {
			s, ok := strings.CutPrefix(l, "+")
			if !ok {
				break
			}
			b.WriteString(s)
			b.WriteByte('\n')
			n++
		}
		return patchOp{kind: 'A', path: p, content: b.String()}, n, nil
	}
	if p, ok := strings.CutPrefix(first, "*** Delete File: "); ok {
		return patchOp{kind: 'D', path: p}, 1, nil
	}
	p, ok := strings.CutPrefix(first, "*** Update File: ")
	if !ok {
		return patchOp{}, 0, errPatch
	}
	op := patchOp{kind: 'U', path: p}
	rest, n := lines[1:], 1
	if len(rest) > 0 {
		if to, ok := strings.CutPrefix(rest[0], "*** Move to: "); ok {
			op.moveTo, rest, n = to, rest[1:], n+1
		}
	}
	for len(rest) > 0 {
		// A blank line between chunks is skipped; before the first one it is
		// the chunk's first line (a context line, or a reject).
		if len(op.chunks) > 0 && strings.TrimSpace(rest[0]) == "" {
			rest, n = rest[1:], n+1
			continue
		}
		if strings.HasPrefix(rest[0], "***") {
			break
		}
		ch, used, err := parsePatchChunk(rest, len(op.chunks) == 0)
		if err != nil {
			return patchOp{}, 0, err
		}
		op.chunks = append(op.chunks, ch)
		rest, n = rest[used:], n+used
	}
	if len(op.chunks) == 0 {
		return patchOp{}, 0, errPatch
	}
	return op, n, nil
}

// parsePatchChunk parses one Update chunk: an "@@" or "@@ <context line>"
// header (only the first chunk may omit it), then context (' ', or an empty
// line), '-' and '+' lines, up to the next line that is none of those or the
// "*** End of File" anchor. The header and the anchor are read with trailing
// whitespace dropped (measured: "@@ z  " is located after a line "z"), the
// other lines as written. Pure.
func parsePatchChunk(lines []string, firstChunk bool) (patchChunk, int, error) {
	var ch patchChunk
	start := 0
	head := strings.TrimRightFunc(lines[0], unicode.IsSpace)
	if head == "@@" {
		start = 1
	} else if c, ok := strings.CutPrefix(head, "@@ "); ok {
		ch.context, start = &c, 1
	} else if !firstChunk {
		return ch, 0, errPatch
	}
	if start >= len(lines) {
		return ch, 0, errPatch
	}
	parsed := 0
scan:
	for _, l := range lines[start:] {
		switch {
		case strings.TrimRightFunc(l, unicode.IsSpace) == "*** End of File":
			if parsed == 0 {
				return ch, 0, errPatch
			}
			ch.eof = true
			parsed++
			break scan
		case l == "":
			ch.old, ch.new = append(ch.old, ""), append(ch.new, "")
		case l[0] == ' ':
			ch.old, ch.new = append(ch.old, l[1:]), append(ch.new, l[1:])
		case l[0] == '+':
			ch.new = append(ch.new, l[1:])
		case l[0] == '-':
			ch.old = append(ch.old, l[1:])
		default:
			if parsed == 0 {
				return ch, 0, errPatch
			}
			break scan
		}
		parsed++
	}
	return ch, start + parsed, nil
}

// applyPatchOps is what applying ops leaves in every file they touch: path (as
// the patch spells it) → content, nil for a file the patch deletes. read gives a
// file's current content (ok=false: no such file). An operation apply_patch
// would fail on (an Update or Delete of a missing file, chunk lines it can't
// find) is an error, as is any other the hook can't vouch for. Pure given read.
func applyPatchOps(ops []patchOp, read func(path string) (string, bool)) (map[string]*string, error) {
	state := map[string]*string{}
	current := func(p string) (string, bool) {
		if c, seen := state[p]; seen {
			if c == nil {
				return "", false
			}
			return *c, true
		}
		return read(p)
	}
	for _, op := range ops {
		switch op.kind {
		case 'A':
			c := op.content
			state[op.path] = &c
		case 'D':
			if _, ok := current(op.path); !ok {
				return nil, errPatch
			}
			state[op.path] = nil
		case 'U':
			cur, ok := current(op.path)
			if !ok {
				return nil, errPatch
			}
			next, err := patchedContent(cur, op.chunks)
			if err != nil {
				return nil, err
			}
			if op.moveTo != "" && op.moveTo != op.path {
				state[op.path] = nil
				state[op.moveTo] = &next
			} else {
				state[op.path] = &next
			}
		}
	}
	return state, nil
}

// patchedContent is content with chunks applied, as apply_patch computes it:
// the file split into lines (its final newline dropped), each chunk's lines
// sought from where the previous chunk ended, replaced bottom-up, and the
// result joined with "\n" and ended with one. Pure.
func patchedContent(content string, chunks []patchChunk) (string, error) {
	lines := strings.Split(content, "\n")
	if lines[len(lines)-1] == "" {
		lines = lines[:len(lines)-1]
	}
	type replacement struct {
		at, n int
		with  []string
	}
	var rs []replacement
	from := 0
	for _, ch := range chunks {
		if ch.context != nil {
			at, ok := seekSequence(lines, []string{*ch.context}, from, false)
			if !ok {
				return "", errPatch
			}
			from = at + 1
		}
		if len(ch.old) == 0 { // a pure addition: at the end, above a final empty line
			at := len(lines)
			if at > 0 && lines[at-1] == "" {
				at--
			}
			rs = append(rs, replacement{at, 0, ch.new})
			continue
		}
		pattern, with := ch.old, ch.new
		at, ok := seekSequence(lines, pattern, from, ch.eof)
		if !ok && pattern[len(pattern)-1] == "" {
			// The trailing empty line stood for the file's final newline.
			pattern = pattern[:len(pattern)-1]
			if len(with) > 0 && with[len(with)-1] == "" {
				with = with[:len(with)-1]
			}
			at, ok = seekSequence(lines, pattern, from, ch.eof)
		}
		if !ok {
			return "", errPatch
		}
		rs = append(rs, replacement{at, len(pattern), with})
		from = at + len(pattern)
	}
	sort.SliceStable(rs, func(i, j int) bool { return rs[i].at < rs[j].at })
	for i := len(rs) - 1; i >= 0; i-- {
		r := rs[i]
		for k := 0; k < r.n && r.at < len(lines); k++ {
			lines = slices.Delete(lines, r.at, r.at+1)
		}
		if r.at > len(lines) {
			return "", errPatch // apply_patch would panic
		}
		lines = slices.Insert(lines, r.at, r.with...)
	}
	if len(lines) == 0 || lines[len(lines)-1] != "" {
		lines = append(lines, "")
	}
	return strings.Join(lines, "\n"), nil
}

// seekSequence finds pattern in lines at or after start (the last place it fits,
// when eof), as apply_patch's seek_sequence does: first exactly, then ignoring
// trailing whitespace, then surrounding whitespace, then with typographic
// dashes, quotes and spaces read as their ASCII forms. Pure.
func seekSequence(lines, pattern []string, start int, eof bool) (int, bool) {
	if len(pattern) == 0 {
		return start, true
	}
	if len(pattern) > len(lines) {
		return 0, false
	}
	from, last := start, len(lines)-len(pattern)
	if eof {
		from = last
	}
	for _, same := range []func(a, b string) bool{
		func(a, b string) bool { return a == b },
		func(a, b string) bool {
			return strings.TrimRightFunc(a, unicode.IsSpace) == strings.TrimRightFunc(b, unicode.IsSpace)
		},
		func(a, b string) bool { return strings.TrimSpace(a) == strings.TrimSpace(b) },
		func(a, b string) bool { return asciiPunct(a) == asciiPunct(b) },
	} {
	next:
		for i := from; i <= last; i++ {
			for k, p := range pattern {
				if !same(lines[i+k], p) {
					continue next
				}
			}
			return i, true
		}
	}
	return 0, false
}

// asciiPunct is seek_sequence's last-resort normalisation: s trimmed, with
// Unicode dashes, single and double quotes and spaces as ASCII. Pure.
func asciiPunct(s string) string {
	return strings.Map(func(r rune) rune {
		switch r {
		case '\u2010', '\u2011', '\u2012', '\u2013', '\u2014', '\u2015', '\u2212':
			return '-'
		case '\u2018', '\u2019', '\u201a', '\u201b':
			return '\''
		case '\u201c', '\u201d', '\u201e', '\u201f':
			return '"'
		case '\u00a0', '\u2002', '\u2003', '\u2004', '\u2005', '\u2006', '\u2007', '\u2008',
			'\u2009', '\u200a', '\u202f', '\u205f', '\u3000':
			return ' '
		}
		return r
	}, strings.TrimSpace(s))
}
