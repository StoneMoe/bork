package peer

import "bork/internal/identity"

type RemotePeer struct {
	identity       identity.Identity
	activeSession  *PeeringSession
	pendingSession *PeeringSession
}

type RemotePeerSnapshot struct {
	PeerID           string
	Address          string
	SessionID        string
	RTTMillis        int64
	Transport        string
	Nickname         string
	Muted            bool
	PlaybackMuted    bool
	ScreenSharing    bool
	ScreenGeneration uint64
	ScreenStreamID   string
}
