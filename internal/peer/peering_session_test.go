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
	"bork/internal/networking"
	"bork/internal/networking/discovery"
	"bork/internal/networking/endpoint"
	"bork/internal/protocol"

	"golang.org/x/crypto/chacha20poly1305"
)

func TestMismatchedChallengeCannotEstablishSession(t *testing.T) {
	remoteIdentity := testRemoteIdentity(t)
	path, _ := NewPath(netip.MustParseAddrPort("127.0.0.1:9000"))
	peerSess := testPeeringSession(t, path)
	peerSess.pendingPing = pendingPing{challenge: 99, path: path, sentAt: time.Now()}
	remotePeer := &RemotePeer{identity: remoteIdentity, candidateSession: peerSess}
	client := &Client{
		logger:      slog.Default(),
		roomTag:     [16]byte{1},
		remotePeers: map[string]*RemotePeer{remoteIdentity.PeerID(): remotePeer},
	}
	packet, err := protocol.MarshalControl(protocol.PacketPong, client.roomTag, peerSess.sessionID, 1, 98, peerSess.ciphers.ControlRecv)
	if err != nil {
		t.Fatal(err)
	}
	client.handleSessionPacket(endpoint.Datagram{Data: packet, From: path.Address()})
	if peerSess.authenticated || remotePeer.session != nil {
		t.Fatal("mismatched challenge established a session")
	}
}

func TestHelloReusesThenReplacesCandidateSession(t *testing.T) {
	client, remoteIdentity, remoteHello := testHelloClient(t)
	address := netip.MustParseAddrPort("127.0.0.1:9001")
	client.handleHello(endpoint.Datagram{Data: remoteHello, From: address})
	remote := client.remotePeers[remoteIdentity.PeerID()]
	first := remote.candidateSession
	challenge := first.pendingPing.challenge

	client.handleHello(endpoint.Datagram{Data: remoteHello, From: address})
	if remote.candidateSession != first || first.pendingPing.challenge != challenge || challenge == 0 {
		t.Fatal("duplicate Hello did not reuse the pending candidate session")
	}

	replacementHello := testRemoteHello(t, client, remoteIdentity, 3)
	client.handleHello(endpoint.Datagram{Data: replacementHello, From: address})
	if remote.candidateSession == nil || remote.candidateSession == first || remote.candidateSession.sessionID == first.sessionID {
		t.Fatal("new handshake transcript did not replace the candidate session")
	}
}

func TestCandidateSessionAuthenticatesOnAlternatePath(t *testing.T) {
	client, remoteIdentity, remoteHello := testHelloClient(t)
	firstPath, _ := NewPath(netip.MustParseAddrPort("127.0.0.1:9010"))
	secondPath, _ := NewPath(netip.MustParseAddrPort("127.0.0.1:9011"))
	client.handleHello(endpoint.Datagram{Data: remoteHello, From: firstPath.Address()})
	remote := client.remotePeers[remoteIdentity.PeerID()]
	peerSess := remote.candidateSession
	client.handleHello(endpoint.Datagram{Data: remoteHello, From: secondPath.Address()})
	answerCandidatePong(t, client, peerSess, secondPath, 1, 20*time.Millisecond)

	if remote.session != peerSess || remote.candidateSession != nil || !peerSess.authenticated || peerSess.path != secondPath || peerSess.candidatePath != nil {
		t.Fatal("alternate path did not authenticate and promote the candidate session")
	}
}

