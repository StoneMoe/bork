package peer

import (
	"bytes"
	"context"
	"crypto/ed25519"
	"errors"
	"net/netip"
	"sort"
	"time"

	"bork/internal/identity"
	"bork/internal/networking/endpoint"
	"bork/internal/protocol"
)

const (
	topologyClaimTTL = 30 * time.Second
	knownPeerTTL     = 2 * topologyClaimTTL
)

type topologyPeer struct {
	identity  identity.Identity
	lastSeen  time.Time
	neighbors map[string]time.Time
}

func (c *Client) recordTopologyClaims(sender identity.Identity, entries []topologyEntry, now time.Time) {
	senderID := sender.PeerID()
	senderPeer := c.rememberTopologyPeer(sender, now)
	claims := make(map[string]time.Time, len(entries))
	for _, entry := range entries {
		peerIdentity, err := identity.FromPublicKey(ed25519.PublicKey(entry.peerID[:]))
		if err != nil {
			continue
		}
		peerID := peerIdentity.PeerID()
		if c.rememberTopologyPeer(peerIdentity, now) == nil {
			continue
		}
		if peerID != senderID && peerID != c.localIdentity.PeerID() {
			claims[peerID] = now.Add(topologyClaimTTL)
		}
	}
	if senderPeer != nil {
		activeClaims := 0
		changed := false
		for peerID, expiresAt := range senderPeer.neighbors {
			if !expiresAt.After(now) {
				continue
			}
			activeClaims++
			if _, exists := claims[peerID]; !exists {
				changed = true
			}
		}
		changed = changed || activeClaims != len(claims)
		senderPeer.neighbors = claims
		if changed {
			c.fanoutDirty = true
		}
	}
}

func (c *Client) bridgePathTo(targetID string, now time.Time) (Path, bool) {
	if targetID == "" || targetID == c.localIdentity.PeerID() {
		return Path{}, false
	}
	if target := c.remotePeers[targetID]; target != nil && target.activeSession != nil && target.activeSession.authenticated && target.activeSession.path.IsDirect() {
		return Path{}, false
	}
	targetPeer := c.topology[targetID]
	if targetPeer == nil {
		return Path{}, false
	}

	intermediaries := make([]string, 0, len(c.remotePeers))
	for peerID, remote := range c.remotePeers {
		if remote.activeSession == nil || !remote.activeSession.authenticated || !remote.activeSession.path.IsDirect() {
			continue
		}
		peer := c.topology[peerID]
		expiresAt, claimed := time.Time{}, false
		if peer != nil {
			expiresAt, claimed = peer.neighbors[targetID]
		}
		if claimed && expiresAt.After(now) {
			intermediaries = append(intermediaries, peerID)
		}
	}
	if len(intermediaries) == 0 {
		return Path{}, false
	}
	sort.Strings(intermediaries)
	intermediary := c.remotePeers[intermediaries[0]]
	path, err := NewBridgePath(
		intermediary.activeSession.path.Address(),
		rawPeerIdentity(intermediary.identity),
		rawPeerIdentity(targetPeer.identity),
	)
	return path, err == nil
}

func (c *Client) sendBridgeHellos(now time.Time) {
	peerIDs := make([]string, 0, len(c.topology))
	for peerID := range c.topology {
		peerIDs = append(peerIDs, peerID)
	}
	sort.Strings(peerIDs)
	for _, peerID := range peerIDs {
		path, ok := c.bridgePathTo(peerID, now)
		if !ok || c.sessionUsesPath(peerID, path) {
			continue
		}
		if remote := c.remotePeers[peerID]; remote != nil && remote.activeSession != nil {
			c.rememberCandidatePath(remote.activeSession, path, now)
		}
		c.sendHelloOnPath(path)
	}
}

func (c *Client) sendControlOnPath(path Path, inner []byte) error {
	return c.sendPacketOnPath(path, inner, false)
}

func (c *Client) sendPacketOnPath(path Path, inner []byte, background bool) error {
	if c.roomNetwork == nil || !path.IsValid() {
		return errors.New("network path is unavailable")
	}
	if path.IsDirect() {
		if background {
			return c.roomNetwork.EnqueueBackground(inner, path.Address())
		}
		return c.roomNetwork.EnqueueControl(inner, path.Address())
	}
	packet, destination, err := c.bridgePacket(path, inner, background)
	if err != nil {
		return err
	}
	if background {
		return c.roomNetwork.EnqueueBackground(packet, destination)
	}
	return c.roomNetwork.EnqueueControl(packet, destination)
}

func (c *Client) writeControlOnPath(ctx context.Context, path Path, inner []byte) error {
	if c.roomNetwork == nil || !path.IsValid() {
		return errors.New("network path is unavailable")
	}
	if path.IsDirect() {
		return c.roomNetwork.WriteControl(ctx, inner, path.Address())
	}
	packet, destination, err := c.bridgePacket(path, inner, false)
	if err != nil {
		return err
	}
	return c.roomNetwork.WriteControl(ctx, packet, destination)
}

func (c *Client) bridgePacket(path Path, inner []byte, background bool) ([]byte, netip.AddrPort, error) {
	origin := rawPeerIdentity(c.localIdentity.Identity)
	if origin == ([32]byte{}) || path.Target() == ([32]byte{}) || path.Target() == origin || path.Intermediary() == origin {
		return nil, netip.AddrPort{}, errors.New("bridge path endpoints are invalid")
	}
	intermediaryIdentity, err := identityFromRaw(path.Intermediary())
	if err != nil {
		return nil, netip.AddrPort{}, err
	}
	intermediary := c.remotePeers[intermediaryIdentity.PeerID()]
	if intermediary == nil || intermediary.activeSession == nil || !intermediary.activeSession.authenticated || !intermediary.activeSession.path.IsDirect() {
		return nil, netip.AddrPort{}, errors.New("bridge intermediary is unavailable")
	}
	adjacent := intermediary.activeSession
	sequence, err := adjacent.control.nextSendSequence()
	if err != nil {
		return nil, netip.AddrPort{}, err
	}
	packet, err := protocol.MarshalBridge(c.roomTag, adjacent.sessionID, sequence, origin, path.Target(), background, inner, adjacent.ciphers.ControlSend)
	if err != nil {
		return nil, netip.AddrPort{}, err
	}
	return packet, adjacent.path.Address(), nil
}

