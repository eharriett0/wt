package cli

import (
	"github.com/eharriett0/wt/internal/coord"
	"github.com/eharriett0/wt/internal/peers"
)

// livePeers is peers.Live; a var so a test can say which sessions are live
// (#250).
var livePeers = peers.Live

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
