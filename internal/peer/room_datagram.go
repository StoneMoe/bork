package peer

import (
	"math"
	"net/netip"
	"time"

	"bork/internal/identity"
	"bork/internal/media"
	"bork/internal/networking/endpoint"
	"bork/internal/protocol"
)

const (
	// Keep replay windows for the room lifetime. Expiring one during DTX would
	// let an old packet recreate it; the fixed cap still bounds retained state.
	// Room members can create arbitrary StreamIDs; bound retained replay
	// state without imposing a member or concurrent-speaker product limit.
	maxRoomDatagramReceiveStreams = 4096
)

type roomDatagramStreamKey struct {
	sender     identity.PeerID
	stream     [16]byte
	packetType protocol.PacketType
}

type roomDatagramReceiveState struct {
	sequenceWindow
	lastSeen time.Time
}

func (c *Client) sendVoiceFrame(frame media.SendFrame) {
	if len(frame.Payload) == 0 || len(frame.Payload) > protocol.MaxRoomDatagramPayload || c.roomNetwork == nil || c.roomDatagramProtector == nil {
		return
	}
	now := time.Now()
	if !frame.Deadline.IsZero() && now.After(frame.Deadline) {
		return
	}
	c.refreshFanout(now)
	if c.voicePacketSequence == math.MaxUint64 {
		return
	}
	c.voicePacketSequence++
	header := protocol.RoomDatagramHeader{
		Type: protocol.PacketVoice, StreamID: [16]byte(c.localPeerID), PacketSequence: c.voicePacketSequence,
	}
	packet, err := protocol.MarshalRoomDatagram(header, frame.Timestamp, frame.Payload, c.roomDatagramProtector)
	if err != nil {
		return
	}
	destinations := c.fanout.destinations
	if !c.fanoutReady(now) {
		destinations = make([]identity.PeerID, 0, len(c.remotePeers))
		for peerID, peer := range c.remotePeers {
			if peer.activeSession != nil && peer.activeSession.authenticated && peer.activeSession.path.IsDirect() {
				destinations = append(destinations, peerID)
			}
		}
	}
	c.sendRealtimeToPeers(packet, destinations, frame.Deadline, frame.SendGeneration)
}

func (c *Client) handleRoomDatagram(packet endpoint.Datagram, mediaPort media.PeerPort) {
	header, err := protocol.ParseRoomDatagramHeader(packet.Data)
	if err != nil {
		return
	}
	remote := c.roomDatagramSender(header.Type, header.StreamID)
	if remote == nil || remote.activeSession == nil || !remote.activeSession.authenticated {
		return
	}
	sourceSession := c.authenticatedDirectSession(packet.From)
	if sourceSession == nil {
		return
	}
	key := roomDatagramStreamKey{sender: remote.peerID, stream: header.StreamID, packetType: header.Type}
	state := c.roomDatagramReceivers[key]
	newState := state == nil
	if state == nil {
		state = &roomDatagramReceiveState{}
	}
	if !state.mayAccept(header.PacketSequence) {
		return
	}
	decoded, err := protocol.ParseRoomDatagram(packet.Data, header, c.roomDatagramProtector)
	if err != nil {
		return
	}
	var fragment decodedScreenVideoFragment
	if header.Type == protocol.PacketScreenVideo {
		if decoded.MediaUnitID == 0 {
			return
		}
		fragment, err = decodeScreenVideoFragment(decoded.Payload)
		if err != nil || !screenVideoMetadataMatchesState(fragment.metadata, remote.screenState) {
			return
		}
	}
	if !state.accept(header.PacketSequence) {
		return
	}
	if newState {
		if len(c.roomDatagramReceivers) >= maxRoomDatagramReceiveStreams {
			var oldestKey roomDatagramStreamKey
			var oldestAt time.Time
			found := false
			for candidate, retained := range c.roomDatagramReceivers {
				if !found || retained.lastSeen.Before(oldestAt) {
					oldestKey = candidate
					oldestAt = retained.lastSeen
					found = true
				}
			}
			if !found {
				return
			}
			delete(c.roomDatagramReceivers, oldestKey)
			c.removeScreenVideoAssembly(oldestKey)
			delete(c.screenVideoReceivers, oldestKey)
		}
		c.roomDatagramReceivers[key] = state
	}
	now := packet.ReceivedAt
	if now.IsZero() {
		now = time.Now()
	}
	// Forwarded datagrams keep the original stream identity, so only the
	// authenticated direct session at the UDP source address is kept alive.
	if now.After(sourceSession.lastAuthenticatedPacketAt) {
		sourceSession.lastAuthenticatedPacketAt = now
	}
	state.lastSeen = now
	if header.Type == protocol.PacketScreenVideo {
		// Only the original sender's direct packets are fanned out. Forwarded
		// packets are still accepted for local preview but are never forwarded again.
		if remote.activeSession.path.IsDirect() && remote.activeSession.path.Address() == packet.From {
			c.queueScreenVideoForward(key, packet.Data, now.Add(screenVideoChunkTTL))
		}
		complete := c.acceptScreenVideoFragment(key, decoded.MediaUnitID, fragment, now)
		if complete == nil {
			return
		}
		c.deliverScreenVideoChunk(ScreenVideoChunk{
			PeerID: remote.peerID, StreamID: header.StreamID,
			ChunkID: complete.chunkID,
			Codec:   remote.screenState.codec, Width: remote.screenState.width, Height: remote.screenState.height,
			DisplayWidth: complete.metadata.displayWidth, DisplayHeight: complete.metadata.displayHeight,
			Timestamp: complete.metadata.timestamp, Duration: complete.metadata.duration,
			KeyFrame: complete.metadata.keyFrame, Bytes: complete.bytes,
		})
		return
	}
	streamKind := media.AudioStreamVoice
	deadline := now.Add(10 * time.Millisecond)
	if header.Type == protocol.PacketScreenAudio {
		streamKind = media.AudioStreamScreen
		deadline = now.Add(screenAudioSendBudget)
	}
	peerID := remote.peerID
	c.forwardRoomDatagram(peerID, packet, deadline)
	if mediaPort != nil {
		mediaPort.SubmitReceived(media.ReceivedFrame{
			SourceID: peerID, StreamKind: streamKind, StreamID: header.StreamID,
			Sequence: header.PacketSequence, Timestamp: decoded.MediaUnitID, Payload: decoded.Payload, ReceivedAt: now,
		})
	}
}

func (c *Client) roomDatagramSender(packetType protocol.PacketType, streamID [16]byte) *RemotePeer {
	if packetType == protocol.PacketVoice {
		return c.remotePeers[identity.PeerID(streamID)]
	}
	for _, remote := range c.remotePeers {
		if remote.screenState.active() && remote.screenState.streamID == streamID {
			return remote
		}
	}
	return nil
}

func (c *Client) authenticatedDirectSession(address netip.AddrPort) *Session {
	for _, peer := range c.remotePeers {
		if peer.activeSession != nil && peer.activeSession.authenticated && peer.activeSession.path.IsDirect() && peer.activeSession.path.Address() == address {
			return peer.activeSession
		}
	}
	return nil
}
