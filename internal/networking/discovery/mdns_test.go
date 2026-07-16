package discovery

import (
	"context"
	"encoding/hex"
	"errors"
	"net"
	"net/netip"
	"sync"
	"testing"
	"time"

	"github.com/grandcat/zeroconf"
)

type fakeMDNSServer struct {
	shutdown chan struct{}
	once     sync.Once
}

func (s *fakeMDNSServer) Shutdown() {
	s.once.Do(func() { close(s.shutdown) })
}

type fakeMDNSResolver struct {
	browse func(context.Context, string, string, chan<- *zeroconf.ServiceEntry) error
}

func (r *fakeMDNSResolver) Browse(ctx context.Context, service, domain string, entries chan<- *zeroconf.ServiceEntry) error {
	return r.browse(ctx, service, domain, entries)
}

func TestParseText(t *testing.T) {
	roomTag, peerHint := parseText([]string{"v=1", "hint=temporary", "room=abcd", "ignored"})
	if roomTag != "abcd" || peerHint != "temporary" {
		t.Fatalf("parseText() = %q, %q", roomTag, peerHint)
	}
}

func TestUsableDiscoveryAddress(t *testing.T) {
	if !usableDiscoveryAddress(netip.MustParseAddr("192.168.1.10")) {
		t.Fatal("private IPv4 should be usable")
	}
	if usableDiscoveryAddress(netip.MustParseAddr("fe80::1")) {
		t.Fatal("zone-less link-local IPv6 should not be usable")
	}
}

func TestMDNSDiscoveryDrainsResolverOnCancellation(t *testing.T) {
	server := &fakeMDNSServer{shutdown: make(chan struct{})}
	browseStarted := make(chan struct{})
	resolverJoined := make(chan struct{})
	resolver := &fakeMDNSResolver{browse: func(ctx context.Context, _ string, _ string, entries chan<- *zeroconf.ServiceEntry) error {
		close(browseStarted)
		go func() {
			<-ctx.Done()
			for range mdnsEntryBuffer + 1 {
				entries <- &zeroconf.ServiceEntry{}
			}
			close(resolverJoined)
			close(entries)
		}()
		return nil
	}}
	discovery := &mdnsDiscovery{
		register: func(string, string, string, int, []string, []net.Interface) (mdnsServer, error) { return server, nil },
		newResolver: func(ipType zeroconf.IPType, _ []net.Interface) (mdnsResolver, error) {
			if ipType == zeroconf.IPv4 {
				return resolver, nil
			}
			return nil, errors.New("IPv6 unavailable")
		},
	}

	ctx, cancel := context.WithCancel(context.Background())
	result := make(chan error, 1)
	go func() {
		result <- discovery.Run(ctx, [16]byte{1}, netip.MustParseAddrPort("0.0.0.0:9000"), make(chan netip.AddrPort))
	}()
	waitForTestSignal(t, browseStarted, "mDNS browse start")
	cancel()
	if err := waitForMDNSResult(t, result); err != nil {
		t.Fatalf("Run() error = %v", err)
	}
	requireTestSignal(t, resolverJoined, "resolver shutdown")
	waitForTestSignal(t, server.shutdown, "server shutdown")
}

func TestMDNSDiscoveryDrainsWhenCancellationInterruptsCandidateDelivery(t *testing.T) {
	server := &fakeMDNSServer{shutdown: make(chan struct{})}
	entryDelivered := make(chan struct{})
	resolverJoined := make(chan struct{})
	roomTag := [16]byte{1}
	resolver := &fakeMDNSResolver{browse: func(ctx context.Context, _ string, _ string, entries chan<- *zeroconf.ServiceEntry) error {
		go func() {
			entries <- &zeroconf.ServiceEntry{
				Text:     []string{"room=" + hex.EncodeToString(roomTag[:]), "hint=other-peer"},
				Port:     9000,
				AddrIPv4: []net.IP{net.ParseIP("192.0.2.10")},
			}
			close(entryDelivered)
			<-ctx.Done()
			for range mdnsEntryBuffer + 1 {
				entries <- &zeroconf.ServiceEntry{}
			}
			close(resolverJoined)
			close(entries)
		}()
		return nil
	}}
	discovery := &mdnsDiscovery{
		register: func(string, string, string, int, []string, []net.Interface) (mdnsServer, error) { return server, nil },
		newResolver: func(ipType zeroconf.IPType, _ []net.Interface) (mdnsResolver, error) {
			if ipType == zeroconf.IPv4 {
				return resolver, nil
			}
			return nil, errors.New("IPv6 unavailable")
		},
	}
	ctx, cancel := context.WithCancel(context.Background())
	result := make(chan error, 1)
	go func() {
		result <- discovery.Run(ctx, roomTag, netip.MustParseAddrPort("0.0.0.0:9000"), make(chan netip.AddrPort))
	}()
	waitForTestSignal(t, entryDelivered, "candidate delivery attempt")
	cancel()
	if err := waitForMDNSResult(t, result); err != nil {
		t.Fatal(err)
	}
	requireTestSignal(t, resolverJoined, "backpressured resolver shutdown")
}

