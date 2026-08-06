package peer

import (
	"bytes"
	"context"
	"encoding/binary"
	"errors"
	"io"
	"log/slog"
	"net/netip"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"bork/internal/identity"
	"bork/internal/invite"
	"bork/internal/networking"
	"bork/internal/networking/endpoint"
	"bork/internal/protocol"
)

type fakeVirtualLANDevice struct {
	name      string
	reads     chan []byte
	writes    chan []byte
	closed    chan struct{}
	closeOnce sync.Once
	readCount atomic.Int32
	cleanups  atomic.Int32
	minOffset int
}

func newFakeVirtualLANDevice(name string) *fakeVirtualLANDevice {
	return &fakeVirtualLANDevice{name: name, reads: make(chan []byte, 128), writes: make(chan []byte, 128), closed: make(chan struct{}), minOffset: 10}
}

func (device *fakeVirtualLANDevice) Read(buffers [][]byte, sizes []int, offset int) (int, error) {
	if offset < device.minOffset {
		return 0, errors.New("insufficient TUN packet headroom")
	}
	select {
	case packet := <-device.reads:
		device.readCount.Add(1)
		copy(buffers[0][offset:], packet)
		sizes[0] = len(packet)
		return 1, nil
	case <-device.closed:
		return 0, errors.New("fake TUN closed")
	}
}

func (device *fakeVirtualLANDevice) Write(buffers [][]byte, offset int) (int, error) {
	if offset < device.minOffset {
		return 0, errors.New("insufficient TUN packet headroom")
	}
	select {
	case device.writes <- append([]byte(nil), buffers[0][offset:]...):
		return 1, nil
	case <-device.closed:
		return 0, errors.New("fake TUN closed")
	}
}

func (device *fakeVirtualLANDevice) Name() (string, error) { return device.name, nil }
func (device *fakeVirtualLANDevice) Close() error {
	device.closeOnce.Do(func() { close(device.closed) })
	return nil
}
func (device *fakeVirtualLANDevice) BatchSize() int { return 1 }

func TestVirtualLANAddressDerivation(t *testing.T) {
	room := [16]byte{1, 2, 3}
	identityKey := []byte("identity")
	first := deriveVirtualLANAddress(room, identityKey)
	if first != deriveVirtualLANAddress(room, identityKey) || !validVirtualLANHost(first) {
		t.Fatalf("derived address = %v", first)
	}
	if first == deriveVirtualLANAddress([16]byte{9}, identityKey) || first == deriveVirtualLANAddress(room, []byte("other")) {
		t.Fatal("address derivation ignored room or identity")
	}
}

func TestVirtualLANStateEnvelopeAndIPv4Validation(t *testing.T) {
	state := virtualLANState{generation: 7, enabled: true, streamID: [16]byte{1}, address: netip.MustParseAddr("100.70.1.2")}
	payload, err := encodeVirtualLANState(state)
	if err != nil {
		t.Fatal(err)
	}
	decoded, err := decodeVirtualLANState(payload)
	if err != nil || decoded != state {
		t.Fatalf("state round trip = %#v, %v", decoded, err)
	}
	for _, bad := range [][]byte{payload[:len(payload)-1], append(append([]byte(nil), payload...), 0)} {
		if _, err := decodeVirtualLANState(bad); err == nil {
			t.Fatal("non-canonical state length accepted")
		}
	}
	disabled, err := encodeVirtualLANState(virtualLANState{generation: 8})
	if err != nil || disabled[1] != 0 || !bytes.Equal(disabled[10:], make([]byte, 20)) {
		t.Fatalf("disabled state is not canonical: %x, %v", disabled, err)
	}
	badDisabled := append([]byte(nil), disabled...)
	badDisabled[29] = 1
	if _, err := decodeVirtualLANState(badDisabled); err == nil {
		t.Fatal("disabled state with address accepted")
	}

	packet := testIPv4Packet(t, state.address, netip.MustParseAddr("100.71.2.3"), 17, []byte("udp"))
	envelopeBytes, err := encodeVirtualLANEnvelope(virtualLANEnvelope{target: [32]byte{2}, packet: packet})
	if err != nil {
		t.Fatal(err)
	}
	envelope, err := decodeVirtualLANEnvelope(envelopeBytes)
	if err != nil || !bytes.Equal(envelope.packet, packet) {
		t.Fatalf("envelope round trip: %v", err)
	}
	if source, destination, err := validateVirtualLANIPv4(packet); err != nil || source != state.address || destination.String() != "100.71.2.3" {
		t.Fatalf("IPv4 validation = %v, %v, %v", source, destination, err)
	}
	badChecksum := append([]byte(nil), packet...)
	badChecksum[8]++
	if _, _, err := validateVirtualLANIPv4(badChecksum); err == nil {
		t.Fatal("bad IPv4 checksum accepted")
	}
	badLength := append([]byte(nil), packet...)
	binary.BigEndian.PutUint16(badLength[2:4], uint16(len(badLength)-1))
	if _, _, err := validateVirtualLANIPv4(badLength); err == nil {
		t.Fatal("bad IPv4 total length accepted")
	}
	if _, _, err := validateVirtualLANIPv4(testIPv4Packet(t, state.address, netip.MustParseAddr("192.0.2.1"), 1, nil)); err == nil {
		t.Fatal("off-overlay destination accepted")
	}
}

