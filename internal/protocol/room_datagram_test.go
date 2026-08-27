package protocol

import (
	"bytes"
	"testing"
)

func TestParseRoomDatagramPreservesForwardedPacket(t *testing.T) {
	protector := NewRoomDatagramCipher([32]byte{1})
	header := RoomDatagramHeader{
		Type: PacketVoice, StreamID: [16]byte{2}, PacketSequence: 3,
	}
	packet, err := MarshalRoomDatagram(header, 4, []byte{5}, protector)
	if err != nil {
		t.Fatal(err)
	}
	original := bytes.Clone(packet)
	decoded, err := ParseRoomDatagram(packet, header, protector)
	if err != nil {
		t.Fatal(err)
	}
	if decoded.MediaUnitID != 4 || !bytes.Equal(decoded.Payload, []byte{5}) {
		t.Fatal("room datagram did not round trip")
	}
	if !bytes.Equal(packet, original) {
		t.Fatal("parsing changed the datagram that forwarders resend")
	}
}
