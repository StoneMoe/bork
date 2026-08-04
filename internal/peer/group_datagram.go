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
	groupPacketsPerSecond = 120.0
	groupPacketBurst      = 24.0
	groupStreamIdle       = 30 * time.Second
	// Signed room members can create arbitrary StreamIDs; bound retained replay
	// state without imposing a member or concurrent-speaker product limit.
	maxGroupReceiveStreams = 4096
)

type groupStreamKey struct {
	sender [32]byte
	stream [16]byte
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
	if err != nil || header.Class != protocol.TrafficAudio || header.SenderID == c.groupSenderID {
		return
	}
	remoteIdentity, err := identity.FromPublicKey(ed25519.PublicKey(header.SenderID[:]))
	if err != nil {
		return
	}
	remote := c.remotePeers[remoteIdentity.PeerID()]
	if remote == nil || remote.session == nil || !remote.session.authenticated || remote.session.audioStreamID == ([16]byte{}) ||
		remote.session.audioStreamID != header.StreamID || !c.authenticatedDirectSource(packet.From) {
		return
	}
	key := groupStreamKey{sender: header.SenderID, stream: header.StreamID}
	state := c.groupReceivers[key]
	newState := state == nil
	if state == nil {
		state = &groupReceiveState{}
	}
	if !state.mayAccept(header.Sequence) {
		return
	}
	decoded, err := protocol.ParseGroupDatagram(packet.Data, c.roomTag, header, c.groupProtector)
	if err != nil || !state.accept(header.Sequence) {
		return
	}
	if newState {
		if len(c.groupReceivers) >= maxGroupReceiveStreams {
			var oldestKey groupStreamKey
			var oldestAt time.Time
			for candidate, retained := range c.groupReceivers {
				if oldestAt.IsZero() || retained.lastSeen.Before(oldestAt) {
					oldestKey = candidate
					oldestAt = retained.lastSeen
				}
			}
			delete(c.groupReceivers, oldestKey)
		}
		c.groupReceivers[key] = state
	}
	now := packet.ReceivedAt
	if now.IsZero() {
		now = time.Now()
	}
	if !state.allowCost(now, 1, groupPacketsPerSecond, groupPacketBurst) {
		return
	}
	state.lastSeen = now
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
		}
	}
}