func TestStalePathDeauthenticatesAndRetries(t *testing.T) {
	client, remoteIdentity, remoteHello := testHelloClient(t)
	address := netip.MustParseAddrPort("127.0.0.1:9020")
	peerSess := establishHelloSession(t, client, remoteIdentity, remoteHello, address)
	candidatePath, _ := NewPath(netip.MustParseAddrPort("127.0.0.1:9021"))
	client.rememberCandidatePath(peerSess, candidatePath, time.Now())
	peerSess.pendingPing = pendingPing{challenge: 100, path: peerSess.path, sentAt: time.Now()}
	peerSess.lastAuthenticatedPacketAt = time.Now().Add(-pathFailoverTimeout - time.Second)
	generation := client.topologyGeneration

	client.expireRemotePeers()
	remote := client.remotePeers[remoteIdentity.PeerID()]
	if remote == nil || remote.session != peerSess || peerSess.authenticated || peerSess.pendingPing.challenge != 0 || peerSess.candidatePath != nil {
		t.Fatal("stale active path was removed or retained authenticated probe state")
	}
	if client.topologyGeneration == generation || !client.fanoutDirty || client.addressHasActivePath(address) {
		t.Fatal("stale path did not invalidate topology and discovery state")
	}

	remembered := client.discoveredAddresses[address]
	remembered.nextProbe = time.Time{}
	client.discoveredAddresses[address] = remembered
	network := client.roomNetwork.(*fakeRoomNetwork)
	before := len(network.sentAddresses())
	client.sendHellos(time.Now())
	if len(network.sentAddresses()) != before+1 {
		t.Fatal("stale direct path did not resume Hello probing")
	}
	client.handleHello(endpoint.Datagram{Data: remoteHello, From: address})
	if peerSess.pendingPing.challenge == 0 || !peerSess.pendingPing.path.SameRoute(peerSess.path) {
		t.Fatal("matching Hello did not retry authentication on the retained session")
	}
}

func TestStaleRoomSessionRetainsControlState(t *testing.T) {
	remoteIdentity := testRemoteIdentity(t)
	path, _ := NewPath(netip.MustParseAddrPort("127.0.0.1:9030"))
	peerSess := testPeeringSession(t, path)
	peerSess.authenticated = true
	peerSess.everAuthenticated = true
	peerSess.lastAuthenticatedPacketAt = time.Now().Add(-remotePeerTimeout - time.Second)
	for range 12 {
		if _, err := peerSess.control.nextSendSequence(); err != nil {
			t.Fatal(err)
		}
	}
	remotePeer := &RemotePeer{identity: remoteIdentity, session: peerSess}
	client := &Client{logger: slog.Default(), remotePeers: map[string]*RemotePeer{remoteIdentity.PeerID(): remotePeer}}
	client.expireRemotePeers()
	if client.remotePeers[remoteIdentity.PeerID()] != remotePeer || remotePeer.session != peerSess || peerSess.authenticated {
		t.Fatal("stale room session was removed or remained authenticated")
	}
	if next, err := peerSess.control.nextSendSequence(); err != nil || next != 13 {
		t.Fatalf("control flow was reset: next sequence = %d, %v", next, err)
	}
}

func TestDirectPathMigrationKeepsSessionAndDiscardsOldPath(t *testing.T) {
	client, remoteIdentity, remoteHello := testHelloClient(t)
	primaryPath, _ := NewPath(netip.MustParseAddrPort("127.0.0.1:9040"))
	candidatePath, _ := NewPath(netip.MustParseAddrPort("127.0.0.1:9041"))
	peerSess := establishHelloSession(t, client, remoteIdentity, remoteHello, primaryPath.Address())
	sessionID := peerSess.sessionID
	sequenceBefore, err := peerSess.control.nextSendSequence()
	if err != nil {
		t.Fatal(err)
	}

	client.handleHello(endpoint.Datagram{Data: remoteHello, From: candidatePath.Address()})
	peerSess.lastAuthenticatedPacketAt = time.Now().Add(-pathFailoverTimeout - time.Second)
	answerCandidatePong(t, client, peerSess, candidatePath, 2, 20*time.Millisecond)
	if peerSess.sessionID != sessionID || !peerSess.authenticated || peerSess.path != candidatePath || peerSess.candidatePath != nil {
		t.Fatal("direct migration replaced the session or retained the previous path")
	}
	if sequenceAfter, err := peerSess.control.nextSendSequence(); err != nil || sequenceAfter <= sequenceBefore {
		t.Fatalf("direct migration reset control flow: before=%d after=%d err=%v", sequenceBefore, sequenceAfter, err)
	}
}

func TestBridgePathPromotesToDirect(t *testing.T) {
	client, _, _ := testHelloClient(t)
	remoteIdentity := testRemoteIdentity(t)
	bridgePath, _ := NewBridgePath(netip.MustParseAddrPort("127.0.0.1:9050"), [32]byte{1}, rawPeerIdentity(remoteIdentity))
	directPath, _ := NewPath(netip.MustParseAddrPort("127.0.0.1:9051"))
	peerSess := testPeeringSession(t, bridgePath)
	peerSess.authenticated = true
	peerSess.everAuthenticated = true
	peerSess.lastAuthenticatedPacketAt = time.Now()
	peerSess.rttMillis = 10
	client.remotePeers[remoteIdentity.PeerID()] = &RemotePeer{identity: remoteIdentity, session: peerSess}
	client.rememberCandidatePath(peerSess, directPath, time.Now())
	answerCandidatePong(t, client, peerSess, directPath, 1, 80*time.Millisecond)
	if !peerSess.authenticated || peerSess.path != directPath || peerSess.candidatePath != nil {
		t.Fatal("direct candidate did not replace a healthy bridge path")
	}
}

