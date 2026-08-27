package peer

import (
	"crypto/ecdh"
	"crypto/rand"
	"fmt"
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
	ciphers   protocol.SessionCiphers
	sessionID [16]byte
	// A Session owns one local Hello and X25519 key. Candidate paths reuse them
	// so changing paths cannot accidentally create a second transcript.
	helloPrivateKey            *ecdh.PrivateKey
	localHello                 protocol.HelloPacket
	localHelloPacket           []byte
	remoteHello                protocol.HelloPacket
	path                       Path
	candidatePath              *pathProbe
	authenticated              bool
	everAuthenticated          bool
	packetFlow                 sessionPacketFlow
	reliable                   *reliableTransport
	lastAuthenticatedPacketAt  time.Time
	rttMillis                  int64
	pendingPing                pendingPing
	lastSessionHelloSentAt     time.Time
	lastTopologyAt             time.Time
	topologySentGeneration     uint64
	topologyReceivedGeneration uint64
	memberStateSentGeneration  uint64
	remoteMemberState          memberState
	screenStateSentGeneration  uint64
	remoteScreenState          screenState
	voiceStreamID              [16]byte
	inboundFanout              fanoutAssignment
}

func (c *Client) newInitiatingSession(path Path, now time.Time) (*PeeringSession, error) {
	var handshakeID [16]byte
	if _, err := rand.Read(handshakeID[:]); err != nil {
		return nil, fmt.Errorf("generate handshake ID: %w", err)
	}
	return c.newSessionWithLocalHello(path, handshakeID, now)
}

func (c *Client) newSessionWithLocalHello(path Path, handshakeID [16]byte, now time.Time) (*PeeringSession, error) {
	privateKey, err := ecdh.X25519().GenerateKey(rand.Reader)
	if err != nil {
		return nil, fmt.Errorf("generate X25519 key: %w", err)
	}
	var publicKey [32]byte
	copy(publicKey[:], privateKey.PublicKey().Bytes())
	packet, err := protocol.MarshalSessionHello(c.roomTag, c.admissionKey, c.localIdentity, handshakeID, publicKey)
	if err != nil {
		return nil, fmt.Errorf("marshal session hello: %w", err)
	}
	localHello, err := protocol.ParseHello(packet, c.roomTag, c.admissionKey)
	if err != nil {
		return nil, fmt.Errorf("parse local session hello: %w", err)
	}
	return &PeeringSession{
		helloPrivateKey:           privateKey,
		localHello:                localHello,
		localHelloPacket:          packet,
		path:                      path,
		lastAuthenticatedPacketAt: now,
	}, nil
}

func (peerSess *PeeringSession) completeSessionHello(remoteHello protocol.HelloPacket) error {
	material, err := protocol.DeriveSession(peerSess.helloPrivateKey, peerSess.localHello, remoteHello)
	if err != nil {
		return err
	}
	peerSess.remoteHello = remoteHello
	peerSess.ciphers = material.Ciphers
	peerSess.sessionID = material.SessionID
	peerSess.reliable = newReliableTransport()
	return nil
}

func (peerSess *PeeringSession) sessionReady() bool {
	return peerSess.ciphers.ControlSend != nil && peerSess.ciphers.ControlRecv != nil
}

func (peerSess *PeeringSession) matchesRemoteHello(hello protocol.HelloPacket) bool {
	return !peerSess.remoteHello.IsProbe() &&
		peerSess.remoteHello.PeerID == hello.PeerID &&
		peerSess.remoteHello.HandshakeID == hello.HandshakeID &&
		peerSess.remoteHello.EphemeralKey == hello.EphemeralKey
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
