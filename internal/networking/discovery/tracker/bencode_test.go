package tracker

import (
	"bytes"
	"errors"
	"net/netip"
	"slices"
	"strconv"
	"strings"
	"testing"
	"time"
)

func TestParseHTTPAnnounceResponse(t *testing.T) {
	compact4 := compactPeerBytes(false, []netip.AddrPort{
		netip.MustParseAddrPort("198.51.100.1:5000"),
		netip.MustParseAddrPort("239.1.1.1:5001"),
		netip.MustParseAddrPort("198.51.100.1:5000"),
	})
	compact6 := compactPeerBytes(true, []netip.AddrPort{
		netip.MustParseAddrPort("[2001:db8::1]:6000"),
		netip.MustParseAddrPort("[ff02::1]:6001"),
	})
	packet := []byte("d8:completei2e5:extrad1:ali1e3:twoee8:intervali60e5:peers")
	packet = appendBencodeString(packet, compact4)
	packet = append(packet, "6:peers6"...)
	packet = appendBencodeString(packet, compact6)
	packet = append(packet, "15:warning message2:oke"...)

	response, err := parseHTTPAnnounceResponse(packet)
	if err != nil {
		t.Fatalf("parseHTTPAnnounceResponse() error = %v", err)
	}
	if response.interval != time.Minute {
		t.Fatalf("interval = %v", response.interval)
	}
	want := []netip.AddrPort{
		netip.MustParseAddrPort("198.51.100.1:5000"),
		netip.MustParseAddrPort("[2001:db8::1]:6000"),
	}
	if !slices.Equal(response.peers, want) {
		t.Fatalf("peers = %v, want %v", response.peers, want)
	}
}

func TestParseHTTPAnnounceFailure(t *testing.T) {
	packet := []byte("d14:failure reason11:not allowede")
	_, err := parseHTTPAnnounceResponse(packet)
	var trackerErr *TrackerError
	if !errors.As(err, &trackerErr) || trackerErr.Message != "not allowed" {
		t.Fatalf("error = %#v", err)
	}

	tooLong := []byte("d14:failure reason")
	tooLong = appendBencodeString(tooLong, bytes.Repeat([]byte{'x'}, maxFailureReasonLength+1))
	tooLong = append(tooLong, 'e')
	if _, err := parseHTTPAnnounceResponse(tooLong); err == nil {
		t.Fatal("oversized failure reason was accepted")
	}
}

func TestParseHTTPAnnounceResponseCapsCombinedPeers(t *testing.T) {
	ipv4 := make([]netip.AddrPort, 0, maxAnnouncePeers)
	for index := 1; index <= maxAnnouncePeers; index++ {
		ipv4 = append(ipv4, netip.AddrPortFrom(netip.AddrFrom4([4]byte{198, 51, 100, byte(index)}), uint16(5000+index)))
	}
	ipv6 := []netip.AddrPort{netip.MustParseAddrPort("[2001:db8::1]:6000")}
	response, err := parseHTTPAnnounceResponse(httpBencodeResponse(30, ipv4, ipv6))
	if err != nil {
		t.Fatalf("parseHTTPAnnounceResponse() error = %v", err)
	}
	if len(response.peers) != maxAnnouncePeers {
		t.Fatalf("peer count = %d, want %d", len(response.peers), maxAnnouncePeers)
	}
}

