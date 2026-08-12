package peer

import (
	"crypto/ed25519"
	"encoding/binary"
	"errors"
	"net/netip"
	"slices"
	"sort"
	"time"

	"bork/internal/identity"
	"bork/internal/networking/discovery"
	"bork/internal/networking/endpoint"
)

const (
	reliableChannelTopology = 1
	topologyMessageVersion  = 1
	topologyRefresh         = 10 * time.Second
)

type topologyEntry struct {
	peerID    [32]byte
	addresses []netip.AddrPort
}

type topologyMessage struct {
	generation    uint64
	audioStreamID [16]byte
	candidates    []netip.AddrPort
	neighbors     []topologyEntry
}

func (c *Client) queueTopologySnapshots(now time.Time, force bool) {
	generation := c.topologyGeneration
	for peerID, peer := range c.remotePeers {
		activeSession := peer.activeSession
		if activeSession == nil || !activeSession.authenticated || activeSession.reliable == nil {
			continue
		}
		if !force && activeSession.topologySentGeneration == generation && now.Sub(activeSession.lastTopologyAt) < topologyRefresh {
			continue
		}
		payload, err := c.marshalTopologyMessage(generation, peerID, activeSession.path.Address())
		if err != nil || activeSession.reliable.queue(reliableChannelTopology, true, payload) != nil {
			continue
		}
		activeSession.topologySentGeneration = generation
		activeSession.lastTopologyAt = now
	}
}

func (c *Client) marshalTopologyMessage(generation uint64, recipientID string, recipientAddress netip.AddrPort) ([]byte, error) {
	if generation == 0 {
		return nil, errors.New("topology generation is zero")
	}
	if c.audioStreamID == ([16]byte{}) {
		return nil, errors.New("topology audio stream is zero")
	}
	candidates := make([]netip.AddrPort, 0, len(c.networkSnapshot.Endpoint.Candidates))
	for _, candidateType := range []endpoint.CandidateType{endpoint.CandidatePortMapped, endpoint.CandidateSTUN, endpoint.CandidateNIC} {
		for _, candidate := range c.networkSnapshot.Endpoint.Candidates {
			if candidate.Type != candidateType {
				continue
			}
			address, err := netip.ParseAddrPort(candidate.Address)
			if err == nil && usableTopologyAddress(address, recipientAddress) && !slices.Contains(candidates, address) {
				candidates = append(candidates, address)
			}
		}
	}
	peerIDs := make([]string, 0, len(c.remotePeers))
	for peerID, peer := range c.remotePeers {
		if peerID != recipientID && peer.activeSession != nil && peer.activeSession.authenticated && peer.activeSession.path.IsDirect() {
			peerIDs = append(peerIDs, peerID)
		}
	}
	sort.Strings(peerIDs)
	neighbors := make([]topologyEntry, 0, len(peerIDs))
	for _, peerID := range peerIDs {
		peer := c.remotePeers[peerID]
		entry := topologyEntry{peerID: rawPeerIdentity(peer.identity)}
		if address := peer.activeSession.path.Address(); usableTopologyAddress(address, recipientAddress) {
			entry.addresses = []netip.AddrPort{address}
		}
		neighbors = append(neighbors, entry)
	}
	return encodeTopologyMessage(topologyMessage{generation: generation, audioStreamID: c.audioStreamID, candidates: candidates, neighbors: neighbors})
}

func encodeTopologyMessage(message topologyMessage) ([]byte, error) {
	if message.generation == 0 || message.audioStreamID == ([16]byte{}) || len(message.candidates) > int(^uint16(0)) || uint64(len(message.neighbors)) > uint64(^uint32(0)) {
		return nil, errors.New("topology message fields are invalid")
	}
	payload := make([]byte, 0, 31+len(message.candidates)*19+len(message.neighbors)*52)
	payload = append(payload, topologyMessageVersion)
	payload = binary.BigEndian.AppendUint64(payload, message.generation)
	payload = append(payload, message.audioStreamID[:]...)
	payload = binary.BigEndian.AppendUint16(payload, uint16(len(message.candidates)))
	for _, address := range message.candidates {
		var err error
		payload, err = appendTopologyAddress(payload, address)
		if err != nil {
			return nil, err
		}
	}
	payload = binary.BigEndian.AppendUint32(payload, uint32(len(message.neighbors)))
	for _, neighbor := range message.neighbors {
		if zeroRawIdentity(neighbor.peerID) || len(neighbor.addresses) > int(^uint16(0)) {
			return nil, errors.New("topology neighbor is invalid")
		}
		payload = append(payload, neighbor.peerID[:]...)
		payload = binary.BigEndian.AppendUint16(payload, uint16(len(neighbor.addresses)))
		for _, address := range neighbor.addresses {
			var err error
			payload, err = appendTopologyAddress(payload, address)
			if err != nil {
				return nil, err
			}
		}
	}
	return payload, nil
}

func appendTopologyAddress(payload []byte, address netip.AddrPort) ([]byte, error) {
	address = netip.AddrPortFrom(address.Addr().Unmap(), address.Port())
	if !address.IsValid() || address.Port() == 0 {
		return nil, errors.New("topology address is invalid")
	}
	ip := address.Addr()
	family := byte(6)
	if ip.Is4() {
		family = 4
	} else if !ip.Is6() {
		return nil, errors.New("topology address family is invalid")
	}
	payload = append(payload, family)
	payload = binary.BigEndian.AppendUint16(payload, address.Port())
	payload = append(payload, ip.AsSlice()...)
	return payload, nil
}

