package peer

import (
	"crypto/ecdh"
	"crypto/ed25519"
	"crypto/rand"
	"log/slog"
	"net/netip"
	"testing"
	"time"

	"bork/internal/identity"
	"bork/internal/invite"
	"bork/internal/media"
	"bork/internal/networking/endpoint"
	"bork/internal/networking/link"
	"bork/internal/protocol"
)

func TestMismatchedChallengeCannotEstablishSession(t *testing.T) {
	peerRemoteIdentity := testRemoteIdentity(t)
	path, err := link.NewPath(netip.MustParseAddrPort("127.0.0.1:9000"))
	if err != nil {
		t.Fatal(err)
	}
	peerSess := testPeeringSession(t, path)
	peerSess.pendingPing = pendingPing{challenge: 99, path: path, sentAt: time.Now()}
	peerRemote := &RemotePeer{identity: peerRemoteIdentity, candidateSess: peerSess}
	client := &Client{
		logger:      slog.Default(),
		roomTag:     [16]byte{1},
		remotePeers: map[string]*RemotePeer{peerRemoteIdentity.PeerID(): peerRemote},
	}
	packet, err := protocol.MarshalControl(protocol.PacketPong, client.roomTag, peerSess.sessionID, 1, 98, peerSess.ciphers.ControlRecv)
	if err != nil {
		t.Fatal(err)
	}
	client.handleSessionPacket(endpoint.Datagram{Data: packet, From: path.Address()})
	if peerSess.authenticated || peerRemote.peerSess != nil {
		t.Fatal("mismatched challenge established a session")
	}
}

func TestRemotePeerExpiryPreservesPeeringSession(t *testing.T) {
	peerRemoteIdentity := testRemoteIdentity(t)
	path, err := link.NewPath(netip.MustParseAddrPort("127.0.0.1:9000"))
	if err != nil {
		t.Fatal(err)
	}
	peerSess := testPeeringSession(t, path)
	peerSess.authenticated = true
	peerSess.everAuthenticated = true
	peerSess.lastAuthenticatedPacketAt = time.Now().Add(-remotePeerTimeout - time.Second)
	for range 12 {
		if _, err := peerSess.control.NextSendSequence(); err != nil {
			t.Fatal(err)
		}
	}
	peerRemote := &RemotePeer{identity: peerRemoteIdentity, peerSess: peerSess}
	client := &Client{
		logger:              slog.Default(),
		remotePeers:         map[string]*RemotePeer{peerRemoteIdentity.PeerID(): peerRemote},
		discoveredAddresses: map[netip.AddrPort]discoveredAddress{path.Address(): {}},
	}
	client.expireRemotePeers()
	kept := client.remotePeers[peerRemoteIdentity.PeerID()]
	if kept != peerRemote || kept.peerSess != peerSess || peerSess.authenticated {
		t.Fatal("expired peering session was replaced, removed, or remained online")
	}
	if next, err := peerSess.control.NextSendSequence(); err != nil || next != 13 {
		t.Fatalf("control flow was reset: next sequence = %d, %v", next, err)
	}
}

func TestSendMediaSkipsCandidateOnlyRemotePeer(t *testing.T) {
	peerRemoteIdentity := testRemoteIdentity(t)
	path, err := link.NewPath(netip.MustParseAddrPort("127.0.0.1:9000"))
	if err != nil {
		t.Fatal(err)
	}
	candidate := testPeeringSession(t, path)
	client := &Client{
		roomNetwork: &fakeRoomNetwork{},
		remotePeers: map[string]*RemotePeer{
			peerRemoteIdentity.PeerID(): {
				identity:      peerRemoteIdentity,
				candidateSess: candidate,
			},
		},
	}
	client.sendMedia(media.SendFrame{Timestamp: 480, Payload: []byte{1}})
	if next, err := candidate.media.NextSendSequence(); err != nil || next != 1 {
		t.Fatalf("candidate media flow advanced: next sequence = %d, %v", next, err)
	}
}

func TestCandidateExpiryReusesTranscriptAssociation(t *testing.T) {
	client, remoteIdentity, remoteHello := testHelloClient(t)
	from := netip.MustParseAddrPort("127.0.0.1:9001")
	client.handleHello(endpoint.Datagram{Data: remoteHello, From: from})
	first := client.remotePeers[remoteIdentity.PeerID()].candidateSess
	challenge := first.pendingPing.challenge
	client.handleHello(endpoint.Datagram{Data: remoteHello, From: from})
	if first.pendingPing.challenge != challenge || challenge == 0 {
		t.Fatal("duplicate hello reset the pending candidate challenge")
	}
	before, err := first.control.NextSendSequence()
	if err != nil {
		t.Fatal(err)
	}
	first.lastAuthenticatedPacketAt = time.Now().Add(-remotePeerTimeout - time.Second)
	client.expireRemotePeers()
	if client.associations[first.transcriptHash] != first {
		t.Fatal("candidate expiry discarded cryptographic association")
	}
	client.handleHello(endpoint.Datagram{Data: remoteHello, From: from})
	second := client.remotePeers[remoteIdentity.PeerID()].candidateSess
	after, err := second.control.NextSendSequence()
	if err != nil {
		t.Fatal(err)
	}
	if second != first || after <= before {
		t.Fatalf("association was recreated or reset: same=%v before=%d after=%d", second == first, before, after)
	}
}

