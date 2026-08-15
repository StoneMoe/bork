package tracker

import (
	"encoding/binary"
	"errors"
	"net/netip"
	"time"
)

const maxAnnouncePeers = 50

type announceResponse struct {
	interval  time.Duration
	peers     []netip.AddrPort
	peerNames []httpPeer
}

type httpPeer struct {
	address netip.Addr
	name    string
	port    uint16
}

func appendCompactPeers(peers []netip.AddrPort, seen map[netip.AddrPort]struct{}, compact []byte, ipv6 bool) ([]netip.AddrPort, error) {
	peerSize := 6
	if ipv6 {
		peerSize = 18
	}
	if len(compact)%peerSize != 0 {
		return nil, errors.New("tracker response has a malformed compact peer list")
	}
	for offset := 0; offset < len(compact) && len(peers) < maxAnnouncePeers; offset += peerSize {
		var address netip.Addr
		if ipv6 {
			var bytes [16]byte
			copy(bytes[:], compact[offset:offset+16])
			address = netip.AddrFrom16(bytes).Unmap()
		} else {
			address = netip.AddrFrom4([4]byte(compact[offset : offset+4]))
		}
		port := binary.BigEndian.Uint16(compact[offset+peerSize-2 : offset+peerSize])
		peers = appendUniquePeer(peers, seen, netip.AddrPortFrom(address, port))
	}
	return peers, nil
}

func appendUniquePeer(peers []netip.AddrPort, seen map[netip.AddrPort]struct{}, peer netip.AddrPort) []netip.AddrPort {
	if len(peers) >= maxAnnouncePeers || !usablePeer(peer) {
		return peers
	}
	if _, exists := seen[peer]; exists {
		return peers
	}
	seen[peer] = struct{}{}
	return append(peers, peer)
}

func usablePeer(peer netip.AddrPort) bool {
	if !peer.IsValid() || peer.Port() == 0 {
		return false
	}
	address := peer.Addr().Unmap()
	return address.IsGlobalUnicast() &&
		!address.IsPrivate() &&
		!address.IsLoopback() &&
		!address.IsLinkLocalUnicast()
}
