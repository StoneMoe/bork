package peer

import (
	"bytes"
	"errors"
	"net/netip"
	"testing"
	"time"

	"bork/internal/protocol"
)

func TestReliableOrderedDeliveryWaitsForPriorMessage(t *testing.T) {
	r := newReliableTransport()
	now := time.Unix(100, 0)

	for index, payload := range [][]byte{{'d'}, {'e'}, {'f'}} {
		if delivered := r.receive(reliableData(1, true, uint64(4+index), 2, uint16(index), 3, payload), now); len(delivered) != 0 {
			t.Fatalf("message 2 fragment %d delivered %d messages", index, len(delivered))
		}
	}
	second := reliableData(1, true, 2, 1, 1, 3, []byte("b"))
	if delivered := r.receive(second, now); len(delivered) != 0 {
		t.Fatalf("incomplete message delivered %d messages", len(delivered))
	}
	second.Payload[0] = 'x'
	if delivered := r.receive(reliableData(1, true, 3, 1, 2, 3, []byte("c")), now); len(delivered) != 0 {
		t.Fatalf("incomplete message delivered %d messages", len(delivered))
	}
	delivered := r.receive(reliableData(1, true, 1, 1, 0, 3, []byte("a")), now)
	if len(delivered) != 2 {
		t.Fatalf("delivered %d messages, want 2", len(delivered))
	}
	if delivered[0].channel != 1 || !bytes.Equal(delivered[0].payload, []byte("abc")) {
		t.Fatalf("first delivery = %#v", delivered[0])
	}
	if delivered[1].channel != 1 || !bytes.Equal(delivered[1].payload, []byte("def")) {
		t.Fatalf("second delivery = %#v", delivered[1])
	}
}

func TestReliableUnorderedDeliveryDoesNotWait(t *testing.T) {
	r := newReliableTransport()
	now := time.Unix(200, 0)
	if delivered := r.receive(reliableData(2, false, 1, 1, 0, 2, []byte("a")), now); len(delivered) != 0 {
		t.Fatalf("incomplete message delivered %d messages", len(delivered))
	}
	if delivered := r.receive(reliableData(2, false, 3, 2, 0, 2, []byte("c")), now); len(delivered) != 0 {
		t.Fatalf("incomplete message delivered %d messages", len(delivered))
	}
	delivered := r.receive(reliableData(2, false, 4, 2, 1, 2, []byte("d")), now)
	if len(delivered) != 1 || !bytes.Equal(delivered[0].payload, []byte("cd")) {
		t.Fatalf("unordered delivery = %#v", delivered)
	}
}

func TestReliableLostFragmentRetransmitsAndACKClearsPending(t *testing.T) {
	r := newReliableTransport()
	input := []byte("lost")
	if err := r.queue(3, false, input); err != nil {
		t.Fatal(err)
	}
	input[0] = 'X'
	now := time.Unix(300, 0)
	first, ok := nextReliablePacket(r, now)
	if !ok || !bytes.Equal(first.Payload, []byte("lost")) {
		t.Fatalf("first packet = %#v, %t", first, ok)
	}
	if _, ok := nextReliablePacket(r, now.Add(initialReliableRTO-time.Nanosecond)); ok {
		t.Fatal("fragment retransmitted before RTO")
	}
	retransmit, ok := nextReliablePacket(r, now.Add(initialReliableRTO))
	if !ok || retransmit.FragmentSequence != first.FragmentSequence || !bytes.Equal(retransmit.Payload, first.Payload) {
		t.Fatalf("retransmission = %#v, %t", retransmit, ok)
	}
	r.receive(protocol.ReliablePacket{
		Channel:   3,
		Flags:     protocol.ReliableFlagAckOnly,
		AckBase:   first.FragmentSequence,
		AckBitmap: 1,
	}, now.Add(initialReliableRTO+time.Millisecond))
	if r.queuedBytes != 0 || r.bytesInFlight != 0 {
		t.Fatalf("queued = %d, in flight = %d", r.queuedBytes, r.bytesInFlight)
	}
}

