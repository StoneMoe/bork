package endpoint

import (
	"context"
	"net/netip"
	"testing"
	"time"
)

func TestCollectSTUNFamiliesKeepsSuccessFromEachFamily(t *testing.T) {
	v4 := netip.MustParseAddrPort("192.0.2.1:3478")
	v6 := netip.MustParseAddrPort("[2001:db8::1]:3478")
	v6Started := make(chan struct{})
	probe := func(ctx context.Context, server netip.AddrPort) stunProbeResult {
		if server == v6 {
			close(v6Started)
			return stunProbeResult{mapped: netip.MustParseAddrPort("[2001:db8::2]:4000"), rttMillis: 2}
		}
		select {
		case <-v6Started:
			return stunProbeResult{mapped: netip.MustParseAddrPort("198.51.100.1:4000"), rttMillis: 1}
		case <-ctx.Done():
			return stunProbeResult{err: ctx.Err()}
		}
	}

	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	results := collectSTUNFamilies(ctx, "stun.example:3478", []netip.AddrPort{v4, v6}, probe)
	if len(results) != 2 || results[0].Family != "ipv4" || results[0].MappedAddress != "198.51.100.1:4000" {
		t.Fatalf("IPv4 result = %#v, want successful IPv4 mapping", results)
	}
	if results[1].Family != "ipv6" || results[1].MappedAddress != "[2001:db8::2]:4000" {
		t.Fatalf("IPv6 result = %#v, want successful IPv6 mapping", results[1])
	}
}

func TestCollectSTUNFamiliesDeadlineKeepsPartialSuccess(t *testing.T) {
	v4 := netip.MustParseAddrPort("192.0.2.1:3478")
	v6 := netip.MustParseAddrPort("[2001:db8::1]:3478")
	probe := func(ctx context.Context, server netip.AddrPort) stunProbeResult {
		if server.Addr().Is4() {
			return stunProbeResult{mapped: netip.MustParseAddrPort("198.51.100.1:4000"), rttMillis: 1}
		}
		<-ctx.Done()
		return stunProbeResult{err: ctx.Err()}
	}

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Millisecond)
	defer cancel()
	results := collectSTUNFamilies(ctx, "stun.example:3478", []netip.AddrPort{v4, v6}, probe)
	if len(results) != 2 || results[0].MappedAddress != "198.51.100.1:4000" || results[0].Error != "" {
		t.Fatalf("IPv4 partial result = %#v, want retained success", results)
	}
	if results[1].Family != "ipv6" || results[1].MappedAddress != "" || results[1].Error != "STUN request timed out" {
		t.Fatalf("IPv6 deadline result = %#v, want family timeout", results[1])
	}
}
