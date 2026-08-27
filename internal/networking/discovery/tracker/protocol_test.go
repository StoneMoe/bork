package tracker

import (
	"net/netip"
	"net/url"
	"strconv"
	"testing"

	"bork/internal/identity"
)

func TestValidateProviderURLRejectsUDP(t *testing.T) {
	if err := ValidateProviderURL("udp://tracker.example:6969/announce"); err == nil {
		t.Fatal("UDP tracker URL must be rejected")
	}
}

func TestTrackerRegistrationIdentityIsSharedAcrossAddresses(t *testing.T) {
	configured, err := parseProvider("https://tracker.example/announce")
	if err != nil {
		t.Fatal(err)
	}
	announcer := Announcer{infoHash: [20]byte{1}, peerID: identity.PeerID{2}}
	ipv4 := announcer.registration(configured, AnnounceCandidate{
		Address: netip.MustParseAddr("1.1.1.1"), Port: 6881,
	})
	ipv6 := announcer.registration(configured, AnnounceCandidate{
		Address: netip.MustParseAddr("2606:4700:4700::1111"), Port: 6881,
	})
	if ipv4.peerID != ipv6.peerID || ipv4.key != ipv6.key {
		t.Fatal("dual-stack registrations must share peer ID and key")
	}
	request, err := announcer.buildHTTPAnnounceURL(configured, ipv4, "")
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
