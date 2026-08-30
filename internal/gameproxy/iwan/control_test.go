package iwan

import (
	"bytes"
	"errors"
	"testing"
	"time"
)

func TestControlPackets_matchPinnedGoldenVectors(t *testing.T) {
	session := goldenSession(t)
	echo := Echo{
		Timestamp:    time.Unix(10, 0),
		CurrentDelay: 7,
		MinimumDelay: 8,
		MaximumDelay: 9,
		RouteMagic:   0x10203040,
	}

	request := BuildEchoRequest(session, echo)
	wantRequest := mustDecodeHex(t, "15001234deadbeef99c0c09fe8ad8af7b76c4418673294ad809698000000000007000000080000000900000000000000544452001020304000000000")
	if !bytes.Equal(request, wantRequest) {
		t.Fatalf("BuildEchoRequest() = %x, want %x", request, wantRequest)
	}
	response, err := BuildEchoResponse(session, request)
	if err != nil {
		t.Fatal(err)
	}
	wantResponse := mustDecodeHex(t, "16001234deadbeef5062e351c140950caebdaf8f18261f8b809698000000000007000000080000000900000000000000544452001020304000000000")
	if !bytes.Equal(response, wantResponse) {
		t.Fatalf("BuildEchoResponse() = %x, want %x", response, wantResponse)
	}
	if closePacket := BuildClose(session); !bytes.Equal(closePacket, mustDecodeHex(t, "17001234deadbeef2137996b1a1445ce03e0310dcae2d080")) {
		t.Fatalf("BuildClose() = %x", closePacket)
	}
	if reject := BuildOpenReject(); !bytes.Equal(reject, mustDecodeHex(t, "1100000000000000c31a4db76082665496e60062b9c50424")) {
		t.Fatalf("BuildOpenReject() = %x", reject)
	}
}

func TestParseControl_validatesSignatureTypeAndSession(t *testing.T) {
	session := goldenSession(t)
	request := BuildEchoRequest(session, Echo{Timestamp: time.Unix(10, 0)})
	response, err := BuildEchoResponse(session, request)
	if err != nil {
		t.Fatal(err)
	}

	parsed, err := ParseControl(response, session)
	if err != nil {
		t.Fatal(err)
	}
	if parsed.Type != TypeEchoResponse || !parsed.Echo.Timestamp.Equal(time.Unix(10, 0)) {
		t.Fatalf("ParseControl() = %#v", parsed)
	}
	corrupt := append([]byte(nil), response...)
	corrupt[8] ^= 1
	if _, err := ParseControl(corrupt, session); !errors.Is(err, ErrInvalidSignature) {
		t.Fatalf("ParseControl() error = %v, want ErrInvalidSignature", err)
	}
	unknown := append([]byte(nil), response...)
	unknown[0] = 0x7f
	signControl(unknown)
	if _, err := ParseControl(unknown, session); !errors.Is(err, ErrUnknownPacketType) {
		t.Fatalf("ParseControl() error = %v, want ErrUnknownPacketType", err)
	}
	mismatch := append([]byte(nil), response...)
	mismatch[2] ^= 1
	signControl(mismatch)
	if _, err := ParseControl(mismatch, session); !errors.Is(err, ErrSessionMismatch) {
		t.Fatalf("ParseControl() error = %v, want ErrSessionMismatch", err)
	}
}

func TestParseOpenReject_acceptsOnlyExactSignedPacket(t *testing.T) {
	packet := BuildOpenReject()
	if err := ParseOpenReject(packet); err != nil {
		t.Fatal(err)
	}
	if err := ParseOpenReject(append(packet, 0)); err == nil {
		t.Fatal("ParseOpenReject accepted trailing data")
	}
}
