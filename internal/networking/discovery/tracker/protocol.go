package tracker

import (
	"encoding/binary"
	"errors"
	"fmt"
	"net/netip"
	"time"
)

const (
	protocolConnectionID uint64 = 0x41727101980

	actionConnect  uint32 = 0
	actionAnnounce uint32 = 1
	actionError    uint32 = 3

	eventNone    uint32 = 0
	eventStarted uint32 = 2
	eventStopped uint32 = 3

	connectRequestSize   = 16
	connectResponseSize  = 16
	announceRequestSize  = 98
	announceResponseHead = 20
	responseHeaderSize   = 8

	maxAnnouncePeers       = 50
	maxTrackerResponseSize = 4096
)

// TrackerError is an error response returned by a tracker.
type TrackerError struct {
	Message string
}

func (e *TrackerError) Error() string {
	if e.Message == "" {
		return "tracker returned an error"
	}
	return "tracker returned an error: " + e.Message
}

type announceRequest struct {
	connectionID uint64
	transaction  uint32
	infoHash     [20]byte
	peerID       [20]byte
	event        uint32
	key          uint32
	numWant      int32
	port         uint16
	explicitIP   netip.Addr
}

type announceResponse struct {
	interval        time.Duration
	peers           []netip.AddrPort
	peerNames       []httpPeer
	externalAddress netip.Addr
}

type httpPeer struct {
	address netip.Addr
	name    string
	port    uint16
}

func marshalConnectRequest(transaction uint32) []byte {
	packet := make([]byte, connectRequestSize)
	binary.BigEndian.PutUint64(packet[0:8], protocolConnectionID)
	binary.BigEndian.PutUint32(packet[8:12], actionConnect)
	binary.BigEndian.PutUint32(packet[12:16], transaction)
	return packet
}

func parseConnectResponse(packet []byte, transaction uint32) (uint64, error) {
	if err := validateResponseHeader(packet, actionConnect, transaction); err != nil {
		return 0, err
	}
	if len(packet) != connectResponseSize {
		return 0, fmt.Errorf("tracker connect response length is %d, want %d", len(packet), connectResponseSize)
	}
	return binary.BigEndian.Uint64(packet[8:16]), nil
}

func marshalAnnounceRequest(request announceRequest) []byte {
	packet := make([]byte, announceRequestSize)
	binary.BigEndian.PutUint64(packet[0:8], request.connectionID)
	binary.BigEndian.PutUint32(packet[8:12], actionAnnounce)
	binary.BigEndian.PutUint32(packet[12:16], request.transaction)
	copy(packet[16:36], request.infoHash[:])
	copy(packet[36:56], request.peerID[:])
	// downloaded, left, and uploaded remain zero.
	binary.BigEndian.PutUint32(packet[80:84], request.event)
	if explicit := request.explicitIP.Unmap(); explicit.Is4() {
		address := explicit.As4()
		copy(packet[84:88], address[:])
	}
	binary.BigEndian.PutUint32(packet[88:92], request.key)
	binary.BigEndian.PutUint32(packet[92:96], uint32(boundNumWant(request.numWant)))
	binary.BigEndian.PutUint16(packet[96:98], request.port)
	return packet
}

func parseAnnounceResponse(packet []byte, transaction uint32, ipv6 bool) (announceResponse, error) {
	if err := validateResponseHeader(packet, actionAnnounce, transaction); err != nil {
		return announceResponse{}, err
	}
	if len(packet) < announceResponseHead {
		return announceResponse{}, errors.New("tracker announce response is truncated")
	}
	response := announceResponse{
		interval: time.Duration(binary.BigEndian.Uint32(packet[8:12])) * time.Second,
	}
	if response.interval <= 0 {
		return announceResponse{}, errors.New("tracker announce interval must be positive")
	}
	seen := make(map[netip.AddrPort]struct{}, maxAnnouncePeers)
	peers, err := appendCompactPeers(response.peers, seen, packet[announceResponseHead:], ipv6)
	if err != nil {
		return announceResponse{}, err
	}
	response.peers = peers
	return response, nil
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

func validateResponseHeader(packet []byte, expectedAction, transaction uint32) error {
	if len(packet) < responseHeaderSize {
		return errors.New("tracker response is truncated")
	}
	if len(packet) > maxTrackerResponseSize {
		return fmt.Errorf("tracker response exceeds %d bytes", maxTrackerResponseSize)
	}
	actualTransaction := binary.BigEndian.Uint32(packet[4:8])
	if actualTransaction != transaction {
		return fmt.Errorf("tracker transaction is %08x, want %08x", actualTransaction, transaction)
	}
	action := binary.BigEndian.Uint32(packet[0:4])
	if action == actionError {
		return &TrackerError{Message: string(packet[8:])}
	}
	if action != expectedAction {
		return fmt.Errorf("tracker action is %d, want %d", action, expectedAction)
	}
	return nil
}

func boundNumWant(numWant int32) int32 {
	if numWant < 0 {
		return 0
	}
	if numWant > maxAnnouncePeers {
		return maxAnnouncePeers
	}
	return numWant
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