func TestHealthyDirectPathRejectsBridgeCandidate(t *testing.T) {
	client, _, _ := testHelloClient(t)
	remoteIdentity := testRemoteIdentity(t)
	directPath, _ := NewPath(netip.MustParseAddrPort("127.0.0.1:9060"))
	bridgePath, _ := NewBridgePath(netip.MustParseAddrPort("127.0.0.1:9061"), [32]byte{1}, rawPeerIdentity(remoteIdentity))
	peerSess := testPeeringSession(t, directPath)
	peerSess.authenticated = true
	peerSess.everAuthenticated = true
	peerSess.lastAuthenticatedPacketAt = time.Now()
	peerSess.rttMillis = 100
	client.remotePeers[remoteIdentity.PeerID()] = &RemotePeer{identity: remoteIdentity, session: peerSess}
	client.rememberCandidatePath(peerSess, bridgePath, time.Now())
	answerCandidatePong(t, client, peerSess, bridgePath, 1, time.Millisecond)
	if !peerSess.authenticated || peerSess.path != directPath || peerSess.candidatePath != nil {
		t.Fatal("bridge candidate displaced a healthy direct path or left a completed probe")
	}
}

func TestCandidatePathReplacementAndExpiry(t *testing.T) {
	client := &Client{}
	activePath, _ := NewPath(netip.MustParseAddrPort("127.0.0.1:9070"))
	peerSess := testPeeringSession(t, activePath)
	firstBridge, _ := NewBridgePath(netip.MustParseAddrPort("127.0.0.1:9071"), [32]byte{1}, [32]byte{9})
	updatedBridge, _ := NewBridgePath(netip.MustParseAddrPort("127.0.0.1:9072"), [32]byte{1}, [32]byte{9})
	newBridge, _ := NewBridgePath(netip.MustParseAddrPort("127.0.0.1:9073"), [32]byte{2}, [32]byte{9})
	directPath, _ := NewPath(netip.MustParseAddrPort("127.0.0.1:9074"))
	otherDirect, _ := NewPath(netip.MustParseAddrPort("127.0.0.1:9075"))

	startedAt := time.Now().Add(-time.Second)
	if !client.rememberCandidatePath(peerSess, firstBridge, startedAt) {
		t.Fatal("first bridge probe was rejected")
	}
	if client.rememberCandidatePath(peerSess, updatedBridge, time.Now()) || peerSess.candidatePath.path != updatedBridge || !peerSess.candidatePath.startedAt.Equal(startedAt) {
		t.Fatal("same bridge route did not refresh only its mutable next hop")
	}
	if !client.rememberCandidatePath(peerSess, newBridge, time.Now()) || peerSess.candidatePath.path != newBridge {
		t.Fatal("newest bridge probe did not replace the previous bridge")
	}
	if !client.rememberCandidatePath(peerSess, directPath, time.Now()) || peerSess.candidatePath.path != directPath {
		t.Fatal("direct probe did not replace a bridge probe")
	}
	if client.rememberCandidatePath(peerSess, newBridge, time.Now()) || peerSess.candidatePath.path != directPath {
		t.Fatal("bridge probe replaced a direct probe")
	}
	if !client.rememberCandidatePath(peerSess, otherDirect, time.Now()) || peerSess.candidatePath.path != otherDirect {
		t.Fatal("newest direct probe did not replace the previous direct probe")
	}

	peerSess.authenticated = true
	peerSess.everAuthenticated = true
	peerSess.lastAuthenticatedPacketAt = time.Now()
	peerSess.candidatePath.startedAt = time.Now().Add(-remotePeerTimeout - time.Second)
	remoteIdentity := testRemoteIdentity(t)
	client.logger = slog.Default()
	client.remotePeers = map[string]*RemotePeer{remoteIdentity.PeerID(): {identity: remoteIdentity, session: peerSess}}
	client.expireRemotePeers()
	if peerSess.candidatePath != nil {
		t.Fatal("expired candidate probe was retained")
	}
}

