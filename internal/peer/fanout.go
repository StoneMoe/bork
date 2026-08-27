package peer

import (
	"encoding/binary"
	"errors"
	"slices"
	"time"

	"bork/internal/identity"
	"bork/internal/networking/endpoint"
	"bork/internal/protocol"
)

const (
	reliableChannelFanout = 2
	fanoutMessageVersion  = 1
	fanoutActivationDelay = time.Second
)

type fanoutAssignment struct {
	generation uint64
	listeners  []identity.PeerID
}

type outboundFanout struct {
	generation   uint64
	activateAt   time.Time
	destinations []identity.PeerID
	assignments  map[identity.PeerID][]identity.PeerID
}

func (c *Client) refreshFanout(now time.Time) {
	if !c.fanoutDirty {
		return
	}
	direct := make(map[identity.PeerID]struct{})
	listeners := make([]identity.PeerID, 0, len(c.remotePeers))
	for peerID, peer := range c.remotePeers {
		if peer.activeSession == nil || !peer.activeSession.authenticated {
			continue
		}
		listeners = append(listeners, peerID)
		if peer.activeSession.path.IsDirect() {
			direct[peerID] = struct{}{}
		}
	}
	plan := buildFanoutPlan(listeners, direct, c.topology, now)
	plan = constrainFanoutToActivePaths(plan, c.remotePeers)
	plan.generation = c.fanout.generation + 1
	if plan.generation == 0 {
		plan.generation = 1
	}
	type deployment struct {
		activeSession *PeeringSession
		payload       []byte
	}
	deployments := make([]deployment, 0, len(plan.assignments)+len(c.fanout.assignments))
	for _, peerID := range plan.destinations {
		peer := c.remotePeers[peerID]
		if peer == nil || peer.activeSession == nil || !peer.activeSession.authenticated || !peer.activeSession.path.IsDirect() || peer.activeSession.reliable == nil {
			return
		}
		payload, err := marshalFanoutAssignment(plan.generation, plan.assignments[peerID])
		if err != nil || peer.activeSession.reliable.canQueue(reliableChannelFanout, true, len(payload)) != nil {
			return
		}
		deployments = append(deployments, deployment{activeSession: peer.activeSession, payload: payload})
	}
	oldOnly := make([]identity.PeerID, 0, len(c.fanout.assignments))
	for peerID := range c.fanout.assignments {
		if _, current := plan.assignments[peerID]; !current {
			oldOnly = append(oldOnly, peerID)
		}
	}
	slices.SortFunc(oldOnly, comparePeerIDs)
	for _, peerID := range oldOnly {
		peer := c.remotePeers[peerID]
		if peer == nil || peer.activeSession == nil || !peer.activeSession.authenticated || !peer.activeSession.path.IsDirect() || peer.activeSession.reliable == nil {
			continue
		}
		payload, err := marshalFanoutAssignment(plan.generation, nil)
		if err != nil || peer.activeSession.reliable.canQueue(reliableChannelFanout, true, len(payload)) != nil {
			return
		}
		deployments = append(deployments, deployment{activeSession: peer.activeSession, payload: payload})
	}
	for _, deployment := range deployments {
		// Preflight above is atomic under the client's single-owner loop.
		_ = deployment.activeSession.reliable.queue(reliableChannelFanout, true, deployment.payload)
	}
	plan.activateAt = now.Add(fanoutActivationDelay)
	c.fanout = plan
	c.fanoutDirty = false
}

func constrainFanoutToActivePaths(plan outboundFanout, peers map[identity.PeerID]*RemotePeer) outboundFanout {
	forced := make(map[identity.PeerID]identity.PeerID)
	targets := make([]identity.PeerID, 0)
	for targetID, peer := range peers {
		if peer == nil || peer.activeSession == nil || !peer.activeSession.authenticated || peer.activeSession.path.IsDirect() {
			continue
		}
		intermediary := peer.activeSession.path.Intermediary()
		forwarder := peers[intermediary]
		if forwarder == nil || forwarder.activeSession == nil || !forwarder.activeSession.authenticated || !forwarder.activeSession.path.IsDirect() || forwarder.activeSession.path.Address() != peer.activeSession.path.Address() {
			continue
		}
		forced[targetID] = intermediary
		targets = append(targets, targetID)
	}
	if len(forced) == 0 {
		return plan
	}
	for forwarderID, assigned := range plan.assignments {
		filtered := assigned[:0]
		for _, targetID := range assigned {
			if _, replace := forced[targetID]; !replace {
				filtered = append(filtered, targetID)
			}
		}
		plan.assignments[forwarderID] = filtered
	}
	slices.SortFunc(targets, comparePeerIDs)
	for _, targetID := range targets {
		forwarderID := forced[targetID]
		if !slices.Contains(plan.destinations, forwarderID) {
			plan.destinations = append(plan.destinations, forwarderID)
		}
		if !slices.Contains(plan.assignments[forwarderID], targetID) {
			plan.assignments[forwarderID] = append(plan.assignments[forwarderID], targetID)
			slices.SortFunc(plan.assignments[forwarderID], comparePeerIDs)
		}
	}
	return plan
}

