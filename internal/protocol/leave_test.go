package protocol

import (
	"bytes"
	"crypto/aes"
	"crypto/cipher"
	"testing"
)

func TestLeaveRoundTripAndAuthentication(t *testing.T) {
	protector := newTestProtector(t, 1)
	roomTag := [16]byte{1}
	sessionID := [16]byte{2}
	packet, err := MarshalControl(PacketLeave, roomTag, sessionID, 3, 0, protector)
	if err != nil {
		t.Fatal(err)
	}
	decoded, err := ParseControl(packet, roomTag, sessionID, protector)
	if err != nil {
		t.Fatal(err)
	}
	if decoded.Type != PacketLeave {
		t.Fatalf("leave decoded as packet type %d", decoded.Type)
	}

	packet[len(packet)-1] ^= 1
	if _, err := ParseControl(packet, roomTag, sessionID, protector); err == nil {
		t.Fatal("tampered leave packet was accepted")
	}
}

func TestLeaveCanBeBridgeInner(t *testing.T) {
	roomTag := [16]byte{1}
	sessionID := [16]byte{2}
	leave, err := MarshalControl(PacketLeave, roomTag, sessionID, 3, 0, newTestProtector(t, 1))
	if err != nil {
		t.Fatal(err)
	}

	origin := [32]byte{4}
	target := [32]byte{5}
	bridgeProtector := newTestProtector(t, 2)
	packet, err := MarshalBridge(roomTag, sessionID, 6, origin, target, false, leave, bridgeProtector)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := ParseBridge(packet, roomTag, sessionID, bridgeProtector); err != nil {
		t.Fatal(err)
	}
}

func newTestProtector(t *testing.T, keyByte byte) cipher.AEAD {
	t.Helper()
	block, err := aes.NewCipher(bytes.Repeat([]byte{keyByte}, 32))
	if err != nil {
		t.Fatal(err)
	}
	protector, err := cipher.NewGCM(block)
	if err != nil {
		t.Fatal(err)
	}
	return protector
}
