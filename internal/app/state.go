package app

import (
	"bork/internal/audio"
	"bork/internal/identity"
	"bork/internal/networking"
	"bork/internal/networking/discovery/tracker"
	"bork/internal/networking/endpoint"
	"bork/internal/peer"
)

const (
	stateChangedEvent       = "bork:state-changed"
	issueEvent              = "bork:issue"
	screenVideoChunkEvent   = "bork:screen-video-chunk"
	screenPreviewChunkEvent = "bork:screen-preview-chunk"
	screenPreviewEndedEvent = "bork:screen-preview-ended"
)

type RemotePeer struct {
	PeerID         identity.PeerID `json:"peerId" ts_type:"string"`
	Nickname       string          `json:"nickname"`
	Muted          bool            `json:"muted"`
	PlaybackMuted  bool            `json:"playbackMuted"`
	Address        string          `json:"address"`
	SessionID      string          `json:"sessionId"`
	RTTMillis      int64           `json:"rttMillis"`
	Transport      string          `json:"transport"`
	Connected      bool            `json:"connected"`
	ScreenSharing  bool            `json:"screenSharing"`
	ScreenStreamID string          `json:"screenStreamId"`
}

type RoomState struct {
	Name          string          `json:"name"`
	PeerID        identity.PeerID `json:"peerId" ts_type:"string"`
	ScreenSharing bool            `json:"screenSharing"`
	RemotePeers   []RemotePeer    `json:"remotePeers"`
	Transfers     []FileTransfer  `json:"transfers"`
}

type FileTransfer struct {
	ID           string          `json:"id"`
	PeerID       identity.PeerID `json:"peerId" ts_type:"string"`
	PeerNickname string          `json:"peerNickname"`
	Direction    string          `json:"direction"`
	Name         string          `json:"name"`
	Size         uint64          `json:"size"`
	Transferred  uint64          `json:"transferred"`
	Status       string          `json:"status"`
	SHA256       string          `json:"sha256"`
	Error        string          `json:"error,omitempty"`
	SavedPath    string          `json:"savedPath,omitempty"`
}

// Width and Height are the fixed coded size. DisplayWidth and DisplayHeight
// describe the centered visible area after the captured window changes size.
type ScreenVideoChunkEvent struct {
	PeerID        identity.PeerID `json:"peerId" ts_type:"string"`
	StreamID      string          `json:"streamId"`
	ChunkID       uint32          `json:"chunkId"`
	Codec         string          `json:"codec"`
	Width         uint16          `json:"width"`
	Height        uint16          `json:"height"`
	DisplayWidth  uint16          `json:"displayWidth"`
	DisplayHeight uint16          `json:"displayHeight"`
	Timestamp     uint64          `json:"timestamp"`
	Duration      uint32          `json:"duration"`
	KeyFrame      bool            `json:"keyFrame"`
	Bytes         []byte          `json:"bytes"`
}

// ScreenPreviewChunkEvent uses the same coded and display size split.
type ScreenPreviewChunkEvent struct {
	CaptureID     uint32 `json:"captureId"`
	Codec         string `json:"codec"`
	Width         int    `json:"width"`
	Height        int    `json:"height"`
	DisplayWidth  int    `json:"displayWidth"`
	DisplayHeight int    `json:"displayHeight"`
	Timestamp     uint64 `json:"timestamp"`
	Duration      uint32 `json:"duration"`
	KeyFrame      bool   `json:"keyFrame"`
	Bytes         []byte `json:"bytes"`
}

type AppIssueType string

const (
	IssueTypeRoom   AppIssueType = "room"
	IssueTypeAudio  AppIssueType = "audio"
	IssueTypeScreen AppIssueType = "screen"
)

type AppIssueLevel string

const (
	IssueLevelWarning AppIssueLevel = "warning"
	IssueLevelError   AppIssueLevel = "error"
)

type AppIssue struct {
	Type    AppIssueType  `json:"type"`
	Level   AppIssueLevel `json:"level"`
	Message string        `json:"message"`
}

type AppSnapshot struct {
	Version     string            `json:"version"`
	Nickname    string            `json:"nickname"`
	Room        *RoomState        `json:"room,omitempty"`
	Audio       audio.Status      `json:"audio"`
	Diagnostics Diagnostics       `json:"diagnostics"`
	GameProxy   GameProxySnapshot `json:"gameProxy"`
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
		RTTMillis: remotePeer.RTTMillis, Transport: remotePeer.Transport, Connected: remotePeer.Connected,
		ScreenSharing: remotePeer.ScreenSharing, ScreenStreamID: remotePeer.ScreenStreamID,
	}
}

func projectTransfers(transfers []peer.FileTransferSnapshot, remotePeers []peer.RemotePeerSnapshot) []FileTransfer {
	nicknames := make(map[identity.PeerID]string, len(remotePeers))
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