func TestReliableACKBitmapHandlesOutOfOrderAndDuplicates(t *testing.T) {
	now := time.Unix(400, 0)
	sender := newReliableTransport()
	for _, payload := range []byte{'a', 'b', 'c'} {
		if err := sender.queue(4, false, []byte{payload}); err != nil {
			t.Fatal(err)
		}
		if _, ok := nextReliablePacket(sender, now); !ok {
			t.Fatal("nextPacket() returned no packet")
		}
	}
	partial := protocol.ReliablePacket{Channel: 4, Flags: protocol.ReliableFlagAckOnly, AckBase: 3, AckBitmap: 1 | 1<<2}
	sender.receive(partial, now.Add(time.Millisecond))
	if sender.queuedBytes != 1 {
		t.Fatalf("queued after sparse ACK = %d, want 1", sender.queuedBytes)
	}
	sender.receive(partial, now.Add(2*time.Millisecond))
	if sender.queuedBytes != 1 {
		t.Fatalf("queued after duplicate ACK = %d, want 1", sender.queuedBytes)
	}
	sender.receive(protocol.ReliablePacket{Channel: 4, Flags: protocol.ReliableFlagAckOnly, AckBase: 3, AckBitmap: 1 | 1<<1 | 1<<2}, now.Add(3*time.Millisecond))
	if sender.queuedBytes != 0 {
		t.Fatalf("queued after complete ACK = %d", sender.queuedBytes)
	}

	receiver := newReliableTransport()
	if delivered := receiver.receive(reliableData(5, false, 3, 3, 0, 1, []byte("c")), now); len(delivered) != 1 {
		t.Fatalf("first out-of-order delivery count = %d", len(delivered))
	}
	if delivered := receiver.receive(reliableData(5, false, 1, 1, 0, 1, []byte("a")), now); len(delivered) != 1 {
		t.Fatalf("second out-of-order delivery count = %d", len(delivered))
	}
	if delivered := receiver.receive(reliableData(5, false, 1, 1, 0, 1, []byte("a")), now); len(delivered) != 0 {
		t.Fatalf("duplicate delivered %d messages", len(delivered))
	}
	ack, ok := nextReliableACK(receiver)
	if !ok || ack.Flags != protocol.ReliableFlagAckOnly || ack.AckBase != 3 || ack.AckBitmap != 1|1<<2 {
		t.Fatalf("ACK = %#v, %t", ack, ok)
	}
	if _, ok := nextReliableACK(receiver); ok {
		t.Fatal("dirty ACK was not cleared")
	}
}

func TestReliableRTTAndTimeoutCongestionResponse(t *testing.T) {
	r := newReliableTransport()
	now := time.Unix(500, 0)
	if err := r.queue(6, false, []byte("rtt")); err != nil {
		t.Fatal(err)
	}
	packet, ok := nextReliablePacket(r, now)
	if !ok {
		t.Fatal("nextPacket() returned no packet")
	}
	r.receive(protocol.ReliablePacket{
		Channel:   6,
		Flags:     protocol.ReliableFlagAckOnly,
		AckBase:   packet.FragmentSequence,
		AckBitmap: 1,
	}, now.Add(10*time.Millisecond))
	if r.srtt != 10*time.Millisecond || r.rto != minimumReliableRTO {
		t.Fatalf("SRTT = %v, RTO = %v", r.srtt, r.rto)
	}

	r.cwnd = 8 * reliableMSS
	r.ssthresh = maximumReliableCwnd
	r.rto = 400 * time.Millisecond
	if err := r.queue(6, false, []byte("timeout")); err != nil {
		t.Fatal(err)
	}
	sentAt := now.Add(time.Second)
	if _, ok := nextReliablePacket(r, sentAt); !ok {
		t.Fatal("nextPacket() returned no packet")
	}
	if _, ok := nextReliablePacket(r, sentAt.Add(400*time.Millisecond)); !ok {
		t.Fatal("timed-out packet was not retransmitted")
	}
	if r.ssthresh != 4*reliableMSS || r.cwnd != minimumReliableCwnd || r.rto != 800*time.Millisecond {
		t.Fatalf("ssthresh = %d, cwnd = %d, RTO = %v", r.ssthresh, r.cwnd, r.rto)
	}
}