func TestAuthenticatedPathIsRetainedAsRoomLifetimeHint(t *testing.T) {
	client, remoteIdentity, remoteHello := testHelloClient(t)
	address := netip.MustParseAddrPort("127.0.0.1:9080")
	peerSess := establishHelloSession(t, client, remoteIdentity, remoteHello, address)
	remembered, exists := client.discoveredAddresses[address]
	if !exists || remembered.source != discovery.SourceAuthenticated || !remembered.expiresAt.IsZero() {
		t.Fatalf("authenticated discovery record = %#v, exists=%v", remembered, exists)
	}
	peerSess.lastAuthenticatedPacketAt = time.Now().Add(-remotePeerTimeout - time.Second)
	client.expireRemotePeers()
	if _, exists := client.discoveredAddresses[address]; !exists {
		t.Fatal("authenticated path was removed when its session became inactive")
	}
}

func TestBridgeBudgetIsBoundedAndRefills(t *testing.T) {
	var budget tokenBudget
	now := time.Unix(1, 0)
	for range 4 {
		if !budget.allowCost(now, 1, 2, 4) {
			t.Fatal("initial bridge burst was rejected")
		}
	}
	if budget.allowCost(now, 1, 2, 4) {
		t.Fatal("bridge burst limit was not enforced")
	}
	if !budget.allowCost(now.Add(time.Second), 1, 2, 4) || !budget.allowCost(now.Add(time.Second), 1, 2, 4) || budget.allowCost(now.Add(time.Second), 1, 2, 4) {
		t.Fatal("bridge budget did not refill at the configured rate")
	}
}

func TestDiscoveryHintBudgetEvictsOldestUnusedAddress(t *testing.T) {
	activeAddress := discoveryBudgetAddress(0)
	activePath, _ := NewPath(activeAddress)
	activeIdentity := testRemoteIdentity(t)
	client := &Client{
		remotePeers: map[string]*RemotePeer{
			activeIdentity.PeerID(): {identity: activeIdentity, session: testPeeringSession(t, activePath)},
		},
		discoveredAddresses: make(map[netip.AddrPort]discoveredAddress),
	}
	base := time.Unix(1000, 0)
	for index := range maxDiscoveryHints {
		client.rememberDiscoveryHint(discovery.Hint{Address: discoveryBudgetAddress(index), Source: discovery.SourceMDNS}, base.Add(time.Duration(index)*time.Second))
	}
	oldestUnused := discoveryBudgetAddress(1)
	newAddress := netip.MustParseAddrPort("198.51.100.1:9000")
	client.rememberDiscoveryHint(discovery.Hint{Address: newAddress, Source: discovery.SourceMDNS}, base.Add(time.Hour))
	if len(client.discoveredAddresses) != maxDiscoveryHints {
		t.Fatalf("discovery hint count = %d, want %d", len(client.discoveredAddresses), maxDiscoveryHints)
	}
	if _, exists := client.discoveredAddresses[activeAddress]; !exists {
		t.Fatal("eviction removed an address used by a session")
	}
	if _, exists := client.discoveredAddresses[oldestUnused]; exists {
		t.Fatal("eviction retained the oldest unused address")
	}
	if _, exists := client.discoveredAddresses[newAddress]; !exists {
		t.Fatal("eviction did not retain the new address")
	}
}

func TestDiscoveryHintBudgetPrioritizesExpiredRecord(t *testing.T) {
	client := &Client{remotePeers: make(map[string]*RemotePeer), discoveredAddresses: make(map[netip.AddrPort]discoveredAddress)}
	base := time.Unix(2000, 0)
	for index := range maxDiscoveryHints {
		client.rememberDiscoveryHint(discovery.Hint{Address: discoveryBudgetAddress(index), Source: discovery.SourceMDNS}, base.Add(time.Duration(index)*time.Second))
	}
	expiredAddress := discoveryBudgetAddress(maxDiscoveryHints - 1)
	expired := client.discoveredAddresses[expiredAddress]
	expired.source = discovery.SourceTracker
	expired.expiresAt = base.Add(30 * time.Minute)
	client.discoveredAddresses[expiredAddress] = expired
	oldest := discoveryBudgetAddress(0)
	newAddress := netip.MustParseAddrPort("198.51.100.2:9000")
	client.rememberDiscoveryHint(discovery.Hint{Address: newAddress, Source: discovery.SourceMDNS}, base.Add(time.Hour))
	if _, exists := client.discoveredAddresses[expiredAddress]; exists {
		t.Fatal("eviction retained an expired tracker record")
	}
	if _, exists := client.discoveredAddresses[oldest]; !exists {
		t.Fatal("eviction chose the oldest record before an expired record")
	}
	if len(client.discoveredAddresses) != maxDiscoveryHints {
		t.Fatalf("discovery hint count = %d, want %d", len(client.discoveredAddresses), maxDiscoveryHints)
	}
}

