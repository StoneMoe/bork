package peer

import (
	"log/slog"
	"net/netip"
	"testing"
	"time"

	"bork/internal/identity"
	"bork/internal/invite"
	"bork/internal/networking"
	"bork/internal/networking/endpoint"
	"bork/internal/protocol"
)

// The pair uses real UDP writes and production packet handlers. Only periodic
// ticks are driven by the test, so a reply loop cannot hide behind a timer.
type helloTestPair struct {
	t       *testing.T
	clients [2]*Client
	paths   [2]Path
	hellos  int
}

func newHelloTestPair(t *testing.T) *helloTestPair {
	t.Helper()
	room, err := invite.New("hello regression")
	if err != nil {
		t.Fatal(err)
	}
	pair := &helloTestPair{t: t}
	logger := slog.New(slog.DiscardHandler)
	options := networking.Options{Endpoint: endpoint.Options{ListenAddress: "127.0.0.1:0"}}
	for index := range pair.clients {
		pair.clients[index], err = NewClient(room, options, logger)
		if err != nil {
			t.Fatal(err)
		}
	}
	if comparePeerIDs(pair.clients[0].localPeerID, pair.clients[1].localPeerID) > 0 {
		pair.clients[0], pair.clients[1] = pair.clients[1], pair.clients[0]
	}
	ctx := t.Context()
	done := make(chan error, 2)
	t.Cleanup(func() { pair.stop(done) })
	for _, client := range pair.clients {
		client.roomNetwork = networking.NewRoomNetwork(room.RoomTag(), room.TrackerHash(), client.localPeerID, options, logger)
		go func() { done <- client.roomNetwork.Run(ctx) }()
	}
	pair.paths[0] = Path{address: helloTestAddress(t, pair.clients[1].roomNetwork)}
	pair.paths[1] = Path{address: helloTestAddress(t, pair.clients[0].roomNetwork)}
	first, second := pair.clients[0], pair.clients[1]
	first.startInitiatingSession(&RemotePeer{peerID: second.localPeerID}, pair.paths[0], time.Now())
	return pair
}

func (pair *helloTestPair) stop(done <-chan error) {
	for range pair.clients {
		if err := <-done; err != nil {
			pair.t.Error(err)
		}
	}
}

func helloTestAddress(t *testing.T, network *networking.RoomNetwork) netip.AddrPort {
	t.Helper()
	timeout := time.NewTimer(time.Second)
	defer timeout.Stop()
	for {
		address, err := netip.ParseAddrPort(network.Snapshot().Endpoint.ListenAddress)
		if err == nil {
			return address
		}
		select {
		case <-network.StateChanges():
		case <-timeout.C:
			t.Fatal("UDP endpoint did not start")
		}
	}
}

func (pair *helloTestPair) session(index int) *Session {
	remote := pair.clients[index].remotePeers[pair.clients[1-index].localPeerID]
	if remote.pendingSession != nil {
		return remote.pendingSession
	}
	return remote.activeSession
}

func (pair *helloTestPair) receive(index int, packet endpoint.Datagram, drop func(int, protocol.PacketType) bool) {
	kind, err := protocol.ParsePrefix(packet.Data)
	if err != nil {
		pair.t.Fatal(err)
	}
	if kind == protocol.PacketSessionHello {
		pair.hellos++
	}
	if drop != nil && drop(index, kind) {
		return
	}
	pair.clients[index].handlePacket(packet, nil)
}

func (pair *helloTestPair) drain(drop func(int, protocol.PacketType) bool) {
	pair.t.Helper()
	quiet := time.NewTimer(100 * time.Millisecond)
	deadline := time.NewTimer(time.Second)
	defer quiet.Stop()
	defer deadline.Stop()
	for {
		select {
		case packet := <-pair.clients[0].roomNetwork.ControlPackets():
			pair.receive(0, packet, drop)
			quiet.Reset(100 * time.Millisecond)
		case packet := <-pair.clients[1].roomNetwork.ControlPackets():
			pair.receive(1, packet, drop)
			quiet.Reset(100 * time.Millisecond)
		case <-quiet.C:
			return
		case <-deadline.C:
			pair.t.Fatalf("Hello exchange did not become quiet: received %d Hellos", pair.hellos)
		}
	}
}

func (pair *helloTestPair) tick() {
	for _, client := range pair.clients {
		// Hello retries are tick-driven. Only Ping retains a per-path clock;
		// advance those clocks to represent the time between real ticks.
		for _, remote := range client.remotePeers {
			for _, session := range []*Session{remote.activeSession, remote.pendingSession} {
				if session == nil {
					continue
				}
				session.pendingPing.sentAt = time.Now().Add(-pingInterval)
				if session.candidatePath != nil {
					session.candidatePath.pendingPing.sentAt = time.Now().Add(-pingInterval)
				}
			}
		}
		client.retrySessionHellos()
		client.sendPings()
	}
}

func (pair *helloTestPair) requireAuthenticated() {
	pair.t.Helper()
	for index, client := range pair.clients {
		remote := client.remotePeers[pair.clients[1-index].localPeerID]
		if remote.activeSession == nil || !remote.activeSession.authenticated || remote.pendingSession != nil {
			pair.t.Fatalf("peer %d did not finish authentication", index)
		}
	}
}

