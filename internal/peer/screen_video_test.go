package peer

import (
	"bytes"
	"encoding/binary"
	"io"
	"log/slog"
	"net/netip"
	"testing"
	"time"

	"bork/internal/invite"
	"bork/internal/networking/endpoint"
	"bork/internal/protocol"
)

func TestScreenStateVideoCodecIsExactAndCanonical(t *testing.T) {
	active := screenState{
		generation: 42, active: true, streamID: [16]byte{9},
		codec: ScreenVideoCodecH264Baseline, width: 1280, height: 720,
	}
	payload, err := encodeScreenState(active)
	if err != nil {
		t.Fatal(err)
	}
	decoded, err := decodeScreenState(payload)
	if err != nil || decoded != active || len(payload) != screenStateSize || payload[0] != 1 || payload[9] != 1 || payload[10] != 1 || binary.BigEndian.Uint16(payload[11:13]) != 1280 || binary.BigEndian.Uint16(payload[13:15]) != 720 {
		t.Fatalf("decoded state = %#v, bytes=%d, err=%v", decoded, len(payload), err)
	}
	mainState := active
	mainState.codec = ScreenVideoCodecH264Main
	mainState.generation++
	mainPayload, err := encodeScreenState(mainState)
	if err != nil || mainPayload[10] != 2 {
		t.Fatalf("main state codec = %d, %v", mainPayload[10], err)
	}
	inactive, err := encodeScreenState(screenState{generation: 44})
	if err != nil {
		t.Fatal(err)
	}

	badVersion := append([]byte(nil), payload...)
	badVersion[0]++
	zeroGeneration := append([]byte(nil), payload...)
	clear(zeroGeneration[1:9])
	badActive := append([]byte(nil), payload...)
	badActive[9] = 2
	badCodec := append([]byte(nil), payload...)
	badCodec[10] = 3
	oddWidth := append([]byte(nil), payload...)
	binary.BigEndian.PutUint16(oddWidth[11:13], 1279)
	activeWithoutStream := append([]byte(nil), payload...)
	clear(activeWithoutStream[15:])
	inactiveWithCodec := append([]byte(nil), inactive...)
	inactiveWithCodec[10] = 1
	inactiveWithDimensions := append([]byte(nil), inactive...)
	binary.BigEndian.PutUint16(inactiveWithDimensions[11:13], 2)
	inactiveWithStream := append([]byte(nil), inactive...)
	inactiveWithStream[15] = 1
	for name, malformed := range map[string][]byte{
		"short": payload[:screenStateSize-1], "trailing": append(append([]byte(nil), payload...), 0),
		"version": badVersion, "generation": zeroGeneration, "active byte": badActive,
		"codec": badCodec, "odd width": oddWidth, "active without stream": activeWithoutStream,
		"inactive codec": inactiveWithCodec, "inactive dimensions": inactiveWithDimensions, "inactive stream": inactiveWithStream,
	} {
		t.Run(name, func(t *testing.T) {
			if _, err := decodeScreenState(malformed); err == nil {
				t.Fatal("malformed screen state was accepted")
			}
		})
	}
	if _, err := encodeScreenState(screenState{generation: 1, active: true, streamID: [16]byte{1}, codec: "avc1.42e01f", width: 1280, height: 720}); err == nil {
		t.Fatal("non-canonical codec was accepted")
	}
}