func answerCandidatePong(t *testing.T, client *Client, peerSess *PeeringSession, path Path, sequence uint64, rtt time.Duration) {
	t.Helper()
	probe := peerSess.candidateProbe(path)
	if probe == nil {
		t.Fatal("candidate probe was not created")
	}
	probe.pendingPing = pendingPing{challenge: sequence + 100, path: path, sentAt: time.Now().Add(-rtt)}
	packet, err := protocol.MarshalControl(protocol.PacketPong, client.roomTag, peerSess.sessionID, sequence, probe.pendingPing.challenge, peerSess.ciphers.ControlRecv)
	if err != nil {
		t.Fatal(err)
	}
	client.handleSessionPacketOnPath(packet, path)
}

func testPeeringSession(t *testing.T, path Path) *PeeringSession {
	t.Helper()
	sendKey := [chacha20poly1305.KeySize]byte{1}
	sendCipher, err := chacha20poly1305.New(sendKey[:])
	if err != nil {
		t.Fatal(err)
	}
	receiveKey := [chacha20poly1305.KeySize]byte{2}
	receiveCipher, err := chacha20poly1305.New(receiveKey[:])
	if err != nil {
		t.Fatal(err)
	}
	material := protocol.SessionMaterial{
		SessionID: [16]byte{2},
		Ciphers: protocol.SessionCiphers{
			ControlSend: sendCipher,
			ControlRecv: receiveCipher,
		},
	}
	return newPeeringSession(path, material, time.Now())
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
	client := NewClient(localIdentity, roomInvite, networking.Options{Endpoint: endpoint.Options{}}, slog.Default())
	client.roomNetwork = &fakeRoomNetwork{}
	if err := client.rotateHelloEpoch(); err != nil {
		t.Fatal(err)
	}
	return client, remoteIdentity, testRemoteHello(t, client, remoteIdentity, 2)
}

func testRemoteHello(t *testing.T, client *Client, remoteIdentity *identity.LocalIdentity, nonceByte byte) []byte {
	t.Helper()
	remotePrivate, err := ecdh.X25519().GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	var remotePublic [32]byte
	copy(remotePublic[:], remotePrivate.PublicKey().Bytes())
	var nonce [16]byte
	nonce[0] = nonceByte
	remoteHello, err := protocol.MarshalHello(client.roomTag, client.admissionKey, remoteIdentity, nonce, remotePublic)
	if err != nil {
		t.Fatal(err)
	}
	return remoteHello
}

func establishHelloSession(t *testing.T, client *Client, remoteIdentity *identity.LocalIdentity, remoteHello []byte, address netip.AddrPort) *PeeringSession {
	t.Helper()
	client.handleHello(endpoint.Datagram{Data: remoteHello, From: address})
	remotePeer := client.remotePeers[remoteIdentity.PeerID()]
	if remotePeer == nil || remotePeer.candidateSession == nil {
		t.Fatal("Hello did not create a candidate session")
	}
	peerSess := remotePeer.candidateSession
	pong, err := protocol.MarshalControl(protocol.PacketPong, client.roomTag, peerSess.sessionID, 1, peerSess.pendingPing.challenge, peerSess.ciphers.ControlRecv)
	if err != nil {
		t.Fatal(err)
	}
	client.handleSessionPacket(endpoint.Datagram{Data: pong, From: address})
	if remotePeer.session != peerSess || !peerSess.authenticated {
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

func discoveryBudgetAddress(index int) netip.AddrPort {
	return netip.AddrPortFrom(netip.AddrFrom4([4]byte{10, byte(index >> 8), byte(index), 1}), 9000)
}
