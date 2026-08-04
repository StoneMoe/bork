package peer

import (
	"fmt"
	"io"
	"log/slog"
	"math"
	"net/netip"
	"reflect"
	"testing"
	"time"

	"bork/internal/identity"
	"bork/internal/invite"
	"bork/internal/media"
	"bork/internal/networking/endpoint"
	"bork/internal/protocol"
)

func TestFanoutPlanUsesOneIntermediaryAndIsDeterministic(t *testing.T) {
	now := time.Unix(100, 0)
	listeners := []string{"leaf-c", "forwarder-b", "leaf-a", "forwarder-a"}
	direct := map[string]struct{}{
		"forwarder-a": {},
		"forwarder-b": {},
	}
	topology := map[string]*topologyPeer{
		"forwarder-a": {neighbors: map[string]time.Time{"leaf-a": now.Add(time.Minute)}},
		"forwarder-b": {neighbors: map[string]time.Time{"leaf-c": now.Add(time.Minute)}},
	}
	wantAssignments := map[string][]string{
		"forwarder-a": {"leaf-a"},
		"forwarder-b": {"leaf-c"},
	}
	first := buildFanoutPlan(append([]string(nil), listeners...), direct, topology, now)
	second := buildFanoutPlan(append([]string(nil), listeners...), direct, topology, now)
	if !reflect.DeepEqual(first.destinations, []string{"forwarder-a", "forwarder-b"}) {
		t.Fatalf("destinations = %v", first.destinations)
	}
	if !reflect.DeepEqual(first.assignments, wantAssignments) {
		t.Fatalf("assignments = %#v", first.assignments)
	}
	if !reflect.DeepEqual(first.destinations, second.destinations) || !reflect.DeepEqual(first.assignments, second.assignments) {
		t.Fatal("fanout planning is not deterministic")
	}
}

func TestFanoutPlanDoesNotUseExpiredOrMultiHopClaims(t *testing.T) {
	now := time.Unix(200, 0)
	direct := map[string]struct{}{"forwarder": {}}
	topology := map[string]*topologyPeer{
		"forwarder": {neighbors: map[string]time.Time{
			"direct-leaf":  now.Add(time.Minute),
			"expired-leaf": now.Add(-time.Second),
		}},
		"direct-leaf": {neighbors: map[string]time.Time{"third-hop": now.Add(time.Minute)}},
	}
	plan := buildFanoutPlan([]string{"forwarder", "direct-leaf", "expired-leaf", "third-hop"}, direct, topology, now)
	if !reflect.DeepEqual(plan.assignments["forwarder"], []string{"direct-leaf"}) {
		t.Fatalf("assignment crossed one intermediary: %#v", plan.assignments)
	}
}

func TestFanoutAssignmentRoundTrip(t *testing.T) {
	identities := []*identity.LocalIdentity{
		testLocalIdentity(t), testLocalIdentity(t),
	}
	peers := map[string]*RemotePeer{}
	listeners := make([]string, 0, len(identities))
	for _, local := range identities {
		peerID := local.PeerID()
		listeners = append(listeners, peerID)
		peers[peerID] = &RemotePeer{identity: local.Identity}
	}
	payload, err := marshalFanoutAssignment(9, listeners, peers)
	if err != nil {
		t.Fatal(err)
	}
	generation, decoded, err := parseFanoutAssignment(payload)
	if err != nil || generation != 9 || len(decoded) != len(listeners) {
		t.Fatalf("decoded generation=%d listeners=%d error=%v", generation, len(decoded), err)
	}
}

