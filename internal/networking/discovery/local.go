package discovery

import (
	"context"
	"errors"
	"fmt"
	"net"
	"net/netip"
	"time"

	"golang.org/x/net/ipv4"
)

const (
	localMulticastAddress  = "239.255.66.75:49736"
	localAnnouncementMagic = "BORKLOC1"
	localAnnounceInterval  = 500 * time.Millisecond
	localMaxPeerHint       = 64
	localMaxAddress        = 96
	localMaxKnownPeers     = 128
	localReceiveBufferSize = 64 * 1024
)

type localAnnouncement struct {
	roomTag  [16]byte
	peerHint string
	address  netip.AddrPort
}

type localDatagram struct {
	data []byte
	from *net.UDPAddr
}

type localAddressSet map[netip.Addr]struct{}

type localNetworkSnapshot struct {
	loopback  *net.Interface
	addresses localAddressSet
}

type localDiscovery struct {
	snapshotNetwork  func() (localNetworkSnapshot, error)
	announceInterval time.Duration
}

func newLocalDiscovery() *localDiscovery {
	return &localDiscovery{snapshotNetwork: snapshotLocalNetwork, announceInterval: localAnnounceInterval}
}

func (l *localDiscovery) Run(ctx context.Context, roomTag [16]byte, listenAddress netip.AddrPort, hints chan<- Hint) error {
	network, err := l.snapshotNetwork()
	if err != nil {
		return err
	}
	address, err := loopbackAddress(listenAddress, network.addresses)
	if err != nil {
		return err
	}
	peerHint, err := newPeerHint()
	if err != nil {
		return err
	}
	announcement, err := marshalLocalAnnouncement(roomTag, peerHint, address, network.addresses)
	if err != nil {
		return err
	}
	if network.loopback == nil {
		return errors.New("no active IPv4 loopback interface for local discovery")
	}
	loopback := network.loopback
	group, err := net.ResolveUDPAddr("udp4", localMulticastAddress)
	if err != nil {
		return fmt.Errorf("resolve local discovery multicast address: %w", err)
	}
	conn, err := net.ListenMulticastUDP("udp4", loopback, group)
	if err != nil {
		return fmt.Errorf("listen for local peer announcements: %w", err)
	}
	packetConn := ipv4.NewPacketConn(conn)
	if err := packetConn.SetMulticastInterface(loopback); err != nil {
		conn.Close()
		return fmt.Errorf("select local discovery interface: %w", err)
	}
	if err := packetConn.SetMulticastLoopback(true); err != nil {
		conn.Close()
		return fmt.Errorf("enable local multicast loopback: %w", err)
	}
	if err := packetConn.SetMulticastTTL(0); err != nil {
		conn.Close()
		return fmt.Errorf("limit local multicast TTL: %w", err)
	}
	if err := conn.SetReadBuffer(64 * 1024); err != nil {
		conn.Close()
		return fmt.Errorf("set local discovery receive buffer: %w", err)
	}

	incoming := make(chan localDatagram, 32)
	readDone := make(chan error, 1)
	go readLocalDatagrams(ctx, conn, incoming, readDone)
	stopReader := func() {
		_ = conn.Close()
		<-readDone
	}
	if _, err := conn.WriteToUDP(announcement, group); err != nil {
		stopReader()
		return fmt.Errorf("announce local peer: %w", err)
	}

	ticker := time.NewTicker(l.announceInterval)
	defer ticker.Stop()
	known := make(localSignatureCache)
	for {
		select {
		case <-ctx.Done():
			stopReader()
			return nil
		case err := <-readDone:
			_ = conn.Close()
			if ctx.Err() != nil || errors.Is(err, net.ErrClosed) {
				return nil
			}
			return fmt.Errorf("read local peer announcement: %w", err)
		case <-ticker.C:
			if _, err := conn.WriteToUDP(announcement, group); err != nil {
				stopReader()
				return fmt.Errorf("announce local peer: %w", err)
			}
		case datagram := <-incoming:
			if datagram.from == nil || !datagram.from.IP.IsLoopback() {
				continue
			}
			announced, err := parseLocalAnnouncement(datagram.data, network.addresses)
			if err != nil || announced.roomTag != roomTag || announced.peerHint == peerHint {
				continue
			}
			signature := announced.peerHint + "\x00" + announced.address.String()
			now := time.Now()
			if known.seen(signature, now) {
				continue
			}
			select {
			case hints <- Hint{Address: announced.address, Source: SourceLocal}:
				known.add(signature, now)
			case <-ctx.Done():
				stopReader()
				return nil
			default:
			}
		}
	}
}

type localSignatureCache map[string]time.Time

func (cache localSignatureCache) seen(signature string, now time.Time) bool {
	if _, exists := cache[signature]; !exists {
		return false
	}
	cache[signature] = now
	return true
}

func (cache localSignatureCache) add(signature string, now time.Time) {
	if len(cache) >= localMaxKnownPeers {
		oldest := ""
		var oldestAt time.Time
		for candidate, lastSeen := range cache {
			if oldest == "" || lastSeen.Before(oldestAt) {
				oldest = candidate
				oldestAt = lastSeen
			}
		}
		delete(cache, oldest)
	}
	cache[signature] = now
}

func readLocalDatagrams(ctx context.Context, conn *net.UDPConn, incoming chan<- localDatagram, done chan<- error) {
	buffer := make([]byte, localReceiveBufferSize)
	for {
		count, from, err := conn.ReadFromUDP(buffer)
		if err != nil {
			done <- err
			return
		}
		if count > localAnnouncementSize() {
			continue
		}
		data := append([]byte(nil), buffer[:count]...)
		select {
		case incoming <- localDatagram{data: data, from: from}:
		case <-ctx.Done():
			done <- nil
			return
		default:
		}
	}
}

