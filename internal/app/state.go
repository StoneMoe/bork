package app

import (
	"bork/internal/audio"
	"bork/internal/networking"
	"bork/internal/networking/discovery/tracker"
	"bork/internal/networking/endpoint"
	"bork/internal/peer"
)

const (
	stateChangedEvent     = "bork:state-changed"
	screenVideoChunkEvent = "bork:screen-video-chunk"
)

type RemotePeer struct {
	PeerID           string `json:"peerId"`
	Nickname         string `json:"nickname"`
	Muted            bool   `json:"muted"`
	PlaybackMuted    bool   `json:"playbackMuted"`
	Address          string `json:"address"`
	SessionID        string `json:"sessionId"`
	RTTMillis        int64  `json:"rttMillis"`
	Transport        string `json:"transport"`
	ScreenSharing    bool   `json:"screenSharing"`
	ScreenGeneration uint64 `json:"screenGeneration"`
	ScreenStreamID   string `json:"screenStreamId"`
}

type RoomState struct {
	Name             string             `json:"name"`
	Phase            string             `json:"phase"`
	ScreenSharing    bool               `json:"screenSharing"`
	RemotePeers      []RemotePeer       `json:"remotePeers"`
	Transfers        []FileTransfer     `json:"transfers"`
	VirtualLAN       VirtualLAN         `json:"virtualLAN"`
	RemoteVirtualLAN []RemoteVirtualLAN `json:"remoteVirtualLAN"`
}

type VirtualLAN struct {
	Status    string `json:"status"`
	Address   string `json:"address"`
	Interface string `json:"interface"`
	Error     string `json:"error"`
}

type RemoteVirtualLAN struct {
	PeerID   string `json:"peerId"`
	Nickname string `json:"nickname"`
	Address  string `json:"address"`
	Conflict bool   `json:"conflict"`
}

type FileTransfer struct {
	ID           string `json:"id"`
	PeerID       string `json:"peerId"`
	PeerNickname string `json:"peerNickname"`
	Direction    string `json:"direction"`
	Name         string `json:"name"`
	Size         uint64 `json:"size"`
	Transferred  uint64 `json:"transferred"`
	Status       string `json:"status"`
	SHA256       string `json:"sha256"`
	Error        string `json:"error,omitempty"`
	SavedPath    string `json:"savedPath,omitempty"`
}

type ScreenVideoChunkEvent struct {
	PeerID     string `json:"peerId"`
	SessionID  string `json:"sessionId"`
	Generation uint64 `json:"generation"`
	StreamID   string `json:"streamId"`
	ChunkID    uint32 `json:"chunkId"`
	Codec      string `json:"codec"`
	Width      uint16 `json:"width"`
	Height     uint16 `json:"height"`
	Timestamp  uint64 `json:"timestamp"`
	Duration   uint32 `json:"duration"`
	KeyFrame   bool   `json:"keyFrame"`
	Bytes      []byte `json:"bytes"`
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
		PeerID: remotePeer.PeerID, Nickname: remotePeer.Nickname, Muted: remotePeer.Muted, PlaybackMuted: remotePeer.PlaybackMuted,
		Address: remotePeer.Address, SessionID: remotePeer.SessionID,
		RTTMillis: remotePeer.RTTMillis, Transport: remotePeer.Transport,
		ScreenSharing: remotePeer.ScreenSharing, ScreenGeneration: remotePeer.ScreenGeneration, ScreenStreamID: remotePeer.ScreenStreamID,
	}
}

func projectTransfers(transfers []peer.FileTransferSnapshot, remotePeers []peer.RemotePeerSnapshot) []FileTransfer {
	nicknames := make(map[string]string, len(remotePeers))
	for _, remotePeer := range remotePeers {
		nicknames[remotePeer.PeerID] = remotePeer.Nickname
	}
	projected := make([]FileTransfer, 0, len(transfers))
	for _, transfer := range transfers {
		value := FileTransfer{
			ID: transfer.ID, PeerID: transfer.PeerID, PeerNickname: nicknames[transfer.PeerID],
			Direction: transfer.Direction, Name: transfer.Name, Size: transfer.Size,
			Transferred: transfer.Transferred, Status: transfer.Status, SHA256: transfer.SHA256, Error: transfer.Error,
		}
		if transfer.Direction == "incoming" {
			value.SavedPath = transfer.Path
		}
		projected = append(projected, value)
	}
	return projected
}

func projectVirtualLAN(snapshot peer.VirtualLANSnapshot) VirtualLAN {
	if snapshot.Status == "" {
		snapshot.Status = "disabled"
	}
	return VirtualLAN{Status: snapshot.Status, Address: snapshot.Address, Interface: snapshot.Interface, Error: snapshot.Error}
}

func projectRemoteVirtualLAN(snapshots []peer.RemoteVirtualLANSnapshot, remotePeers []peer.RemotePeerSnapshot) []RemoteVirtualLAN {
	nicknames := make(map[string]string, len(remotePeers))
	for _, remotePeer := range remotePeers {
		nicknames[remotePeer.PeerID] = remotePeer.Nickname
	}
	projected := make([]RemoteVirtualLAN, 0, len(snapshots))
	for _, snapshot := range snapshots {
		projected = append(projected, RemoteVirtualLAN{PeerID: snapshot.PeerID, Nickname: nicknames[snapshot.PeerID], Address: snapshot.Address, Conflict: snapshot.Conflict})
	}
	return projected
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