func TestReplayedHelloDoesNotPoisonTimedOutPrimaryPath(t *testing.T) {
	client, remoteIdentity, remoteHello := testHelloClient(t)
	primaryAddress := netip.MustParseAddrPort("127.0.0.1:9010")
	replayAddress := netip.MustParseAddrPort("127.0.0.1:9011")
	peerSess := establishHelloSession(t, client, remoteIdentity, remoteHello, primaryAddress)

	peerSess.lastAuthenticatedPacketAt = time.Now().Add(-remotePeerTimeout - time.Second)
	client.expireRemotePeers()
	if peerSess.authenticated {
		t.Fatal("timed-out session remained authenticated")
	}

	client.handleHello(endpoint.Datagram{Data: remoteHello, From: replayAddress})
	if peerSess.path.Address() != primaryAddress {
		t.Fatalf("replayed Hello changed primary path to %v", peerSess.path.Address())
	}
	if peerSess.pendingPing.challenge == 0 || peerSess.pendingPing.path.Address() != primaryAddress {
		t.Fatal("replayed Hello diverted the primary liveness probe")
	}
	replayProbe := peerSess.candidatePath(replayAddress)
	if replayProbe == nil || replayProbe.pendingPing.challenge == 0 || replayProbe.pendingPing.path.Address() != replayAddress {
		t.Fatal("replayed Hello did not create an independent candidate path probe")
	}
	otherAddress := netip.MustParseAddrPort("127.0.0.1:9012")
	client.handleHello(endpoint.Datagram{Data: remoteHello, From: otherAddress})
	if peerSess.candidatePath(replayAddress) == nil || peerSess.candidatePath(otherAddress) == nil {
		t.Fatal("bounded path probes did not retain concurrent candidates")
	}

	for _, probe := range peerSess.candidatePaths {
		probe.startedAt = time.Now().Add(-remotePeerTimeout - time.Second)
	}
	client.expireRemotePeers()
	if len(peerSess.candidatePaths) != 0 {
		t.Fatal("candidate path probe survived expiry while the session was unauthenticated")
	}
	if peerSess.pendingPing.path.Address() != primaryAddress {
		t.Fatal("candidate path expiry changed the primary liveness probe")
	}

	client.handleHello(endpoint.Datagram{Data: remoteHello, From: replayAddress})
	if peerSess.candidatePath(replayAddress) == nil {
		t.Fatal("replayed Hello did not recreate a candidate path probe")
	}
	client.handleHello(endpoint.Datagram{Data: remoteHello, From: primaryAddress})
	if peerSess.candidatePath(replayAddress) == nil {
		t.Fatal("unproven primary-path Hello cleared the candidate probe")
	}
}

func TestMatchingCandidatePathPongMigratesExistingSession(t *testing.T) {
	client, remoteIdentity, remoteHello := testHelloClient(t)
	primaryAddress := netip.MustParseAddrPort("127.0.0.1:9020")
	candidateAddress := netip.MustParseAddrPort("127.0.0.1:9021")
	peerSess := establishHelloSession(t, client, remoteIdentity, remoteHello, primaryAddress)
	peerRemote := client.remotePeers[remoteIdentity.PeerID()]
	sessionID := peerSess.sessionID
	controlSequenceBefore, err := peerSess.control.NextSendSequence()
	if err != nil {
		t.Fatal(err)
	}

	client.handleHello(endpoint.Datagram{Data: remoteHello, From: candidateAddress})
	peerSess.pendingPing.challenge = 101
	candidateProbe := peerSess.candidatePath(candidateAddress)
	if candidateProbe == nil {
		t.Fatal("candidate path probe was not created")
	}
	candidateProbe.pendingPing.challenge = 202
	wrongPong, err := protocol.MarshalControl(protocol.PacketPong, client.roomTag, sessionID, 2, 101, peerSess.ciphers.ControlRecv)
	if err != nil {
		t.Fatal(err)
	}
	client.handleSessionPacket(endpoint.Datagram{Data: wrongPong, From: candidateAddress})
	if peerSess.path.Address() != primaryAddress || peerSess.candidatePath(candidateAddress) == nil {
		t.Fatal("primary challenge promoted the candidate path")
	}

	matchingPong, err := protocol.MarshalControl(protocol.PacketPong, client.roomTag, sessionID, 3, 202, peerSess.ciphers.ControlRecv)
	if err != nil {
		t.Fatal(err)
	}
	client.handleSessionPacket(endpoint.Datagram{Data: matchingPong, From: candidateAddress})
	if peerRemote.peerSess != peerSess || peerSess.sessionID != sessionID {
		t.Fatal("path migration replaced the authenticated session")
	}
	if !peerSess.authenticated || peerSess.path.Address() != candidateAddress {
		t.Fatalf("candidate path was not promoted: authenticated=%v path=%v", peerSess.authenticated, peerSess.path.Address())
	}
	if len(peerSess.candidatePaths) != 0 {
		t.Fatal("promoted candidate path probe was not cleared")
	}
	controlSequenceAfter, err := peerSess.control.NextSendSequence()
	if err != nil || controlSequenceAfter <= controlSequenceBefore {
		t.Fatalf("path migration reset the control sequence: before=%d after=%d err=%v", controlSequenceBefore, controlSequenceAfter, err)
	}
}

