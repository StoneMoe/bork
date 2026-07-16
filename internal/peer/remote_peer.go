package peer

import "bork/internal/identity"

type RemotePeer struct {
	identity      identity.Identity
	peerSess      *PeeringSession
	candidateSess *PeeringSession
}

type RemotePeerSnapshot struct {
	PeerID    string `json:"peerId"`
	Address   string `json:"address"`
	SessionID string `json:"sessionId"`
	RTTMillis int64  `json:"rttMillis"`
}
