package peer

import (
	"bytes"

	"bork/internal/identity"
)

type RemotePeer struct {
	peerID         identity.PeerID
	activeSession  *Session
	pendingSession *Session
	memberState    memberState
	screenState    screenState
}

type RemotePeerSnapshot struct {
	PeerID         identity.PeerID
	Address        string
	SessionID      string
	RTTMillis      int64
	Transport      string
	Connected      bool
	Nickname       string
	Muted          bool
	PlaybackMuted  bool
	ScreenSharing  bool
	ScreenStreamID string
}

func comparePeerIDs(left, right identity.PeerID) int {
	return bytes.Compare(left[:], right[:])
}
