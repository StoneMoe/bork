package app

import (
	"bork/internal/audio"
	"bork/internal/networking"
	"bork/internal/networking/discovery/tracker"
	"bork/internal/networking/endpoint"
	"bork/internal/peer"
)

const stateChangedEvent = "bork:state-changed"

type RemotePeer struct {
	PeerID    string `json:"peerId"`
	Nickname  string `json:"nickname"`
	Muted     bool   `json:"muted"`
	Address   string `json:"address"`
	SessionID string `json:"sessionId"`
	RTTMillis int64  `json:"rttMillis"`
	Transport string `json:"transport"`
}

type RoomState struct {
	Name        string       `json:"name"`
	Phase       string       `json:"phase"`
	RemotePeers []RemotePeer `json:"remotePeers"`
}

type AppError struct {
	ID      uint64 `json:"id"`
	Message string `json:"message"`
}

type AppSnapshot struct {
	Revision    uint64       `json:"revision"`
	PeerID      string       `json:"peerId"`
	Nickname    string       `json:"nickname"`
	Room        *RoomState   `json:"room,omitempty"`
	Audio       audio.Status `json:"audio"`
	Diagnostics Diagnostics  `json:"diagnostics"`
	Error       *AppError    `json:"error,omitempty"`
}

type Diagnostics struct {
	ListenAddress    string                    `json:"listenAddress"`
	Candidates       []endpoint.Candidate      `json:"candidates"`
	STUN             []endpoint.STUNResult     `json:"stun"`
	NetworkError     string                    `json:"networkError,omitempty"`
	DiscoveryError   string                    `json:"discoveryError,omitempty"`
	PortMappingError string                    `json:"portMappingError,omitempty"`
	Tracker          []tracker.ProviderStatus  `json:"tracker"`
	Connectivity     peer.ConnectivitySnapshot `json:"connectivity"`
}

func projectRemotePeer(remotePeer peer.RemotePeerSnapshot) RemotePeer {
	return RemotePeer{
		PeerID: remotePeer.PeerID, Nickname: remotePeer.Nickname, Muted: remotePeer.Muted,
		Address: remotePeer.Address, SessionID: remotePeer.SessionID,
		RTTMillis: remotePeer.RTTMillis, Transport: remotePeer.Transport,
	}
}

func projectDiagnostics(snapshot networking.RoomSnapshot, connectivity peer.ConnectivitySnapshot) Diagnostics {
	return Diagnostics{
		ListenAddress:    snapshot.Endpoint.ListenAddress,
		Candidates:       snapshot.Endpoint.Candidates,
		STUN:             snapshot.Endpoint.STUN,
		NetworkError:     snapshot.NetworkError,
		DiscoveryError:   snapshot.DiscoveryError,
		PortMappingError: snapshot.PortMappingError,
		Tracker:          snapshot.Tracker,
		Connectivity:     connectivity,
	}
}