func TestScreenStateGenerationAndStartResetAreSessionScoped(t *testing.T) {
	client := testClient(t, func() roomNetwork { return newFakeRoomNetwork() })
	client.stateChanges = make(chan struct{}, 8)
	remoteIdentity := testRemoteIdentity(t)
	path, err := NewPath(netip.MustParseAddrPort("192.0.2.100:9000"))
	if err != nil {
		t.Fatal(err)
	}
	session := testPeeringSession(t, path)
	session.authenticated = true
	remote := &RemotePeer{identity: remoteIdentity, session: session}
	client.remotePeers[remoteIdentity.PeerID()] = remote
	message := func(generation uint64, codec string, stream [16]byte) []byte {
		state := screenState{generation: generation}
		if stream != ([16]byte{}) {
			state.active, state.streamID, state.codec, state.width, state.height = true, stream, codec, 1280, 720
		}
		payload, encodeErr := encodeScreenState(state)
		if encodeErr != nil {
			t.Fatal(encodeErr)
		}
		return payload
	}

	client.handleScreenState(remote, message(2, ScreenVideoCodecH264Baseline, [16]byte{1}))
	client.handleScreenState(remote, message(1, "", [16]byte{}))
	client.handleScreenState(remote, message(2, ScreenVideoCodecH264Main, [16]byte{2}))
	if session.remoteScreenState.streamID != ([16]byte{1}) || session.remoteScreenState.codec != ScreenVideoCodecH264Baseline {
		t.Fatal("stale or duplicate state replaced the current state")
	}
	replacement := testPeeringSession(t, path)
	replacement.authenticated = true
	remote.session = replacement
	client.handleScreenState(remote, message(1, ScreenVideoCodecH264Main, [16]byte{3}))
	if replacement.remoteScreenState.generation != 1 || replacement.remoteScreenState.streamID != ([16]byte{3}) || replacement.remoteScreenState.codec != ScreenVideoCodecH264Main {
		t.Fatalf("replacement session state = %#v", replacement.remoteScreenState)
	}

	client.screenVideoSendSequence = 99
	client.screenVideoChunkID = 12
	if err := client.startScreenShare(ScreenVideoCodecH264Baseline, 1280, 720); err != nil {
		t.Fatal(err)
	}
	firstStream := client.localScreenState.streamID
	if firstStream == ([16]byte{}) || client.screenVideoSendSequence != 0 || client.screenVideoChunkID != 0 || !client.localScreenState.active || client.localScreenState.codec != ScreenVideoCodecH264Baseline {
		t.Fatal("screen start did not create and reset its video stream")
	}
	client.screenVideoSendSequence = 9
	client.screenVideoChunkID = 8
	if err := client.stopScreenShare(); err != nil {
		t.Fatal(err)
	}
	if err := client.startScreenShare(ScreenVideoCodecH264Main, 640, 360); err != nil {
		t.Fatal(err)
	}
	if client.localScreenState.streamID == firstStream || client.screenVideoSendSequence != 0 || client.screenVideoChunkID != 0 || client.localScreenState.codec != ScreenVideoCodecH264Main || client.localScreenState.width != 640 {
		t.Fatal("a new screen start reused stream, sequence, or configuration")
	}
	snapshot, _ := client.StateSnapshot()
	if !snapshot.ScreenSharing || len(snapshot.RemotePeers) != 1 || !snapshot.RemotePeers[0].ScreenSharing {
		t.Fatalf("screen sharing snapshot = %#v", snapshot)
	}
}

func TestScreenStateQueueRetriesAdmissionUnordered(t *testing.T) {
	client := testClient(t, func() roomNetwork { return newFakeRoomNetwork() })
	remoteIdentity := testRemoteIdentity(t)
	session := testAuthenticatedSession(t, "192.0.2.101:9000")
	client.remotePeers[remoteIdentity.PeerID()] = &RemotePeer{identity: remoteIdentity, session: session}
	session.reliable.queuedBytes = maxQueuedReliableBytes
	client.queueScreenStates()
	if session.screenStateSentGeneration != 0 || len(session.reliable.outbound) != 0 {
		t.Fatal("rejected screen state admission was committed")
	}
	session.reliable.queuedBytes = 0
	client.queueScreenStates()
	if session.screenStateSentGeneration != 1 || len(session.reliable.outbound) != 1 || session.reliable.channels[reliableChannelScreenState].ordered {
		t.Fatal("screen state was not retried on unordered channel 4")
	}
}

func TestScreenVideoCommandsBeforeLoopFail(t *testing.T) {
	client, _, _ := newVirtualLANUnitClient(t)
	if err := client.StartScreenShare(ScreenVideoCodecH264Baseline, 640, 360); err == nil {
		t.Fatal("StartScreenShare() before Loop succeeded")
	}
	if _, err := client.SendScreenVideoChunk(0, 66_667, true, []byte{1}); err == nil {
		t.Fatal("SendScreenVideoChunk() before Loop succeeded")
	}
	if err := client.StopScreenShare(); err == nil {
		t.Fatal("StopScreenShare() before Loop succeeded")
	}
}