func TestCandidatePathPingDoesNotRefreshPrimaryLiveness(t *testing.T) {
	client, remoteIdentity, remoteHello := testHelloClient(t)
	primaryAddress := netip.MustParseAddrPort("127.0.0.1:9030")
	candidateAddress := netip.MustParseAddrPort("127.0.0.1:9031")
	peerSess := establishHelloSession(t, client, remoteIdentity, remoteHello, primaryAddress)
	client.handleHello(endpoint.Datagram{Data: remoteHello, From: candidateAddress})
	oldAuthenticatedAt := time.Now().Add(-remotePeerTimeout - time.Second)
	peerSess.lastAuthenticatedPacketAt = oldAuthenticatedAt
	ping, err := protocol.MarshalControl(protocol.PacketPing, client.roomTag, peerSess.sessionID, 2, 77, peerSess.ciphers.ControlRecv)
	if err != nil {
		t.Fatal(err)
	}
	client.handleSessionPacket(endpoint.Datagram{Data: ping, From: candidateAddress})
	if !peerSess.lastAuthenticatedPacketAt.Equal(oldAuthenticatedAt) {
		t.Fatal("unproven candidate path refreshed primary liveness")
	}
}

func TestCandidatePathSetIsBoundedAndPrioritizesDiscoveredAddress(t *testing.T) {
	client, remoteIdentity, remoteHello := testHelloClient(t)
	primaryAddress := netip.MustParseAddrPort("127.0.0.1:9040")
	peerSess := establishHelloSession(t, client, remoteIdentity, remoteHello, primaryAddress)
	for index := range maxCandidatePaths {
		address := netip.AddrPortFrom(netip.AddrFrom4([4]byte{192, 0, 2, byte(index + 1)}), 9040)
		client.handleHello(endpoint.Datagram{Data: remoteHello, From: address})
	}
	if len(peerSess.candidatePaths) != maxCandidatePaths {
		t.Fatalf("candidate path count = %d, want %d", len(peerSess.candidatePaths), maxCandidatePaths)
	}
	discovered := netip.MustParseAddrPort("198.51.100.1:9040")
	client.rememberDiscoveredAddress(discovered, time.Now())
	client.handleHello(endpoint.Datagram{Data: remoteHello, From: discovered})
	if len(peerSess.candidatePaths) != maxCandidatePaths || peerSess.candidatePath(discovered) == nil {
		t.Fatal("discovered migration address did not replace an untrusted path probe")
	}
}

func TestInactiveRemotePeerCanBeEvictedWithoutDroppingAssociation(t *testing.T) {
	inactiveIdentity := testRemoteIdentity(t)
	activeIdentity := testRemoteIdentity(t)
	path, err := link.NewPath(netip.MustParseAddrPort("127.0.0.1:9002"))
	if err != nil {
		t.Fatal(err)
	}
	inactiveSession := testPeeringSession(t, path)
	inactiveSession.authenticated = false
	inactiveSession.lastAuthenticatedPacketAt = time.Unix(1, 0)
	activeSession := testPeeringSession(t, path)
	activeSession.authenticated = true
	client := &Client{
		remotePeers: map[string]*RemotePeer{
			inactiveIdentity.PeerID(): {identity: inactiveIdentity, peerSess: inactiveSession},
			activeIdentity.PeerID():   {identity: activeIdentity, peerSess: activeSession},
		},
		associations:        map[[32]byte]*PeeringSession{inactiveSession.transcriptHash: inactiveSession},
		discoveredAddresses: map[netip.AddrPort]discoveredAddress{},
	}
	if !client.evictInactiveRemotePeer() {
		t.Fatal("inactive peer was not evicted")
	}
	if client.remotePeers[inactiveIdentity.PeerID()] != nil || client.remotePeers[activeIdentity.PeerID()] == nil {
		t.Fatal("wrong remote peer was evicted")
	}
	if client.associations[inactiveSession.transcriptHash] != inactiveSession {
		t.Fatal("eviction discarded the nonce-safety association")
	}
}

