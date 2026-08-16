package portmap

import (
	"encoding/hex"
	"net/netip"
	"testing"
)

func TestBuildPCPMapRequestAddressFamilies(t *testing.T) {
	nonce := [12]byte{0, 1, 2, 3, 4, 5, 6, 7, 8, 9, 10, 11}
	tests := []struct {
		name    string
		local   string
		wantHex string
	}{
		{
			name:    "IPv4",
			local:   "192.168.1.10",
			wantHex: "0201000000000e1000000000000000000000ffffc0a8010a000102030405060708090a0b110000009c409c4000000000000000000000ffff00000000",
		},
		{
			name:    "IPv6",
			local:   "2001:db8:1::1234",
			wantHex: "0201000000000e1020010db8000100000000000000001234000102030405060708090a0b110000009c409c4000000000000000000000000000000000",
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			request := buildPCPMapRequest(netip.MustParseAddr(test.local), nonce, 40000, 40000, netip.Addr{}, 3600)
			if got := hex.EncodeToString(request); got != test.wantHex {
				t.Fatalf("request = %s, want %s", got, test.wantHex)
			}
		})
	}

	deletion := buildPCPMapRequest(netip.MustParseAddr("192.168.1.10"), nonce, 40000, 0, netip.Addr{}, 0)
	if got := hex.EncodeToString(deletion[42:60]); got != "000000000000000000000000000000000000" {
		t.Fatalf("deletion suggested endpoint = %s, want all zeroes", got)
	}
}

func TestParsePCPMapResponseAcceptsIPv6(t *testing.T) {
	nonce := [12]byte{0, 1, 2, 3, 4, 5, 6, 7, 8, 9, 10, 11}
	message, err := hex.DecodeString("0281000000000e100000002a000000000000000000000000000102030405060708090a0b110000009c40b26e26064700470000000000000000001111")
	if err != nil {
		t.Fatal(err)
	}

	response, err := parsePCPMapResponse(message, nonce, 40000)
	if err != nil {
		t.Fatal(err)
	}
	wantAddress := netip.MustParseAddrPort("[2606:4700:4700::1111]:45678")
	if response.externalAddress != wantAddress || response.lifetime != 3600 || response.epoch != 42 {
		t.Fatalf("response = %+v, want address %s, lifetime 3600, epoch 42", response, wantAddress)
	}
}

func TestPCPDialAddresses(t *testing.T) {
	tests := []struct {
		name        string
		route       pcpRoute
		wantNetwork string
		wantLocal   string
		wantGateway string
	}{
		{
			name:        "IPv4 default gateway",
			route:       pcpRoute{local: netip.MustParseAddr("192.168.1.10"), gateway: netip.MustParseAddr("192.168.1.1")},
			wantNetwork: "udp4",
			wantLocal:   "192.168.1.10:0",
			wantGateway: "192.168.1.1:5351",
		},
		{
			name:        "IPv6 link-local gateway keeps zone",
			route:       pcpRoute{local: netip.MustParseAddr("2001:db8:1::1234"), gateway: netip.MustParseAddr("fe80::1%Ethernet")},
			wantNetwork: "udp6",
			wantLocal:   "[2001:db8:1::1234]:0",
			wantGateway: "[fe80::1%Ethernet]:5351",
		},
		{
			name:        "IPv6 anycast lets OS choose local address",
			route:       pcpRoute{gateway: netip.MustParseAddr(pcpIPv6Anycast)},
			wantNetwork: "udp6",
			wantGateway: "[2001:1::1]:5351",
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			network, local, gateway := pcpDialAddresses(test.route, pcpGatewayPort)
			if network != test.wantNetwork || gateway.String() != test.wantGateway {
				t.Fatalf("dial target = %s %s, want %s %s", network, gateway, test.wantNetwork, test.wantGateway)
			}
			if test.wantLocal == "" {
				if local != nil {
					t.Fatalf("local address = %s, want OS-selected address", local)
				}
			} else if local == nil || local.String() != test.wantLocal {
				t.Fatalf("local address = %v, want %s", local, test.wantLocal)
			}
		})
	}
}
