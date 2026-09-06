//go:build game_proxy

package iwan

import (
	"errors"
	"net/netip"
	"reflect"
	"testing"
)

func TestResolvedIPv4AddressesUnmapsAndFilters(t *testing.T) {
	addresses := []netip.Addr{
		netip.MustParseAddr("::ffff:112.95.75.230"),
		netip.MustParseAddr("2001:db8::1"),
		netip.MustParseAddr("192.0.2.10"),
	}
	want := []netip.Addr{
		netip.MustParseAddr("112.95.75.230"),
		netip.MustParseAddr("192.0.2.10"),
	}

	if got := resolvedIPv4Addresses(addresses); !reflect.DeepEqual(got, want) {
		t.Fatalf("resolved addresses = %v, want %v", got, want)
	}
}

func TestNormalizeOptionsUsesIPv4Only(t *testing.T) {
	base := Options{Node: Node{Server: "::ffff:112.95.75.230", Username: "user", Password: "secret"}}
	normalized, _, err := normalizeOptions(base)
	if err != nil {
		t.Fatal(err)
	}
	if normalized.Node.Server != "112.95.75.230" {
		t.Fatalf("normalized server = %q", normalized.Node.Server)
	}

	base.Node.Server = "2001:db8::1"
	if _, _, err := normalizeOptions(base); !errors.Is(err, ErrInvalidOptions) {
		t.Fatalf("IPv6 server error = %v, want ErrInvalidOptions", err)
	}
}

func TestNormalizeOptionsEnforcesIPv4MinimumMTU(t *testing.T) {
	for _, mtu := range []uint16{46, 67, 68} {
		_, _, err := normalizeOptions(Options{Node: Node{
			Server: "127.0.0.1", Username: "user", Password: "secret", MTU: mtu,
		}})
		if mtu < 68 && !errors.Is(err, ErrInvalidOptions) {
			t.Fatalf("normalizeOptions(MTU=%d) error = %v, want ErrInvalidOptions", mtu, err)
		}
		if mtu == 68 && err != nil {
			t.Fatalf("normalizeOptions(MTU=68): %v", err)
		}
	}
}