func TestGroupDatagramFanoutForwardsIdenticalCiphertext(t *testing.T) {
	room, err := invite.New("fanout integration")
	if err != nil {
		t.Fatal(err)
	}
	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	speakerIdentity := testLocalIdentity(t)
	forwarderIdentity := testLocalIdentity(t)
	listenerIdentity := testLocalIdentity(t)
	speakerNetwork := newFakeRoomNetwork()
	forwarderNetwork := newFakeRoomNetwork()
	listenerNetwork := newFakeRoomNetwork()
	newTestClient := func(local *identity.LocalIdentity, network *fakeRoomNetwork) *Client {
		client := newClient(local, room, func() roomNetwork { return network }, logger)
		client.roomNetwork = network
		return client
	}
	speaker := newTestClient(speakerIdentity, speakerNetwork)
	forwarder := newTestClient(forwarderIdentity, forwarderNetwork)
	listener := newTestClient(listenerIdentity, listenerNetwork)
	speaker.initGroupStream()

	speakerAddress := netip.MustParseAddrPort("192.0.2.1:9000")
	forwarderAddress := netip.MustParseAddrPort("192.0.2.2:9000")
	listenerAddress := netip.MustParseAddrPort("192.0.2.3:9000")
	directSession := func(address netip.AddrPort) *PeeringSession {
		path, pathErr := NewPath(address)
		if pathErr != nil {
			t.Fatal(pathErr)
		}
		session := testPeeringSession(t, path)
		session.authenticated = true
		session.everAuthenticated = true
		return session
	}
	speakerToForwarder := directSession(forwarderAddress)
	bridgeToListener, err := NewBridgePath(forwarderAddress, rawPeerIdentity(forwarderIdentity.Identity), rawPeerIdentity(listenerIdentity.Identity))
	if err != nil {
		t.Fatal(err)
	}
	speakerToListener := testPeeringSession(t, bridgeToListener)
	speakerToListener.authenticated = true
	speakerToListener.everAuthenticated = true
	speakerToListener.audioStreamID = speaker.groupStreamID
	speaker.remotePeers[forwarderIdentity.PeerID()] = &RemotePeer{identity: forwarderIdentity.Identity, session: speakerToForwarder}
	speaker.remotePeers[listenerIdentity.PeerID()] = &RemotePeer{identity: listenerIdentity.Identity, session: speakerToListener}
	speaker.topology[forwarderIdentity.PeerID()] = &topologyPeer{
		identity:  forwarderIdentity.Identity,
		neighbors: map[string]time.Time{listenerIdentity.PeerID(): time.Now().Add(time.Minute)},
	}
	speaker.topology[listenerIdentity.PeerID()] = &topologyPeer{identity: listenerIdentity.Identity, neighbors: map[string]time.Time{}}

	forwarderSpeakerSession := directSession(speakerAddress)
	forwarderSpeakerSession.audioStreamID = speaker.groupStreamID
	forwarder.remotePeers[speakerIdentity.PeerID()] = &RemotePeer{identity: speakerIdentity.Identity, session: forwarderSpeakerSession}
	forwarder.remotePeers[listenerIdentity.PeerID()] = &RemotePeer{identity: listenerIdentity.Identity, session: directSession(listenerAddress)}
	listener.remotePeers[speakerIdentity.PeerID()] = &RemotePeer{identity: speakerIdentity.Identity, session: speakerToListener}
	listener.remotePeers[forwarderIdentity.PeerID()] = &RemotePeer{identity: forwarderIdentity.Identity, session: directSession(forwarderAddress)}

	now := time.Now()
	speaker.refreshFanout(now)
	assignmentPacket, ok := nextReliablePacket(speakerToForwarder.reliable, now)
	if !ok || assignmentPacket.Channel != reliableChannelFanout {
		t.Fatalf("fanout assignment was not queued: %#v", assignmentPacket)
	}
	forwarder.handleReliableMessage(forwarder.remotePeers[speakerIdentity.PeerID()], deliveredReliableMessage{
		channel: reliableChannelFanout, payload: assignmentPacket.Payload,
	})
	speakerToForwarder.reliable.receive(protocol.ReliablePacket{
		Channel: reliableChannelFanout, Flags: protocol.ReliableFlagAckOnly,
		AckBase: assignmentPacket.FragmentSequence, AckBitmap: 1,
	}, now)
	speaker.fanout.activateAt = now.Add(-time.Second)
	payload := []byte{1, 2, 3, 4}
	speaker.sendGroupMedia(media.SendFrame{Timestamp: 480, Payload: payload, Deadline: now.Add(time.Second), Generation: 1})
	if len(speakerNetwork.realtimeBatches) != 1 || len(speakerNetwork.realtimeBatches[0].Datagrams) != 1 {
		t.Fatalf("speaker batches = %#v", speakerNetwork.realtimeBatches)
	}
	original := speakerNetwork.realtimeBatches[0].Datagrams[0]
	if original.Destination != forwarderAddress {
		t.Fatalf("speaker destination = %v", original.Destination)
	}
	forwarder.handleGroupDatagram(endpoint.Datagram{Data: original.Data, From: speakerAddress, ReceivedAt: now}, nil)
	if len(forwarderNetwork.realtimeBatches) != 1 || len(forwarderNetwork.realtimeBatches[0].Datagrams) != 1 {
		t.Fatalf("forwarder batches = %#v", forwarderNetwork.realtimeBatches)
	}
	forwarded := forwarderNetwork.realtimeBatches[0].Datagrams[0]
	if forwarded.Destination != listenerAddress || string(forwarded.Data) != string(original.Data) {
		t.Fatal("forwarder changed ciphertext or selected the wrong listener")
	}
	flow := media.NewFlow()
	listener.handleGroupDatagram(endpoint.Datagram{Data: forwarded.Data, From: forwarderAddress, ReceivedAt: now}, flow)
	received, ok := flow.TakeReceived()
	if !ok || received.SourceID != speakerIdentity.PeerID() || string(received.Payload) != string(payload) {
		t.Fatalf("listener frame = %#v, ok=%v", received, ok)
	}
}