func TestScreenVideoFragmentCodecIsStrictAndBounded(t *testing.T) {
	metadata := screenVideoTestMetadata()
	data := bytes.Repeat([]byte{7}, MaxScreenVideoChunkBytes)
	fragments, err := encodeScreenVideoFragments(metadata, data)
	if err != nil {
		t.Fatal(err)
	}
	if len(fragments) != maxScreenVideoFragments || len(fragments) <= 255 {
		t.Fatalf("fragment count = %d, want %d", len(fragments), maxScreenVideoFragments)
	}
	joined := make([]byte, 0, len(data))
	for index, payload := range fragments {
		decoded, err := decodeScreenVideoFragment(payload)
		if err != nil || int(decoded.index) != index || int(decoded.count) != len(fragments) || decoded.metadata != metadata || decoded.totalSize != MaxScreenVideoChunkBytes || len(decoded.bytes) == 0 || len(decoded.bytes) > maxScreenVideoFragmentBytes {
			t.Fatalf("fragment %d = %#v, err=%v", index, decoded, err)
		}
		joined = append(joined, decoded.bytes...)
	}
	if !bytes.Equal(joined, data) {
		t.Fatal("fragment round trip changed encoded video bytes")
	}
	if _, err := encodeScreenVideoFragments(metadata, make([]byte, MaxScreenVideoChunkBytes+1)); err == nil {
		t.Fatal("oversized video chunk was fragmented")
	}
	nonCanonical := metadata
	nonCanonical.codec = "avc1.42e01f"
	if _, err := encodeScreenVideoFragments(nonCanonical, []byte{1}); err == nil {
		t.Fatal("non-canonical codec was fragmented")
	}

	valid, err := encodeScreenVideoFragments(metadata, []byte{1})
	if err != nil {
		t.Fatal(err)
	}
	badVersion := append([]byte(nil), valid[0]...)
	badVersion[0]++
	badFlags := append([]byte(nil), valid[0]...)
	badFlags[1] |= 2
	badCodec := append([]byte(nil), valid[0]...)
	badCodec[2] = 3
	badIndex := append([]byte(nil), valid[0]...)
	binary.BigEndian.PutUint16(badIndex[3:5], 1)
	badCount := append([]byte(nil), valid[0]...)
	binary.BigEndian.PutUint16(badCount[5:7], 2)
	zeroGeneration := append([]byte(nil), valid[0]...)
	clear(zeroGeneration[7:15])
	zeroDuration := append([]byte(nil), valid[0]...)
	clear(zeroDuration[23:27])
	badTotal := append([]byte(nil), valid[0]...)
	binary.BigEndian.PutUint32(badTotal[27:31], 2)
	oddWidth := append([]byte(nil), valid[0]...)
	binary.BigEndian.PutUint16(oddWidth[31:33], 1279)
	for name, malformed := range map[string][]byte{
		"empty data": valid[0][:screenVideoFragmentHeaderSize], "version": badVersion, "flags": badFlags,
		"codec": badCodec, "index": badIndex, "count": badCount, "generation": zeroGeneration,
		"duration": zeroDuration, "total size": badTotal, "dimensions": oddWidth,
		"trailing": append(append([]byte(nil), valid[0]...), 0),
	} {
		t.Run(name, func(t *testing.T) {
			if _, err := decodeScreenVideoFragment(malformed); err == nil {
				t.Fatal("malformed video fragment was accepted")
			}
		})
	}
}