func TestVirtualLANRejectsWrongAndAmbiguousRouting(t *testing.T) {
	client, local, room := newVirtualLANUnitClient(t)
	client.roomNetwork = &fakeRoomNetwork{}
	client.localVirtualLAN = virtualLANState{generation: 2, enabled: true, streamID: [16]byte{9}, address: deriveVirtualLANAddress(room.RoomTag(), local.PublicKey())}
	duplicate := netip.MustParseAddr("100.80.1.2")
	for index := byte(1); index <= 2; index++ {
		remote, err := identity.LoadOrCreate(t.TempDir())
		if err != nil {
			t.Fatal(err)
		}
		path, _ := NewPath(netip.AddrPortFrom(netip.MustParseAddr("192.0.2."+string('0'+index)), 1000+uint16(index)))
		client.remotePeers[remote.PeerID()] = &RemotePeer{identity: remote.Identity, session: &PeeringSession{authenticated: true, path: path, remoteVirtualLAN: virtualLANState{generation: 1, enabled: true, streamID: [16]byte{index}, address: duplicate}}}
	}
	client.routeVirtualLANPacket(testIPv4Packet(t, client.localVirtualLAN.address, duplicate, 17, []byte("x")))
	if len(client.roomNetwork.(*fakeRoomNetwork).realtimeBatches) != 0 {
		t.Fatal("ambiguous duplicate address was routed")
	}
	wrongSource := testIPv4Packet(t, netip.MustParseAddr("100.90.1.1"), duplicate, 17, nil)
	client.routeVirtualLANPacket(wrongSource)
	if len(client.roomNetwork.(*fakeRoomNetwork).realtimeBatches) != 0 {
		t.Fatal("packet with wrong local source was routed")
	}
}