func TestForwardGroupDatagramSendsOneExternalRealtimeBatch(t *testing.T) {
	network := newFakeRoomNetwork()
	senderAddress := netip.MustParseAddrPort("192.0.2.1:9000")
	firstAddress := netip.MustParseAddrPort("192.0.2.2:9000")
	secondAddress := netip.MustParseAddrPort("192.0.2.3:9000")
	senderPath, err := NewPath(senderAddress)
	if err != nil {
		t.Fatal(err)
	}
	firstPath, err := NewPath(firstAddress)
	if err != nil {
		t.Fatal(err)
	}
	secondPath, err := NewPath(secondAddress)
	if err != nil {
		t.Fatal(err)
	}
	senderSession := &PeeringSession{
		path: senderPath, authenticated: true,
		inboundFanout: fanoutAssignment{generation: 1, listeners: []string{"first", "second"}},
	}
	client := &Client{
		roomNetwork: network,
		remotePeers: map[string]*RemotePeer{
			"sender": {session: senderSession},
			"first":  {session: &PeeringSession{path: firstPath, authenticated: true}},
			"second": {session: &PeeringSession{path: secondPath, authenticated: true}},
		},
	}
	deadline := time.Now().Add(time.Second)
	packet := endpoint.Datagram{Data: []byte{1, 2, 3}, From: senderAddress}
	client.forwardGroupDatagram("sender", protocol.TrafficInteractive, packet, deadline)

	if len(network.realtimeBatches) != 1 {
		t.Fatalf("realtime batch calls = %d, want 1", len(network.realtimeBatches))
	}
	batch := network.realtimeBatches[0]
	if batch.Class != protocol.TrafficInteractive || batch.Generation != 0 || batch.Deadline != deadline {
		t.Fatalf("realtime batch metadata = %#v", batch)
	}
	if len(batch.Datagrams) != 2 || batch.Datagrams[0].Destination != firstAddress || batch.Datagrams[1].Destination != secondAddress {
		t.Fatalf("realtime batch datagrams = %#v", batch.Datagrams)
	}
	client.remotePeers["sender"].session = &PeeringSession{path: senderPath, authenticated: true}
	client.forwardGroupDatagram("sender", protocol.TrafficInteractive, packet, deadline)
	if len(network.realtimeBatches) != 1 {
		t.Fatal("fanout assignment survived a session replacement")
	}
}

