package peer

import (
	"net/netip"
	"time"

	"bork/internal/networking/link"
	"bork/internal/protocol"
)

type pendingPing struct {
	challenge uint64
	path      link.Path
	sentAt    time.Time
}

type pathProbe struct {
	path        link.Path
	startedAt   time.Time
	pendingPing pendingPing
}

type PeeringSession struct {
	transcriptHash            [32]byte
	ciphers                   protocol.LinkCiphers
	sessionID                 [16]byte
	path                      link.Path
	candidatePaths            map[netip.AddrPort]*pathProbe
	authenticated             bool
	everAuthenticated         bool
	control                   link.ControlFlow
	media                     *link.MediaFlow
	lastAuthenticatedPacketAt time.Time
	rttMillis                 int64
	pendingPing               pendingPing
}

func newPeeringSession(path link.Path, material protocol.SessionMaterial, now time.Time) (*PeeringSession, error) {
	ciphers, err := protocol.NewLinkCiphers(material.Keys)
	if err != nil {
		return nil, err
	}
	return &PeeringSession{
		transcriptHash:            material.TranscriptHash,
		ciphers:                   ciphers,
		sessionID:                 material.SessionID,
		path:                      path,
		candidatePaths:            make(map[netip.AddrPort]*pathProbe),
		media:                     link.NewMediaFlow(now),
		lastAuthenticatedPacketAt: now,
	}, nil
}

func (peerSess *PeeringSession) acceptsPath(path link.Path) bool {
	if peerSess.path.Address() == path.Address() {
		return true
	}
	_, exists := peerSess.candidatePaths[path.Address()]
	return exists
}

func (peerSess *PeeringSession) candidatePath(address netip.AddrPort) *pathProbe {
	return peerSess.candidatePaths[address]
}

func (peerSess *PeeringSession) clearCandidatePaths() {
	clear(peerSess.candidatePaths)
}