func TestReliableTimeoutWaveKeepsExpiredSiblingsEligible(t *testing.T) {
	r := newReliableTransport()
	if err := r.queue(11, false, bytes.Repeat([]byte{1}, 3*reliableMSS)); err != nil {
		t.Fatal(err)
	}
	now := time.Unix(550, 0)
	for sequence := uint64(1); sequence <= 3; sequence++ {
		packet, ok := nextReliablePacket(r, now)
		if !ok || packet.FragmentSequence != sequence {
			t.Fatalf("initial packet = %#v, %t; want sequence %d", packet, ok, sequence)
		}
	}

	timeoutAt := now.Add(initialReliableRTO)
	for sequence := uint64(1); sequence <= 3; sequence++ {
		packet, ok := nextReliablePacket(r, timeoutAt)
		if !ok || packet.FragmentSequence != sequence {
			t.Fatalf("timeout packet = %#v, %t; want sequence %d", packet, ok, sequence)
		}
	}
	if r.cwnd != minimumReliableCwnd || r.rto != 2*initialReliableRTO {
		t.Fatalf("cwnd = %d, RTO = %v", r.cwnd, r.rto)
	}
}

func TestReliableTimeoutWaveSurvivesPartialACK(t *testing.T) {
	r := newReliableTransport()
	if err := r.queue(12, false, bytes.Repeat([]byte{1}, 3*reliableMSS)); err != nil {
		t.Fatal(err)
	}
	now := time.Unix(560, 0)
	for range 3 {
		if _, ok := nextReliablePacket(r, now); !ok {
			t.Fatal("nextPacket() returned no initial packet")
		}
	}
	timeoutAt := now.Add(initialReliableRTO)
	first, ok := nextReliablePacket(r, timeoutAt)
	if !ok || first.FragmentSequence != 1 {
		t.Fatalf("first timeout packet = %#v, %t", first, ok)
	}
	r.receive(protocol.ReliablePacket{
		Channel:   12,
		Flags:     protocol.ReliableFlagAckOnly,
		AckBase:   first.FragmentSequence,
		AckBitmap: 1,
	}, timeoutAt.Add(time.Nanosecond))
	waveCwnd, waveSsthresh, waveRTO := r.cwnd, r.ssthresh, r.rto

	retryAt := now.Add(2 * initialReliableRTO)
	for sequence := uint64(2); sequence <= 3; sequence++ {
		packet, ok := nextReliablePacket(r, retryAt)
		if !ok || packet.FragmentSequence != sequence {
			t.Fatalf("remaining timeout packet = %#v, %t; want sequence %d", packet, ok, sequence)
		}
		if r.cwnd != waveCwnd || r.ssthresh != waveSsthresh || r.rto != waveRTO {
			t.Fatalf("same wave backed off again: cwnd=%d ssthresh=%d RTO=%v", r.cwnd, r.ssthresh, r.rto)
		}
	}
}

func TestReliablePersistentLossStartsLaterTimeoutWaves(t *testing.T) {
	r := newReliableTransport()
	if err := r.queue(13, false, []byte("lost")); err != nil {
		t.Fatal(err)
	}
	now := time.Unix(570, 0)
	if _, ok := nextReliablePacket(r, now); !ok {
		t.Fatal("nextPacket() returned no initial packet")
	}

	timeoutAt := now.Add(initialReliableRTO)
	for wave, wantRTO := range []time.Duration{time.Second, 2 * time.Second, 4 * time.Second, maximumReliableRTO} {
		if _, ok := nextReliablePacket(r, timeoutAt); !ok {
			t.Fatalf("wave %d returned no retransmission", wave+1)
		}
		if r.rto != wantRTO {
			t.Fatalf("wave %d RTO = %v, want %v", wave+1, r.rto, wantRTO)
		}
		timeoutAt = timeoutAt.Add(wantRTO)
	}
}