func TestSendRealtimeToPeersChunksOrderedDestinations(t *testing.T) {
	network := newFakeRoomNetwork()
	client := &Client{roomNetwork: network, remotePeers: make(map[string]*RemotePeer)}
	peerIDs := []string{"missing", "unauthenticated", "indirect"}
	wantDestinations := make([]netip.AddrPort, 0, endpoint.MaxRealtimeBatchDatagrams+3)
	client.remotePeers["unauthenticated"] = &RemotePeer{session: &PeeringSession{
		path: Path{address: largeFanoutTestAddress(endpoint.MaxRealtimeBatchDatagrams + 3)},
	}}
	client.remotePeers["indirect"] = &RemotePeer{session: &PeeringSession{
		path: Path{
			address:      largeFanoutTestAddress(endpoint.MaxRealtimeBatchDatagrams + 4),
			intermediary: [32]byte{1}, target: [32]byte{2},
		},
		authenticated: true,
	}}
	for index := range endpoint.MaxRealtimeBatchDatagrams + 3 {
		peerID := fmt.Sprintf("listener-%d", index)
		address := largeFanoutTestAddress(index)
		peerIDs = append(peerIDs, peerID)
		client.remotePeers[peerID] = &RemotePeer{session: &PeeringSession{
			path: Path{address: address}, authenticated: true,
		}}
		wantDestinations = append(wantDestinations, address)
	}

	packet := []byte{1, 2, 3, 4}
	deadline := time.Now().Add(time.Second)
	client.sendRealtimeToPeers(protocol.TrafficCustomRealtime, packet, peerIDs, deadline, 17)
	requireBoundedRealtimeFanout(t, network.realtimeBatches, wantDestinations, packet, protocol.TrafficCustomRealtime, deadline, 17)
}

func largeFanoutTestAddress(index int) netip.AddrPort {
	return netip.AddrPortFrom(netip.AddrFrom4([4]byte{10, 1, byte(index / 256), byte(index)}), 9000)
}

func requireBoundedRealtimeFanout(
	t *testing.T,
	batches []endpoint.RealtimeBatch,
	wantDestinations []netip.AddrPort,
	packet []byte,
	class protocol.TrafficClass,
	deadline time.Time,
	generation uint64,
) {
	t.Helper()
	wantBatches := (len(wantDestinations) + endpoint.MaxRealtimeBatchDatagrams - 1) / endpoint.MaxRealtimeBatchDatagrams
	if len(batches) != wantBatches {
		t.Fatalf("realtime batch calls = %d, want %d", len(batches), wantBatches)
	}
	destinationIndex := 0
	for index, batch := range batches {
		wantDatagrams := min(endpoint.MaxRealtimeBatchDatagrams, len(wantDestinations)-destinationIndex)
		if len(batch.Datagrams) != wantDatagrams {
			t.Fatalf("batch %d datagrams = %d, want %d", index, len(batch.Datagrams), wantDatagrams)
		}
		if batch.Class != class || batch.Deadline != deadline || batch.Generation != generation {
			t.Fatalf("batch %d metadata = %#v", index, batch)
		}
		for _, datagram := range batch.Datagrams {
			if len(datagram.Data) != len(packet) || &datagram.Data[0] != &packet[0] {
				t.Fatal("fanout chunk did not retain the immutable packet bytes")
			}
			if destinationIndex >= len(wantDestinations) {
				t.Fatalf("unexpected destination %d = %s", destinationIndex, datagram.Destination)
			}
			if datagram.Destination != wantDestinations[destinationIndex] {
				t.Fatalf("destination %d = %s, want %s", destinationIndex, datagram.Destination, wantDestinations[destinationIndex])
			}
			destinationIndex++
		}
	}
	if destinationIndex != len(wantDestinations) {
		t.Fatalf("sent destinations = %d, want %d", destinationIndex, len(wantDestinations))
	}
}

func TestRefreshFanoutDoesNotWaitForUnavailableOldForwarder(t *testing.T) {
	local := testLocalIdentity(t)
	room, err := invite.New("fanout refresh")
	if err != nil {
		t.Fatal(err)
	}
	client := newClient(local, room, func() roomNetwork { return nil }, slog.Default())
	forwarder := testLocalIdentity(t)
	path, err := NewPath(netip.MustParseAddrPort("192.0.2.10:9000"))
	if err != nil {
		t.Fatal(err)
	}
	session := testPeeringSession(t, path)
	session.authenticated = true
	client.remotePeers[forwarder.PeerID()] = &RemotePeer{identity: forwarder.Identity, session: session}
	client.fanout = outboundFanout{
		generation:  4,
		assignments: map[string][]string{"unavailable-old-forwarder": nil},
	}
	client.fanoutDirty = true

	client.refreshFanout(time.Now())
	if client.fanout.generation != 5 || client.fanoutDirty {
		t.Fatalf("new fanout was not installed: %#v, dirty=%t", client.fanout, client.fanoutDirty)
	}
	packet, ok := nextReliablePacket(session.reliable, time.Now())
	if !ok || packet.Channel != reliableChannelFanout {
		t.Fatalf("current assignment was not queued: %#v, %t", packet, ok)
	}
}