func TestScreenVideoReassemblyHandlesLossReorderingAndTTL(t *testing.T) {
	metadata := screenVideoTestMetadata()
	data := bytes.Repeat([]byte{0x5a}, maxScreenVideoFragmentBytes*2+17)
	fragments, err := encodeScreenVideoFragments(metadata, data)
	if err != nil || len(fragments) != 3 {
		t.Fatalf("test fragments = %d, err=%v", len(fragments), err)
	}
	client := screenVideoReassemblyClient()
	key := groupStreamKey{sender: [32]byte{1}, stream: [16]byte{2}, class: protocol.TrafficInteractive}
	startedAt := time.Unix(100, 0)

	first, _ := decodeScreenVideoFragment(fragments[0])
	if complete := client.acceptScreenVideoFragment(key, 1, first, []byte("old-0"), startedAt); complete != nil {
		t.Fatal("incomplete chunk was delivered")
	}
	for index := len(fragments) - 1; index >= 0; index-- {
		fragment, _ := decodeScreenVideoFragment(fragments[index])
		complete := client.acceptScreenVideoFragment(key, 2, fragment, []byte{byte(index)}, startedAt.Add(time.Millisecond))
		if index != 0 && complete != nil {
			t.Fatal("reordered chunk completed too early")
		}
		if index == 0 && (complete == nil || complete.metadata != metadata || !bytes.Equal(complete.bytes, data) || len(complete.packets) != len(fragments)) {
			t.Fatalf("reordered completion = %#v", complete)
		}
	}
	if client.screenVideoRetainedBytes != 0 {
		t.Fatalf("completed chunk retained %d bytes", client.screenVideoRetainedBytes)
	}
	if complete := client.acceptScreenVideoFragment(key, 1, first, []byte("late"), startedAt.Add(time.Millisecond)); complete != nil {
		t.Fatal("older lost chunk replaced a completed newer chunk")
	}

	reorderedKey := groupStreamKey{sender: [32]byte{2}, stream: [16]byte{3}, class: protocol.TrafficInteractive}
	middle, _ := decodeScreenVideoFragment(fragments[1])
	last, _ := decodeScreenVideoFragment(fragments[len(fragments)-1])
	client.acceptScreenVideoFragment(reorderedKey, 1, first, []byte("one-0"), startedAt)
	client.acceptScreenVideoFragment(reorderedKey, 1, middle, []byte("one-1"), startedAt)
	client.acceptScreenVideoFragment(reorderedKey, 2, first, []byte("two-0"), startedAt.Add(time.Millisecond))
	if complete := client.acceptScreenVideoFragment(reorderedKey, 1, last, []byte("one-2"), startedAt.Add(2*time.Millisecond)); complete == nil || !bytes.Equal(complete.bytes, data) {
		t.Fatal("next-frame reordering discarded a recoverable chunk")
	}

	lossKey := groupStreamKey{sender: [32]byte{3}, stream: [16]byte{4}, class: protocol.TrafficInteractive}
	client.acceptScreenVideoFragment(lossKey, 1, first, []byte("lost"), startedAt)
	client.acceptScreenVideoFragment(lossKey, 2, first, []byte("new"), startedAt.Add(time.Millisecond))
	if complete := client.acceptScreenVideoFragment(lossKey, 1, last, []byte("late"), startedAt.Add(2*time.Millisecond)); complete != nil {
		t.Fatal("lost older chunk completed after a newer chunk arrived")
	}

	ttlKey := groupStreamKey{sender: [32]byte{5}, stream: [16]byte{6}, class: protocol.TrafficInteractive}
	client.acceptScreenVideoFragment(ttlKey, 1, first, []byte("ttl"), startedAt)
	if complete := client.acceptScreenVideoFragment(ttlKey, 1, last, []byte("late"), startedAt.Add(screenVideoChunkTTL)); complete != nil {
		t.Fatal("expired assembly completed")
	}
	client.expireScreenVideoChunks(startedAt.Add(screenVideoChunkTTL + time.Second))
	if client.screenVideoRetainedBytes != 0 {
		t.Fatalf("expired assemblies retained %d bytes", client.screenVideoRetainedBytes)
	}
}

func TestScreenVideoReassemblyBoundsMemoryAndRejectsMetadataChanges(t *testing.T) {
	metadata := screenVideoTestMetadata()
	data := bytes.Repeat([]byte{1}, MaxScreenVideoChunkBytes)
	payloads, err := encodeScreenVideoFragments(metadata, data)
	if err != nil {
		t.Fatal(err)
	}
	first, _ := decodeScreenVideoFragment(payloads[0])
	client := screenVideoReassemblyClient()
	now := time.Unix(200, 0)
	for index := range 2500 {
		var sender [32]byte
		binary.BigEndian.PutUint32(sender[:4], uint32(index+1))
		key := groupStreamKey{sender: sender, stream: [16]byte{1}, class: protocol.TrafficInteractive}
		client.acceptScreenVideoFragment(key, 1, first, make([]byte, protocol.MaxDatagramSize), now.Add(time.Duration(index)*time.Nanosecond))
		if client.screenVideoRetainedBytes > maxScreenVideoRetainedBytes {
			t.Fatalf("retained bytes exceeded bound: %d", client.screenVideoRetainedBytes)
		}
	}

	mismatchClient := screenVideoReassemblyClient()
	mismatchKey := groupStreamKey{sender: [32]byte{9}, stream: [16]byte{9}, class: protocol.TrafficInteractive}
	mismatchClient.acceptScreenVideoFragment(mismatchKey, 1, first, []byte("first"), now)
	second, _ := decodeScreenVideoFragment(payloads[1])
	second.metadata.keyFrame = !second.metadata.keyFrame
	if complete := mismatchClient.acceptScreenVideoFragment(mismatchKey, 1, second, []byte("second"), now); complete != nil || mismatchClient.screenVideoRetainedBytes != 0 {
		t.Fatal("inconsistent chunk metadata was retained or completed")
	}

	client.expireScreenVideoChunks(now.Add(screenVideoChunkTTL + time.Second))
	if client.screenVideoRetainedBytes != 0 {
		t.Fatalf("cleanup retained %d bytes", client.screenVideoRetainedBytes)
	}
}

