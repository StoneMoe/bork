package peer

import (
	"crypto/ecdh"
	"crypto/rand"
	"fmt"
	"time"

	"bork/internal/identity"
	"bork/internal/protocol"
)

type pendingPing struct {
	packetSequence uint64
	path           Path
	sentAt         time.Time
}

type pathProbe struct {
	path        Path
	startedAt   time.Time
	pendingPing pendingPing
}

type Session struct {
	ciphers protocol.SessionCiphers
	// A Session owns one local Hello and X25519 key. Candidate paths reuse them
	// so changing paths cannot accidentally create a second transcript.
	helloPrivateKey           *ecdh.PrivateKey
	localHello                protocol.SessionHello
	localHelloPacket          []byte
	remoteHello               protocol.SessionHello
	path                      Path
	candidatePath             *pathProbe
	authenticated             bool
	everAuthenticated         bool
	packetFlow                sessionPacketFlow
	reliable                  *reliableTransport
	lastAuthenticatedPacketAt time.Time
	rttMillis                 int64
	pendingPing               pendingPing
	lastSessionHelloSentAt    time.Time
	lastTopologyAt            time.Time
	topologySentRevision      uint64
	memberStateSentRevision   uint64
	screenStateSentRevision   uint64
	inboundFanout             []identity.PeerID
}

func (c *Client) newInitiatingSession(path Path, now time.Time) (*Session, error) {
	var sessionID [16]byte
	if _, err := rand.Read(sessionID[:]); err != nil {
		return nil, fmt.Errorf("generate session ID: %w", err)
	}
	return c.newSessionWithLocalHello(path, sessionID, now)
}

func (c *Client) newSessionWithLocalHello(path Path, sessionID [16]byte, now time.Time) (*Session, error) {
	privateKey, err := ecdh.X25519().GenerateKey(rand.Reader)
	if err != nil {
		return nil, fmt.Errorf("generate X25519 key: %w", err)
	}
	var publicKey [32]byte
	copy(publicKey[:], privateKey.PublicKey().Bytes())
	packet, err := protocol.MarshalSessionHello(c.admissionKey, c.localPeerID, sessionID, publicKey)
	if err != nil {
		return nil, fmt.Errorf("marshal session hello: %w", err)
	}
	localHello, err := protocol.ParseSessionHello(packet, c.admissionKey)
	if err != nil {
		return nil, fmt.Errorf("parse local session hello: %w", err)
	}
	return &Session{
		helloPrivateKey:           privateKey,
		localHello:                localHello,
		localHelloPacket:          packet,
		path:                      path,
		lastAuthenticatedPacketAt: now,
	}, nil
}

func (session *Session) id() [16]byte { return session.localHello.SessionID }

func (session *Session) completeSessionHello(remoteHello protocol.SessionHello) error {
	ciphers, err := protocol.DeriveSession(session.helloPrivateKey, session.localHello, remoteHello)
	if err != nil {
		return err
	}
	session.remoteHello = remoteHello
	session.ciphers = ciphers
	session.reliable = newReliableTransport()
	return nil
}

func (session *Session) sessionReady() bool {
	return session.ciphers.Send != nil && session.ciphers.Receive != nil
}

func (session *Session) matchesRemoteHello(hello protocol.SessionHello) bool {
	return session.remoteHello.SessionID == hello.SessionID &&
		session.remoteHello.PeerID == hello.PeerID &&
		session.remoteHello.EphemeralKey == hello.EphemeralKey
}

func (session *Session) acceptsPath(path Path) bool {
	return session.path.SameRoute(path) || session.candidateProbe(path) != nil
}

func (session *Session) candidateProbe(path Path) *pathProbe {
	if session.candidatePath != nil && session.candidatePath.path.SameRoute(path) {
		return session.candidatePath
	}
	return nil
}

func (session *Session) acceptsDataPath(path Path) bool {
	return session.authenticated && session.path.SameRoute(path)
}

func (session *Session) clearCandidatePath() {
	session.candidatePath = nil
}
