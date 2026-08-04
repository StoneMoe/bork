package peer

import (
	"net/netip"
	"testing"
)

func TestDirectPathRouteUsesAddress(t *testing.T) {
	first, err := NewPath(netip.MustParseAddrPort("[::ffff:198.51.100.1]:40000"))
	if err != nil {
		t.Fatal(err)
	}
	same, _ := NewPath(netip.MustParseAddrPort("198.51.100.1:40000"))
	different, _ := NewPath(netip.MustParseAddrPort("198.51.100.2:40000"))
	if !first.IsDirect() || !first.IsValid() || !first.SameRoute(same) || first.SameRoute(different) {
		t.Fatalf("direct path comparison failed: %+v %+v %+v", first, same, different)
	}
}

func TestBridgePathIdentityIgnoresMutableNextHopAddress(t *testing.T) {
	intermediary := [32]byte{1}
	target := [32]byte{2}
	first, err := NewBridgePath(netip.MustParseAddrPort("198.51.100.1:40000"), intermediary, target)
	if err != nil {
		t.Fatal(err)
	}
	second, err := NewBridgePath(netip.MustParseAddrPort("198.51.100.2:40001"), intermediary, target)
	if err != nil {
		t.Fatal(err)
	}
	different, _ := NewBridgePath(second.Address(), [32]byte{3}, target)
	if first == second || first.IsDirect() || !first.IsValid() || !first.SameRoute(second) || first.SameRoute(different) {
		t.Fatalf("bridge path comparison failed: %+v %+v %+v", first, second, different)
	}
	if first.Intermediary() != intermediary || first.Target() != target {
		t.Fatalf("bridge path endpoints = %x, %x", first.Intermediary(), first.Target())
	}
}

func TestBridgePathRejectsInvalidEndpoints(t *testing.T) {
	address := netip.MustParseAddrPort("198.51.100.1:40000")
	if _, err := NewBridgePath(address, [32]byte{}, [32]byte{2}); err == nil {
		t.Fatal("zero intermediary was accepted")
	}
	if _, err := NewBridgePath(address, [32]byte{1}, [32]byte{1}); err == nil {
		t.Fatal("equal bridge endpoints were accepted")
	}
}