func (pair *helloTestPair) requireSameSessionsOnDirectPaths(sessions [2]*Session, ids [2][16]byte) {
	pair.t.Helper()
	for index, session := range sessions {
		if pair.session(index) != session || session.id() != ids[index] || session.path != pair.paths[index] {
			pair.t.Fatalf("peer %d did not preserve its Session on the new direct path", index)
		}
	}
}

func dropHandshakePacketOnce(dropped *bool, receiver, wantedReceiver int, kind, wanted protocol.PacketType) bool {
	if *dropped || receiver != wantedReceiver || kind != wanted {
		return false
	}
	*dropped = true
	return true
}

func TestSessionHelloHandshakeStopsAfterRetry(t *testing.T) {
	for _, test := range []struct {
		name     string
		receiver int
		drop     protocol.PacketType
	}{{"normal", 0, 0}, {"lost initiator Hello", 1, protocol.PacketSessionHello}, {"lost responder Hello", 0, protocol.PacketSessionHello}, {"lost Pong", 0, protocol.PacketPong}} {
		t.Run(test.name, func(t *testing.T) {
			pair := newHelloTestPair(t)
			dropped := false
			pair.drain(func(receiver int, kind protocol.PacketType) bool {
				return dropHandshakePacketOnce(&dropped, receiver, test.receiver, kind, test.drop)
			})
			if dropped {
				pair.tick()
				pair.drain(nil)
			}
			pair.requireAuthenticated()
			if dropped != (test.drop != 0) || pair.hellos > 8 {
				t.Fatalf("unexpected handshake: dropped=%v, Hellos=%d", dropped, pair.hellos)
			}
		})
	}
}

func newHelloTestPairMissingResponderHello(t *testing.T) *helloTestPair {
	t.Helper()
	pair := newHelloTestPair(t)
	dropped := false
	pair.drain(func(receiver int, kind protocol.PacketType) bool {
		return dropHandshakePacketOnce(&dropped, receiver, 0, kind, protocol.PacketSessionHello)
	})
	return pair
}

func TestSessionHelloTickRestoresIncompleteHandshakeOnNewPath(t *testing.T) {
	pair := newHelloTestPairMissingResponderHello(t)
	sessions := [2]*Session{pair.session(0), pair.session(1)}
	for index, session := range sessions {
		// The initiator still lacks the responder Hello when the original
		// bridges disappear. Both real endpoints remain reachable directly.
		session.path = Path{address: pair.paths[index].Address(), intermediary: identity.PeerID{255}, target: pair.clients[1-index].localPeerID}
	}
	second := pair.clients[1]
	if err := second.initHelloProbe(); err != nil {
		t.Fatal(err)
	}
	before := pair.hellos
	second.sendHelloProbeOnPath(pair.paths[1])
	pair.drain(nil)
	if sessions[0].sessionReady() || pair.hellos != before+1 {
		t.Fatal("matching candidate Hello generated another Hello")
	}
	sessions = [2]*Session{pair.session(0), pair.session(1)}
	ids := [2][16]byte{sessions[0].id(), sessions[1].id()}
	pair.tick()
	pair.drain(nil)
	pair.requireAuthenticated()
	pair.requireSameSessionsOnDirectPaths(sessions, ids)
}

func (pair *helloTestPair) sendMatchingHellos() {
	for range 3 {
		pair.clients[0].sendSessionHelloOnPath(pair.session(0), pair.paths[0])
	}
	pair.drain(nil)
}

func TestSessionHelloOnlyTickRetriesPendingSessions(t *testing.T) {
	pair := newHelloTestPairMissingResponderHello(t)
	before := pair.hellos
	pair.sendMatchingHellos()
	if pair.hellos != before+3 {
		t.Fatal("pending Session replied to matching Hellos")
	}
	before = pair.hellos
	pair.tick()
	pair.drain(nil)
	pair.requireAuthenticated()
	if pair.hellos != before+2 {
		t.Fatal("tick did not send one Hello for each pending Session")
	}
	before = pair.hellos
	pair.sendMatchingHellos()
	if pair.hellos != before+3 {
		t.Fatal("active Session replied to matching Hellos")
	}
	before = pair.hellos
	pair.tick()
	pair.drain(nil)
	if pair.hellos != before {
		t.Fatal("tick retried an active Session's Hello")
	}
}

func TestSessionHelloOldHelloRegistersNewPathBeforeReply(t *testing.T) {
	pair := newHelloTestPair(t)
	pair.drain(nil)
	pair.requireAuthenticated()
	second := pair.clients[1]
	sessions := [2]*Session{pair.session(0), pair.session(1)}
	ids := [2][16]byte{sessions[0].id(), sessions[1].id()}
	for index, session := range sessions {
		session.path = Path{address: pair.paths[index].Address(), intermediary: identity.PeerID{255}, target: pair.clients[1-index].localPeerID}
	}
	oldID := ids[1]
	oldID[0] ^= 1
	old, err := second.newSessionWithLocalHello(pair.paths[1], oldID, time.Now())
	if err != nil {
		t.Fatal(err)
	}
	before := pair.hellos
	// The initiator corrects this old Hello on a previously unknown path.
	// Registering that outgoing path lets it accept the next Ping/Pong there.
	second.sendSessionHelloOnPath(old, pair.paths[1])
	pair.drain(nil)
	pair.tick()
	pair.drain(nil)
	if pair.hellos != before+2 {
		t.Fatal("old Hello correction did not stop after advertising the current Hello")
	}
	pair.requireAuthenticated()
	pair.requireSameSessionsOnDirectPaths(sessions, ids)
}