func TestRefreshFanoutDeploysUnchangedPlanToReplacementSession(t *testing.T) {
	room, err := invite.New("fanout replacement")
	if err != nil {
		t.Fatal(err)
	}
	client := newClient(testLocalIdentity(t), room, func() roomNetwork { return nil }, slog.Default())
	forwarder := testLocalIdentity(t)
	path, err := NewPath(netip.MustParseAddrPort("192.0.2.40:9000"))
	if err != nil {
		t.Fatal(err)
	}
	firstSession := testPeeringSession(t, path)
	firstSession.authenticated = true
	client.remotePeers[forwarder.PeerID()] = &RemotePeer{identity: forwarder.Identity, session: firstSession}
	client.refreshFanout(time.Now())
	firstGeneration := client.fanout.generation

	replacement := testPeeringSession(t, path)
	replacement.authenticated = true
	client.remotePeers[forwarder.PeerID()].session = replacement
	client.fanoutDirty = true
	client.refreshFanout(time.Now())

	if client.fanoutDirty || client.fanout.generation != firstGeneration+1 || replacement.reliable.queuedBytes == 0 {
		t.Fatalf("replacement session did not receive unchanged plan: fanout=%#v dirty=%t queued=%d", client.fanout, client.fanoutDirty, replacement.reliable.queuedBytes)
	}
}

func TestRefreshFanoutFullAvailableRevocationRetriesAtomically(t *testing.T) {
	room, err := invite.New("fanout revocation backpressure")
	if err != nil {
		t.Fatal(err)
	}
	client := newClient(testLocalIdentity(t), room, func() roomNetwork { return nil }, slog.Default())
	current := testLocalIdentity(t)
	old := testLocalIdentity(t)
	currentSession := testAuthenticatedSession(t, "192.0.2.41:9000")
	oldSession := testAuthenticatedSession(t, "192.0.2.42:9000")
	client.remotePeers[current.PeerID()] = &RemotePeer{identity: current.Identity, session: currentSession}
	client.remotePeers[old.PeerID()] = &RemotePeer{identity: old.Identity, session: oldSession}
	client.topology[current.PeerID()] = &topologyPeer{
		identity: current.Identity,
		neighbors: map[string]time.Time{
			old.PeerID(): time.Now().Add(time.Minute),
		},
	}
	client.fanout = outboundFanout{generation: 4, assignments: map[string][]string{old.PeerID(): nil}}
	oldSession.reliable.queuedBytes = maxQueuedReliableBytes

	client.refreshFanout(time.Now())
	if !client.fanoutDirty || client.fanout.generation != 4 || currentSession.reliable.queuedBytes != 0 {
		t.Fatalf("blocked revocation partially deployed plan: fanout=%#v dirty=%t current queued=%d", client.fanout, client.fanoutDirty, currentSession.reliable.queuedBytes)
	}

	oldSession.reliable.queuedBytes = 0
	client.refreshFanout(time.Now())
	if client.fanoutDirty || client.fanout.generation != 5 || currentSession.reliable.queuedBytes == 0 || oldSession.reliable.queuedBytes == 0 {
		t.Fatal("fanout deployment did not atomically retry assignment and revocation")
	}
}