func TestReliableQueueAndReassemblyBounds(t *testing.T) {
	r := newReliableTransport()
	if err := r.queue(0, false, []byte("x")); err == nil {
		t.Fatal("queue accepted channel zero")
	}
	if err := r.queue(1, false, nil); err == nil {
		t.Fatal("queue accepted an empty message")
	}
	if err := r.queue(1, false, make([]byte, protocol.MaxReliableFragments*reliableMSS+1)); err == nil {
		t.Fatal("queue accepted an oversized message")
	}
	remaining := maxQueuedReliableBytes
	maximumMessage := protocol.MaxReliableFragments * reliableMSS
	for remaining > 0 {
		size := min(remaining, maximumMessage)
		if err := r.queue(1, false, make([]byte, size)); err != nil {
			t.Fatalf("queue %d bytes: %v", size, err)
		}
		remaining -= size
	}
	if r.queuedBytes != maxQueuedReliableBytes {
		t.Fatalf("queued = %d, want %d", r.queuedBytes, maxQueuedReliableBytes)
	}
	if err := r.queue(1, false, []byte{1}); err == nil {
		t.Fatal("queue exceeded its byte bound")
	}

	receiver := newReliableTransport()
	now := time.Unix(600, 0)
	fragment := bytes.Repeat([]byte{1}, reliableMSS)
	packetCount := maxReassemblyReliableBytes/reliableMSS + 2
	for index := 0; index < packetCount; index++ {
		receiver.receive(reliableData(2, false, uint64(index+1), uint64(index+1), 0, 2, fragment), now.Add(time.Duration(index)))
	}
	if receiver.reassemblyBytes > maxReassemblyReliableBytes {
		t.Fatalf("reassembly bytes = %d", receiver.reassemblyBytes)
	}
	if _, exists := receiver.channels[2].reassemblies[1]; !exists {
		t.Fatal("oldest retained assembly was evicted")
	}
	ack, ok := nextReliableACK(receiver)
	if !ok || protocol.AckContains(ack.AckBase, ack.AckBitmap, uint64(packetCount)) {
		t.Fatalf("unretained fragment was acknowledged: %#v", ack)
	}
}

func TestReliableFirstSendsStayWithinACKWindow(t *testing.T) {
	sender := newReliableTransport()
	receiver := newReliableTransport()
	for index := range 70 {
		if err := sender.queue(9, false, []byte{byte(index)}); err != nil {
			t.Fatal(err)
		}
	}
	now := time.Unix(650, 0)
	lost, ok := nextReliablePacket(sender, now)
	if !ok || lost.FragmentSequence != 1 {
		t.Fatalf("first packet = %#v, %t", lost, ok)
	}
	latest := uint64(0)
	for {
		packet, available := nextReliablePacket(sender, now)
		if !available {
			break
		}
		latest = packet.FragmentSequence
		receiver.receive(packet, now)
	}
	if latest != 64 {
		t.Fatalf("latest first send = %d, want 64", latest)
	}
	partial, ok := nextReliableACK(receiver)
	if !ok || partial.AckBase != 64 || protocol.AckContains(partial.AckBase, partial.AckBitmap, 1) {
		t.Fatalf("partial ACK = %#v, %t", partial, ok)
	}
	sender.receive(partial, now.Add(time.Millisecond))
	if _, ok := nextReliablePacket(sender, now.Add(2*time.Millisecond)); ok {
		t.Fatal("sender advanced beyond the representable ACK window")
	}

	retransmitAt := now.Add(initialReliableRTO)
	retransmit, ok := nextReliablePacket(sender, retransmitAt)
	if !ok || retransmit.FragmentSequence != lost.FragmentSequence {
		t.Fatalf("retransmission = %#v, %t", retransmit, ok)
	}
	receiver.receive(retransmit, retransmitAt)
	complete, ok := nextReliableACK(receiver)
	if !ok || !protocol.AckContains(complete.AckBase, complete.AckBitmap, 1) {
		t.Fatalf("complete ACK = %#v, %t", complete, ok)
	}
	sender.receive(complete, retransmitAt.Add(time.Millisecond))
	next, ok := nextReliablePacket(sender, retransmitAt.Add(2*time.Millisecond))
	if !ok || next.FragmentSequence != 65 {
		t.Fatalf("first send after recovery = %#v, %t", next, ok)
	}
}

func TestReliableRejectsInconsistentFragmentWithoutACK(t *testing.T) {
	r := newReliableTransport()
	now := time.Unix(675, 0)
	r.receive(reliableData(10, true, 1, 1, 0, 2, []byte("a")), now)
	if _, ok := nextReliableACK(r); !ok {
		t.Fatal("retained fragment was not acknowledged")
	}
	r.receive(reliableData(10, true, 2, 1, 1, 3, []byte("b")), now)
	ack, ok := nextReliableACK(r)
	if !ok || ack.AckBase != 1 || protocol.AckContains(ack.AckBase, ack.AckBitmap, 2) {
		t.Fatalf("inconsistent fragment was acknowledged: %#v, %t", ack, ok)
	}
}

