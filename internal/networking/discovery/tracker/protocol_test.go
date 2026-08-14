package tracker

import (
	"encoding/binary"
	"net/netip"
	"net/url"
	"strconv"
	"testing"
)

func TestUDPIPv6AnnounceWireFormat(t *testing.T) {
	request := marshalAnnounceRequest(announceRequest{
		explicitIP: netip.MustParseAddr("2001:4860:4860::8888"),
	})
	if binary.BigEndian.Uint32(request[84:88]) != 0 {
		t.Fatalf("IPv6 announce IP field = %x, want zero", request[84:88])
	}

	const transaction = uint32(0x12345678)
	packet := make([]byte, announceResponseHead+18)
	binary.BigEndian.PutUint32(packet[0:4], actionAnnounce)
	binary.BigEndian.PutUint32(packet[4:8], transaction)
	if _, err := parseAnnounceResponse(packet, transaction, true); err == nil {
		t.Fatal("zero announce interval must be rejected")
	}
	binary.BigEndian.PutUint32(packet[8:12], 60)
	address := netip.MustParseAddr("2606:4700:4700::1111").As16()
	copy(packet[announceResponseHead:announceResponseHead+16], address[:])
	binary.BigEndian.PutUint16(packet[announceResponseHead+16:], 6881)

	response, err := parseAnnounceResponse(packet, transaction, true)
	if err != nil {
		t.Fatal(err)
	}
	want := netip.MustParseAddrPort("[2606:4700:4700::1111]:6881")
	if len(response.peers) != 1 || response.peers[0] != want {
		t.Fatalf("IPv6 peers = %v, want [%v]", response.peers, want)
	}
}

func TestTrackerRegistrationIdentityIsSharedAcrossAddresses(t *testing.T) {
	configured, err := parseProvider("https://tracker.example/announce")
	if err != nil {
		t.Fatal(err)
	}
	announcer := Announcer{infoHash: [20]byte{1}, identityKey: [32]byte{2}}
	ipv4 := announcer.registration(configured, AnnounceCandidate{
		Address: netip.MustParseAddr("1.1.1.1"), Port: 6881,
	})
	ipv6 := announcer.registration(configured, AnnounceCandidate{
		Address: netip.MustParseAddr("2606:4700:4700::1111"), Port: 6881,
	})
	if ipv4.peerID != ipv6.peerID || ipv4.key != ipv6.key {
		t.Fatal("dual-stack registrations must share peer ID and key")
	}
	request, err := announcer.buildHTTPAnnounceURL(configured, ipv4, eventNone)
	if err != nil {
		t.Fatal(err)
	}
	parsed, err := url.Parse(request)
	if err != nil {
		t.Fatal(err)
	}
	wantKey := strconv.FormatUint(uint64(ipv4.key), 10)
	if key := parsed.Query().Get("key"); key != wantKey {
		t.Fatalf("HTTP announce key = %q, want %q", key, wantKey)
	}
	if err := ValidateProviderURL("https://tracker.example/announce?key=1"); err == nil {
		t.Fatal("configured HTTP tracker key must not replace the generated key")
	}
}
