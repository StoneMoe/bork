package app

import (
	"bork/internal/audio"
	"bork/internal/networking"
	"bork/internal/networking/endpoint"
	"bork/internal/peer"
)

const stateChangedEvent = "bork:state-changed"

type RemotePeer struct {
	PeerID    string `json:"peerId"`
	Address   string `json:"address"`
	SessionID string `json:"sessionId"`
	RTTMillis int64  `json:"rttMillis"`
}

type RoomState struct {
	Name         string       `json:"name"`
	Phase        string       `json:"phase"`
	LocalAddress string       `json:"localAddress"`
	RemotePeers  []RemotePeer `json:"remotePeers"`
}

type AppError struct {
	ID      uint64 `json:"id"`
	Message string `json:"message"`
}

type AppSnapshot struct {
	Revision    uint64       `json:"revision"`
	PeerID      string       `json:"peerId"`
	Room        *RoomState   `json:"room,omitempty"`
	Audio       audio.Status `json:"audio"`
	Diagnostics Diagnostics  `json:"diagnostics"`
	Error       *AppError    `json:"error,omitempty"`
}

type Diagnostics struct {
	ListenAddress  string                `json:"listenAddress"`
	Candidates     []endpoint.Candidate  `json:"candidates"`
	STUN           []endpoint.STUNResult `json:"stun"`
	NetworkError   string                `json:"networkError,omitempty"`
	DiscoveryError string                `json:"discoveryError,omitempty"`
}

func projectRemotePeer(remotePeer peer.RemotePeerSnapshot) RemotePeer {
	return RemotePeer{PeerID: remotePeer.PeerID, Address: remotePeer.Address, SessionID: remotePeer.SessionID, RTTMillis: remotePeer.RTTMillis}
}

func projectDiagnostics(snapshot networking.RoomSnapshot) Diagnostics {
	return Diagnostics{
		ListenAddress:  snapshot.Endpoint.ListenAddress,
		Candidates:     append([]endpoint.Candidate{}, snapshot.Endpoint.Candidates...),
		STUN:           append([]endpoint.STUNResult{}, snapshot.Endpoint.STUN...),
		NetworkError:   snapshot.NetworkError,
		DiscoveryError: snapshot.DiscoveryError,
	}
}