func TestReliableChannelModeCannotChange(t *testing.T) {
	r := newReliableTransport()
	if err := r.queue(7, true, []byte("local")); err != nil {
		t.Fatal(err)
	}
	if err := r.queue(7, false, []byte("mismatch")); err == nil {
		t.Fatal("queue accepted a local mode change")
	}
	if delivered := r.receive(reliableData(7, false, 1, 1, 0, 1, []byte("remote")), time.Unix(700, 0)); len(delivered) != 0 {
		t.Fatalf("mismatched packet delivered %d messages", len(delivered))
	}
	if _, ok := nextReliableACK(r); ok {
		t.Fatal("mismatched packet was acknowledged")
	}

	receiver := newReliableTransport()
	if delivered := receiver.receive(reliableData(8, false, 1, 1, 0, 1, []byte("remote")), time.Unix(700, 0)); len(delivered) != 1 {
		t.Fatalf("remote delivery count = %d", len(delivered))
	}
	if err := receiver.queue(8, true, []byte("local")); err == nil {
		t.Fatal("queue accepted a remotely established mode change")
	}
}

func TestReliableQueueRejectionDoesNotCommitState(t *testing.T) {
	network := newFakeRoomNetwork()
	client := testClient(t, func() roomNetwork { return network })
	client.roomNetwork = network
	remote := testRemoteIdentity(t)
	path, err := NewPath(netip.MustParseAddrPort("192.0.2.80:9000"))
	if err != nil {
		t.Fatal(err)
	}
	session := testPeeringSession(t, path)
	session.authenticated = true
	client.remotePeers[remote.PeerID()] = &RemotePeer{identity: remote, session: session}
	transport := session.reliable
	if err := transport.queue(3, false, []byte("new")); err != nil {
		t.Fatal(err)
	}
	now := time.Unix(800, 0)
	transport.receive(reliableData(3, false, 1, 1, 0, 1, []byte("ack")), now)
	fragment := transport.outbound[0]
	network.enqueueErrors[path.Address()] = errors.New("queue full")

	client.sendReliable(now)
	if fragment.transmissions != 0 || !fragment.sentAt.IsZero() || transport.bytesInFlight != 0 || !transport.channels[3].ackDirty ||
		transport.cwnd != initialReliableCwnd || transport.rto != initialReliableRTO || fragment.timeoutPending {
		t.Fatalf("rejected first send changed state: fragment=%#v inFlight=%d cwnd=%d rto=%v dirty=%t",
			fragment, transport.bytesInFlight, transport.cwnd, transport.rto, transport.channels[3].ackDirty)
	}

	delete(network.enqueueErrors, path.Address())
	client.sendReliable(now)
	if fragment.transmissions != 1 || !fragment.sentAt.Equal(now) || transport.bytesInFlight != len(fragment.payload) || transport.channels[3].ackDirty {
		t.Fatalf("admitted first send was not committed: fragment=%#v inFlight=%d dirty=%t", fragment, transport.bytesInFlight, transport.channels[3].ackDirty)
	}

	transport.cwnd = 8 * reliableMSS
	transport.ssthresh = maximumReliableCwnd
	transport.rto = 400 * time.Millisecond
	timeoutAt := now.Add(transport.rto)
	network.enqueueErrors[path.Address()] = errors.New("queue full")
	client.sendReliable(timeoutAt)
	if fragment.transmissions != 1 || !fragment.sentAt.Equal(now) || transport.cwnd != 8*reliableMSS ||
		transport.ssthresh != maximumReliableCwnd || transport.rto != 400*time.Millisecond || fragment.timeoutPending {
		t.Fatalf("rejected retransmit changed state: fragment=%#v cwnd=%d ssthresh=%d rto=%v",
			fragment, transport.cwnd, transport.ssthresh, transport.rto)
	}

	delete(network.enqueueErrors, path.Address())
	client.sendReliable(timeoutAt)
	if fragment.transmissions != 2 || !fragment.sentAt.Equal(timeoutAt) || transport.cwnd != minimumReliableCwnd ||
		transport.ssthresh != 4*reliableMSS || transport.rto != 800*time.Millisecond || fragment.timeoutPending {
		t.Fatalf("admitted retransmit was not committed: fragment=%#v cwnd=%d ssthresh=%d rto=%v",
			fragment, transport.cwnd, transport.ssthresh, transport.rto)
	}

	transport.receive(reliableData(4, false, 1, 1, 0, 1, []byte("ack only")), timeoutAt)
	network.enqueueErrors[path.Address()] = errors.New("queue full")
	ackCursor := transport.ackCursor
	client.sendReliable(timeoutAt.Add(time.Millisecond))
	if !transport.channels[4].ackDirty || transport.ackCursor != ackCursor {
		t.Fatalf("rejected ACK changed state: dirty=%t cursor=%d, want %d", transport.channels[4].ackDirty, transport.ackCursor, ackCursor)
	}
}

