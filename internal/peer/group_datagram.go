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
	groupPacketsPerSecond       = 120.0
	groupPacketBurst            = 24.0
	screenVideoPacketsPerSecond = 192.0
	screenVideoPacketBurst      = float64(maxScreenVideoFragments)
	groupStreamIdle             = 30 * time.Second
	// Signed room members can create arbitrary StreamIDs; bound retained replay
	// state without imposing a member or concurrent-speaker product limit.
	maxGroupReceiveStreams = 4096
)

type groupStreamKey struct {
	sender [32]byte
	stream [16]byte
	class  protocol.TrafficClass
}

type groupReceiveState struct {
	sequenceWindow
	tokenBudget
	lastSeen time.Time
}

func (c *Client) initGroupStream() {
	previous := c.groupStreamID
	c.groupStreamID = [16]byte{}
	for c.groupStreamID == ([16]byte{}) {
		rand.Read(c.groupStreamID[:])
	}
	c.groupSendSequence = 0
	if previous != ([16]byte{}) {
		c.markTopologyDirty()
	}
}

func (c *Client) sendGroupMedia(frame media.SendFrame) {
	if len(frame.Payload) == 0 || len(frame.Payload) > protocol.MaxGroupDatagramPayload || c.roomNetwork == nil || c.groupProtector == nil {
		return
	}
	now := time.Now()
	if !frame.Deadline.IsZero() && now.After(frame.Deadline) {
		return
	}
	if c.groupStreamPendingTopology {
		c.queueTopologySnapshots(now, false)
		if !c.groupStreamTopologyReady() {
			return
		}
		c.groupStreamPendingTopology = false
	}
	c.refreshFanout(now)
	if c.groupSendSequence == math.MaxUint64 {
		c.initGroupStream()
		c.groupStreamPendingTopology = true
		c.queueTopologySnapshots(now, true)
		return
	}
	c.groupSendSequence++
	header := protocol.GroupDatagramHeader{
		Class: protocol.TrafficAudio, SenderID: c.groupSenderID,
		StreamID: c.groupStreamID, Sequence: c.groupSendSequence,
	}
	packet, err := protocol.MarshalGroupDatagram(c.roomTag, header, frame.Timestamp, frame.Payload, c.groupProtector, c.localIdentity)
	if err != nil {
		return
	}
	destinations := c.fanout.destinations
	if !c.fanoutReady(now) {
		destinations = make([]string, 0, len(c.remotePeers))
		for peerID, peer := range c.remotePeers {
			if peer.session != nil && peer.session.authenticated && peer.session.path.IsDirect() {
				destinations = append(destinations, peerID)
			}
		}
	}
	c.sendRealtimeToPeers(protocol.TrafficAudio, packet, destinations, frame.Deadline, frame.Generation)
}