func (c *Client) handleBridgePacket(packet endpoint.Datagram) {
	outerPath, err := NewPath(packet.From)
	if err != nil {
		return
	}
	header, err := protocol.ParseSessionHeader(packet.Data)
	if err != nil || header.RoomTag != c.roomTag || header.Type != protocol.PacketBridgeControl {
		return
	}
	previous, adjacent, isPendingSession := c.sessionForHeader(header, outerPath)
	if adjacent == nil || isPendingSession || !adjacent.authenticated || !adjacent.path.IsDirect() || !adjacent.acceptsDataPath(outerPath) || !adjacent.control.mayReceive(header.Sequence) {
		return
	}
	decoded, err := protocol.ParseBridge(packet.Data, c.roomTag, adjacent.sessionID, adjacent.ciphers.ControlRecv)
	if err != nil || !adjacent.control.commitReceived(header.Sequence) {
		return
	}
	now := time.Now()
	adjacent.lastAuthenticatedPacketAt = now
	c.rememberAuthenticatedPath(outerPath, now)

	localID := rawPeerIdentity(c.localIdentity.Identity)
	previousID := rawPeerIdentity(previous.identity)
	if decoded.Target == localID {
		path, pathErr := NewBridgePath(packet.From, previousID, decoded.Origin)
		if pathErr == nil {
			c.handleBridgedInner(decoded.Inner, path, decoded.Origin)
		}
		return
	}
	if decoded.Origin != previousID {
		return
	}
	c.forwardBridgePacket(decoded)
}

func (c *Client) handleBridgedInner(inner []byte, path Path, origin [32]byte) {
	packetType, roomTag, err := protocol.ParsePrefix(inner)
	if err != nil || roomTag != c.roomTag {
		return
	}
	switch packetType {
	case protocol.PacketHello:
		hello, err := protocol.ParseHello(inner, c.roomTag, c.admissionKey)
		if err == nil && bytes.Equal(hello.IdentityKey, origin[:]) {
			c.handleHelloOnPath(inner, path)
		}
	case protocol.PacketPing, protocol.PacketPong:
		c.handleSessionPacketOnPath(inner, path)
	case protocol.PacketReliable:
		c.handleReliablePacketOnPath(inner, path)
	case protocol.PacketLeave:
		c.handleLeavePacketOnPath(inner, path)
	}
}

func (c *Client) forwardBridgePacket(decoded protocol.BridgePacket) {
	targetIdentity, err := identityFromRaw(decoded.Target)
	if err != nil {
		return
	}
	target := c.remotePeers[targetIdentity.PeerID()]
	if target == nil || target.activeSession == nil || !target.activeSession.authenticated || !target.activeSession.path.IsDirect() || c.roomNetwork == nil {
		return
	}
	adjacent := target.activeSession
	sequence, err := adjacent.control.nextSendSequence()
	if err != nil {
		return
	}
	packet, err := protocol.MarshalBridge(c.roomTag, adjacent.sessionID, sequence, decoded.Origin, decoded.Target, decoded.Background, decoded.Inner, adjacent.ciphers.ControlSend)
	if err == nil {
		if decoded.Background {
			_ = c.roomNetwork.EnqueueBackground(packet, adjacent.path.Address())
		} else {
			_ = c.roomNetwork.EnqueueControl(packet, adjacent.path.Address())
		}
	}
}

func (c *Client) sessionUsesPath(peerID string, path Path) bool {
	remote := c.remotePeers[peerID]
	if remote == nil {
		return false
	}
	if activeSession := remote.activeSession; activeSession != nil {
		if activeSession.authenticated && activeSession.path.SameRoute(path) {
			return true
		}
		if activeSession.candidateProbe(path) != nil {
			return true
		}
	}
	return remote.pendingSession != nil && remote.pendingSession.acceptsPath(path)
}

func (c *Client) expireTopology(now time.Time) {
	changed := false
	for _, peer := range c.topology {
		for neighbor, expiresAt := range peer.neighbors {
			if !expiresAt.After(now) {
				delete(peer.neighbors, neighbor)
				changed = true
			}
		}
	}
	for peerID, peer := range c.topology {
		remote := c.remotePeers[peerID]
		if peer.lastSeen.Add(knownPeerTTL).Before(now) && (remote == nil || remote.activeSession == nil || !remote.activeSession.authenticated) {
			delete(c.topology, peerID)
			for _, remaining := range c.topology {
				delete(remaining.neighbors, peerID)
			}
			changed = true
		}
	}
	if changed {
		c.fanoutDirty = true
	}
}

func (c *Client) rememberTopologyPeer(peerIdentity identity.Identity, now time.Time) *topologyPeer {
	peerID := peerIdentity.PeerID()
	if peerID == "" || peerID == c.localIdentity.PeerID() {
		return nil
	}
	if peer, exists := c.topology[peerID]; exists {
		peer.lastSeen = now
		return peer
	}
	peer := &topologyPeer{identity: peerIdentity, lastSeen: now, neighbors: make(map[string]time.Time)}
	c.topology[peerID] = peer
	return peer
}