func TestScreenVideoDatagramsRequireMatchingStateAndEnforceReplayAndRate(t *testing.T) {
	room, err := invite.New("screen validation")
	if err != nil {
		t.Fatal(err)
	}
	local := testLocalIdentity(t)
	remote := testLocalIdentity(t)
	client := newClient(local, room, func() roomNetwork { return newFakeRoomNetwork() }, slog.Default())
	source := netip.MustParseAddrPort("192.0.2.109:9000")
	stream := [16]byte{7}
	session := testAuthenticatedSession(t, source.String())
	session.remoteScreenState = screenState{
		generation: 3, active: true, streamID: stream,
		codec: ScreenVideoCodecH264Baseline, width: 1280, height: 720,
	}
	client.remotePeers[remote.PeerID()] = &RemotePeer{identity: remote.Identity, session: session}
	now := time.Unix(300, 0)

	packet := func(sequence uint64, chunkID uint32, metadata screenVideoMetadata) []byte {
		payloads, encodeErr := encodeScreenVideoFragments(metadata, []byte{0, 0, 0, 1, 0x65})
		if encodeErr != nil {
			t.Fatal(encodeErr)
		}
		header := protocol.GroupDatagramHeader{
			Class: protocol.TrafficInteractive, SenderID: rawPeerIdentity(remote.Identity), StreamID: stream, Sequence: sequence,
		}
		encoded, marshalErr := protocol.MarshalGroupDatagram(client.roomTag, header, chunkID, payloads[0], client.groupProtector, remote)
		if marshalErr != nil {
			t.Fatal(marshalErr)
		}
		return encoded
	}

	metadata := screenVideoTestMetadata()
	metadata.generation = 3
	mismatch := metadata
	mismatch.width = 640
	client.handleGroupDatagram(endpoint.Datagram{Data: packet(1, 1, mismatch), From: source, ReceivedAt: now}, nil)
	if len(client.screenVideoChunks) != 0 || len(client.groupReceivers) != 0 {
		t.Fatal("datagram that did not match channel 4 state was retained")
	}
	valid := packet(1, 1, metadata)
	client.handleGroupDatagram(endpoint.Datagram{Data: valid, From: source, ReceivedAt: now}, nil)
	select {
	case chunk := <-client.ScreenVideoChunks():
		if chunk.Generation != 3 || chunk.StreamID != stream || chunk.ChunkID != 1 || chunk.Codec != metadata.codec || !chunk.KeyFrame || chunk.Timestamp != metadata.timestamp {
			t.Fatalf("delivered chunk = %#v", chunk)
		}
	default:
		t.Fatal("matching screen video datagram was not delivered")
	}
	client.handleGroupDatagram(endpoint.Datagram{Data: valid, From: source, ReceivedAt: now}, nil)
	if len(client.screenVideoChunks) != 0 {
		t.Fatal("replayed screen video datagram was delivered")
	}

	key := groupStreamKey{sender: rawPeerIdentity(remote.Identity), stream: stream, class: protocol.TrafficInteractive}
	client.groupReceivers[key].tokens = 0
	client.groupReceivers[key].updatedAt = now
	rateLimited := packet(2, 2, metadata)
	client.handleGroupDatagram(endpoint.Datagram{Data: rateLimited, From: source, ReceivedAt: now}, nil)
	if client.screenVideoReceivers[key].newestChunkID != 1 || len(client.screenVideoChunks) != 0 {
		t.Fatal("rate-limited screen video datagram reached reassembly")
	}
	client.handleGroupDatagram(endpoint.Datagram{Data: rateLimited, From: source, ReceivedAt: now.Add(time.Second)}, nil)
	if len(client.screenVideoChunks) != 0 {
		t.Fatal("rate-limited sequence was replayable")
	}
}