func TestRefreshFanoutDoesNotPartiallyQueueCurrentPlan(t *testing.T) {
	room, err := invite.New("fanout partial queue")
	if err != nil {
		t.Fatal(err)
	}
	client := newClient(testLocalIdentity(t), room, func() roomNetwork { return nil }, slog.Default())
	first := testLocalIdentity(t)
	second := testLocalIdentity(t)
	firstSession := testAuthenticatedSession(t, "192.0.2.30:9000")
	secondSession := testAuthenticatedSession(t, "192.0.2.31:9000")
	secondReliable := secondSession.reliable
	secondSession.reliable = nil
	client.remotePeers[first.PeerID()] = &RemotePeer{identity: first.Identity, session: firstSession}
	client.remotePeers[second.PeerID()] = &RemotePeer{identity: second.Identity, session: secondSession}

	client.refreshFanout(time.Now())
	client.refreshFanout(time.Now())
	if firstSession.reliable.queuedBytes != 0 {
		t.Fatalf("available current assignment was repeatedly queued: %d bytes", firstSession.reliable.queuedBytes)
	}
	secondSession.reliable = secondReliable
	client.refreshFanout(time.Now())
	if client.fanoutDirty || firstSession.reliable.queuedBytes == 0 || secondSession.reliable.queuedBytes == 0 {
		t.Fatal("complete current plan was not queued and installed")
	}
}

func TestForgedNewGroupStreamDoesNotAllocateReceiverState(t *testing.T) {
	room, err := invite.New("group receiver authentication")
	if err != nil {
		t.Fatal(err)
	}
	client := newClient(testLocalIdentity(t), room, func() roomNetwork { return nil }, slog.Default())
	remote := testLocalIdentity(t)
	address := netip.MustParseAddrPort("192.0.2.20:9000")
	path, err := NewPath(address)
	if err != nil {
		t.Fatal(err)
	}
	session := testPeeringSession(t, path)
	session.authenticated = true
	client.remotePeers[remote.PeerID()] = &RemotePeer{identity: remote.Identity, session: session}
	header := protocol.GroupDatagramHeader{
		Class: protocol.TrafficAudio, SenderID: rawPeerIdentity(remote.Identity),
		StreamID: [16]byte{1}, Sequence: 1,
	}
	protector := protocol.NewGroupDatagramCipher([32]byte{1})
	packet, err := protocol.MarshalGroupDatagram(client.roomTag, header, 1, []byte("forged"), protector, remote)
	if err != nil {
		t.Fatal(err)
	}
	client.handleGroupDatagram(endpoint.Datagram{Data: packet, From: address, ReceivedAt: time.Now()}, nil)
	if len(client.groupReceivers) != 0 {
		t.Fatal("unauthenticated new stream allocated receiver state")
	}
}

func TestGroupDatagramStreamFollowsAuthenticatedSessionTopology(t *testing.T) {
	room, err := invite.New("group stream topology")
	if err != nil {
		t.Fatal(err)
	}
	client := newClient(testLocalIdentity(t), room, func() roomNetwork { return nil }, slog.Default())
	remote := testLocalIdentity(t)
	address := netip.MustParseAddrPort("192.0.2.50:9000")
	path, err := NewPath(address)
	if err != nil {
		t.Fatal(err)
	}
	oldStream := [16]byte{1}
	newStream := [16]byte{2}
	session := testPeeringSession(t, path)
	session.authenticated = true
	session.audioStreamID = oldStream
	peer := &RemotePeer{identity: remote.Identity, session: session}
	client.remotePeers[remote.PeerID()] = peer
	flow := media.NewFlow()
	packet := func(stream [16]byte, sequence uint64) []byte {
		header := protocol.GroupDatagramHeader{
			Class: protocol.TrafficAudio, SenderID: rawPeerIdentity(remote.Identity),
			StreamID: stream, Sequence: sequence,
		}
		encoded, marshalErr := protocol.MarshalGroupDatagram(client.roomTag, header, uint32(sequence), []byte("audio"), client.groupProtector, remote)
		if marshalErr != nil {
			t.Fatal(marshalErr)
		}
		return encoded
	}

	client.handleGroupDatagram(endpoint.Datagram{Data: packet(oldStream, 1), From: address, ReceivedAt: time.Now()}, flow)
	if _, ok := flow.TakeReceived(); !ok {
		t.Fatal("advertised old stream was rejected")
	}

	replacement := testPeeringSession(t, path)
	replacement.authenticated = true
	peer.session = replacement
	client.handleGroupDatagram(endpoint.Datagram{Data: packet(oldStream, 2), From: address, ReceivedAt: time.Now()}, flow)
	client.handleGroupDatagram(endpoint.Datagram{Data: packet(newStream, 1), From: address, ReceivedAt: time.Now()}, flow)
	newKey := groupStreamKey{sender: rawPeerIdentity(remote.Identity), stream: newStream}
	if _, ok := flow.TakeReceived(); ok || replacement.audioStreamID != ([16]byte{}) || client.groupReceivers[newKey] != nil {
		t.Fatal("replacement session accepted audio before a topology snapshot")
	}

	topology, err := encodeTopologyMessage(topologyMessage{generation: 1, audioStreamID: newStream})
	if err != nil {
		t.Fatal(err)
	}
	client.handleTopologySnapshot(peer, topology)
	client.handleGroupDatagram(endpoint.Datagram{Data: packet(oldStream, 3), From: address, ReceivedAt: time.Now()}, flow)
	client.handleGroupDatagram(endpoint.Datagram{Data: packet(newStream, 1), From: address, ReceivedAt: time.Now()}, flow)
	received, ok := flow.TakeReceived()
	if !ok || received.StreamID != newStream {
		t.Fatalf("new topology stream was not accepted: %#v, ok=%t", received, ok)
	}
}

