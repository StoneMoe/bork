package tracker

import (
	"encoding/binary"
	"encoding/hex"
	"errors"
	"net/netip"
	"slices"
	"testing"
	"time"
)

func TestConnectCodecVector(t *testing.T) {
	const transaction = 0x12345678
	request := marshalConnectRequest(transaction)
	if got, want := hex.EncodeToString(request), "00000417271019800000000012345678"; got != want {
		t.Fatalf("connect request = %s, want %s", got, want)
	}

	response := mustDecodeHex(t, "00000000123456780102030405060708")
	connectionID, err := parseConnectResponse(response, transaction)
	if err != nil {
		t.Fatalf("parseConnectResponse() error = %v", err)
	}
	if connectionID != 0x0102030405060708 {
		t.Fatalf("connection ID = %016x", connectionID)
	}
}

func TestAnnounceRequestVector(t *testing.T) {
	var infoHash, peerID [20]byte
	for index := range infoHash {
		infoHash[index] = byte(index)
		peerID[index] = byte(index + 20)
	}
	packet := marshalAnnounceRequest(announceRequest{
		connectionID: 0x0102030405060708,
		transaction:  0x11223344,
		infoHash:     infoHash,
		peerID:       peerID,
		event:        eventStarted,
		key:          0xaabbccdd,
		numWant:      99,
		port:         6881,
	})
	want := "01020304050607080000000111223344" +
		"000102030405060708090a0b0c0d0e0f10111213" +
		"1415161718191a1b1c1d1e1f2021222324252627" +
		"000000000000000000000000000000000000000000000000" +
		"0000000200000000aabbccdd000000201ae1"
	if got := hex.EncodeToString(packet); got != want {
		t.Fatalf("announce request =\n%s\nwant\n%s", got, want)
	}
	if len(packet) != announceRequestSize {
		t.Fatalf("announce request length = %d", len(packet))
	}
	if downloaded := binary.BigEndian.Uint64(packet[56:64]); downloaded != 0 {
		t.Fatalf("downloaded = %d", downloaded)
	}
	if left := binary.BigEndian.Uint64(packet[64:72]); left != 0 {
		t.Fatalf("left = %d", left)
	}
	if uploaded := binary.BigEndian.Uint64(packet[72:80]); uploaded != 0 {
		t.Fatalf("uploaded = %d", uploaded)
	}
	if ip := binary.BigEndian.Uint32(packet[84:88]); ip != 0 {
		t.Fatalf("IP = %d", ip)
	}
	if numWant := int32(binary.BigEndian.Uint32(packet[92:96])); numWant != maxAnnouncePeers {
		t.Fatalf("num_want = %d", numWant)
	}

	negative := marshalAnnounceRequest(announceRequest{numWant: -1})
	if got := binary.BigEndian.Uint32(negative[92:96]); got != 0 {
		t.Fatalf("negative num_want encoded as %d", got)
	}
	explicit := marshalAnnounceRequest(announceRequest{explicitIP: netip.MustParseAddr("8.8.8.8")})
	if got := netip.AddrFrom4([4]byte(explicit[84:88])); got != netip.MustParseAddr("8.8.8.8") {
		t.Fatalf("explicit IP = %s", got)
	}
}

func TestParseAnnounceResponseIPv4FiltersAndDeduplicates(t *testing.T) {
	const transaction = 0x11223344
	packet := announceResponsePacket(transaction, 60, false, []netip.AddrPort{
		netip.MustParseAddrPort("192.0.2.1:6881"),
		netip.MustParseAddrPort("239.1.1.1:6882"),
		netip.MustParseAddrPort("0.0.0.0:6883"),
		netip.MustParseAddrPort("198.51.100.2:0"),
		netip.MustParseAddrPort("192.168.1.10:6884"),
		netip.MustParseAddrPort("127.0.0.1:6885"),
		netip.MustParseAddrPort("192.0.2.1:6881"),
	})
	response, err := parseAnnounceResponse(packet, transaction, false)
	if err != nil {
		t.Fatalf("parseAnnounceResponse() error = %v", err)
	}
	if response.interval != time.Minute {
		t.Fatalf("interval = %v", response.interval)
	}
	want := []netip.AddrPort{netip.MustParseAddrPort("192.0.2.1:6881")}
	if !slices.Equal(response.peers, want) {
		t.Fatalf("peers = %v, want %v", response.peers, want)
	}
}

func TestParseAnnounceResponseIPv6(t *testing.T) {
	const transaction = 9
	packet := announceResponsePacket(transaction, 120, true, []netip.AddrPort{
		netip.MustParseAddrPort("[2001:db8::1]:7000"),
		netip.MustParseAddrPort("[ff02::1]:7001"),
		netip.MustParseAddrPort("[::]:7002"),
		netip.MustParseAddrPort("[fd00::1]:7003"),
	})
	response, err := parseAnnounceResponse(packet, transaction, true)
	if err != nil {
		t.Fatalf("parseAnnounceResponse() error = %v", err)
	}
	want := []netip.AddrPort{netip.MustParseAddrPort("[2001:db8::1]:7000")}
	if !slices.Equal(response.peers, want) {
		t.Fatalf("peers = %v, want %v", response.peers, want)
	}
}

func TestParseAnnounceResponseCapsPeers(t *testing.T) {
	peers := make([]netip.AddrPort, 0, maxAnnouncePeers+10)
	for index := 1; index <= maxAnnouncePeers+10; index++ {
		peers = append(peers, netip.AddrPortFrom(netip.AddrFrom4([4]byte{198, 51, 100, byte(index)}), uint16(6000+index)))
	}
	response, err := parseAnnounceResponse(announceResponsePacket(7, 30, false, peers), 7, false)
	if err != nil {
		t.Fatalf("parseAnnounceResponse() error = %v", err)
	}
	if len(response.peers) != maxAnnouncePeers {
		t.Fatalf("peer count = %d, want %d", len(response.peers), maxAnnouncePeers)
	}
}