func TestReliableDiscardChannelUpdatesAccounting(t *testing.T) {
	r := newReliableTransport()
	if err := r.queue(7, false, bytes.Repeat([]byte{1}, reliableMSS+1)); err != nil {
		t.Fatal(err)
	}
	if err := r.queue(8, false, []byte("keep")); err != nil {
		t.Fatal(err)
	}
	if _, ok := nextReliablePacket(r, time.Unix(850, 0)); !ok {
		t.Fatal("nextPacket() returned no packet")
	}
	r.receive(reliableData(7, false, 1, 1, 0, 2, []byte("partial")), time.Unix(850, 0))

	r.discardOutboundChannel(7)
	r.discardInboundChannel(7)
	if r.queuedBytes != len("keep") || r.bytesInFlight != 0 || r.reassemblyBytes != 0 || r.pendingChannel(7) {
		t.Fatalf("discard accounting queued=%d inFlight=%d reassembly=%d", r.queuedBytes, r.bytesInFlight, r.reassemblyBytes)
	}
	if !r.pendingChannel(8) {
		t.Fatal("discard removed another channel")
	}
}

func TestReliableReassemblyMetadataIsBoundedAndRetainedAfterACK(t *testing.T) {
	r := newReliableTransport()
	now := time.Unix(900, 0)
	for index := 0; index < maxReliableAssemblies+100; index++ {
		r.receive(reliableData(7, false, uint64(index+1), uint64(index+1), 0, 2, []byte{1}), now)
	}
	if r.reassemblyCount != maxReliableAssemblies || r.reassemblyParts != maxReliableAssemblies {
		t.Fatalf("reassembly metadata count=%d parts=%d", r.reassemblyCount, r.reassemblyParts)
	}
}

func TestReliableSchedulerIsDeterministicallyFair(t *testing.T) {
	network := newFakeRoomNetwork()
	client := testClient(t, func() roomNetwork { return network })
	client.roomNetwork = network
	busyIdentity := testRemoteIdentity(t)
	otherIdentity := testRemoteIdentity(t)
	if busyIdentity.PeerID() > otherIdentity.PeerID() {
		busyIdentity, otherIdentity = otherIdentity, busyIdentity
	}
	busyPath, _ := NewPath(netip.MustParseAddrPort("192.0.2.81:9000"))
	otherPath, _ := NewPath(netip.MustParseAddrPort("192.0.2.82:9000"))
	busy := testPeeringSession(t, busyPath)
	other := testPeeringSession(t, otherPath)
	busy.authenticated = true
	other.authenticated = true
	client.remotePeers[busyIdentity.PeerID()] = &RemotePeer{identity: busyIdentity, session: busy}
	client.remotePeers[otherIdentity.PeerID()] = &RemotePeer{identity: otherIdentity, session: other}
	for index := range maxReliablePacketsPerTick + 8 {
		if err := busy.reliable.queue(3, false, []byte{byte(index)}); err != nil {
			t.Fatal(err)
		}
	}
	if err := other.reliable.queue(3, false, []byte("other")); err != nil {
		t.Fatal(err)
	}

	client.sendReliable(time.Unix(900, 0))
	sent := network.sentAddresses()
	if len(sent) != maxReliablePacketsPerTick || sent[0] != busyPath.Address() || sent[1] != otherPath.Address() {
		t.Fatalf("first scheduling round destinations = %v", sent)
	}
	for index, destination := range sent[2:] {
		if destination != busyPath.Address() {
			t.Fatalf("destination %d = %s, want busy peer %s", index+2, destination, busyPath.Address())
		}
	}
}

