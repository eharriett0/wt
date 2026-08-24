// wt append (eharriett0/wt#22): a coordinated, section-scoped append to a
// structured shared doc. It inserts text at the end of a named section under an
// exclusive file lock, so parallel gotcha-additions from multiple windows to the
// same file can't clobber each other (a lost update). Companion to the section-
// aware grading in `wt check`/`wt status` and the block-id reservation (#23).
package cli

import (
	"flag"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"

	"github.com/eharriett0/wt/internal/config"
	"github.com/eharriett0/wt/internal/lock"
	"github.com/eharriett0/wt/internal/section"
	"github.com/eharriett0/wt/internal/ui"
)

// defaultSectionDelimiter partitions by markdown headings when a doc has no
// configured structured_doc pattern and none is given via --pattern.
const defaultSectionDelimiter = `^#{1,6}\s`

func cmdAppend(args []string) int {
	fs := flag.NewFlagSet("append", flag.ContinueOnError)
	sec := fs.String("section", "", "heading of the section to append under (exact or contains match)")
	pattern := fs.String("pattern", "", "section-delimiter regexp (default: the doc's structured_doc config, else markdown headings)")
	file := fs.String("file", "", "read the appended text from a file (or - for stdin) instead of the argument — opaque to the shell (#75)")
	pos, _, err := parseInterspersed(fs, args)
	if err != nil {
		return 64
	}
	// text from --file (opaque to the shell) or the positional args after <doc>.
	minPos := 2
	if *file != "" {
		minPos = 1
	}
	if len(pos) < minPos || strings.TrimSpace(*sec) == "" {
		ui.Err(`usage: wt append <doc> --section "<heading>" "<text>"   (or --file <path>)`)
		return 64
	}
	doc := pos[0]
	text, ferr := readFreeform(*file, pos[1:])
	if ferr != nil {
		ui.Err("could not read --file: %v", ferr)
		return 1
	}
	warnSuspiciousFreeform(*file, text)

	return withConfig(func(c *config.Config) int {
		delim := *pattern
		if delim == "" {
			delim = c.StructuredDocs[filepath.Base(doc)]
		}
		if delim == "" {
			delim = defaultSectionDelimiter
		}
		absDoc := doc
		if a, e := filepath.Abs(doc); e == nil {
			absDoc = a
		}

		f, oerr := os.OpenFile(absDoc, os.O_RDWR, 0o644)
		if oerr != nil {
			ui.Err("open %s: %v", doc, oerr)
			return 1
		}
		defer f.Close()
		// Serialize the read-modify-write against other windows' `wt append` to
		// the same doc — without this, two parallel appends lose one update.
		if lerr := lock.Exclusive(f); lerr != nil {
			ui.Err("lock %s: %v", doc, lerr)
			return 1
		}
		defer lock.Release(f)

		content, rerr := io.ReadAll(f)
		if rerr != nil {
			ui.Err("read %s: %v", doc, rerr)
			return 1
		}
		out, found, serr := section.InsertUnderSection(string(content), delim, *sec, text)
		if serr != nil {
			ui.Err("bad section delimiter regexp: %v", serr)
			return 64
		}
		if !found {
			ui.Err("section %q not found in %s", *sec, filepath.Base(doc))
			if re, cerr := section.Compile(delim); cerr == nil {
				if heads := section.Headings(section.Parse(string(content), re)); len(heads) > 0 {
					fmt.Fprintln(os.Stderr, "  available sections:")
					for _, h := range heads {
						fmt.Fprintln(os.Stderr, "    "+h)
					}
				}
			}
			return 1
		}

		// In-place rewrite UNDER the lock: truncate + rewrite the same fd. A
		// temp-file+rename would be crash-atomic but drops the flock's inode,
		// letting a concurrent append interleave — the exact race we're closing.
		if _, e := f.Seek(0, 0); e != nil {
			ui.Err("seek %s: %v", doc, e)
			return 1
		}
		if e := f.Truncate(0); e != nil {
			ui.Err("truncate %s: %v", doc, e)
			return 1
		}
		if _, e := f.WriteString(out); e != nil {
			ui.Err("write %s: %v", doc, e)
			return 1
		}
		ui.OK("appended to section %q in %s", *sec, filepath.Base(doc))
		echoStored(text)
		return 0
	})
}