func TestDiscoveredAddressLimitEvictsOldestUnusedAddress(t *testing.T) {
	activeAddress := netip.MustParseAddrPort("192.0.2.1:9000")
	activePath, err := link.NewPath(activeAddress)
	if err != nil {
		t.Fatal(err)
	}
	activeIdentity := testRemoteIdentity(t)
	client := &Client{
		remotePeers: map[string]*RemotePeer{
			activeIdentity.PeerID(): {identity: activeIdentity, peerSess: testPeeringSession(t, activePath)},
		},
		discoveredAddresses: make(map[netip.AddrPort]discoveredAddress),
	}
	base := time.Unix(1000, 0)
	for index := range maxDiscoveredAddresses {
		address := netip.AddrPortFrom(netip.AddrFrom4([4]byte{192, 0, 2, byte(index + 1)}), 9000)
		client.rememberDiscoveredAddress(address, base.Add(time.Duration(index)*time.Second))
	}
	oldestUnused := netip.MustParseAddrPort("192.0.2.2:9000")
	newAddress := netip.MustParseAddrPort("198.51.100.1:9000")
	client.rememberDiscoveredAddress(newAddress, base.Add(time.Hour))
	if _, exists := client.discoveredAddresses[activeAddress]; !exists {
		t.Fatal("LRU eviction removed an address used by an active session")
	}
	if _, exists := client.discoveredAddresses[oldestUnused]; exists {
		t.Fatal("LRU eviction retained the oldest unused address")
	}
	if _, exists := client.discoveredAddresses[newAddress]; !exists {
		t.Fatal("LRU eviction did not retain the new address")
	}
}

func testPeeringSession(t *testing.T, path link.Path) *PeeringSession {
	t.Helper()
	material := protocol.SessionMaterial{SessionID: [16]byte{2}, TranscriptHash: [32]byte{3}}
	material.Keys.ControlSend[0] = 1
	material.Keys.ControlRecv[0] = 2
	material.Keys.VoiceSend[0] = 3
	material.Keys.VoiceRecv[0] = 4
	session, err := newPeeringSession(path, material, time.Now())
	if err != nil {
		t.Fatal(err)
	}
	return session
}

func testHelloClient(t *testing.T) (*Client, *identity.LocalIdentity, []byte) {
	t.Helper()
	localIdentity, err := identity.LoadOrCreate(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	remoteIdentity, err := identity.LoadOrCreate(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	roomInvite, err := invite.New("Hello test")
	if err != nil {
		t.Fatal(err)
	}
	client := NewClient(localIdentity, roomInvite, endpoint.Options{}, slog.Default())
	client.roomNetwork = &fakeRoomNetwork{}
	if err := client.rotateHelloEpoch(); err != nil {
		t.Fatal(err)
	}
	remotePrivate, err := ecdh.X25519().GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	var remotePublic [32]byte
	copy(remotePublic[:], remotePrivate.PublicKey().Bytes())
	remoteHello, err := protocol.MarshalHello(client.roomTag, client.admissionKey, remoteIdentity, [16]byte{2}, remotePublic)
	if err != nil {
		t.Fatal(err)
	}
	return client, remoteIdentity, remoteHello
}

func establishHelloSession(t *testing.T, client *Client, remoteIdentity *identity.LocalIdentity, remoteHello []byte, address netip.AddrPort) *PeeringSession {
	t.Helper()
	client.handleHello(endpoint.Datagram{Data: remoteHello, From: address})
	peerRemote := client.remotePeers[remoteIdentity.PeerID()]
	if peerRemote == nil || peerRemote.candidateSess == nil {
		t.Fatal("Hello did not create a candidate session")
	}
	peerSess := peerRemote.candidateSess
	pong, err := protocol.MarshalControl(protocol.PacketPong, client.roomTag, peerSess.sessionID, 1, peerSess.pendingPing.challenge, peerSess.ciphers.ControlRecv)
	if err != nil {
		t.Fatal(err)
	}
	client.handleSessionPacket(endpoint.Datagram{Data: pong, From: address})
	if peerRemote.peerSess != peerSess || !peerSess.authenticated {
		t.Fatal("candidate session was not authenticated")
	}
	return peerSess
}

func testRemoteIdentity(t *testing.T) identity.Identity {
	t.Helper()
	publicKey, _, err := ed25519.GenerateKey(nil)
	if err != nil {
		t.Fatal(err)
	}
	peerIdentity, err := identity.FromPublicKey(publicKey)
	if err != nil {
		t.Fatal(err)
	}
	return peerIdentity
}