func TestMDNSDiscoveryUsesAvailableAddressFamily(t *testing.T) {
	for _, test := range []struct {
		name      string
		available zeroconf.IPType
	}{
		{name: "IPv4 only", available: zeroconf.IPv4},
		{name: "IPv6 only", available: zeroconf.IPv6},
	} {
		t.Run(test.name, func(t *testing.T) {
			server := &fakeMDNSServer{shutdown: make(chan struct{})}
			requested := make(chan zeroconf.IPType, 2)
			browseStarted := make(chan struct{})
			resolverJoined := make(chan struct{})
			resolver := &fakeMDNSResolver{browse: func(ctx context.Context, _ string, _ string, entries chan<- *zeroconf.ServiceEntry) error {
				close(browseStarted)
				go func() {
					<-ctx.Done()
					close(resolverJoined)
					close(entries)
				}()
				return nil
			}}
			discovery := &mdnsDiscovery{
				register: func(string, string, string, int, []string, []net.Interface) (mdnsServer, error) { return server, nil },
				newResolver: func(ipType zeroconf.IPType, _ []net.Interface) (mdnsResolver, error) {
					requested <- ipType
					if ipType == test.available {
						return resolver, nil
					}
					return nil, errors.New("address family unavailable")
				},
			}

			ctx, cancel := context.WithCancel(context.Background())
			result := make(chan error, 1)
			go func() {
				result <- discovery.Run(ctx, [16]byte{1}, netip.MustParseAddrPort("[::]:9000"), make(chan netip.AddrPort))
			}()
			waitForTestSignal(t, browseStarted, "available family browse start")
			first := waitForRequestedFamily(t, requested)
			second := waitForRequestedFamily(t, requested)
			if first != zeroconf.IPv4 || second != zeroconf.IPv6 {
				t.Fatalf("resolver families = %v, %v", first, second)
			}
			select {
			case err := <-result:
				t.Fatalf("Run() stopped while one family was active: %v", err)
			default:
			}

			cancel()
			if err := waitForMDNSResult(t, result); err != nil {
				t.Fatalf("Run() error = %v", err)
			}
			requireTestSignal(t, resolverJoined, "resolver shutdown")
			waitForTestSignal(t, server.shutdown, "server shutdown")
		})
	}
}

func TestMDNSDiscoveryClosesPartialResourcesAfterBrowseFailure(t *testing.T) {
	browseFailure := errors.New("browse failed")
	server := &fakeMDNSServer{shutdown: make(chan struct{})}
	resolverJoined := make(chan struct{})
	resolver := &fakeMDNSResolver{browse: func(_ context.Context, _ string, _ string, entries chan<- *zeroconf.ServiceEntry) error {
		go func() {
			for range mdnsEntryBuffer + 1 {
				entries <- &zeroconf.ServiceEntry{}
			}
			close(resolverJoined)
			close(entries)
		}()
		return browseFailure
	}}
	discovery := &mdnsDiscovery{
		register: func(string, string, string, int, []string, []net.Interface) (mdnsServer, error) { return server, nil },
		newResolver: func(ipType zeroconf.IPType, _ []net.Interface) (mdnsResolver, error) {
			if ipType == zeroconf.IPv4 {
				return resolver, nil
			}
			return nil, errors.New("IPv6 unavailable")
		},
	}

	err := discovery.Run(context.Background(), [16]byte{1}, netip.MustParseAddrPort("0.0.0.0:9000"), make(chan netip.AddrPort))
	if !errors.Is(err, browseFailure) {
		t.Fatalf("Run() error = %v", err)
	}
	requireTestSignal(t, resolverJoined, "failed resolver shutdown")
	waitForTestSignal(t, server.shutdown, "server shutdown")
}

func waitForRequestedFamily(t *testing.T, requested <-chan zeroconf.IPType) zeroconf.IPType {
	t.Helper()
	select {
	case ipType := <-requested:
		return ipType
	case <-time.After(2 * time.Second):
		t.Fatal("timed out waiting for resolver family")
		return 0
	}
}

func waitForMDNSResult(t *testing.T, result <-chan error) error {
	t.Helper()
	select {
	case err := <-result:
		return err
	case <-time.After(2 * time.Second):
		t.Fatal("timed out waiting for mDNS discovery to stop")
		return nil
	}
}

func waitForTestSignal(t *testing.T, signal <-chan struct{}, description string) {
	t.Helper()
	select {
	case <-signal:
	case <-time.After(2 * time.Second):
		t.Fatalf("timed out waiting for %s", description)
	}
}

func requireTestSignal(t *testing.T, signal <-chan struct{}, description string) {
	t.Helper()
	select {
	case <-signal:
	default:
		t.Fatalf("%s was not joined before Run returned", description)
	}
}
