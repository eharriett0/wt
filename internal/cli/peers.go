package cli

import (
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"

	"github.com/eharriett0/wt/internal/collide"
	"github.com/eharriett0/wt/internal/coord"
	"github.com/eharriett0/wt/internal/gitx"
	"github.com/eharriett0/wt/internal/peers"
)

// livePeers is peers.Live; a var so a test can say which sessions are live
// (#250).
var livePeers = peers.Live

// sessionNote renders the live sessions working in a worktree as a message
// suffix, " (live: `api-worker`, idle; coordinate with SendMessage)", or ""
// (#250, reshaped: wt detects a collision, the agent talks to the session in
// the other window). Annotation only.
type sessionNote func(worktree string) string

// worktreeSessionNotes is the sessionNote wt uses. It reads the listing, and
// the repository's worktrees, once and only when first asked for a note: the
// callers ask only when they have another window's collision to show.
func worktreeSessionNotes() sessionNote {
	var once sync.Once
	var by map[string][]peers.Peer
	return func(wt string) string {
		if wt == "" {
			return ""
		}
		once.Do(func() {
			live, ok := livePeers()
			if !ok || len(live) == 0 {
				return
			}
			paths, err := gitx.WorktreePaths()
			if err != nil {
				return
			}
			self, _ := coord.SessionToken(os.Getenv)
			by = assignPeers(paths, live, self)
		})
		return collisionNote(by[realPath(wt)])
	}
}

// assignPeers credits each live session, but self's, to the deepest worktree
// holding its working directory: worktrees nest (one under the main
// checkout's root), and a nested worktree's session is not its parent's.
// Keyed by realPath. A session outside every worktree is credited to none.
// Pure but for resolving symlinks.
func assignPeers(worktrees []string, live map[string]peers.Peer, self string) map[string][]peers.Peer {
	wts := make([]string, len(worktrees))
	for i, w := range worktrees {
		wts[i] = realPath(w)
	}
	ids := make([]string, 0, len(live))
	for id := range live {
		ids = append(ids, id)
	}
	sort.Strings(ids)
	by := map[string][]peers.Peer{}
	for _, id := range ids {
		p := live[id]
		if id == self || p.Cwd == "" {
			continue
		}
		cwd, best := realPath(p.Cwd), ""
		for _, w := range wts {
			if within(cwd, w) && len(w) > len(best) {
				best = w
			}
		}
		if best != "" {
			by[best] = append(by[best], p)
		}
	}
	return by
}

// within reports whether path p is dir or under it. Pure.
func within(p, dir string) bool {
	rel, err := filepath.Rel(dir, p)
	return err == nil && rel != ".." && !strings.HasPrefix(rel, ".."+string(filepath.Separator))
}

// realPath is p with the symlinks of its deepest existing ancestor resolved
// and the rest kept (macOS's /var is /private/var, which is how git reports a
// worktree there), or p cleaned when nothing of it exists.
func realPath(p string) string {
	p = filepath.Clean(p)
	rest := ""
	for q := p; ; {
		if r, err := filepath.EvalSymlinks(q); err == nil {
			return filepath.Join(r, rest)
		}
		parent := filepath.Dir(q)
		if parent == q {
			return p
		}
		rest = filepath.Join(filepath.Base(q), rest)
		q = parent
	}
}

// collisionNote renders ps for a collision line, "" for none. Pure.
func collisionNote(ps []peers.Peer) string {
	if len(ps) == 0 {
		return ""
	}
	parts := make([]string, 0, len(ps))
	for _, p := range ps {
		s := "an unnamed session"
		if p.Name != "" {
			s = "`" + p.Name + "`"
		}
		if p.Status != "" {
			s += ", " + p.Status
		}
		parts = append(parts, s)
	}
	return " (live: " + strings.Join(parts, "; ") + "; coordinate with SendMessage)"
}

// labelSessionNotes maps each other window the per-turn banner will show
// (overlaps, minus current) to its live sessions' note (#250): the banner
// names windows by label, and a label two worktrees share gets both their
// notes. note is asked only when there is such a window. Pure given note.
func labelSessionNotes(ws []collide.Window, current string, overlaps []StatusOverlap, note sessionNote) map[string]string {
	shown := map[string]bool{}
	for _, o := range overlaps {
		for _, l := range windowsExcluding(o.Windows, current) {
			shown[l] = true
		}
	}
	if len(shown) == 0 || note == nil {
		return nil
	}
	notes := map[string]string{}
	for _, w := range ws {
		l := w.Label()
		if !shown[l] {
			continue
		}
		if n := note(w.Worktree); n != "" && !strings.Contains(notes[l], n) {
			notes[l] += n
		}
	}
	return notes
}

// annotateLive sets each entry's liveNote from note, by the worktree it was
// graded against (#250). A nil note leaves them empty.
func annotateLive(entries []CheckEntry, note sessionNote) {
	if note == nil {
		return
	}
	for i := range entries {
		entries[i].liveNote = note(entries[i].otherWorktree)
	}
}

// peerLookup finds the live Claude Code session a record came from (#250).
type peerLookup func(session string) (peers.Peer, bool)

// livePeerOf is the peerLookup wt uses: only a real session token is looked
// up, and a listing wt couldn't read finds nothing (unknown, never "gone").
func livePeerOf(session string) (peers.Peer, bool) {
	if session == "" || session == coord.SessionNone {
		return peers.Peer{}, false
	}
	live, ok := livePeers()
	if !ok {
		return peers.Peer{}, false
	}
	p, ok := live[session]
	return p, ok
}

// livePeerName is coord.NameLive's lookup, through livePeerOf.
func livePeerName(session string) (name, status string, ok bool) {
	p, ok := livePeerOf(session)
	return p.Name, p.Status, ok
}

// postedByLive is postedBy plus, for another party's record whose session is
// live on this machine, its name and status and how to reach it (#250): an
// agent blocked by a hold can ask the holder with SendMessage. Yours, and a
// record wt can't attribute, read as postedBy does. Annotation only: a
// session the listing doesn't show is never called gone.
func postedByLive(r coord.Record, v coord.View) string {
	s := postedBy(r, v)
	if p, ok := otherPeer(r, v, livePeerOf); ok {
		s += " (" + peers.Describe(p) + ")"
	}
	return s
}

// otherPeer is the live session behind r when r is another party's: another
// session in this checkout, or another window, never a record this session
// provably wrote. Pure given lookup.
func otherPeer(r coord.Record, v coord.View, lookup peerLookup) (peers.Peer, bool) {
	if lookup == nil || v.ProvablyWrote(r) {
		return peers.Peer{}, false
	}
	switch v.Attribute(r) {
	case coord.AttrAnotherSession, coord.AttrOtherWindow:
		return lookup(r.Session)
	}
	return peers.Peer{}, false
}
