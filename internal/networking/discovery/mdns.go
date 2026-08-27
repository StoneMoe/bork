package discovery

import (
	"context"
	"encoding/hex"
	"errors"
	"fmt"
	"net"
	"net/netip"
	"strings"

	"github.com/grandcat/zeroconf"
)

const (
	mdnsService     = "_bork._udp"
	mdnsDomain      = "local."
	mdnsEntryBuffer = 32
)

type mdnsServer interface {
	Shutdown()
}

type mdnsResolver interface {
	Browse(context.Context, string, string, chan<- *zeroconf.ServiceEntry) error
}

type mdnsDiscovery struct {
	register    func(string, string, string, int, []string, []net.Interface) (mdnsServer, error)
	newResolver func(zeroconf.IPType, []net.Interface) (mdnsResolver, error)
}

func newMDNSDiscovery() *mdnsDiscovery {
	return &mdnsDiscovery{
		register: func(instance, service, domain string, port int, text []string, interfaces []net.Interface) (mdnsServer, error) {
			return zeroconf.Register(instance, service, domain, port, text, interfaces)
		},
		newResolver: func(ipType zeroconf.IPType, interfaces []net.Interface) (mdnsResolver, error) {
			options := []zeroconf.ClientOption{zeroconf.SelectIPTraffic(ipType)}
			if len(interfaces) > 0 {
				options = append(options, zeroconf.SelectIfaces(interfaces))
			}
			return zeroconf.NewResolver(options...)
		},
	}
}

func (m *mdnsDiscovery) Run(ctx context.Context, roomTag [16]byte, listenAddress netip.AddrPort, hints chan<- Hint) error {
	if !listenAddress.IsValid() || listenAddress.Port() == 0 {
		return errors.New("mDNS requires a non-zero peer port")
	}
	listenIP := listenAddress.Addr().Unmap()
	if listenIP.IsLoopback() {
		<-ctx.Done()
		return nil
	}
	interfaces, err := mdnsInterfaces(listenIP)
	if err != nil {
		return err
	}
	port := listenAddress.Port()
	roomTagText := hex.EncodeToString(roomTag[:])
	announcementID, err := newAnnouncementID()
	if err != nil {
		return err
	}
	server, err := m.register("bork-"+announcementID, mdnsService, mdnsDomain, int(port), []string{
		"v=1",
		"room=" + roomTagText,
		"hint=" + announcementID,
	}, interfaces)
	if err != nil {
		return fmt.Errorf("register mDNS service: %w", err)
	}
	defer server.Shutdown()

	browseCtx, stopBrowses := context.WithCancel(ctx)
	defer stopBrowses()
	browseDone := make(chan string, 2)
	activeBrowses := 0
	var startupErrors []error
	families := []struct {
		name   string
		ipType zeroconf.IPType
	}{
		{name: "IPv4", ipType: zeroconf.IPv4},
		{name: "IPv6", ipType: zeroconf.IPv6},
	}
	if listenIP.Is4() {
		families = families[:1]
	} else if !listenIP.IsUnspecified() {
		families = families[1:]
	}
	for _, family := range families {
		resolver, err := m.newResolver(family.ipType, interfaces)
		if err != nil {
			startupErrors = append(startupErrors, fmt.Errorf("%s: %w", family.name, err))
			continue
		}
		entries := make(chan *zeroconf.ServiceEntry, mdnsEntryBuffer)
		if err := resolver.Browse(browseCtx, mdnsService, mdnsDomain, entries); err != nil {
			drainMDNSEntries(entries)
			startupErrors = append(startupErrors, fmt.Errorf("%s: %w", family.name, err))
			continue
		}
		activeBrowses++
		go consumeMDNSEntries(browseCtx, family.name, entries, browseDone, roomTagText, announcementID, listenIP.Is4(), hints)
	}
	if activeBrowses == 0 {
		if ctx.Err() != nil {
			return nil
		}
		return fmt.Errorf("start mDNS resolvers: %w", errors.Join(startupErrors...))
	}

	select {
	case <-ctx.Done():
		stopBrowses()
		for activeBrowses > 0 {
			<-browseDone
			activeBrowses--
		}
		return nil
	case family := <-browseDone:
		activeBrowses--
		stopBrowses()
		for activeBrowses > 0 {
			<-browseDone
			activeBrowses--
		}
		if ctx.Err() != nil {
			return nil
		}
		return fmt.Errorf("%s mDNS browse stopped", family)
	}
}

func mdnsInterfaces(boundAddress netip.Addr) ([]net.Interface, error) {
	if boundAddress.IsUnspecified() {
		return nil, nil
	}
	interfaces, err := net.Interfaces()
	if err != nil {
		return nil, fmt.Errorf("list mDNS interfaces: %w", err)
	}
	for _, networkInterface := range interfaces {
		addresses, err := networkInterface.Addrs()
		if err != nil {
			continue
		}
		for _, candidate := range addresses {
			ip, _, err := net.ParseCIDR(candidate.String())
			if err != nil {
				continue
			}
			parsed, ok := netip.AddrFromSlice(ip)
			if ok && parsed.Unmap() == boundAddress {
				return []net.Interface{networkInterface}, nil
			}
		}
	}
	return nil, fmt.Errorf("mDNS listen address %s is not assigned to an active interface", boundAddress)
}

func consumeMDNSEntries(
	ctx context.Context,
	family string,
	entries <-chan *zeroconf.ServiceEntry,
	done chan<- string,
	roomTagText string,
	localAnnouncementID string,
	ipv4Only bool,
	hints chan<- Hint,
) {
	defer func() { done <- family }()
	for entry := range entries {
		if ctx.Err() != nil || entry == nil {
			continue
		}
		entryRoomTag, entryAnnouncementID := parseText(entry.Text)
		if entryRoomTag != roomTagText || entryAnnouncementID == "" || entryAnnouncementID == localAnnouncementID || entry.Port < 1 || entry.Port > 65535 {
			continue
		}
		for _, ip := range append(append([]net.IP(nil), entry.AddrIPv4...), entry.AddrIPv6...) {
			address, ok := netip.AddrFromSlice(ip)
			if !ok {
				continue
			}
			address = address.Unmap()
			if !usableDiscoveryAddress(address) || (ipv4Only && !address.Is4()) {
				continue
			}
			discovered := netip.AddrPortFrom(address, uint16(entry.Port))
			select {
			case hints <- Hint{Address: discovered, Source: SourceMDNS}:
			case <-ctx.Done():
				continue
			}
		}
	}
}

func drainMDNSEntries(entries <-chan *zeroconf.ServiceEntry) {
	for range entries {
	}
}

func parseText(records []string) (roomTag, announcementID string) {
	for _, record := range records {
		key, value, ok := strings.Cut(record, "=")
		if !ok {
			continue
		}
		switch key {
		case "room":
			roomTag = value
		case "hint":
			announcementID = value
		}
	}
	return roomTag, announcementID
}

func usableDiscoveryAddress(address netip.Addr) bool {
	return address.IsValid() &&
		!address.IsUnspecified() &&
		!address.IsLoopback() &&
		!address.IsMulticast() &&
		!address.IsLinkLocalUnicast()
}
