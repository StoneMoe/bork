//go:build game_proxy

package netfilter

import (
	"errors"
	"net/netip"
	"slices"
	"testing"
)

func TestTCPOwnerFromTable_matches_full_tuple_and_decodes_network_order(t *testing.T) {
	local := netip.MustParseAddrPort("127.0.0.2:41001")
	remote := netip.MustParseAddrPort("127.0.0.1:43017")
	table := []byte{
		1, 0, 0, 0, // Row count.
		5, 0, 0, 0, // ESTABLISHED.
		127, 0, 0, 2,
		0xa0, 0x29, 0xff, 0xcc, // Only the low WORD holds the port.
		127, 0, 0, 1,
		0xa8, 0x09, 0xdd, 0xee,
		0x78, 0x56, 0x34, 0x12,
	}
	pid, err := tcpOwnerFromTable(table, local, remote)
	if err != nil || pid != 0x12345678 {
		t.Fatalf("owner = %x, %v", pid, err)
	}
	crowded := append(slices.Clone(table), table[4:]...)
	crowded[0] = 2
	crowded[11] = 3 // Another process using the same source port on a different address.
	crowded[24] = 0x99
	pid, err = tcpOwnerFromTable(crowded, local, remote)
	if err != nil || pid != 0x12345678 {
		t.Fatalf("owner among same-port rows = %x, %v", pid, err)
	}
	for _, test := range []struct {
		name   string
		mutate func([]byte) []byte
	}{
		{"local address", func(data []byte) []byte { data[11]++; return data }},
		{"local port", func(data []byte) []byte { data[13]++; return data }},
		{"remote address", func(data []byte) []byte { data[19]++; return data }},
		{"remote port", func(data []byte) []byte { data[21]++; return data }},
		{"missing owner", func(data []byte) []byte { clear(data[24:28]); return data }},
		{"time wait", func(data []byte) []byte { data[4] = 11; return data }},
		{"truncated header", func(data []byte) []byte { return data[:3] }},
		{"truncated row", func(data []byte) []byte { return data[:27] }},
		{"invalid count", func(data []byte) []byte { data[3] = 0xff; return data }},
		{"duplicate tuple", func(data []byte) []byte { data[0] = 2; return append(data, data[4:]...) }},
	} {
		t.Run(test.name, func(t *testing.T) {
			pid, err := tcpOwnerFromTable(test.mutate(slices.Clone(table)), local, remote)
			if pid != 0 || !errors.Is(err, ErrTCPRedirectOwner) {
				t.Fatalf("owner = %d, %v, want rejection", pid, err)
			}
		})
	}
}