func (c *Client) fanoutReady(now time.Time) bool {
	if c.fanout.destinations == nil || now.Before(c.fanout.activateAt) {
		return false
	}
	for _, forwarderID := range c.fanout.destinations {
		forwarder := c.remotePeers[forwarderID]
		if forwarder == nil || forwarder.activeSession == nil || !forwarder.activeSession.authenticated || forwarder.activeSession.reliable == nil || forwarder.activeSession.reliable.pendingChannel(reliableChannelFanout) {
			return false
		}
	}
	return true
}

func buildFanoutPlan(listeners []identity.PeerID, direct map[identity.PeerID]struct{}, topology map[identity.PeerID]*topologyPeer, now time.Time) outboundFanout {
	slices.SortFunc(listeners, comparePeerIDs)
	uncovered := make(map[identity.PeerID]struct{}, len(listeners))
	for _, peerID := range listeners {
		uncovered[peerID] = struct{}{}
	}
	plan := outboundFanout{assignments: make(map[identity.PeerID][]identity.PeerID)}
	for len(uncovered) > 0 {
		var selected identity.PeerID
		var coverage []identity.PeerID
		forwarders := make([]identity.PeerID, 0, len(direct))
		for peerID := range direct {
			forwarders = append(forwarders, peerID)
		}
		slices.SortFunc(forwarders, comparePeerIDs)
		for _, forwarder := range forwarders {
			candidate := make([]identity.PeerID, 0)
			if _, exists := uncovered[forwarder]; exists {
				candidate = append(candidate, forwarder)
			}
			if peer := topology[forwarder]; peer != nil {
				for neighbor, expiresAt := range peer.neighbors {
					if _, exists := uncovered[neighbor]; exists && expiresAt.After(now) {
						candidate = append(candidate, neighbor)
					}
				}
			}
			slices.SortFunc(candidate, comparePeerIDs)
			if len(candidate) > len(coverage) {
				selected, coverage = forwarder, candidate
			}
		}
		if selected.IsZero() || len(coverage) == 0 {
			break
		}
		plan.destinations = append(plan.destinations, selected)
		for _, listener := range coverage {
			delete(uncovered, listener)
			if listener != selected {
				plan.assignments[selected] = append(plan.assignments[selected], listener)
			}
		}
		if _, exists := plan.assignments[selected]; !exists {
			plan.assignments[selected] = nil
		}
	}
	return plan
}

func marshalFanoutAssignment(generation uint64, listeners []identity.PeerID) ([]byte, error) {
	if generation == 0 {
		return nil, errors.New("fanout generation is zero")
	}
	payload := make([]byte, 1+8+4+len(listeners)*32)
	payload[0] = fanoutMessageVersion
	binary.BigEndian.PutUint64(payload[1:9], generation)
	binary.BigEndian.PutUint32(payload[9:13], uint32(len(listeners)))
	offset := 13
	for _, peerID := range listeners {
		if peerID.IsZero() {
			return nil, errors.New("fanout listener is zero")
		}
		copy(payload[offset:offset+32], peerID[:])
		offset += 32
	}
	return payload, nil
}

