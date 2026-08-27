package peer

import (
	"encoding/binary"
	"errors"
	"net/netip"
	"slices"
	"time"

	"bork/internal/identity"
	"bork/internal/networking/discovery"
	"bork/internal/networking/endpoint"
)

const (
	topologyRefresh = 10 * time.Second
)

type topologyEntry struct {
	peerID    identity.PeerID
	addresses []netip.AddrPort
}

type topologyMessage struct {
	candidates []netip.AddrPort
	neighbors  []topologyEntry
}

func (c *Client) queueTopologySnapshots(now time.Time, force bool) {
	revision := c.topologyRevision
	for peerID, peer := range c.remotePeers {
		activeSession := peer.activeSession
		if activeSession == nil || !activeSession.authenticated || activeSession.reliable == nil {
			continue
		}
		if !force && activeSession.topologySentRevision == revision && now.Sub(activeSession.lastTopologyAt) < topologyRefresh {
			continue
		}
		payload, err := c.marshalTopologyMessage(peerID, activeSession.path.Address())
		if err != nil || activeSession.reliable.queue(reliableChannelTopology, payload) != nil {
			continue
		}
		activeSession.topologySentRevision = revision
		activeSession.lastTopologyAt = now
	}
}

func (c *Client) marshalTopologyMessage(recipientID identity.PeerID, recipientAddress netip.AddrPort) ([]byte, error) {
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
	peerIDs := make([]identity.PeerID, 0, len(c.remotePeers))
	for peerID, peer := range c.remotePeers {
		if peerID != recipientID && peer.activeSession != nil && peer.activeSession.authenticated && peer.activeSession.path.IsDirect() {
			peerIDs = append(peerIDs, peerID)
		}
	}
	slices.SortFunc(peerIDs, comparePeerIDs)
	neighbors := make([]topologyEntry, 0, len(peerIDs))
	for _, peerID := range peerIDs {
		peer := c.remotePeers[peerID]
		entry := topologyEntry{peerID: peer.peerID}
		if address := peer.activeSession.path.Address(); usableTopologyAddress(address, recipientAddress) {
			entry.addresses = []netip.AddrPort{address}
		}
		neighbors = append(neighbors, entry)
	}
	return encodeTopologyMessage(topologyMessage{candidates: candidates, neighbors: neighbors})
}

func encodeTopologyMessage(message topologyMessage) ([]byte, error) {
	if len(message.candidates) > int(^uint16(0)) {
		return nil, errors.New("topology message fields are invalid")
	}
	peerIDSize := len(identity.PeerID{})
	payload := make([]byte, 0, 2+len(message.candidates)*19+len(message.neighbors)*(peerIDSize+20))
	payload = binary.BigEndian.AppendUint16(payload, uint16(len(message.candidates)))
	for _, address := range message.candidates {
		var err error
		payload, err = appendTopologyAddress(payload, address)
		if err != nil {
			return nil, err
		}
	}
	for _, neighbor := range message.neighbors {
		if neighbor.peerID.IsZero() || len(neighbor.addresses) > int(^uint16(0)) {
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
	if len(payload) < 2 {
		return topologyMessage{}, errors.New("topology message header is invalid")
	}
	message := topologyMessage{}
	offset := 2
	candidateCount := int(binary.BigEndian.Uint16(payload[0:2]))
	message.candidates = make([]netip.AddrPort, 0, candidateCount)
	for range candidateCount {
		address, next, err := parseTopologyAddress(payload, offset)
		if err != nil {
			return topologyMessage{}, err
		}
		offset = next
		message.candidates = append(message.candidates, address)
	}
	peerIDSize := len(identity.PeerID{})
	for offset < len(payload) {
		if len(payload)-offset < peerIDSize+2 {
			return topologyMessage{}, errors.New("topology neighbor is truncated")
		}
		var entry topologyEntry
		copy(entry.peerID[:], payload[offset:offset+peerIDSize])
		offset += peerIDSize
		if entry.peerID.IsZero() {
			return topologyMessage{}, errors.New("topology neighbor peer ID is zero")
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
	if err != nil || sender == nil || sender.activeSession == nil {
		return
	}
	c.recordTopologyClaims(sender.peerID, message.neighbors, now)
	for _, address := range message.candidates {
		if usableTopologyAddress(address, sender.activeSession.path.Address()) {
			c.addDiscoveryHintAt(discovery.Hint{Address: address, Source: discovery.SourceTopology, ExpiresAt: now.Add(topologyHintTTL)}, now)
		}
	}
	for _, entry := range message.neighbors {
		if entry.peerID == c.localPeerID {
			continue
		}
		for _, address := range entry.addresses {
			if usableTopologyAddress(address, sender.activeSession.path.Address()) {
				c.addDiscoveryHintAt(discovery.Hint{Address: address, Source: discovery.SourceTopology, ExpiresAt: now.Add(topologyHintTTL)}, now)
			}
		}
	}
}