func TestScreenVideoForwardingWaitsForCompleteChunkAndBatchesDestinationMajor(t *testing.T) {
	room, err := invite.New("screen forwarding")
	if err != nil {
		t.Fatal(err)
	}
	local := testLocalIdentity(t)
	remote := testLocalIdentity(t)
	network := newFakeRoomNetwork()
	client := newClient(local, room, func() roomNetwork { return network }, slog.New(slog.NewTextHandler(io.Discard, nil)))
	client.roomNetwork = network
	source := netip.MustParseAddrPort("192.0.2.110:9000")
	stream := [16]byte{7}
	metadata := screenVideoTestMetadata()
	senderSession := testAuthenticatedSession(t, source.String())
	senderSession.remoteScreenState = screenState{
		generation: metadata.generation, active: true, streamID: stream,
		codec: metadata.codec, width: metadata.width, height: metadata.height,
	}
	senderSession.inboundFanout = fanoutAssignment{generation: 1}
	client.remotePeers[remote.PeerID()] = &RemotePeer{identity: remote.Identity, session: senderSession}
	destinations := make([]netip.AddrPort, 0, 5)
	for index := range 5 {
		peerID := string(rune('a' + index))
		address := netip.AddrPortFrom(netip.AddrFrom4([4]byte{192, 0, 2, byte(120 + index)}), 9000)
		client.remotePeers[peerID] = &RemotePeer{session: testAuthenticatedSession(t, address.String())}
		senderSession.inboundFanout.listeners = append(senderSession.inboundFanout.listeners, peerID)
		destinations = append(destinations, address)
	}

	data := bytes.Repeat([]byte{0x31}, maxScreenVideoFragmentBytes*64)
	payloads, err := encodeScreenVideoFragments(metadata, data)
	if err != nil || len(payloads) != 64 {
		t.Fatalf("payloads = %d, err=%v", len(payloads), err)
	}
	packets := make([][]byte, len(payloads))
	now := time.Now()
	for index, payload := range payloads {
		header := protocol.GroupDatagramHeader{
			Class: protocol.TrafficInteractive, SenderID: rawPeerIdentity(remote.Identity),
			StreamID: stream, Sequence: uint64(index + 1),
		}
		packets[index], err = protocol.MarshalGroupDatagram(client.roomTag, header, 1, payload, client.groupProtector, remote)
		if err != nil {
			t.Fatal(err)
		}
		if index < len(payloads)-1 {
			client.handleGroupDatagram(endpoint.Datagram{Data: packets[index], From: source, ReceivedAt: now}, nil)
		}
	}
	if len(network.realtimeBatches) != 0 || len(client.screenVideoChunks) != 0 {
		t.Fatal("incomplete chunk was forwarded or delivered")
	}
	client.handleGroupDatagram(endpoint.Datagram{Data: packets[len(packets)-1], From: source, ReceivedAt: now}, nil)
	flat := flattenRealtimeBatches(network.realtimeBatches)
	if len(flat) != len(destinations)*len(packets) {
		t.Fatalf("forwarded datagrams = %d", len(flat))
	}
	for index, datagram := range flat {
		packetIndex := index % len(packets)
		if datagram.Destination != destinations[index/len(packets)] || !bytes.Equal(datagram.Data, packets[packetIndex]) {
			t.Fatalf("datagram %d changed original ciphertext order", index)
		}
	}
	select {
	case chunk := <-client.ScreenVideoChunks():
		if chunk.PeerID != remote.PeerID() || chunk.StreamID != stream || chunk.Generation != metadata.generation || chunk.ChunkID != 1 || chunk.Codec != metadata.codec || !chunk.KeyFrame || !bytes.Equal(chunk.Bytes, data) {
			t.Fatalf("completed chunk = %#v", chunk)
		}
	default:
		t.Fatal("complete video chunk was not delivered")
	}
}