func TestTrackerErrorResponse(t *testing.T) {
	packet := make([]byte, responseHeaderSize+len("not registered"))
	binary.BigEndian.PutUint32(packet[0:4], actionError)
	binary.BigEndian.PutUint32(packet[4:8], 42)
	copy(packet[8:], "not registered")
	_, err := parseConnectResponse(packet, 42)
	var trackerErr *TrackerError
	if !errors.As(err, &trackerErr) {
		t.Fatalf("error = %v, want TrackerError", err)
	}
	if trackerErr.Message != "not registered" {
		t.Fatalf("tracker error message = %q", trackerErr.Message)
	}

	binary.BigEndian.PutUint32(packet[4:8], 43)
	_, err = parseConnectResponse(packet, 42)
	if errors.As(err, &trackerErr) {
		t.Fatalf("mismatched transaction accepted as tracker error: %v", err)
	}
}

func TestProtocolRejectsMalformedResponses(t *testing.T) {
	connect := make([]byte, connectResponseSize)
	binary.BigEndian.PutUint32(connect[4:8], 5)
	announce := make([]byte, announceResponseHead)
	binary.BigEndian.PutUint32(announce[0:4], actionAnnounce)
	binary.BigEndian.PutUint32(announce[4:8], 5)

	tests := []struct {
		name  string
		parse func([]byte) error
		data  []byte
	}{
		{name: "empty", data: nil, parse: func(packet []byte) error { _, err := parseConnectResponse(packet, 5); return err }},
		{name: "short header", data: make([]byte, 7), parse: func(packet []byte) error { _, err := parseConnectResponse(packet, 5); return err }},
		{name: "connect truncated", data: connect[:15], parse: func(packet []byte) error { _, err := parseConnectResponse(packet, 5); return err }},
		{name: "connect trailing", data: append(append([]byte(nil), connect...), 0), parse: func(packet []byte) error { _, err := parseConnectResponse(packet, 5); return err }},
		{name: "wrong action", data: append([]byte(nil), connect...), parse: func(packet []byte) error {
			binary.BigEndian.PutUint32(packet[0:4], actionAnnounce)
			_, err := parseConnectResponse(packet, 5)
			return err
		}},
		{name: "wrong transaction", data: append([]byte(nil), connect...), parse: func(packet []byte) error {
			binary.BigEndian.PutUint32(packet[4:8], 6)
			_, err := parseConnectResponse(packet, 5)
			return err
		}},
		{name: "announce truncated", data: announce[:19], parse: func(packet []byte) error { _, err := parseAnnounceResponse(packet, 5, false); return err }},
		{name: "IPv4 peer fragment", data: append(append([]byte(nil), announce...), 1), parse: func(packet []byte) error { _, err := parseAnnounceResponse(packet, 5, false); return err }},
		{name: "IPv6 peer fragment", data: append(append([]byte(nil), announce...), make([]byte, 17)...), parse: func(packet []byte) error { _, err := parseAnnounceResponse(packet, 5, true); return err }},
		{name: "oversized", data: oversizedResponse(5), parse: func(packet []byte) error { _, err := parseAnnounceResponse(packet, 5, false); return err }},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			if err := test.parse(test.data); err == nil {
				t.Fatal("parse error = nil")
			}
		})
	}
}

func FuzzParseConnectResponse(f *testing.F) {
	f.Add(mustDecodeHex(f, "00000000123456780102030405060708"), uint32(0x12345678))
	f.Add([]byte("malformed"), uint32(1))
	f.Fuzz(func(t *testing.T, packet []byte, transaction uint32) {
		_, _ = parseConnectResponse(packet, transaction)
	})
}

func FuzzParseAnnounceResponse(f *testing.F) {
	f.Add(announceResponsePacket(3, 60, false, []netip.AddrPort{netip.MustParseAddrPort("192.0.2.1:6881")}), uint32(3), false)
	f.Add([]byte("malformed"), uint32(1), true)
	f.Fuzz(func(t *testing.T, packet []byte, transaction uint32, ipv6 bool) {
		_, _ = parseAnnounceResponse(packet, transaction, ipv6)
	})
}

type fatalf interface {
	Helper()
	Fatalf(string, ...any)
}

func mustDecodeHex(t fatalf, encoded string) []byte {
	t.Helper()
	decoded, err := hex.DecodeString(encoded)
	if err != nil {
		t.Fatalf("decode test vector: %v", err)
	}
	return decoded
}

func announceResponsePacket(transaction, interval uint32, ipv6 bool, peers []netip.AddrPort) []byte {
	peerSize := 6
	if ipv6 {
		peerSize = 18
	}
	packet := make([]byte, announceResponseHead, announceResponseHead+len(peers)*peerSize)
	binary.BigEndian.PutUint32(packet[0:4], actionAnnounce)
	binary.BigEndian.PutUint32(packet[4:8], transaction)
	binary.BigEndian.PutUint32(packet[8:12], interval)
	for _, peer := range peers {
		if ipv6 {
			address := peer.Addr().As16()
			packet = append(packet, address[:]...)
		} else {
			address := peer.Addr().As4()
			packet = append(packet, address[:]...)
		}
		packet = binary.BigEndian.AppendUint16(packet, peer.Port())
	}
	return packet
}

func oversizedResponse(transaction uint32) []byte {
	packet := make([]byte, maxTrackerResponseSize+1)
	binary.BigEndian.PutUint32(packet[0:4], actionAnnounce)
	binary.BigEndian.PutUint32(packet[4:8], transaction)
	return packet
}
