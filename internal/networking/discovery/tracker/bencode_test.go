package tracker

import (
	"bytes"
	"context"
	"encoding/binary"
	"fmt"
	"net/netip"
	"slices"
	"testing"
	"time"
)

func TestParseHTTPAnnounceResponsePeerList(t *testing.T) {
	packet := []byte(
		"d8:intervali60e5:peersl" +
			"d2:ip7:1.1.1.17:peer id20:ABCDEFGHIJKLMNOPQRST4:porti6881ee" +
			"d2:ip20:2001:4860:4860::88887:peer id20:QRSTUVWXYZABCDEFGHIJ4:porti443ee" +
			"e6:peers618:",
	)
	address := netip.MustParseAddr("2001:4860:4860::8888").As16()
	packet = append(packet, address[:]...)
	port := [2]byte{}
	binary.BigEndian.PutUint16(port[:], 443)
	packet = append(packet, port[:]...)
	packet = append(packet, 'e')

	response, err := parseHTTPAnnounceResponse(packet)
	if err != nil {
		t.Fatal(err)
	}
	want := []netip.AddrPort{
		netip.MustParseAddrPort("1.1.1.1:6881"),
		netip.MustParseAddrPort("[2001:4860:4860::8888]:443"),
	}
	if response.interval != time.Minute {
		t.Fatalf("interval = %s, want %s", response.interval, time.Minute)
	}
	if !slices.Equal(response.peers, want) {
		t.Fatalf("peers = %v, want %v", response.peers, want)
	}
}

func TestParseHTTPAnnounceResponseCompactPeers(t *testing.T) {
	packet := append([]byte("d8:intervali60e5:peers6:"), 1, 1, 1, 1, 0x1a, 0xe1)
	packet = append(packet, 'e')

	response, err := parseHTTPAnnounceResponse(packet)
	if err != nil {
		t.Fatal(err)
	}
	want := []netip.AddrPort{netip.MustParseAddrPort("1.1.1.1:6881")}
	if !slices.Equal(response.peers, want) {
		t.Fatalf("peers = %v, want %v", response.peers, want)
	}
}

func TestResolveHTTPAnnouncePeerName(t *testing.T) {
	packet := []byte(
		"d8:intervali300e5:peersl" +
			"d2:ip12:peer.example7:peer id20:ABCDEFGHIJKLMNOPQRST4:porti6881eeee",
	)
	response, err := parseHTTPAnnounceResponse(packet)
	if err != nil {
		t.Fatal(err)
	}
	announcer := Announcer{lookupNetIP: func(_ context.Context, network, name string) ([]netip.Addr, error) {
		if network != "ip" || name != "peer.example" {
			t.Fatalf("lookup = %q %q", network, name)
		}
		return []netip.Addr{netip.MustParseAddr("1.1.1.1")}, nil
	}}
	response = announcer.resolveHTTPPeerNames(context.Background(), response)
	want := []netip.AddrPort{netip.MustParseAddrPort("1.1.1.1:6881")}
	if !slices.Equal(response.peers, want) {
		t.Fatalf("peers = %v, want %v", response.peers, want)
	}
}

func TestParseHTTPAnnounceResponseLargePeerList(t *testing.T) {
	var packet bytes.Buffer
	packet.WriteString("d8:intervali60e5:peersl")
	for index := range 150 {
		address := fmt.Sprintf("1.1.%d.%d", index/250, index%250+1)
		fmt.Fprintf(&packet, "d2:ip%d:%s7:peer id20:ABCDEFGHIJKLMNOPQRST4:porti6881ee", len(address), address)
	}
	packet.WriteString("ee")

	response, err := parseHTTPAnnounceResponse(packet.Bytes())
	if err != nil {
		t.Fatal(err)
	}
	if len(response.peers) != maxAnnouncePeers {
		t.Fatalf("peer count = %d, want %d", len(response.peers), maxAnnouncePeers)
	}
}