func TestGroupStreamRotationWaitsForTopologyDelivery(t *testing.T) {
	room, err := invite.New("group stream rotation")
	if err != nil {
		t.Fatal(err)
	}
	network := newFakeRoomNetwork()
	client := newClient(testLocalIdentity(t), room, func() roomNetwork { return network }, slog.Default())
	client.roomNetwork = network
	client.initGroupStream()
	oldStream := client.groupStreamID
	oldTopologyGeneration := client.topologyGeneration
	remote := testLocalIdentity(t)
	path, err := NewPath(netip.MustParseAddrPort("192.0.2.60:9000"))
	if err != nil {
		t.Fatal(err)
	}
	session := testPeeringSession(t, path)
	session.authenticated = true
	client.remotePeers[remote.PeerID()] = &RemotePeer{identity: remote.Identity, session: session}
	client.groupSendSequence = math.MaxUint64
	frame := media.SendFrame{Timestamp: 1, Payload: []byte("audio"), Deadline: time.Now().Add(time.Second), Generation: 1}

	client.sendGroupMedia(frame)
	if len(network.realtimeBatches) != 0 || client.groupStreamID == oldStream || client.groupSendSequence != 0 ||
		!client.groupStreamPendingTopology || client.topologyGeneration != oldTopologyGeneration+1 {
		t.Fatal("sequence exhaustion did not rotate and wait for topology")
	}
	client.sendGroupMedia(frame)
	if len(network.realtimeBatches) != 0 {
		t.Fatal("audio was sent while rotated topology remained pending")
	}

	now := time.Now()
	sawTopology := false
	for {
		packet, ok := nextReliablePacket(session.reliable, now)
		if !ok {
			break
		}
		if packet.Channel == reliableChannelTopology {
			sawTopology = true
		}
		session.reliable.receive(protocol.ReliablePacket{
			Channel: packet.Channel, Flags: protocol.ReliableFlagAckOnly,
			AckBase: packet.FragmentSequence, AckBitmap: 1,
		}, now)
	}
	if !sawTopology {
		t.Fatal("rotated topology snapshot was not queued")
	}

	client.sendGroupMedia(frame)
	if len(network.realtimeBatches) != 1 || len(network.realtimeBatches[0].Datagrams) != 1 {
		t.Fatalf("audio did not resume after topology delivery: %#v", network.realtimeBatches)
	}
	header, err := protocol.ParseGroupDatagramHeader(network.realtimeBatches[0].Datagrams[0].Data, client.roomTag)
	if err != nil || header.StreamID != client.groupStreamID || header.Sequence != 1 {
		t.Fatalf("resumed audio used wrong stream: %#v, err=%v", header, err)
	}
}

func testAuthenticatedSession(t *testing.T, address string) *PeeringSession {
	t.Helper()
	path, err := NewPath(netip.MustParseAddrPort(address))
	if err != nil {
		t.Fatal(err)
	}
	session := testPeeringSession(t, path)
	session.authenticated = true
	return session
}

func testLocalIdentity(t testing.TB) *identity.LocalIdentity {
	t.Helper()
	local, err := identity.LoadOrCreate(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	return local
}
