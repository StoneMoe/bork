package protocol

import "testing"

func TestWireV1RejectsOtherVersionsAndZeroEstablishedSequence(t *testing.T) {
	wrongMagic := make([]byte, prefixSize)
	copy(wrongMagic, []byte{'B', 'R', 'K', '2', Version, byte(PacketHello)})
	if _, _, err := ParsePrefix(wrongMagic); err == nil {
		t.Fatal("ParsePrefix() accepted wire v2 magic")
	}
	wrongVersion := make([]byte, prefixSize)
	copy(wrongVersion, []byte{'B', 'R', 'K', '1', 2, byte(PacketHello)})
	if _, _, err := ParsePrefix(wrongVersion); err == nil {
		t.Fatal("ParsePrefix() accepted wire version 2")
	}
	packet := appendEstablishedHeader(nil, PacketVoice, [16]byte{1}, [16]byte{2}, 0)
	if _, err := ParseEstablishedHeader(packet); err == nil {
		t.Fatal("ParseEstablishedHeader() accepted sequence zero")
	}
}

func TestEstablishedHeaderKeepsRoomRoutingFieldsClear(t *testing.T) {
	roomTag := [16]byte{1, 2}
	sessionID := [16]byte{3, 4}
	packet := appendEstablishedHeader(nil, PacketVoice, roomTag, sessionID, 9)
	header, err := ParseEstablishedHeader(packet)
	if err != nil {
		t.Fatal(err)
	}
	if header.RoomTag != roomTag || header.SessionID != sessionID || header.Sequence != 9 {
		t.Fatalf("header = %#v", header)
	}
}

func TestValidPacketSizeRejectsPrefixOnlyPackets(t *testing.T) {
	if ValidPacketSize(PacketHello, prefixSize) || ValidPacketSize(PacketPing, prefixSize) || ValidPacketSize(PacketVoice, prefixSize) {
		t.Fatal("ValidPacketSize() accepted a prefix-only packet")
	}
	if !ValidPacketSize(PacketHello, helloPacketSize) || !ValidPacketSize(PacketPing, controlPacketSize) || !ValidPacketSize(PacketVoice, MaxVoicePacketSize) {
		t.Fatal("ValidPacketSize() rejected a valid packet size")
	}
}