func TestReliableSchedulerRotatesChannelsWithinPeer(t *testing.T) {
	r := newReliableTransport()
	if err := r.queue(reliableChannelFileData, false, bytes.Repeat([]byte{1}, 3*reliableMSS)); err != nil {
		t.Fatal(err)
	}
	if err := r.queue(reliableChannelMemberState, false, []byte("state")); err != nil {
		t.Fatal(err)
	}
	now := time.Unix(925, 0)
	first, ok := nextReliablePacket(r, now)
	if !ok || first.Channel != reliableChannelFileData {
		t.Fatalf("first packet = %#v, %t", first, ok)
	}
	second, ok := nextReliablePacket(r, now)
	if !ok || second.Channel != reliableChannelMemberState {
		t.Fatalf("second packet = %#v, %t; want member state", second, ok)
	}
}

func TestReliableACKsCannotStarveData(t *testing.T) {
	network := newFakeRoomNetwork()
	client := testClient(t, func() roomNetwork { return network })
	client.roomNetwork = network
	remoteIdentity := testRemoteIdentity(t)
	path, _ := NewPath(netip.MustParseAddrPort("192.0.2.87:9000"))
	session := testPeeringSession(t, path)
	session.authenticated = true
	client.remotePeers[remoteIdentity.PeerID()] = &RemotePeer{identity: remoteIdentity, session: session}
	now := time.Unix(930, 0)
	for channel := uint16(1); channel <= maxReliableChannels; channel++ {
		session.reliable.receive(reliableData(channel, false, 1, 1, 0, 1, []byte{1}), now)
	}
	if err := session.reliable.queue(1, false, []byte("outgoing")); err != nil {
		t.Fatal(err)
	}
	fragment := session.reliable.outbound[0]

	client.sendReliable(now)
	if fragment.transmissions != 1 {
		t.Fatal("ACK-only traffic starved queued data")
	}
}

func TestRejectedReliableDataDoesNotPinACKScheduling(t *testing.T) {
	r := newReliableTransport()
	now := time.Unix(940, 0)
	r.receive(reliableData(1, false, 1, 1, 0, 1, []byte("first")), now)
	ack, reservation, ok := r.nextSend(now)
	if !ok || !ack.AckOnly() {
		t.Fatalf("first packet = %#v, %t; want ACK", ack, ok)
	}
	r.commit(reservation)
	r.receive(reliableData(1, false, 2, 2, 0, 1, []byte("second")), now)
	if err := r.queue(reliableChannelFileData, false, []byte("file")); err != nil {
		t.Fatal(err)
	}
	data, reservation, ok := r.nextSend(now)
	if !ok || data.AckOnly() {
		t.Fatalf("second packet = %#v, %t; want data", data, ok)
	}
	r.reject(reservation)
	next, _, ok := r.nextSend(now)
	if !ok || !next.AckOnly() {
		t.Fatalf("packet after rejection = %#v, %t; want ACK", next, ok)
	}
}

func TestReliableSchedulerContinuesAfterPeerEnqueueFailure(t *testing.T) {
	network := newFakeRoomNetwork()
	client := testClient(t, func() roomNetwork { return network })
	client.roomNetwork = network
	failedIdentity := testRemoteIdentity(t)
	servedIdentity := testRemoteIdentity(t)
	if failedIdentity.PeerID() > servedIdentity.PeerID() {
		failedIdentity, servedIdentity = servedIdentity, failedIdentity
	}
	failedPath, _ := NewPath(netip.MustParseAddrPort("192.0.2.83:9000"))
	servedPath, _ := NewPath(netip.MustParseAddrPort("192.0.2.84:9000"))
	failed := testPeeringSession(t, failedPath)
	served := testPeeringSession(t, servedPath)
	failed.authenticated = true
	served.authenticated = true
	client.remotePeers[failedIdentity.PeerID()] = &RemotePeer{identity: failedIdentity, session: failed}
	client.remotePeers[servedIdentity.PeerID()] = &RemotePeer{identity: servedIdentity, session: served}
	if err := failed.reliable.queue(3, false, []byte("failed")); err != nil {
		t.Fatal(err)
	}
	if err := served.reliable.queue(3, false, []byte("served")); err != nil {
		t.Fatal(err)
	}
	network.enqueueErrors[failedPath.Address()] = errors.New("queue full")

	client.sendReliable(time.Unix(950, 0))
	sent := network.sentAddresses()
	if len(sent) != 1 || sent[0] != servedPath.Address() || failed.reliable.outbound[0].transmissions != 0 || served.reliable.outbound[0].transmissions != 1 {
		t.Fatalf("failure isolation sends=%v failed transmissions=%d served transmissions=%d",
			sent, failed.reliable.outbound[0].transmissions, served.reliable.outbound[0].transmissions)
	}
}