func TestVirtualLANStateAddressIsBoundToSenderIdentity(t *testing.T) {
	client, _, room := newVirtualLANUnitClient(t)
	remote, err := identity.LoadOrCreate(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	sender := &RemotePeer{identity: remote.Identity, session: &PeeringSession{authenticated: true}}
	client.remotePeers[remote.PeerID()] = sender
	wrong := virtualLANState{generation: 1, enabled: true, streamID: [16]byte{1}, address: netip.MustParseAddr("100.80.1.2")}
	if wrong.address == deriveVirtualLANAddress(room.RoomTag(), remote.PublicKey()) {
		wrong.address = netip.MustParseAddr("100.80.1.3")
	}
	payload, _ := encodeVirtualLANState(wrong)
	client.handleVirtualLANState(sender, payload)
	if sender.session.remoteVirtualLAN.generation != 0 {
		t.Fatal("sender advertised an address not derived from its identity")
	}
	correct := wrong
	correct.address = deriveVirtualLANAddress(room.RoomTag(), remote.PublicKey())
	payload, _ = encodeVirtualLANState(correct)
	client.handleVirtualLANState(sender, payload)
	if sender.session.remoteVirtualLAN != correct {
		t.Fatalf("derived sender address was rejected: %#v", sender.session.remoteVirtualLAN)
	}
}

func TestVirtualLANDirectDeliveryAndIntermediaryAuthorization(t *testing.T) {
	client, local, room := newVirtualLANUnitClient(t)
	network := &fakeRoomNetwork{}
	client.roomNetwork = network
	localAddress := deriveVirtualLANAddress(room.RoomTag(), local.PublicKey())
	client.localVirtualLAN = virtualLANState{generation: 2, enabled: true, streamID: [16]byte{9}, address: localAddress}
	client.virtualLANWorker = &virtualLANWorker{writes: make(chan []byte, 2)}

	senderIdentity, _ := identity.LoadOrCreate(t.TempDir())
	senderAddress := netip.MustParseAddr("100.75.1.1")
	senderPathAddress := netip.MustParseAddrPort("192.0.2.10:1010")
	senderPath, _ := NewPath(senderPathAddress)
	senderStream := [16]byte{3}
	sender := &RemotePeer{identity: senderIdentity.Identity, session: &PeeringSession{authenticated: true, path: senderPath, remoteVirtualLAN: virtualLANState{generation: 1, enabled: true, streamID: senderStream, address: senderAddress}}}
	client.remotePeers[senderIdentity.PeerID()] = sender

	directPacket := testIPv4Packet(t, senderAddress, localAddress, 1, []byte("icmp"))
	encoded, header := testVirtualLANDatagram(t, client, senderIdentity, senderStream, rawPeerIdentity(local.Identity), directPacket, 1)
	client.handleVirtualLANDatagram(sender, header, endpoint.Datagram{Data: encoded, From: senderPathAddress, ReceivedAt: time.Now()})
	select {
	case injected := <-client.virtualLANWorker.writes:
		if !bytes.Equal(injected, directPacket) {
			t.Fatal("wrong direct packet injected")
		}
	default:
		t.Fatal("authorized direct packet was not injected")
	}
	wrongSourcePacket := testIPv4Packet(t, netip.MustParseAddr("100.77.1.1"), localAddress, 1, nil)
	encoded, header = testVirtualLANDatagram(t, client, senderIdentity, senderStream, rawPeerIdentity(local.Identity), wrongSourcePacket, 2)
	client.handleVirtualLANDatagram(sender, header, endpoint.Datagram{Data: encoded, From: senderPathAddress, ReceivedAt: time.Now()})
	wrongDestinationPacket := testIPv4Packet(t, senderAddress, netip.MustParseAddr("100.78.1.1"), 1, nil)
	encoded, header = testVirtualLANDatagram(t, client, senderIdentity, senderStream, rawPeerIdentity(local.Identity), wrongDestinationPacket, 3)
	client.handleVirtualLANDatagram(sender, header, endpoint.Datagram{Data: encoded, From: senderPathAddress, ReceivedAt: time.Now()})
	if len(client.virtualLANWorker.writes) != 0 {
		t.Fatal("packet with wrong source or destination was injected")
	}

	targetIdentity, _ := identity.LoadOrCreate(t.TempDir())
	targetAddress := netip.MustParseAddr("100.76.1.1")
	targetPathAddress := netip.MustParseAddrPort("192.0.2.11:1011")
	targetPath, _ := NewPath(targetPathAddress)
	target := &RemotePeer{identity: targetIdentity.Identity, session: &PeeringSession{authenticated: true, path: targetPath, remoteVirtualLAN: virtualLANState{generation: 1, enabled: true, streamID: [16]byte{4}, address: targetAddress}}}
	client.remotePeers[targetIdentity.PeerID()] = target
	sender.session.inboundFanout = fanoutAssignment{generation: 1, listeners: []string{targetIdentity.PeerID()}}
	forwardPacket := testIPv4Packet(t, senderAddress, targetAddress, 17, []byte("udp"))
	encoded, header = testVirtualLANDatagram(t, client, senderIdentity, senderStream, rawPeerIdentity(targetIdentity.Identity), forwardPacket, 4)
	client.handleVirtualLANDatagram(sender, header, endpoint.Datagram{Data: encoded, From: senderPathAddress, ReceivedAt: time.Now()})
	if len(network.realtimeBatches) != 1 || len(network.realtimeBatches[0].Datagrams) != 1 || network.realtimeBatches[0].Datagrams[0].Destination != targetPathAddress {
		t.Fatalf("authorized intermediary forwarding = %#v", network.realtimeBatches)
	}
	network.realtimeBatches = nil
	sender.session.inboundFanout.listeners = nil
	encoded, header = testVirtualLANDatagram(t, client, senderIdentity, senderStream, rawPeerIdentity(targetIdentity.Identity), forwardPacket, 5)
	client.handleVirtualLANDatagram(sender, header, endpoint.Datagram{Data: encoded, From: senderPathAddress, ReceivedAt: time.Now()})
	if len(network.realtimeBatches) != 0 {
		t.Fatal("unauthorized intermediary forwarding succeeded")
	}
}

func TestVirtualLANFanoutUsesActiveBridgeIntermediary(t *testing.T) {
	firstForwarder, _ := identity.LoadOrCreate(t.TempDir())
	secondForwarder, _ := identity.LoadOrCreate(t.TempDir())
	targetIdentity, _ := identity.LoadOrCreate(t.TempDir())
	firstAddress := netip.MustParseAddrPort("192.0.2.20:2020")
	secondAddress := netip.MustParseAddrPort("192.0.2.21:2021")
	firstPath, _ := NewPath(firstAddress)
	secondPath, _ := NewPath(secondAddress)
	bridgePath, _ := NewBridgePath(firstAddress, rawPeerIdentity(firstForwarder.Identity), rawPeerIdentity(targetIdentity.Identity))
	peers := map[string]*RemotePeer{
		firstForwarder.PeerID():  {identity: firstForwarder.Identity, session: &PeeringSession{authenticated: true, path: firstPath}},
		secondForwarder.PeerID(): {identity: secondForwarder.Identity, session: &PeeringSession{authenticated: true, path: secondPath}},
		targetIdentity.PeerID():  {identity: targetIdentity.Identity, session: &PeeringSession{authenticated: true, path: bridgePath}},
	}
	plan := outboundFanout{
		destinations: []string{secondForwarder.PeerID()},
		assignments:  map[string][]string{secondForwarder.PeerID(): {targetIdentity.PeerID()}},
	}
	plan = constrainFanoutToActivePaths(plan, peers)
	if !containsPeerID(plan.destinations, firstForwarder.PeerID()) || !containsPeerID(plan.assignments[firstForwarder.PeerID()], targetIdentity.PeerID()) || containsPeerID(plan.assignments[secondForwarder.PeerID()], targetIdentity.PeerID()) {
		t.Fatalf("path-constrained fanout = %#v", plan)
	}
}

func TestVirtualLANFakeTUNEndToEndAndCleanup(t *testing.T) {
	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	room, err := invite.New("virtual LAN integration")
	if err != nil {
		t.Fatal(err)
	}
	firstIdentity, _ := identity.LoadOrCreate(t.TempDir())
	secondIdentity, _ := identity.LoadOrCreate(t.TempDir())
	options := networking.Options{Endpoint: endpoint.Options{ListenAddress: "[::]:0", STUNServers: []string{}, STUNRefresh: 0}}
	first := NewClient(firstIdentity, room, options, logger)
	second := NewClient(secondIdentity, room, options, logger)
	firstDevice, secondDevice := installFakeVirtualLAN(first), installFakeVirtualLAN(second)
	ctx, cancel := context.WithCancel(context.Background())
	firstDone, secondDone := make(chan error, 1), make(chan error, 1)
	go func() { firstDone <- first.Loop(ctx, nil) }()
	go func() { secondDone <- second.Loop(ctx, nil) }()
	waitForAuthenticatedRemotePeer(t, first, first.StateChanges(), secondIdentity.PeerID())
	waitForAuthenticatedRemotePeer(t, second, second.StateChanges(), firstIdentity.PeerID())
	if err := first.EnableVirtualLAN(); err != nil {
		t.Fatal(err)
	}
	if err := second.EnableVirtualLAN(); err != nil {
		t.Fatal(err)
	}
	waitForVirtualLANPeer(t, first, secondIdentity.PeerID())
	waitForVirtualLANPeer(t, second, firstIdentity.PeerID())
	firstAddress := deriveVirtualLANAddress(room.RoomTag(), firstIdentity.PublicKey())
	secondAddress := deriveVirtualLANAddress(room.RoomTag(), secondIdentity.PublicKey())
	for _, packet := range [][]byte{
		testIPv4Packet(t, firstAddress, secondAddress, 1, []byte("icmp-shaped")),
		testIPv4Packet(t, firstAddress, secondAddress, 17, []byte("udp-shaped")),
	} {
		deadline := time.Now().Add(5 * time.Second)
		for {
			firstDevice.reads <- packet
			select {
			case injected := <-secondDevice.writes:
				if !bytes.Equal(injected, packet) {
					t.Fatal("injected packet differs")
				}
				goto delivered
			case <-time.After(50 * time.Millisecond):
				if time.Now().After(deadline) {
					t.Fatal("timed out waiting for fake TUN delivery")
				}
			}
		}
	delivered:
	}
	if err := first.DisableVirtualLAN(); err != nil {
		t.Fatal(err)
	}
	if snapshot, _ := first.StateSnapshot(); snapshot.VirtualLAN.Status != "disabled" || snapshot.VirtualLAN.Address != "" || snapshot.VirtualLAN.Interface != "" {
		t.Fatalf("disabled snapshot = %#v", snapshot.VirtualLAN)
	}
	select {
	case <-firstDevice.closed:
	default:
		t.Fatal("TUN device was not closed on disable")
	}
	if firstDevice.cleanups.Load() != 1 {
		t.Fatalf("disable cleanups = %d", firstDevice.cleanups.Load())
	}
	cancel()
	if err := <-firstDone; err != nil {
		t.Fatal(err)
	}
	if err := <-secondDone; err != nil {
		t.Fatal(err)
	}
	select {
	case <-secondDevice.closed:
	default:
		t.Fatal("TUN device was not closed on loop shutdown")
	}
	if secondDevice.cleanups.Load() != 1 {
		t.Fatalf("shutdown cleanups = %d", secondDevice.cleanups.Load())
	}
}

func TestVirtualLANEventQueueIsBounded(t *testing.T) {
	client, _, _ := newVirtualLANUnitClient(t)
	client.virtualLANEvents = make(chan virtualLANEvent, 1)
	device := newFakeVirtualLANDevice("fake")
	worker := &virtualLANWorker{}
	done := make(chan error, 1)
	go client.readVirtualLAN(worker, device, done)
	for index := 0; index < 10; index++ {
		device.reads <- []byte{byte(index)}
	}
	deadline := time.Now().Add(time.Second)
	for device.readCount.Load() < 10 && time.Now().Before(deadline) {
		time.Sleep(time.Millisecond)
	}
	if device.readCount.Load() != 10 || len(client.virtualLANEvents) != 1 {
		t.Fatalf("reads=%d queued=%d", device.readCount.Load(), len(client.virtualLANEvents))
	}
	_ = device.Close()
	<-done
}

func TestVirtualLANStopCancelsSetup(t *testing.T) {
	client, _, _ := newVirtualLANUnitClient(t)
	started := make(chan struct{})
	client.virtualLANCreate = func(ctx context.Context, _ string, _ int) (virtualLANDevice, error) {
		close(started)
		<-ctx.Done()
		return nil, ctx.Err()
	}
	enableResult := make(chan error, 1)
	client.handleVirtualLANCommand(virtualLANCommand{enable: true, result: enableResult})
	worker := client.virtualLANWorker
	<-started
	disableResult := make(chan error, 1)
	client.handleVirtualLANCommand(virtualLANCommand{enable: false, result: disableResult})
	if err := <-disableResult; err != nil {
		t.Fatal(err)
	}
	select {
	case event := <-client.virtualLANEvents:
		client.handleVirtualLANEvent(event)
	case <-time.After(time.Second):
		t.Fatal("cancelled setup did not return")
	}
	if err := <-enableResult; !errors.Is(err, context.Canceled) {
		t.Fatalf("enable result = %v, want context cancellation", err)
	}
	<-worker.done
	if snapshot := client.virtualLANSnapshot(); snapshot.Status != "disabled" || snapshot.Error != "" {
		t.Fatalf("snapshot after setup cancellation = %#v", snapshot)
	}
}

func newVirtualLANUnitClient(t *testing.T) (*Client, *identity.LocalIdentity, invite.Invite) {
	t.Helper()
	room, err := invite.New("virtual LAN unit")
	if err != nil {
		t.Fatal(err)
	}
	local, err := identity.LoadOrCreate(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	return newClient(local, room, func() roomNetwork { return newFakeRoomNetwork() }, slog.Default()), local, room
}

func installFakeVirtualLAN(client *Client) *fakeVirtualLANDevice {
	device := newFakeVirtualLANDevice("fake-" + client.localIdentity.PeerID()[:6])
	client.virtualLANCreate = func(context.Context, string, int) (virtualLANDevice, error) { return device, nil }
	client.virtualLANConfigure = func(context.Context, string, netip.Addr, int) (func() error, error) {
		return func() error { device.cleanups.Add(1); return nil }, nil
	}
	return device
}

func waitForVirtualLANPeer(t *testing.T, client *Client, peerID string) {
	t.Helper()
	deadline := time.NewTimer(8 * time.Second)
	defer deadline.Stop()
	for {
		snapshot, _ := client.StateSnapshot()
		for _, remote := range snapshot.RemoteVirtualLAN {
			if remote.PeerID == peerID {
				time.Sleep(100 * time.Millisecond)
				return
			}
		}
		select {
		case <-client.StateChanges():
		case <-deadline.C:
			t.Fatalf("timed out waiting for virtual LAN peer %s", peerID)
		}
	}
}

func testVirtualLANDatagram(t *testing.T, client *Client, sender *identity.LocalIdentity, stream [16]byte, target [32]byte, packet []byte, sequence uint64) ([]byte, protocol.GroupDatagramHeader) {
	t.Helper()
	payload, err := encodeVirtualLANEnvelope(virtualLANEnvelope{target: target, packet: packet})
	if err != nil {
		t.Fatal(err)
	}
	header := protocol.GroupDatagramHeader{Class: protocol.TrafficCustomRealtime, SenderID: rawPeerIdentity(sender.Identity), StreamID: stream, Sequence: sequence}
	encoded, err := protocol.MarshalGroupDatagram(client.roomTag, header, 0, payload, client.groupProtector, sender)
	if err != nil {
		t.Fatal(err)
	}
	return encoded, header
}

func testIPv4Packet(t *testing.T, source, destination netip.Addr, protocolNumber byte, payload []byte) []byte {
	t.Helper()
	packet := make([]byte, 20+len(payload))
	packet[0] = 0x45
	binary.BigEndian.PutUint16(packet[2:4], uint16(len(packet)))
	packet[8] = 64
	packet[9] = protocolNumber
	sourceBytes, destinationBytes := source.As4(), destination.As4()
	copy(packet[12:16], sourceBytes[:])
	copy(packet[16:20], destinationBytes[:])
	copy(packet[20:], payload)
	binary.BigEndian.PutUint16(packet[10:12], ^uint16(ipv4Checksum(packet[:20])))
	return packet
}
