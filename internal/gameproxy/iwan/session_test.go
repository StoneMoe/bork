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
	wantDNS := []netip.Addr{netip.MustParseAddr("8.8.8.8"), netip.MustParseAddr("9.9.9.9")}
	if len(session.DNS) != 2 || session.DNS[0] != wantDNS[0] || session.DNS[1] != wantDNS[1] {
		t.Fatalf("DNS = %v, want %v", session.DNS, wantDNS)
	}
}

func TestParseOpenACK_rejectsMissingOrInvalidRequiredFields(t *testing.T) {
	credentials, err := NewCredentials("myuser", "mypassword")
	if err != nil {
		t.Fatal(err)
	}
	valid := mustDecodeHex(t, "12011234deadbeef623721978dc2931a7569a4c8c6f5b0360304057804060a141e28080301")
	tests := []struct {
		name   string
		packet []byte
	}{
		{name: "zero token", packet: replaceSignedHeader(t, valid, "12010000deadbeef")},
		{name: "zero session", packet: replaceSignedHeader(t, valid, "1201123400000000")},
		{name: "plaintext flag", packet: replaceSignedHeader(t, valid, "12001234deadbeef")},
		{name: "missing MTU", packet: append(append([]byte(nil), valid[:24]...), valid[28:]...)},
		{name: "unspecified IPv4", packet: append(append([]byte(nil), valid[:30]...), 0, 0, 0, 0, 8, 3, 1)},
		{name: "missing XOR acknowledgement", packet: append([]byte(nil), valid[:34]...)},
		{name: "unknown critical TLV", packet: append(append([]byte(nil), valid...), 0x80, 0x02)},
		{name: "truncated TLV", packet: append(append([]byte(nil), valid...), 0x01)},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			if _, err := ParseOpenACK(test.packet, credentials); err == nil {
				t.Fatalf("ParseOpenACK(%x) succeeded", test.packet)
			}
		})
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

func TestSessionData_rejectsPlaintextMismatchAndOversize(t *testing.T) {
	session := goldenSession(t)
	valid := mustDecodeHex(t, "18011234deadbeefca98dbd31af80d23c393")
	tests := []struct {
		name   string
		packet []byte
	}{
		{name: "plaintext type", packet: append([]byte{0x14, 0}, valid[2:]...)},
		{name: "wrong token", packet: append([]byte{0x18, 1, 0xff, 0xff}, valid[4:]...)},
		{name: "wrong session", packet: append([]byte{0x18, 1, 0x12, 0x34, 1, 2, 3, 4}, valid[8:]...)},
		{name: "empty payload", packet: valid[:8]},
		{name: "oversized payload", packet: append(append([]byte(nil), valid[:8]...), make([]byte, int(session.MTU)+1)...)},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			if _, err := session.ParseData(test.packet); err == nil {
				t.Fatalf("ParseData(%x) succeeded", test.packet)
			}
		})
	}
	if _, err := session.BuildData(make([]byte, int(session.MTU)+1)); !errors.Is(err, ErrOversizedPacket) {
		t.Fatalf("BuildData() error = %v, want ErrOversizedPacket", err)
	}
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
