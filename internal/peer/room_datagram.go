package peer

import (
	"crypto/ed25519"
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
	roomDatagramStreamIdle = 30 * time.Second
	// Signed room members can create arbitrary StreamIDs; bound retained replay
	// state without imposing a member or concurrent-speaker product limit.
	maxRoomDatagramReceiveStreams = 4096
)

type roomDatagramStreamKey struct {
	sender [32]byte
	stream [16]byte
	class  protocol.TrafficClass
}

type roomDatagramReceiveState struct {
	sequenceWindow
	lastSeen time.Time
}

func (c *Client) initAudioStream() {
	previous := c.audioStreamID
	c.audioStreamID = [16]byte{}
	for c.audioStreamID == ([16]byte{}) {
		rand.Read(c.audioStreamID[:])
	}
	c.audioSendSequence = 0
	if previous != ([16]byte{}) {
		c.markTopologyDirty()
	}
}

func (c *Client) sendAudioFrame(frame media.SendFrame) {
	if len(frame.Payload) == 0 || len(frame.Payload) > protocol.MaxRoomDatagramPayload || c.roomNetwork == nil || c.roomDatagramProtector == nil {
		return
	}
	now := time.Now()
	if !frame.Deadline.IsZero() && now.After(frame.Deadline) {
		return
	}
	if c.audioStreamPendingTopology {
		c.queueTopologySnapshots(now, false)
		if !c.audioStreamTopologyReady() {
			return
		}
		c.audioStreamPendingTopology = false
	}
	c.refreshFanout(now)
	if c.audioSendSequence == math.MaxUint64 {
		c.initAudioStream()
		c.audioStreamPendingTopology = true
		c.queueTopologySnapshots(now, true)
		return
	}
	c.audioSendSequence++
	header := protocol.RoomDatagramHeader{
		Class: protocol.TrafficAudio, SenderID: c.roomDatagramSenderID,
		StreamID: c.audioStreamID, Sequence: c.audioSendSequence,
	}
	packet, err := protocol.MarshalRoomDatagram(c.roomTag, header, frame.Timestamp, frame.Payload, c.roomDatagramProtector, c.localIdentity)
	if err != nil {
		return
	}
	destinations := c.fanout.destinations
	if !c.fanoutReady(now) {
		destinations = make([]string, 0, len(c.remotePeers))
		for peerID, peer := range c.remotePeers {
			if peer.activeSession != nil && peer.activeSession.authenticated && peer.activeSession.path.IsDirect() {
				destinations = append(destinations, peerID)
			}
		}
	}
	c.sendRealtimeToPeers(protocol.TrafficAudio, packet, destinations, frame.Deadline, frame.Generation)
}

func (c *Client) handleRoomDatagram(packet endpoint.Datagram, mediaPort media.PeerPort) {
	header, err := protocol.ParseRoomDatagramHeader(packet.Data, c.roomTag)
	if err != nil || (header.Class != protocol.TrafficAudio && header.Class != protocol.TrafficInteractive) || header.SenderID == c.roomDatagramSenderID {
		return
	}
	remoteIdentity, err := identity.FromPublicKey(ed25519.PublicKey(header.SenderID[:]))
	if err != nil {
		return
	}
	remote := c.remotePeers[remoteIdentity.PeerID()]
	if remote == nil || remote.activeSession == nil || !remote.activeSession.authenticated {
		return
	}
	if !c.authenticatedDirectSource(packet.From) {
		return
	}
	if header.Class == protocol.TrafficAudio && (remote.activeSession.audioStreamID == ([16]byte{}) || remote.activeSession.audioStreamID != header.StreamID) {
		return
	}
	if header.Class == protocol.TrafficInteractive && (!remote.activeSession.remoteScreenState.active || remote.activeSession.remoteScreenState.streamID != header.StreamID) {
		return
	}
	key := roomDatagramStreamKey{sender: header.SenderID, stream: header.StreamID, class: header.Class}
	state := c.roomDatagramReceivers[key]
	newState := state == nil
	if state == nil {
		state = &roomDatagramReceiveState{}
	}
	if !state.mayAccept(header.Sequence) {
		return
	}
	decoded, err := protocol.ParseRoomDatagram(packet.Data, c.roomTag, header, c.roomDatagramProtector)
	if err != nil {
		return
	}
	var fragment decodedScreenVideoFragment
	if header.Class == protocol.TrafficInteractive {
		if decoded.Timestamp == 0 {
			return
		}
		fragment, err = decodeScreenVideoFragment(decoded.Payload)
		if err != nil || !screenVideoFragmentMatchesState(fragment, remote.activeSession.remoteScreenState) {
			return
		}
	}
	if !state.accept(header.Sequence) {
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
	state.lastSeen = now
	if header.Class == protocol.TrafficInteractive {
		complete := c.acceptScreenVideoFragment(key, decoded.Timestamp, fragment, packet.Data, now)
		if complete == nil {
			return
		}
		c.forwardScreenVideoChunk(remoteIdentity.PeerID(), packet.From, complete.packets, complete.deadline)
		c.deliverScreenVideoChunk(ScreenVideoChunk{
			PeerID: remoteIdentity.PeerID(), SessionID: remote.activeSession.sessionID, Generation: complete.metadata.generation, StreamID: header.StreamID,
			ChunkID: complete.chunkID,
			Codec:   complete.metadata.codec, Width: complete.metadata.width, Height: complete.metadata.height,
			Timestamp: complete.metadata.timestamp, Duration: complete.metadata.duration,
			KeyFrame: complete.metadata.keyFrame, Bytes: complete.bytes,
		})
		return
	}
	c.forwardRoomDatagram(remoteIdentity.PeerID(), header.Class, packet, now.Add(10*time.Millisecond))
	if mediaPort != nil {
		mediaPort.SubmitReceived(media.ReceivedFrame{
			SourceID: remoteIdentity.PeerID(), StreamID: header.StreamID,
			Sequence: header.Sequence, Timestamp: decoded.Timestamp,
			Payload: decoded.Payload, ReceivedAt: now,
		})
	}
}

func (c *Client) audioStreamTopologyReady() bool {
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

func (c *Client) authenticatedDirectSource(address netip.AddrPort) bool {
	for _, peer := range c.remotePeers {
		if peer.activeSession != nil && peer.activeSession.authenticated && peer.activeSession.path.IsDirect() && peer.activeSession.path.Address() == address {
			return true
		}
	}
	return false
}

func (c *Client) expireRoomDatagramStreams(now time.Time) {
	for key, state := range c.roomDatagramReceivers {
		if state.lastSeen.Add(roomDatagramStreamIdle).Before(now) {
			delete(c.roomDatagramReceivers, key)
			c.removeScreenVideoAssembly(key)
			delete(c.screenVideoReceivers, key)
		}
	}
}
