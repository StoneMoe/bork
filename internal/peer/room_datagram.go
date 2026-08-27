package peer

import (
	"crypto/rand"
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
	// Signed room members can create arbitrary StreamIDs; bound retained replay
	// state without imposing a member or concurrent-speaker product limit.
	maxRoomDatagramReceiveStreams = 4096
)

type roomDatagramStreamKey struct {
	sender identity.PeerID
	stream [16]byte
	class  protocol.TrafficClass
}

type roomDatagramReceiveState struct {
	sequenceWindow
	lastSeen time.Time
}

func (c *Client) initVoiceStream() {
	previous := c.voiceStreamID
	c.voiceStreamID = [16]byte{}
	for c.voiceStreamID == ([16]byte{}) {
		rand.Read(c.voiceStreamID[:])
	}
	c.voicePacketSequence = 0
	if previous != ([16]byte{}) {
		c.markTopologyDirty()
	}
}

func (c *Client) sendVoiceFrame(frame media.SendFrame) {
	if len(frame.Payload) == 0 || len(frame.Payload) > protocol.MaxRoomDatagramPayload || c.roomNetwork == nil || c.roomDatagramProtector == nil {
		return
	}
	now := time.Now()
	if !frame.Deadline.IsZero() && now.After(frame.Deadline) {
		return
	}
	if c.voiceStreamPendingTopology {
		c.queueTopologySnapshots(now, false)
		if !c.voiceStreamTopologyReady() {
			return
		}
		c.voiceStreamPendingTopology = false
	}
	c.refreshFanout(now)
	if c.voicePacketSequence == math.MaxUint64 {
		c.initVoiceStream()
		c.voiceStreamPendingTopology = true
		c.queueTopologySnapshots(now, true)
		return
	}
	c.voicePacketSequence++
	header := protocol.RoomDatagramHeader{
		Class: protocol.TrafficVoice, SenderID: c.localIdentity.PeerID,
		StreamID: c.voiceStreamID, PacketSequence: c.voicePacketSequence,
	}
	packet, err := protocol.MarshalRoomDatagram(c.roomTag, header, frame.Timestamp, frame.Payload, c.roomDatagramProtector, c.localIdentity)
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
	c.sendRealtimeToPeers(protocol.TrafficVoice, packet, destinations, frame.Deadline, frame.SendGeneration)
}

func (c *Client) handleRoomDatagram(packet endpoint.Datagram, mediaPort media.PeerPort) {
	header, err := protocol.ParseRoomDatagramHeader(packet.Data, c.roomTag)
	if err != nil || (header.Class != protocol.TrafficVoice && header.Class != protocol.TrafficScreenVideo && header.Class != protocol.TrafficScreenAudio) || header.SenderID == c.localIdentity.PeerID {
		return
	}
	remote := c.remotePeers[header.SenderID]
	if remote == nil || remote.activeSession == nil || !remote.activeSession.authenticated {
		return
	}
	sourceSession := c.authenticatedDirectSession(packet.From)
	if sourceSession == nil {
		return
	}
	if header.Class == protocol.TrafficVoice && (remote.activeSession.voiceStreamID == ([16]byte{}) || remote.activeSession.voiceStreamID != header.StreamID) {
		return
	}
	if (header.Class == protocol.TrafficScreenVideo || header.Class == protocol.TrafficScreenAudio) && (!remote.activeSession.remoteScreenState.active || remote.activeSession.remoteScreenState.streamID != header.StreamID) {
		return
	}
	key := roomDatagramStreamKey{sender: header.SenderID, stream: header.StreamID, class: header.Class}
	state := c.roomDatagramReceivers[key]
	newState := state == nil
	if state == nil {
		state = &roomDatagramReceiveState{}
	}
	if !state.mayAccept(header.PacketSequence) {
		return
	}
	decoded, err := protocol.ParseRoomDatagram(packet.Data, c.roomTag, header, c.roomDatagramProtector)
	if err != nil {
		return
	}
	var fragment decodedScreenVideoFragment
	if header.Class == protocol.TrafficScreenVideo {
		if decoded.MediaSequence == 0 {
			return
		}
		fragment, err = decodeScreenVideoFragment(decoded.Payload)
		if err != nil || !screenVideoMetadataMatchesState(fragment.metadata, remote.activeSession.remoteScreenState) {
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
	// Forwarded datagrams keep the original sender's signature, so only the
	// authenticated direct session at the UDP source address is kept alive.
	if now.After(sourceSession.lastAuthenticatedPacketAt) {
		sourceSession.lastAuthenticatedPacketAt = now
	}
	state.lastSeen = now
	if header.Class == protocol.TrafficScreenVideo {
		complete := c.acceptScreenVideoFragment(key, decoded.MediaSequence, fragment, packet.Data, now)
		if complete == nil {
			return
		}
		c.forwardScreenVideoChunk(header.SenderID, packet.From, complete.packets, complete.deadline)
		c.deliverScreenVideoChunk(ScreenVideoChunk{
			PeerID: header.SenderID, SessionID: remote.activeSession.sessionID, Generation: complete.metadata.generation, StreamID: header.StreamID,
			ChunkID: complete.chunkID,
			Codec:   complete.metadata.codec, Width: remote.activeSession.remoteScreenState.width, Height: remote.activeSession.remoteScreenState.height,
			DisplayWidth: complete.metadata.displayWidth, DisplayHeight: complete.metadata.displayHeight,
			Timestamp: complete.metadata.timestamp, Duration: complete.metadata.duration,
			KeyFrame: complete.metadata.keyFrame, Bytes: complete.bytes,
		})
		return
	}
	streamKind := media.AudioStreamVoice
	deadline := now.Add(10 * time.Millisecond)
	if header.Class == protocol.TrafficScreenAudio {
		streamKind = media.AudioStreamScreen
		deadline = now.Add(screenAudioSendBudget)
	}
	peerID := header.SenderID
	c.forwardRoomDatagram(peerID, header.Class, packet, deadline)
	if mediaPort != nil {
		mediaPort.SubmitReceived(media.ReceivedFrame{
			SourceID: peerID, StreamKind: streamKind, StreamID: header.StreamID,
			Sequence: header.PacketSequence, Timestamp: decoded.MediaSequence, Payload: decoded.Payload, ReceivedAt: now,
		})
	}
}

func (c *Client) voiceStreamTopologyReady() bool {
	for _, peer := range c.remotePeers {
		activeSession := peer.activeSession
		if activeSession == nil || !activeSession.authenticated {
			continue
		}
		if activeSession.reliable == nil || activeSession.topologySentGeneration != c.topologyGeneration || activeSession.reliable.pendingChannel(reliableChannelTopology) {
			return false
		}
	}
	return true
}

func (c *Client) authenticatedDirectSession(address netip.AddrPort) *PeeringSession {
	for _, peer := range c.remotePeers {
		if peer.activeSession != nil && peer.activeSession.authenticated && peer.activeSession.path.IsDirect() && peer.activeSession.path.Address() == address {
			return peer.activeSession
		}
	}
	return nil
}