func decodeTopologyMessage(payload []byte) (topologyMessage, error) {
	if len(payload) < 31 || payload[0] != topologyMessageVersion {
		return topologyMessage{}, errors.New("topology message header is invalid")
	}
	message := topologyMessage{generation: binary.BigEndian.Uint64(payload[1:9])}
	if message.generation == 0 {
		return topologyMessage{}, errors.New("topology generation is zero")
	}
	copy(message.audioStreamID[:], payload[9:25])
	if message.audioStreamID == ([16]byte{}) {
		return topologyMessage{}, errors.New("topology audio stream is zero")
	}
	offset := 27
	candidateCount := int(binary.BigEndian.Uint16(payload[25:27]))
	message.candidates = make([]netip.AddrPort, 0, candidateCount)
	for range candidateCount {
		address, next, err := parseTopologyAddress(payload, offset)
		if err != nil {
			return topologyMessage{}, err
		}
		offset = next
		message.candidates = append(message.candidates, address)
	}
	if len(payload)-offset < 4 {
		return topologyMessage{}, errors.New("topology neighbor count is truncated")
	}
	neighborCount := uint64(binary.BigEndian.Uint32(payload[offset : offset+4]))
	offset += 4
	if neighborCount > uint64((len(payload)-offset)/34) {
		return topologyMessage{}, errors.New("topology neighbor count is invalid")
	}
	message.neighbors = make([]topologyEntry, 0, int(neighborCount))
	for range neighborCount {
		if len(payload)-offset < 34 {
			return topologyMessage{}, errors.New("topology neighbor is truncated")
		}
		var entry topologyEntry
		copy(entry.peerID[:], payload[offset:offset+32])
		offset += 32
		if zeroRawIdentity(entry.peerID) {
			return topologyMessage{}, errors.New("topology neighbor identity is zero")
		}
		addressCount := int(binary.BigEndian.Uint16(payload[offset : offset+2]))
		offset += 2
		entry.addresses = make([]netip.AddrPort, 0, addressCount)
		for range addressCount {
			address, next, err := parseTopologyAddress(payload, offset)
			if err != nil {
				return topologyMessage{}, err
			}
			offset = next
			entry.addresses = append(entry.addresses, address)
		}
		message.neighbors = append(message.neighbors, entry)
	}
	if offset != len(payload) {
		return topologyMessage{}, errors.New("topology message has trailing data")
	}
	return message, nil
}

func parseTopologyAddress(payload []byte, offset int) (netip.AddrPort, int, error) {
	if len(payload)-offset < 3 {
		return netip.AddrPort{}, offset, errors.New("topology address is truncated")
	}
	family := payload[offset]
	port := binary.BigEndian.Uint16(payload[offset+1 : offset+3])
	offset += 3
	size := 0
	if family == 4 {
		size = 4
	} else if family == 6 {
		size = 16
	} else {
		return netip.AddrPort{}, offset, errors.New("topology address family is invalid")
	}
	if port == 0 || len(payload)-offset < size {
		return netip.AddrPort{}, offset, errors.New("topology address is invalid")
	}
	var address netip.Addr
	if size == 4 {
		address = netip.AddrFrom4([4]byte(payload[offset : offset+4]))
	} else {
		address = netip.AddrFrom16([16]byte(payload[offset : offset+16]))
	}
	offset += size
	return netip.AddrPortFrom(address.Unmap(), port), offset, nil
}

func (c *Client) handleTopologySnapshot(sender *RemotePeer, payload []byte) {
	c.handleTopologySnapshotAt(sender, payload, time.Now())
}

func (c *Client) handleTopologySnapshotAt(sender *RemotePeer, payload []byte, now time.Time) {
	message, err := decodeTopologyMessage(payload)
	if err != nil || sender == nil || sender.activeSession == nil || message.generation < sender.activeSession.topologyReceivedGeneration {
		return
	}
	if message.generation > sender.activeSession.topologyReceivedGeneration {
		sender.activeSession.topologyReceivedGeneration = message.generation
		sender.activeSession.audioStreamID = message.audioStreamID
	}
	c.recordTopologyClaims(sender.identity, message.neighbors, now)
	for _, address := range message.candidates {
		if usableTopologyAddress(address, sender.activeSession.path.Address()) {
			c.addDiscoveryHintAt(discovery.Hint{Address: address, Source: discovery.SourceTopology, ExpiresAt: now.Add(topologyHintTTL)}, now)
		}
	}
	for _, entry := range message.neighbors {
		peerIdentity, identityErr := identity.FromPublicKey(ed25519.PublicKey(entry.peerID[:]))
		if identityErr != nil || peerIdentity.PeerID() == c.localIdentity.PeerID() {
			continue
		}
		for _, address := range entry.addresses {
			if usableTopologyAddress(address, sender.activeSession.path.Address()) {
				c.addDiscoveryHintAt(discovery.Hint{Address: address, Source: discovery.SourceTopology, ExpiresAt: now.Add(topologyHintTTL)}, now)
			}
		}
	}
}