func TestFileDataReliableUsesBackgroundLane(t *testing.T) {
	network := newFakeRoomNetwork()
	client := testClient(t, func() roomNetwork { return network })
	client.roomNetwork = network
	remoteIdentity := testRemoteIdentity(t)
	path, _ := NewPath(netip.MustParseAddrPort("192.0.2.85:9000"))
	session := testPeeringSession(t, path)
	session.authenticated = true
	client.remotePeers[remoteIdentity.PeerID()] = &RemotePeer{identity: remoteIdentity, session: session}
	if err := session.reliable.queue(reliableChannelFileData, false, []byte("file data")); err != nil {
		t.Fatal(err)
	}

	client.sendReliable(time.Unix(960, 0))
	if len(network.background) != 1 || len(network.sentPackets) != 1 {
		t.Fatalf("background packets = %d, all packets = %d", len(network.background), len(network.sentPackets))
	}
}

func TestFileDataACKUsesControlLaneBeforeNewData(t *testing.T) {
	network := newFakeRoomNetwork()
	client := testClient(t, func() roomNetwork { return network })
	client.roomNetwork = network
	remoteIdentity := testRemoteIdentity(t)
	path, _ := NewPath(netip.MustParseAddrPort("192.0.2.86:9000"))
	session := testPeeringSession(t, path)
	session.authenticated = true
	client.remotePeers[remoteIdentity.PeerID()] = &RemotePeer{identity: remoteIdentity, session: session}
	now := time.Unix(970, 0)
	session.reliable.receive(reliableData(reliableChannelFileData, false, 1, 1, 0, 1, []byte("incoming")), now)
	if err := session.reliable.queue(reliableChannelFileData, false, []byte("outgoing")); err != nil {
		t.Fatal(err)
	}

	client.sendReliable(now)
	if len(network.sentPackets) != 2 || len(network.background) != 1 {
		t.Fatalf("all packets = %d, background packets = %d", len(network.sentPackets), len(network.background))
	}
	header, err := protocol.ParseEstablishedHeader(network.sentPackets[0])
	if err != nil {
		t.Fatal(err)
	}
	first, err := protocol.ParseReliable(network.sentPackets[0], client.roomTag, header.SessionID, session.ciphers.ControlSend)
	if err != nil || !first.AckOnly() {
		t.Fatalf("first packet = %#v, %v; want ACK-only", first, err)
	}
}

func reliableData(channel uint16, ordered bool, fragmentSequence, messageSequence uint64, fragmentIndex, fragmentCount uint16, payload []byte) protocol.ReliablePacket {
	flags := byte(0)
	if ordered {
		flags = protocol.ReliableFlagOrdered
	}
	return protocol.ReliablePacket{
		Channel:          channel,
		Flags:            flags,
		FragmentSequence: fragmentSequence,
		MessageSequence:  messageSequence,
		FragmentIndex:    fragmentIndex,
		FragmentCount:    fragmentCount,
		Payload:          payload,
	}
}

func nextReliablePacket(r *reliableTransport, now time.Time) (protocol.ReliablePacket, bool) {
	packet, reservation, ok := r.nextPacket(now)
	if ok {
		r.commit(reservation)
	}
	return packet, ok
}

func nextReliableACK(r *reliableTransport) (protocol.ReliablePacket, bool) {
	packet, reservation, ok := r.nextAck()
	if ok {
		r.commit(reservation)
	}
	return packet, ok
}
