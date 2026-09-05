//go:build game_proxy

package iwan

import (
	"bytes"
	"errors"
	"net/netip"
	"testing"
)

func TestParseOpenACK_matchesPinnedGoldenVector(t *testing.T) {
	credentials, err := NewCredentials("myuser", "mypassword")
	if err != nil {
		t.Fatal(err)
	}
	packet := mustDecodeHex(t, "12011234deadbeef623721978dc2931a7569a4c8c6f5b0360304057804060a141e28050608080808060a0808080809090909080301")

	session, err := ParseOpenACK(packet, credentials)
	if err != nil {
		t.Fatal(err)
	}

	if session.Token != (Token{0x12, 0x34}) || session.ID != (SessionID{0xde, 0xad, 0xbe, 0xef}) {
		t.Fatalf("session identity = %x/%x", session.Token, session.ID)
	}
	if session.MTU != 1400 || session.Address != netip.MustParseAddr("10.20.30.40") {
		t.Fatalf("session parameters = MTU %d, address %s", session.MTU, session.Address)
	}
	if !session.Encrypt {
		t.Fatal("session did not enable negotiated encryption")
	}
	wantDNS := []netip.Addr{netip.MustParseAddr("8.8.8.8"), netip.MustParseAddr("9.9.9.9")}
	if len(session.DNS) != 2 || session.DNS[0] != wantDNS[0] || session.DNS[1] != wantDNS[1] {
		t.Fatalf("DNS = %v, want %v", session.DNS, wantDNS)
	}
}

func TestParseOpenACK_acceptsReferenceOptionalFields(t *testing.T) {
	credentials, err := NewCredentials("myuser", "mypassword")
	if err != nil {
		t.Fatal(err)
	}
	packet := mustDecodeHex(t, "12000000000000005fc1ba606ada994907572530eeb780c404060a141e28800201")
	session, err := ParseOpenACK(packet, credentials)
	if err != nil {
		t.Fatal(err)
	}
	if session.Token != (Token{}) || session.ID != (SessionID{}) || session.MTU != 0 || session.Encrypt {
		t.Fatalf("optional OPENACK fields = %#v", session)
	}
	if session.Address != netip.MustParseAddr("10.20.30.40") {
		t.Fatalf("address = %v", session.Address)
	}
}

func TestParseOpenACK_rejectsInvalidSignatureAndMissingAddress(t *testing.T) {
	credentials, err := NewCredentials("myuser", "mypassword")
	if err != nil {
		t.Fatal(err)
	}
	missingAddress := mustDecodeHex(t, "12001234deadbeef1070b455b0e4219473084222b15c73ba03040578")
	badSignature := mustDecodeHex(t, "12001234deadbeef1070b455b0e4219473084222b15c73ba04060a141e28")
	badSignature[8] ^= 1
	for _, packet := range [][]byte{missingAddress, badSignature, packetWithUnspecifiedAddress(t)} {
		if _, err := ParseOpenACK(packet, credentials); err == nil {
			t.Fatalf("ParseOpenACK(%x) succeeded", packet)
		}
	}
}

func TestSessionData_matchesPinnedXORVector(t *testing.T) {
	session := goldenSession(t)

	packet, err := session.BuildData([]byte("hello iwan"))
	if err != nil {
		t.Fatal(err)
	}
	want := mustDecodeHex(t, "18011234deadbeefca98dbd31af80d23c393")
	if !bytes.Equal(packet, want) {
		t.Fatalf("BuildData() = %x, want %x", packet, want)
	}
	payload, err := session.ParseData(packet)
	if err != nil {
		t.Fatal(err)
	}
	if string(payload) != "hello iwan" {
		t.Fatalf("ParseData() = %q", payload)
	}
}

func TestSessionData_matchesPinnedPlaintextVector(t *testing.T) {
	session := plaintextGoldenSession(t)
	packet, err := session.BuildData([]byte("hello iwan"))
	if err != nil {
		t.Fatal(err)
	}
	want := mustDecodeHex(t, "14001234deadbeef68656c6c6f206977616e")
	if !bytes.Equal(packet, want) {
		t.Fatalf("BuildData() = %x, want %x", packet, want)
	}
	payload, err := session.ParseData(packet)
	if err != nil {
		t.Fatal(err)
	}
	if string(payload) != "hello iwan" {
		t.Fatalf("ParseData() = %q", payload)
	}
}

func TestSessionData_acceptsReferenceIdentityAndRejectsEmpty(t *testing.T) {
	session := goldenSession(t)
	valid := mustDecodeHex(t, "18011234deadbeefca98dbd31af80d23c393")
	tests := []struct {
		name   string
		packet []byte
	}{
		{name: "wrong token", packet: append([]byte{0x18, 1, 0xff, 0xff}, valid[4:]...)},
		{name: "wrong session", packet: append([]byte{0x18, 1, 0x12, 0x34, 1, 2, 3, 4}, valid[8:]...)},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			if payload, err := session.ParseData(test.packet); err != nil || string(payload) != "hello iwan" {
				t.Fatalf("ParseData(%x) = %q, %v", test.packet, payload, err)
			}
		})
	}
	if _, err := session.ParseData(valid[:8]); !errors.Is(err, ErrMalformedPacket) {
		t.Fatalf("ParseData(empty) error = %v, want ErrMalformedPacket", err)
	}
	if _, err := session.BuildData(make([]byte, int(session.MTU)+1)); !errors.Is(err, ErrOversizedPacket) {
		t.Fatalf("BuildData() error = %v, want ErrOversizedPacket", err)
	}
}

func TestSessionData_accepts_inbound_payload_above_outbound_MTU(t *testing.T) {
	session := goldenSession(t)
	packet := make([]byte, headerSize+int(session.MTU)+1)
	writeHeader(packet, wireHeader{typ: TypeDataXOR, flags: 1, token: session.Token, session: session.ID})
	xorBytes(session.xorKey, packet[headerSize:])

	payload, err := session.ParseData(packet)
	if err != nil {
		t.Fatal(err)
	}
	if len(payload) != int(session.MTU)+1 {
		t.Fatalf("inbound payload length = %d, want %d", len(payload), int(session.MTU)+1)
	}
}

func plaintextGoldenSession(t testing.TB) Session {
	t.Helper()
	credentials, err := NewCredentials("myuser", "mypassword")
	if err != nil {
		t.Fatal(err)
	}
	packet := mustDecodeHex(t, "12001234deadbeef1070b455b0e4219473084222b15c73ba0304057804060a141e28")
	session, err := ParseOpenACK(packet, credentials)
	if err != nil {
		t.Fatal(err)
	}
	return session
}

func packetWithUnspecifiedAddress(t testing.TB) []byte {
	t.Helper()
	packet := mustDecodeHex(t, "12001234deadbeef1070b455b0e4219473084222b15c73ba040600000000")
	return packet
}

func goldenSession(t testing.TB) Session {
	t.Helper()
	credentials, err := NewCredentials("myuser", "mypassword")
	if err != nil {
		t.Fatal(err)
	}
	packet := mustDecodeHex(t, "12011234deadbeef623721978dc2931a7569a4c8c6f5b0360304057804060a141e28080301")
	session, err := ParseOpenACK(packet, credentials)
	if err != nil {
		t.Fatal(err)
	}
	return session
}

func replaceSignedHeader(t *testing.T, packet []byte, headerHex string) []byte {
	t.Helper()
	header := mustDecodeHex(t, headerHex)
	copyPacket := append([]byte(nil), packet...)
	copy(copyPacket[:8], header)
	signControl(copyPacket)
	return copyPacket
}