func TestScreenVideoSenderBatchesDestinationMajor(t *testing.T) {
	room, err := invite.New("screen sender")
	if err != nil {
		t.Fatal(err)
	}
	network := newFakeRoomNetwork()
	client := newClient(testLocalIdentity(t), room, func() roomNetwork { return network }, slog.Default())
	client.roomNetwork = network
	client.localScreenState = screenState{
		generation: 2, active: true, streamID: [16]byte{8},
		codec: ScreenVideoCodecH264Baseline, width: 1280, height: 720,
	}
	client.fanout = outboundFanout{generation: 1, activateAt: time.Now().Add(-time.Second), assignments: make(map[string][]string)}
	destinations := make([]netip.AddrPort, 0, 5)
	for index := range 5 {
		peerID := string(rune('k' + index))
		address := netip.AddrPortFrom(netip.AddrFrom4([4]byte{192, 0, 2, byte(140 + index)}), 9000)
		session := testAuthenticatedSession(t, address.String())
		session.screenStateSentGeneration = client.localScreenState.generation
		client.remotePeers[peerID] = &RemotePeer{session: session}
		client.fanout.destinations = append(client.fanout.destinations, peerID)
		client.fanout.assignments[peerID] = nil
		destinations = append(destinations, address)
	}
	client.fanoutDirty = false
	data := bytes.Repeat([]byte{0x41}, maxScreenVideoFragmentBytes*64)
	if sent, err := client.sendScreenVideoChunk(123, 66_667, true, data); err != nil || !sent {
		t.Fatal(err)
	}
	flat := flattenRealtimeBatches(network.realtimeBatches)
	if len(flat) != len(destinations)*64 {
		t.Fatalf("sender datagrams = %d", len(flat))
	}
	for index, datagram := range flat {
		if datagram.Destination != destinations[index/64] {
			t.Fatalf("sender datagram %d destination = %s", index, datagram.Destination)
		}
		header, err := protocol.ParseGroupDatagramHeader(datagram.Data, client.roomTag)
		if err != nil || header.Class != protocol.TrafficInteractive || header.StreamID != client.localScreenState.streamID || header.Sequence != uint64(index%64+1) {
			t.Fatalf("sender datagram %d header = %#v, %v", index, header, err)
		}
	}
}

func TestScreenVideoKeyframeWaitsForScreenStateACK(t *testing.T) {
	client, _, _ := newVirtualLANUnitClient(t)
	network := newFakeRoomNetwork()
	client.roomNetwork = network
	client.localScreenState = screenState{generation: 2, active: true, streamID: [16]byte{8}, codec: ScreenVideoCodecH264Baseline, width: 640, height: 360}
	peerID := "viewer"
	session := testAuthenticatedSession(t, "192.0.2.150:9000")
	session.screenStateSentGeneration = client.localScreenState.generation
	payload, _ := encodeScreenState(client.localScreenState)
	if err := session.reliable.queue(reliableChannelScreenState, false, payload); err != nil {
		t.Fatal(err)
	}
	client.remotePeers[peerID] = &RemotePeer{session: session}
	if sent, err := client.sendScreenVideoChunk(0, 66_667, true, []byte{0, 0, 0, 1, 0x65}); err != nil || sent {
		t.Fatalf("pending state send = %t, %v", sent, err)
	}
	session.reliable.discardOutboundChannel(reliableChannelScreenState)
	if sent, err := client.sendScreenVideoChunk(66_667, 66_667, true, []byte{0, 0, 0, 1, 0x65}); err != nil || !sent {
		t.Fatalf("ready state send = %t, %v", sent, err)
	}
}

func screenVideoTestMetadata() screenVideoMetadata {
	return screenVideoMetadata{
		generation: 7, codec: ScreenVideoCodecH264Baseline, width: 1280, height: 720,
		timestamp: 123_456, duration: 66_667, keyFrame: true,
	}
}

func screenVideoReassemblyClient() *Client {
	return &Client{
		screenVideoReceivers: make(map[groupStreamKey]*screenVideoReceiveState),
		screenVideoChunks:    make(chan ScreenVideoChunk, maxCompletedScreenVideoChunks),
	}
}

func flattenRealtimeBatches(batches []endpoint.RealtimeBatch) []endpoint.RealtimeDatagram {
	var flattened []endpoint.RealtimeDatagram
	for _, batch := range batches {
		flattened = append(flattened, batch.Datagrams...)
	}
	return flattened
}