func snapshotLocalNetwork() (localNetworkSnapshot, error) {
	interfaces, err := net.Interfaces()
	if err != nil {
		return localNetworkSnapshot{}, fmt.Errorf("list network interfaces for local discovery: %w", err)
	}
	snapshot := localNetworkSnapshot{addresses: make(localAddressSet)}
	for index := range interfaces {
		candidate := &interfaces[index]
		if candidate.Flags&net.FlagUp == 0 {
			continue
		}
		addresses, err := candidate.Addrs()
		if err != nil {
			continue
		}
		for _, address := range addresses {
			ip, _, err := net.ParseCIDR(address.String())
			if err != nil {
				continue
			}
			parsed, ok := netip.AddrFromSlice(ip)
			if !ok {
				continue
			}
			parsed = parsed.Unmap().WithZone("")
			if !parsed.IsValid() || parsed.IsUnspecified() || parsed.IsMulticast() {
				continue
			}
			snapshot.addresses[parsed] = struct{}{}
			if snapshot.loopback == nil && candidate.Flags&net.FlagLoopback != 0 && parsed.Is4() && parsed.IsLoopback() {
				copy := *candidate
				snapshot.loopback = &copy
			}
		}
	}
	if snapshot.loopback == nil {
		return localNetworkSnapshot{}, errors.New("no active IPv4 loopback interface for local discovery")
	}
	return snapshot, nil
}

func marshalLocalAnnouncement(roomTag [16]byte, peerHint string, address netip.AddrPort, localAddresses localAddressSet) ([]byte, error) {
	if !validPeerHint(peerHint) {
		return nil, errors.New("local discovery peer hint is invalid")
	}
	if !isLocalAddress(address, localAddresses) {
		return nil, errors.New("local discovery address is not reachable from this host")
	}
	encodedAddress := address.String()
	if len(encodedAddress) == 0 || len(encodedAddress) > localMaxAddress {
		return nil, errors.New("local discovery address is too long")
	}
	packet := make([]byte, 0, len(localAnnouncementMagic)+16+2+len(peerHint)+len(encodedAddress))
	packet = append(packet, localAnnouncementMagic...)
	packet = append(packet, roomTag[:]...)
	packet = append(packet, byte(len(peerHint)), byte(len(encodedAddress)))
	packet = append(packet, peerHint...)
	packet = append(packet, encodedAddress...)
	return packet, nil
}

func parseLocalAnnouncement(packet []byte, localAddresses localAddressSet) (localAnnouncement, error) {
	headerSize := len(localAnnouncementMagic) + 16 + 2
	if len(packet) < headerSize || len(packet) > localAnnouncementSize() || string(packet[:len(localAnnouncementMagic)]) != localAnnouncementMagic {
		return localAnnouncement{}, errors.New("local discovery packet header is invalid")
	}
	var announcement localAnnouncement
	offset := len(localAnnouncementMagic)
	copy(announcement.roomTag[:], packet[offset:offset+16])
	offset += 16
	peerLength := int(packet[offset])
	addressLength := int(packet[offset+1])
	offset += 2
	if peerLength < 1 || peerLength > localMaxPeerHint || addressLength < 1 || addressLength > localMaxAddress || len(packet) != offset+peerLength+addressLength {
		return localAnnouncement{}, errors.New("local discovery packet length is invalid")
	}
	announcement.peerHint = string(packet[offset : offset+peerLength])
	if !validPeerHint(announcement.peerHint) {
		return localAnnouncement{}, errors.New("local discovery peer hint is invalid")
	}
	offset += peerLength
	address, err := netip.ParseAddrPort(string(packet[offset:]))
	if err != nil || !isLocalAddress(address, localAddresses) {
		return localAnnouncement{}, errors.New("local discovery address is invalid")
	}
	announcement.address = address
	return announcement, nil
}

func validPeerHint(peerHint string) bool {
	if len(peerHint) == 0 || len(peerHint) > localMaxPeerHint {
		return false
	}
	for index := range len(peerHint) {
		if peerHint[index] < 0x21 || peerHint[index] > 0x7e {
			return false
		}
	}
	return true
}

func localAnnouncementSize() int {
	return len(localAnnouncementMagic) + 16 + 2 + localMaxPeerHint + localMaxAddress
}

func loopbackAddress(listenAddress netip.AddrPort, localAddresses localAddressSet) (netip.AddrPort, error) {
	if !listenAddress.IsValid() || listenAddress.Port() == 0 {
		return netip.AddrPort{}, errors.New("local discovery requires a valid UDP listen address")
	}
	address := listenAddress.Addr().Unmap()
	if address.IsUnspecified() {
		if address.Is4() {
			address = netip.AddrFrom4([4]byte{127, 0, 0, 1})
		} else {
			address = netip.IPv6Loopback()
		}
	}
	result := netip.AddrPortFrom(address, listenAddress.Port())
	if !isLocalAddress(result, localAddresses) {
		return netip.AddrPort{}, errors.New("UDP listen address is not reachable from this host")
	}
	return result, nil
}

func isLocalAddress(address netip.AddrPort, localAddresses localAddressSet) bool {
	if !address.IsValid() || address.Port() == 0 || address.Addr().IsUnspecified() || address.Addr().IsMulticast() {
		return false
	}
	if address.Addr().IsLoopback() {
		return true
	}
	want := address.Addr().Unmap().WithZone("")
	_, exists := localAddresses[want]
	return exists
}