func parseFanoutAssignment(payload []byte) (uint64, []identity.PeerID, error) {
	if len(payload) < 13 || payload[0] != fanoutMessageVersion {
		return 0, nil, errors.New("fanout assignment header is invalid")
	}
	generation := binary.BigEndian.Uint64(payload[1:9])
	count := int(binary.BigEndian.Uint32(payload[9:13]))
	if generation == 0 || count > (len(payload)-13)/32 || len(payload) != 13+count*32 {
		return 0, nil, errors.New("fanout assignment length is invalid")
	}
	listeners := make([]identity.PeerID, count)
	for index := range listeners {
		copy(listeners[index][:], payload[13+index*32:13+(index+1)*32])
		if listeners[index].IsZero() {
			return 0, nil, errors.New("fanout listener is zero")
		}
	}
	return generation, listeners, nil
}

func (c *Client) handleReliableMessage(sender *RemotePeer, message deliveredReliableMessage) {
	if sender == nil {
		return
	}
	if message.channel == reliableChannelTopology {
		c.handleTopologySnapshot(sender, message.payload)
		return
	}
	if message.channel == reliableChannelMemberState {
		c.handleMemberState(sender, message.payload)
		return
	}
	if message.channel == reliableChannelScreenState {
		c.handleScreenState(sender, message.payload)
		return
	}
	if message.channel == reliableChannelFileControl || message.channel == reliableChannelFileData {
		c.handleFileMessage(sender, message)
		return
	}
	if message.channel != reliableChannelFanout {
		return
	}
	if sender.activeSession == nil {
		return
	}
	generation, encodedListeners, err := parseFanoutAssignment(message.payload)
	if err != nil {
		return
	}
	if generation <= sender.activeSession.inboundFanout.generation {
		return
	}
	listeners := make([]identity.PeerID, 0, len(encodedListeners))
	seen := make(map[identity.PeerID]struct{}, len(encodedListeners))
	for _, listener := range encodedListeners {
		if listener == c.localIdentity.PeerID {
			continue
		}
		if _, duplicate := seen[listener]; duplicate {
			continue
		}
		seen[listener] = struct{}{}
		listeners = append(listeners, listener)
	}
	sender.activeSession.inboundFanout = fanoutAssignment{generation: generation, listeners: listeners}
}

func (c *Client) sendRealtimePacketsToPeers(class protocol.TrafficClass, packets [][]byte, peerIDs []identity.PeerID, deadline time.Time, sendGeneration uint64) bool {
	if c.roomNetwork == nil || len(packets) == 0 {
		return false
	}
	admitted := false
	batch := endpoint.RealtimeBatch{
		Class: class, Deadline: deadline, SendGeneration: sendGeneration,
		Datagrams: make([]endpoint.RealtimeDatagram, 0, endpoint.MaxRealtimeBatchDatagrams),
	}
	for _, peerID := range peerIDs {
		peer := c.remotePeers[peerID]
		if peer == nil || peer.activeSession == nil || !peer.activeSession.authenticated || !peer.activeSession.path.IsDirect() {
			continue
		}
		for _, packet := range packets {
			batch.Datagrams = append(batch.Datagrams, endpoint.RealtimeDatagram{Data: packet, Destination: peer.activeSession.path.Address()})
			if len(batch.Datagrams) == endpoint.MaxRealtimeBatchDatagrams {
				admitted = c.roomNetwork.SendRealtimeBatch(batch) == nil || admitted
				batch.Datagrams = make([]endpoint.RealtimeDatagram, 0, endpoint.MaxRealtimeBatchDatagrams)
			}
		}
	}
	if len(batch.Datagrams) > 0 {
		admitted = c.roomNetwork.SendRealtimeBatch(batch) == nil || admitted
	}
	return admitted
}

func (c *Client) forwardRoomDatagram(senderID identity.PeerID, class protocol.TrafficClass, packet endpoint.Datagram, deadline time.Time) {
	if c.roomNetwork == nil {
		return
	}
	sender := c.remotePeers[senderID]
	if sender == nil || sender.activeSession == nil || !sender.activeSession.authenticated || !sender.activeSession.path.IsDirect() || sender.activeSession.path.Address() != packet.From {
		return
	}
	assignment := sender.activeSession.inboundFanout
	if assignment.generation == 0 {
		return
	}
	c.sendRealtimeToPeers(class, packet.Data, assignment.listeners, deadline, 0)
}

func (c *Client) sendRealtimeToPeers(class protocol.TrafficClass, packet []byte, peerIDs []identity.PeerID, deadline time.Time, sendGeneration uint64) {
	c.sendRealtimePacketsToPeers(class, [][]byte{packet}, peerIDs, deadline, sendGeneration)
}
