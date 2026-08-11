package peer

import (
	"time"

	"bork/internal/protocol"
)

type pendingPing struct {
	challenge uint64
	path      Path
	sentAt    time.Time
}

type pathProbe struct {
	path        Path
	startedAt   time.Time
	pendingPing pendingPing
}

type PeeringSession struct {
	ciphers                    protocol.SessionCiphers
	sessionID                  [16]byte
	path                       Path
	candidatePath              *pathProbe
	authenticated              bool
	everAuthenticated          bool
	control                    controlFlow
	reliable                   *reliableTransport
	lastAuthenticatedPacketAt  time.Time
	rttMillis                  int64
	pendingPing                pendingPing
	lastHelloSentAt            time.Time
	lastTopologyAt             time.Time
	topologySentGeneration     uint64
	topologyReceivedGeneration uint64
	memberStateSentGeneration  uint64
	remoteMemberState          memberState
	screenStateSentGeneration  uint64
	remoteScreenState          screenState
	audioStreamID              [16]byte
	inboundFanout              fanoutAssignment
}

func newPeeringSession(path Path, material protocol.SessionMaterial, now time.Time) *PeeringSession {
	return &PeeringSession{
		ciphers:                   material.Ciphers,
		sessionID:                 material.SessionID,
		path:                      path,
		reliable:                  newReliableTransport(),
		lastAuthenticatedPacketAt: now,
	}
}

func (peerSess *PeeringSession) acceptsPath(path Path) bool {
	return peerSess.path.SameRoute(path) || peerSess.candidateProbe(path) != nil
}

func (peerSess *PeeringSession) candidateProbe(path Path) *pathProbe {
	if peerSess.candidatePath != nil && peerSess.candidatePath.path.SameRoute(path) {
		return peerSess.candidatePath
	}
	return nil
}

func (peerSess *PeeringSession) acceptsDataPath(path Path) bool {
	return peerSess.authenticated && peerSess.path.SameRoute(path)
}

func (peerSess *PeeringSession) clearCandidatePath() {
	peerSess.candidatePath = nil
}