func TestParseHTTPAnnounceResponseRejectsMalformedBencode(t *testing.T) {
	valid := httpBencodeResponse(30, nil, nil)
	tooMany := []byte("d1:al")
	for range maxBencodeItems + 1 {
		tooMany = append(tooMany, "0:"...)
	}
	tooMany = append(tooMany, "e8:intervali1ee"...)
	tooDeep := []byte("d1:a")
	tooDeep = append(tooDeep, strings.Repeat("l", maxBencodeDepth+1)...)
	tooDeep = append(tooDeep, "0:"...)
	tooDeep = append(tooDeep, strings.Repeat("e", maxBencodeDepth+1)...)
	tooDeep = append(tooDeep, "8:intervali1ee"...)
	longKey := []byte("d129:")
	longKey = append(longKey, bytes.Repeat([]byte{'a'}, 129)...)
	longKey = append(longKey, "0:8:intervali1ee"...)

	tests := map[string][]byte{
		"empty":                 nil,
		"root list":             []byte("le"),
		"missing interval":      []byte("de"),
		"trailing data":         append(append([]byte(nil), valid...), 'x'),
		"leading integer zero":  []byte("d8:intervali01e5:peers0:e"),
		"leading integer plus":  []byte("d8:intervali+1e5:peers0:e"),
		"negative interval":     []byte("d8:intervali-1e5:peers0:e"),
		"oversized interval":    []byte("d8:intervali4294967296e5:peers0:e"),
		"leading string zero":   []byte("d8:intervali1e5:peers00:e"),
		"unordered keys":        []byte("d5:peers0:8:intervali1ee"),
		"duplicate keys":        []byte("d8:intervali1e8:intervali2ee"),
		"wrong interval type":   []byte("d8:interval1:x5:peers0:e"),
		"fragmented IPv4 peer":  []byte("d8:intervali1e5:peers1:xe"),
		"fragmented IPv6 peer":  []byte("d8:intervali1e5:peers0:6:peers61:xe"),
		"truncated dictionary":  []byte("d8:intervali1e"),
		"invalid unknown value": []byte("d1:ax8:intervali1ee"),
		"nested too deeply":     tooDeep,
		"too many items":        tooMany,
		"key too long":          longKey,
		"declared string huge":  []byte("d8:intervali1e5:peers65537:e"),
		"oversized response":    bytes.Repeat([]byte{'x'}, maxHTTPTrackerResponseSize+1),
	}
	for name, packet := range tests {
		t.Run(name, func(t *testing.T) {
			if _, err := parseHTTPAnnounceResponse(packet); err == nil {
				t.Fatal("parse error = nil")
			}
		})
	}
}

func TestHTTPTrackerParsesExternalIP(t *testing.T) {
	response := append([]byte("d11:external ip4:"), []byte{8, 8, 8, 8}...)
	response = append(response, []byte("8:intervali60e5:peers0:e")...)
	parsed, err := parseHTTPAnnounceResponse(response)
	if err != nil {
		t.Fatal(err)
	}
	if parsed.externalAddress != netip.MustParseAddr("8.8.8.8") {
		t.Fatalf("external address = %s", parsed.externalAddress)
	}
}

func FuzzParseHTTPAnnounceResponse(f *testing.F) {
	f.Add(httpBencodeResponse(60, []netip.AddrPort{netip.MustParseAddrPort("192.0.2.1:6881")}, nil))
	f.Add([]byte("not-bencode"))
	f.Fuzz(func(t *testing.T, packet []byte) {
		_, _ = parseHTTPAnnounceResponse(packet)
	})
}

func httpBencodeResponse(interval int, ipv4, ipv6 []netip.AddrPort) []byte {
	packet := []byte("d8:intervali" + strconv.Itoa(interval) + "e5:peers")
	packet = appendBencodeString(packet, compactPeerBytes(false, ipv4))
	if ipv6 != nil {
		packet = append(packet, "6:peers6"...)
		packet = appendBencodeString(packet, compactPeerBytes(true, ipv6))
	}
	return append(packet, 'e')
}

func compactPeerBytes(ipv6 bool, peers []netip.AddrPort) []byte {
	packet := announceResponsePacket(1, 1, ipv6, peers)
	return append([]byte(nil), packet[announceResponseHead:]...)
}

func appendBencodeString(destination, value []byte) []byte {
	destination = strconv.AppendInt(destination, int64(len(value)), 10)
	destination = append(destination, ':')
	return append(destination, value...)
}
