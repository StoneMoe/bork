package protocol

import (
	"bytes"
	"crypto/aes"
	"crypto/cipher"
	"testing"
)

func TestLeaveRoundTripAndAuthentication(t *testing.T) {
	protector := newTestProtector(t, 1)
	sessionID := [16]byte{2}
	packet, err := MarshalControl(PacketLeave, sessionID, 3, 0, protector)
	if err != nil {
		t.Fatal(err)
	}
	pingSequence, err := ParseControl(packet, sessionID, protector)
	if err != nil {
		t.Fatal(err)
	}
	if pingSequence != 0 {
		t.Fatalf("leave decoded with ping sequence %d", pingSequence)
	}

	packet[len(packet)-1] ^= 1
	if _, err := ParseControl(packet, sessionID, protector); err == nil {
		t.Fatal("tampered leave packet was accepted")
	}
}

func TestLeaveCanBeBridgeInner(t *testing.T) {
	sessionID := [16]byte{2}
	leave, err := MarshalControl(PacketLeave, sessionID, 3, 0, newTestProtector(t, 1))
	if err != nil {
		t.Fatal(err)
	}

	target := [16]byte{5}
	bridgeProtector := newTestProtector(t, 2)
	packet, err := MarshalBridge(sessionID, 6, target, false, leave, bridgeProtector)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := ParseBridge(packet, sessionID, bridgeProtector); err != nil {
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