func (c *Client) handleGroupDatagram(packet endpoint.Datagram, mediaPort media.PeerPort) {
	header, err := protocol.ParseGroupDatagramHeader(packet.Data, c.roomTag)
	if err != nil || (header.Class != protocol.TrafficAudio && header.Class != protocol.TrafficInteractive) || header.SenderID == c.groupSenderID {
		return
	}
	remoteIdentity, err := identity.FromPublicKey(ed25519.PublicKey(header.SenderID[:]))
	if err != nil {
		return
	}
	remote := c.remotePeers[remoteIdentity.PeerID()]
	if remote == nil || remote.session == nil || !remote.session.authenticated {
		return
	}
	if !c.authenticatedDirectSource(packet.From) {
		return
	}
	if header.Class == protocol.TrafficAudio && (remote.session.audioStreamID == ([16]byte{}) || remote.session.audioStreamID != header.StreamID) {
		return
	}
	if header.Class == protocol.TrafficInteractive && (!remote.session.remoteScreenState.active || remote.session.remoteScreenState.streamID != header.StreamID) {
		return
	}
	key := groupStreamKey{sender: header.SenderID, stream: header.StreamID, class: header.Class}
	state := c.groupReceivers[key]
	newState := state == nil
	if state == nil {
		state = &groupReceiveState{}
	}
	if !state.mayAccept(header.Sequence) {
		return
	}
	decoded, err := protocol.ParseGroupDatagram(packet.Data, c.roomTag, header, c.groupProtector)
	if err != nil {
		return
	}
	var fragment decodedScreenVideoFragment
	if header.Class == protocol.TrafficInteractive {
		if decoded.Timestamp == 0 {
			return
		}
		fragment, err = decodeScreenVideoFragment(decoded.Payload)
		if err != nil || !screenVideoFragmentMatchesState(fragment, remote.session.remoteScreenState) {
			return
		}
	}
	if !state.accept(header.Sequence) {
		return
	}
	if newState {
		if len(c.groupReceivers) >= maxGroupReceiveStreams {
			var oldestKey groupStreamKey
			var oldestAt time.Time
			found := false
			for candidate, retained := range c.groupReceivers {
				if !found || retained.lastSeen.Before(oldestAt) {
					oldestKey = candidate
					oldestAt = retained.lastSeen
					found = true
				}
			}
			if !found {
				return
			}
			delete(c.groupReceivers, oldestKey)
			c.removeScreenVideoAssembly(oldestKey)
			delete(c.screenVideoReceivers, oldestKey)
		}
		c.groupReceivers[key] = state
	}
	now := packet.ReceivedAt
	if now.IsZero() {
		now = time.Now()
	}
	rate, burst := groupPacketsPerSecond, groupPacketBurst
	if header.Class == protocol.TrafficInteractive {
		rate, burst = screenVideoPacketsPerSecond, screenVideoPacketBurst
	}
	if !state.allowCost(now, 1, rate, burst) {
		return
	}
	state.lastSeen = now
	if header.Class == protocol.TrafficInteractive {
		complete := c.acceptScreenVideoFragment(key, decoded.Timestamp, fragment, packet.Data, now)
		if complete == nil {
			return
		}
		c.forwardScreenVideoChunk(remoteIdentity.PeerID(), packet.From, complete.packets, complete.deadline)
		c.deliverScreenVideoChunk(ScreenVideoChunk{
			PeerID: remoteIdentity.PeerID(), SessionID: remote.session.sessionID, Generation: complete.metadata.generation, StreamID: header.StreamID,
			ChunkID: complete.chunkID,
			Codec:   complete.metadata.codec, Width: complete.metadata.width, Height: complete.metadata.height,
			Timestamp: complete.metadata.timestamp, Duration: complete.metadata.duration,
			KeyFrame: complete.metadata.keyFrame, Bytes: complete.bytes,
		})
		return
	}
	c.forwardGroupDatagram(remoteIdentity.PeerID(), header.Class, packet, now.Add(10*time.Millisecond))
	if mediaPort != nil {
		mediaPort.SubmitReceived(media.ReceivedFrame{
			SourceID: remoteIdentity.PeerID(), StreamID: header.StreamID,
			Sequence: header.Sequence, Timestamp: decoded.Timestamp,
			Payload: decoded.Payload, ReceivedAt: now,
		})
	}
}

func (c *Client) groupStreamTopologyReady() bool {
	for _, peer := range c.remotePeers {
		session := peer.session
		if session == nil || !session.authenticated {
			continue
		}
		if session.reliable == nil || session.topologySentGeneration != c.topologyGeneration || session.reliable.pendingChannel(reliableChannelTopology) {
			return false
		}
	}
	return true
}

func (c *Client) authenticatedDirectSource(address netip.AddrPort) bool {
	for _, peer := range c.remotePeers {
		if peer.session != nil && peer.session.authenticated && peer.session.path.IsDirect() && peer.session.path.Address() == address {
			return true
		}
	}
	return false
}

func (c *Client) expireGroupStreams(now time.Time) {
	for key, state := range c.groupReceivers {
		if state.lastSeen.Add(groupStreamIdle).Before(now) {
			delete(c.groupReceivers, key)
			c.removeScreenVideoAssembly(key)
			delete(c.screenVideoReceivers, key)
		}
	}
}
